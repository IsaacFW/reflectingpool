// Package e2e drives the real program through a real browser: it builds the
// binary, starts it on a small tree of files, and works through the interface
// the way a person would. Run it with scripts/e2e.sh.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

var (
	base      string // the server's address
	setupCode string
	root      string // the folder being scanned
	bin       string // the program, built from this source
)

const (
	user     = "isaac"
	password = "correct horse battery"
)

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}

// tree lays out two shares with a film, a picture, a text file, a hardlink and
// one folder with enough files that the list cannot hold them all at once.
func tree(root string) {
	write(filepath.Join(root, "media/movies/alien.mkv"), make([]byte, 5<<20))
	write(filepath.Join(root, "media/movies/short.mp4"), make([]byte, 1<<20))
	var pic bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := range 200 {
		for x := range 320 {
			img.Set(x, y, color.RGBA{uint8(x / 2), uint8(y), 160, 255})
		}
	}
	if err := png.Encode(&pic, img); err != nil {
		panic(err)
	}
	write(filepath.Join(root, "media/pictures/sunset.png"), pic.Bytes())
	write(filepath.Join(root, "docs/notes.txt"), []byte("hello from the notes file\n<script>document.title='run'</script>\n"))
	write(filepath.Join(root, "docs/taxes/2024.zip"), make([]byte, 300<<10))
	write(filepath.Join(root, "docs/taxes/receipt.pdf"), []byte("%PDF-1.1\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\ntrailer<</Root 1 0 R>>\n"))
	if err := os.Link(filepath.Join(root, "media/movies/short.mp4"), filepath.Join(root, "docs/short-link.mp4")); err != nil {
		panic(err)
	}
	write(filepath.Join(root, "media/words/art.txt"), make([]byte, 10))
	write(filepath.Join(root, "media/words/party plans.txt"), make([]byte, 9000))
	write(filepath.Join(root, "media/words/the art of war.txt"), make([]byte, 50))
	write(filepath.Join(root, "inbox/a.mkv"), make([]byte, 300<<10))
	write(filepath.Join(root, "inbox/b.mkv"), make([]byte, 200<<10))
	write(filepath.Join(root, "inbox/c.mp4"), make([]byte, 100<<10))
	write(filepath.Join(root, "inbox/sub/deep1.txt"), make([]byte, 500))
	write(filepath.Join(root, "inbox/sub/deep2.txt"), make([]byte, 400))
	write(filepath.Join(root, "inbox/x.txt"), make([]byte, 100))
	write(filepath.Join(root, "inbox/last.txt"), make([]byte, 50))
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		os.MkdirAll(filepath.Join(root, "media/movies"), 0o755)
		if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=20:size=640x360:rate=5",
			"-c:v", "mpeg2video", filepath.Join(root, "media/movies/clip.mpg")).CombinedOutput(); err != nil {
			panic(fmt.Sprintf("making a test film: %v\n%s", err, out))
		}
	}
	for i := range 3000 {
		write(filepath.Join(root, fmt.Sprintf("docs/many/file-%04d.bin", i)), make([]byte, 3000-i))
	}
}

func TestMain(m *testing.M) {
	os.Exit(serve(m))
}

func serve(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "rp-e2e-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)
	root = filepath.Join(tmp, "pool")
	tree(root)

	bin = filepath.Join(tmp, "reflectingpool")
	build := exec.Command("go", "build", "-o", bin, "./cmd/reflectingpool")
	build.Dir = ".." // the program's module
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the program: %v\n%s", err, out)
		return 1
	}
	var stop func()
	if base, setupCode, stop, err = start(root, filepath.Join(tmp, "data")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	return m.Run()
}

// start runs the program on a folder and returns its address and the setup
// code it printed.
func start(root, data string) (base, code string, stop func(), err error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, err
	}
	addr := l.Addr().String()
	l.Close()
	base = "http://" + addr

	srv := exec.Command(bin, "serve")
	srv.Env = append(os.Environ(), "RP_ROOTS="+root, "RP_DATA="+data, "RP_LISTEN="+addr, "RP_INSECURE_HTTP=1")
	logs, err := srv.StderrPipe()
	if err != nil {
		return "", "", nil, err
	}
	if err := srv.Start(); err != nil {
		return "", "", nil, err
	}
	stop = func() { srv.Process.Kill(); srv.Wait() }

	// The setup code is printed once, on a line of its own, in the log.
	found := make(chan string, 1)
	go func() {
		line := regexp.MustCompile(`^\S+ \S+ {5}(\S+)$`)
		sc := bufio.NewScanner(logs)
		for sc.Scan() {
			if m := line.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case code = <-found:
	case <-time.After(20 * time.Second):
		stop()
		return "", "", nil, fmt.Errorf("the program did not print a setup code")
	}
	for range 100 {
		if res, err := http.Get(base + "/api/health"); err == nil {
			res.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return base, code, stop, nil
}

// page is one browser tab, with everything the page complained about.
type page struct {
	t   *testing.T
	ctx context.Context

	mu        sync.Mutex
	complaint []string
	allow     []string // parts of complaints this test brings about on purpose
}

func open(t *testing.T) *page {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox, // the container is the sandbox
		chromedp.WindowSize(1440, 900),
	)
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(alloc)
	t.Cleanup(func() { cancel(); cancelAlloc() })
	p := &page{t: t, ctx: ctx}
	if err := chromedp.Do(ctx); err != nil { // starts the browser
		t.Fatalf("starting the browser: %v", err)
	}
	// A slower processor brings out mistakes of timing that a fast machine
	// hides: RP_E2E_SLOW=6 runs the page six times slower.
	if rate, _ := strconv.ParseFloat(os.Getenv("RP_E2E_SLOW"), 64); rate > 1 {
		if _, err := chromedp.Call(ctx, emulation.SetCPUThrottlingRate, emulation.SetCPUThrottlingRateParams{Rate: rate}); err != nil {
			t.Fatalf("slowing the page: %v", err)
		}
	}
	// Everything the page writes to the console, every uncaught error, and
	// everything the browser refuses (which is how a breach of the content
	// security policy shows).
	console := chromedp.Console(ctx)
	go func() {
		for m, err := range console {
			if err != nil {
				return
			}
			// Being signed out is asked for on purpose: the page finds out
			// whether there is a session by asking for it. A wrong password
			// is tried on purpose too. And the films in the test pool are
			// mostly empty files, of which no preview can be made: the server
			// says so with a 422, and the page shows its reason.
			if strings.Contains(m.Text, "status of 401") || strings.Contains(m.Text, "status of 422") || slices.ContainsFunc(p.allow, func(a string) bool { return strings.Contains(m.Text, a) }) {
				continue
			}
			if m.IsException() || m.Type == "error" || m.Type == "warning" {
				p.mu.Lock()
				p.complaint = append(p.complaint, fmt.Sprintf("%s %s (%s)", m.Source, m, m.URL))
				p.mu.Unlock()
			}
		}
	}()
	return p
}

// do runs one step with a time limit of its own, so that a wait which never
// ends says which step it was and shows what the page looked like.
func (p *page) do(step string, actions ...chromedp.Action[chromedp.Void]) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	if err := chromedp.Do(ctx, actions...); err != nil {
		p.shot("failed-" + step)
		p.t.Fatalf("%s: %v\nthe page complained: %q\nthe page said:\n%s", step, err, p.complaints(), p.text("body"))
	}
	p.shot(step)
}

func (p *page) shot(name string) {
	dir := os.Getenv("RP_E2E_SHOTS")
	if dir == "" {
		return
	}
	if buf, err := chromedp.Run(p.ctx, chromedp.CaptureScreenshot()); err == nil {
		os.WriteFile(filepath.Join(dir, strings.NewReplacer(" ", "-", "/", "-").Replace(name)+".png"), buf, 0o644)
	}
}

func (p *page) complaints() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.complaint...)
}

// text returns what an element says.
func (p *page) text(sel string) string {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()
	s, err := chromedp.Run(ctx, chromedp.Text(sel))
	if err != nil {
		p.t.Errorf("reading %s: %v", sel, err)
	}
	return s
}

// eval runs an expression in the page.
func eval[T any](p *page, expr string) T {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()
	v, err := chromedp.Run(ctx, chromedp.Evaluate[T](expr))
	if err != nil {
		p.t.Errorf("evaluating %s: %v", expr, err)
	}
	return v
}

// until waits for an expression to become true in the page.
func until(expr string) chromedp.Action[chromedp.Void] {
	return chromedp.Poll[chromedp.Void](expr)
}

// run is an expression evaluated for its effect.
func run(expr string) chromedp.Action[chromedp.Void] {
	return chromedp.Evaluate[chromedp.Void](expr)
}

// has is an XPath for an element of the given kind holding exactly this text.
func has(tag, s string) string {
	return fmt.Sprintf(`//%s[normalize-space(.)=%q]`, tag, s)
}

// row is an XPath for the row of the list whose name is s.
func row(s string) string {
	return fmt.Sprintf(`//div[@role="row"][.//span[contains(@class,"fs")][.=%q]]`, s)
}

func TestTheInterface(t *testing.T) {
	p := open(t)

	// First start: the account is made with the code from the log, and
	// nothing has been scanned.
	p.do("01 setup page", chromedp.Navigate(base+"/"), chromedp.WaitVisible(`#su-code`))
	p.do("02 account created",
		chromedp.SendKeys(`#su-code`, setupCode), chromedp.SendKeys(`#su-user`, user),
		chromedp.SendKeys(`#su-pass`, password), chromedp.SendKeys(`#su-again`, password),
		chromedp.Click(`button[type=submit]`),
		chromedp.WaitVisible(has("h1", "Nothing has been scanned yet")),
	)

	// The scan dialog offers Balanced first.
	p.do("03 scan dialog",
		chromedp.Click(has("button", "Start the first scan")),
		chromedp.WaitVisible(`dialog[open]`),
	)
	if !eval[bool](p, `document.querySelector('input[name=intensity][value=balanced]').checked`) {
		t.Error("the scan dialog did not preselect Balanced")
	}
	p.do("04 space after the first scan",
		chromedp.Click(has("button", "Start scan")),
		chromedp.WaitVisible(has("h1", "Overview")), // the front page, once there is an index
		chromedp.Click(has("a", "Space")),
		chromedp.WaitVisible(row("media")),
	)
	// Three thousand small files take more room on disk than their contents
	// add up to, so docs leads on disk and media leads by apparent size.
	if got := p.text(`.vl-row`); !strings.Contains(got, "docs") {
		t.Errorf("the share that takes the most room on disk should be the first row; it is %q", got)
	}
	p.do("04b apparent sizes",
		chromedp.Click(has("button", "Apparent")),
		until(`document.querySelector('.vl-row .fs')?.textContent === 'media'`),
		chromedp.Click(has("button", "On disk")),
		until(`document.querySelector('.vl-row .fs')?.textContent === 'docs'`),
	)
	// A bar is as long as the row's share of the folder.
	if w := eval[float64](p, `(() => { const f = document.querySelector('.vl-row .fill'); return f.getBoundingClientRect().width / f.parentElement.getBoundingClientRect().width; })()`); w < 0.5 || w > 0.8 {
		t.Errorf("the first row's bar covers %.2f of its column; docs holds about two thirds of the pool", w)
	}

	// Into a folder, select a file, and look at it.
	p.do("05 inside media/pictures",
		chromedp.DoubleClick(row("media")),
		chromedp.WaitVisible(row("pictures")),
		chromedp.DoubleClick(row("pictures")),
		chromedp.WaitVisible(row("sunset.png")),
	)
	if got := eval[string](p, `location.href`); !strings.Contains(got, "/space?path=") || !strings.Contains(got, "pictures") {
		t.Errorf("the address does not name the folder: %s", got)
	}
	p.do("06 picture preview",
		chromedp.Click(row("sunset.png")),
		chromedp.WaitVisible(`.insp .pv img`),
		until(`(() => { const i = document.querySelector('.insp .pv img'); return i && i.complete && i.naturalWidth === 320; })()`),
	)

	// Record what it is for. The note is written into the share.
	p.do("07 note saved",
		chromedp.SendKeys(`#an-note`, "Holiday photo, keep"),
		chromedp.Click(`//button[starts-with(normalize-space(.), "Save")]`),
		chromedp.WaitVisible(row("sunset.png")+`//span[.="described"]`),
	)
	if data, err := os.ReadFile(filepath.Join(root, "media/.reflection/annotations.jsonl")); err != nil || !strings.Contains(string(data), "Holiday photo, keep") {
		t.Errorf("the note is not in the share's annotation file: %q, %v", data, err)
	}
	p.do("08 note there after a reload",
		chromedp.Reload(),
		chromedp.WaitVisible(row("sunset.png")),
		chromedp.Click(row("sunset.png")),
		chromedp.WaitVisible(`#an-note`),
		until(`document.querySelector('#an-note').value === 'Holiday photo, keep'`),
	)

	// Text is shown as text: markup in a file must not run.
	p.do("09 text preview",
		chromedp.Navigate(base+"/space?path="+root+"/docs/notes.txt"),
		chromedp.WaitVisible(`.insp .pv-text`),
	)
	if got := p.text(`.insp .pv-text`); !strings.Contains(got, "hello from the notes file") || !strings.Contains(got, "<script>") {
		t.Errorf("text preview = %q", got)
	}
	if got := eval[string](p, `document.title`); got != "Reflecting Pool" {
		t.Errorf("a script inside a previewed file ran: the title is %q", got)
	}

	// A folder with 3,000 files: only the rows in view are in the page, and
	// the end of the list is reachable.
	p.do("10 a long list",
		chromedp.Navigate(base+"/space?path="+root+"/docs/many&sort=name&desc=0"),
		chromedp.WaitVisible(row("file-0000.bin")),
	)
	if n := eval[int](p, `document.querySelectorAll('.vl-row').length`); n > 80 {
		t.Errorf("%d rows are in the page for a list of 3,000; only those in view should be", n)
	}
	p.do("11 the end of a long list",
		run(`document.querySelector('.vl').scrollTop = 1e9`),
		chromedp.WaitVisible(row("file-2999.bin")),
	)

	// A list too long for a browser to lay out at full height (hundreds of
	// thousands of rows) maps the scroll bar onto the list instead. The limit
	// is lowered here so that 3,000 rows are enough to be in that case.
	p.do("11b a list past the height limit",
		run(`window.rpListMaxPx = 20000`),
		chromedp.Click(`//div[@role="columnheader"]/button[starts-with(., "Name")]`), // redraw: Z to A
		chromedp.WaitVisible(row("file-2999.bin")),
		run(`document.querySelector('.vl').scrollTop = 1e9`),
		chromedp.WaitVisible(row("file-0000.bin")),
		run(`(() => { const v = document.querySelector('.vl'); v.scrollTop = (v.scrollHeight - v.clientHeight) / 2; })()`),
		until(`[...document.querySelectorAll('.vl-row .fs')].some((e) => /^file-1[45]\d\d\.bin$/.test(e.textContent))`),
		chromedp.Focus(`.vl`),
		chromedp.KeyEvent(kb.Home),
		chromedp.WaitVisible(row("file-2999.bin")+`[contains(@class,"sel")]`),
		chromedp.KeyEvent(kb.End),
		chromedp.WaitVisible(row("file-0000.bin")+`[contains(@class,"sel")]`),
		run(`delete window.rpListMaxPx`),
	)
	// Every row drawn must sit inside the scrolled view, also in that case.
	if out := eval[int](p, `(() => { const v = document.querySelector('.vl').getBoundingClientRect(); return [...document.querySelectorAll('.vl-row.sel')].filter((r) => { const b = r.getBoundingClientRect(); return b.top < v.top || b.bottom > v.bottom + 1; }).length; })()`); out != 0 {
		t.Errorf("the selected row at the end of a scaled list is drawn outside the view")
	}

	// The keyboard: down to the first row, Enter to open, Backspace to go up.
	p.do("12 keyboard",
		chromedp.Navigate(base+"/space?path="+root+"/docs&sort=name&desc=0"),
		chromedp.WaitVisible(row("many")),
		chromedp.Focus(`.vl`),
		chromedp.KeyEvent(kb.ArrowDown),
		chromedp.WaitVisible(`.vl-row.sel`),
		chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible(row("file-0000.bin")),
		chromedp.Focus(`.vl`),
		chromedp.KeyEvent(kb.Backspace),
		chromedp.WaitVisible(row("taxes")),
		chromedp.WaitVisible(`.vl-row.sel`),
	)
	if got := p.text(`.vl-row.sel`); !strings.Contains(got, "many") {
		t.Errorf("after going up, the folder just left should be selected; selected is %q", got)
	}

	// A hardlinked file says so, and names its other place.
	p.do("13 hardlink",
		chromedp.Click(row("short-link.mp4")),
		chromedp.WaitVisible(`.insp .links`),
	)
	if got := p.text(`.insp .links`); !strings.Contains(got, "media/movies/short.mp4") {
		t.Errorf("the other name of a hardlinked file is not shown: %q", got)
	}

	// A file the browser cannot play says so instead of showing a dead player
	// (the test's films are empty), and a PDF opens in a frame of our own.
	p.do("13b a film that will not play",
		chromedp.WaitVisible(`//div[contains(@class,"pv-msg")][contains(., "No preview")]`),
	)
	p.do("13c pdf",
		chromedp.DoubleClick(row("taxes")),
		chromedp.WaitVisible(row("receipt.pdf")),
		chromedp.Click(row("receipt.pdf")),
		chromedp.WaitVisible(`.insp .pv iframe`),
		chromedp.Sleep(500*time.Millisecond), // a refused frame is reported a moment after it is made
		chromedp.Click(`.crumbs a:last-of-type`),
		chromedp.WaitVisible(row("many")),
	)

	// Settings: units and theme.
	p.do("14 settings",
		chromedp.Click(has("a", "Settings")),
		chromedp.WaitVisible(has("h1", "Settings")),
		chromedp.Click(has("button", "GiB, TiB (binary)")),
		chromedp.Click(has("button", "Dark")),
		until(`document.documentElement.dataset.theme === 'dark'`),
	)
	p.do("15 dark theme, binary units",
		chromedp.Click(has("a", "Space")),
		chromedp.WaitVisible(row("media")),
	)
	if got := p.text(`.vl-body`); !strings.Contains(got, "MiB") {
		t.Errorf("sizes are not in binary units after choosing them: %q", got)
	}

	// Signing out and in again.
	p.do("16 signed out",
		chromedp.Click(has("a", "Settings")),
		chromedp.Click(has("button", "Sign out")),
		chromedp.WaitVisible(`#si-user`),
	)
	p.do("17 wrong password",
		chromedp.SendKeys(`#si-user`, user), chromedp.SendKeys(`#si-pass`, "not the password"),
		chromedp.Click(`button[type=submit]`),
		chromedp.WaitVisible(has("p", "Wrong username or password.")),
	)
	p.do("18 signed in again",
		run(`document.querySelector("#si-pass").value = ""`), chromedp.SendKeys(`#si-pass`, password),
		chromedp.Click(`button[type=submit]`),
		chromedp.WaitVisible(has("h1", "Settings")),
	)

	// Nothing may have gone wrong quietly: no script error, and nothing
	// refused by the content security policy.
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

// A scan long enough to watch: the strip that shows it, stopping it, and the
// list on a tree the size of a real pool. It needs such a tree, so it only
// runs when RP_E2E_BIG names one, for example the benchmark volume:
//
//	RP_E2E_MOUNT=rp-bench:/bench RP_E2E_BIG=/bench/pool scripts/e2e.sh -run TestScanInProgress
func TestScanInProgress(t *testing.T) {
	big := os.Getenv("RP_E2E_BIG")
	if big == "" {
		t.Skip("set RP_E2E_BIG to a large folder to run this")
	}
	base, code, stop, err := start(big, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	p := open(t)
	soFar := func() int {
		digits := regexp.MustCompile(`([\d,]+) (?:of about [\d,]+ )?files and folders read`).FindStringSubmatch(p.text(`.banner.info`))
		if digits == nil {
			return -1
		}
		n := 0
		fmt.Sscan(strings.ReplaceAll(digits[1], ",", ""), &n)
		return n
	}

	p.do("big 01 first scan started, low impact",
		chromedp.Navigate(base+"/"), chromedp.WaitVisible(`#su-code`),
		chromedp.SendKeys(`#su-code`, code), chromedp.SendKeys(`#su-user`, user),
		chromedp.SendKeys(`#su-pass`, password), chromedp.SendKeys(`#su-again`, password),
		chromedp.Click(`button[type=submit]`),
		chromedp.Click(has("button", "Start the first scan")),
		chromedp.Click(`input[name=intensity][value=low]`),
		chromedp.Click(has("button", "Start scan")),
		chromedp.WaitVisible(`//div[contains(@class,"banner")][contains(., "Scanning, low impact")]`),
	)
	// The strip follows the scan, and Stop ends it. With the tree's metadata
	// in memory even a low-impact scan takes only seconds, so there is no
	// waiting about before stopping it.
	first := soFar()
	p.do("big 02 the count moves, then stop",
		until(fmt.Sprintf(`(() => { const m = /([\d,]+) (?:of about [\d,]+ )?files and folders read/.exec(document.querySelector('.banner.info')?.textContent || ''); return m && Number(m[1].replaceAll(',', '')) > %d; })()`, first)),
		chromedp.Click(has("button", "Stop")),
		chromedp.WaitVisible(`//div[contains(@class,"banner")][contains(., "The last scan did not finish")]`),
		chromedp.WaitVisible(has("h1", "Nothing has been scanned yet")),
	)
	if first < 0 {
		t.Errorf("the strip did not say how much had been read")
	}
	p.do("big 04 scanned at full speed",
		chromedp.Click(has("button", "Start the first scan")),
		chromedp.Click(`input[name=intensity][value=aggressive]`),
		chromedp.Click(has("button", "Start scan")),
		until(`!!document.querySelector('.vl-row')`),
	)
	p.do("big 05 a share",
		chromedp.WaitVisible(row("media")),
		chromedp.DoubleClick(row("media")),
		until(`document.querySelectorAll('.vl-row').length >= 20`),
	)
	p.do("big 06 inside a folder of films",
		chromedp.Navigate(base+"/space?path="+big+"/media/t00/m00/d00000"),
		until(`document.querySelectorAll('.vl-row').length >= 8`),
		chromedp.Click(`.vl-row`),
		chromedp.WaitVisible(`.insp .facts`),
	)
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

// signIn opens an address in a new tab's first visit, where there is no
// session yet, and signs in. The page stays at the address it was opened at.
func (p *page) signIn(step, address string, ready string) {
	p.t.Helper()
	p.do(step,
		chromedp.Navigate(base+address), chromedp.WaitVisible(`#si-user`),
		chromedp.SendKeys(`#si-user`, user), chromedp.SendKeys(`#si-pass`, password),
		chromedp.Click(`button[type=submit]`),
		chromedp.WaitVisible(ready),
	)
}

// eventually waits for a file in the pool to contain some text: saves are
// written behind the screen, a moment after it has moved on.
func eventually(t *testing.T, file, want string) {
	t.Helper()
	var data []byte
	for range 100 {
		data, _ = os.ReadFile(filepath.Join(root, file))
		if strings.Contains(string(data), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s never contained %q; it holds %q", file, want, data)
}

var (
	ctrl = chromedp.KeyModifiers(kb.ModifierCtrl)
	alt  = chromedp.KeyModifiers(kb.ModifierAlt)
)

// heading waits for the review screen to be on the item with this name.
func heading(name string) chromedp.Action[chromedp.Void] {
	return chromedp.WaitVisible(fmt.Sprintf(`//div[contains(@class,"rv-form")]//h2[.=%q]`, name))
}

func TestReview(t *testing.T) {
	p := open(t)
	// A file deleted under the review and a cut network are part of the test.
	p.allow = []string{"status of 409", "ERR_BLOCKED_BY_CLIENT", "Failed to fetch"}
	// One share, by file type: the films (600 kB) come before the text files.
	p.signIn("review 01 signed in", "/review?shares=inbox&groups=type", `#rv-note`)
	p.do("review 02 the largest film is first", heading("a.mkv"),
		until(`document.querySelector('.gnow .cnt').textContent === '3' && document.querySelector('.gnow b').textContent === 'Video files' && document.activeElement.id === 'rv-note'`))

	// Ctrl+Enter saves and moves on at once; with nothing typed it skips.
	p.do("review 03 saved and on to the next",
		chromedp.SendKeys(`#rv-note`, "First film"), chromedp.KeyEvent(kb.Enter, ctrl), heading("b.mkv"))
	eventually(t, "inbox/.reflection/annotations.jsonl", "First film")
	p.do("review 04 skipped with a blank form", chromedp.KeyEvent(kb.Enter, ctrl), heading("c.mp4"))
	eventually(t, "inbox/.reflection/skipped.jsonl", "b.mkv")

	// Ctrl+Z goes back when nothing is typed; Ctrl+Enter then returns.
	p.do("review 05 back one item",
		chromedp.KeyEvent("z", ctrl), heading("b.mkv"),
		chromedp.WaitVisible(`//div[contains(@class,"note-line")][contains(., "Looking back")]`),
		chromedp.KeyEvent(kb.Enter, ctrl), heading("c.mp4"),
	)
	// Up in the empty note brings back the last note; Down clears it again.
	p.do("review 06 the last note comes back",
		until(`document.activeElement.id === 'rv-note'`),
		chromedp.KeyEvent(kb.ArrowUp), until(`document.querySelector('#rv-note').value === 'First film'`),
		chromedp.KeyEvent(kb.ArrowDown), until(`document.querySelector('#rv-note').value === ''`),
	)

	// The rest of the group in one go, and the next group starts.
	p.do("review 07 skip the rest of the group",
		chromedp.Click(`//div[contains(@class,"gnow")]//button[starts-with(normalize-space(.), "Skip the")]`),
		chromedp.Click(has("button", "Yes, skip them")),
		heading("deep1.txt"),
		chromedp.WaitVisible(`//div[contains(@class,"note-line")][contains(., "Finished video")]`),
	)
	eventually(t, "inbox/.reflection/skipped.jsonl", "c.mp4")

	// Describe the folder instead: what is inside it leaves the queue.
	p.do("review 08 the folder instead",
		chromedp.KeyEvent(kb.ArrowUp, alt), heading("sub"),
		chromedp.SendKeys(`#rv-note`, "Scratch folder"), chromedp.KeyEvent(kb.Enter, ctrl),
		heading("x.txt"),
		chromedp.WaitVisible(`//div[contains(@class,"note-line")][contains(., "2 items inside it left the queue")]`),
	)

	// The item is deleted before its note is saved: the note is not lost
	// without a word, and the item can still be passed over.
	if err := os.Remove(filepath.Join(root, "inbox/x.txt")); err != nil {
		t.Fatal(err)
	}
	p.do("review 09 a note for a file that has gone",
		chromedp.SendKeys(`#rv-note`, "written too late"), chromedp.KeyEvent(kb.Enter, ctrl),
		chromedp.WaitVisible(`//div[contains(@class,"banner")][contains(., "Not saved: x.txt")][contains(., "no longer where")]`),
		chromedp.Click(has("button", "Skip it")),
		heading("last.txt"),
	)

	// A save that cannot reach the server: the text is kept, and Retry
	// sends it. The browser is told to refuse the request, which is what a
	// dropped connection looks like to the page.
	offline := func(off bool) chromedp.Action[chromedp.Void] {
		return func(ctx context.Context, t *chromedp.Target) (chromedp.Void, error) {
			if _, err := cdp.Call(ctx, t, network.Enable, network.EnableParams{}); err != nil {
				return chromedp.Void{}, err
			}
			var block []*network.BlockPattern
			if off {
				block = []*network.BlockPattern{{URLPattern: "*://*:*/api/entries/*/annotation", Block: true}}
			}
			_, err := cdp.Call(ctx, t, network.SetBlockedURLs, network.SetBlockedURLsParams{URLPatterns: block})
			return chromedp.Void{}, err
		}
	}
	p.do("review 10 the network drops",
		offline(true),
		chromedp.SendKeys(`#rv-note`, "kept while offline"), chromedp.KeyEvent(kb.Enter, ctrl),
		chromedp.WaitVisible(`//div[contains(@class,"banner")][contains(., "Not saved: last.txt")][contains(., "Your text is kept")]`),
	)
	p.do("review 11 retry once it is back",
		offline(false),
		chromedp.Click(has("button", "Retry")),
		chromedp.WaitVisible(has("h2", "This queue is finished")),
	)
	eventually(t, "inbox/.reflection/annotations.jsonl", "kept while offline")

	// Another order, chosen in the builder, with a look at what it gives.
	p.do("review 12 the builder",
		chromedp.Click(has("button", "Build another queue")),
		chromedp.Click(has("button", "Folders first, largest first")),
		chromedp.WaitVisible(`//div[contains(@class,"bprev")]//b[contains(., "to go") or contains(., "item")]`),
	)
	p.do("review 13 a queue of folders",
		chromedp.Click(has("button", "Use this order")),
		until(`location.search.includes('kind=dir') && !!document.querySelector('#rv-note')`),
	)

	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

func TestAnnotations(t *testing.T) {
	p := open(t)
	p.signIn("annotations 01 signed in", "/annotations", has("h1", "Annotations"))
	// The notes written by the tests before this one show in the coverage.
	p.do("annotations 02 coverage", chromedp.WaitVisible(`//tr[.//span[.="inbox"]]//button[.="Review this share"]`))

	// A prefix is added to the list, then put on an item from Space.
	p.do("annotations 03 a prefix added",
		chromedp.Click(has("button", "Add a prefix")),
		chromedp.SendKeys(`input[aria-label="Prefix"]`, "KEEP"),
		chromedp.SendKeys(`input[aria-label="What KEEP means"]`, "Do not delete"),
		chromedp.Click(has("button", "Save the list")),
		chromedp.WaitVisible(`//div[contains(@class,"toast")][contains(., "prefix list is saved")]`),
	)
	p.do("annotations 04 the prefix on an item",
		chromedp.Navigate(base+"/space?path="+root+"/media/movies/alien.mkv"),
		chromedp.WaitVisible(`#an-prefixes`),
		chromedp.SendKeys(`#an-prefixes`, "KEEP"),
		chromedp.Click(`//button[starts-with(normalize-space(.), "Save")]`),
		chromedp.WaitVisible(row("alien.mkv")+`//span[contains(@class,"mark")][.="described"]`),
		chromedp.Click(has("a", "Annotations")),
		chromedp.WaitVisible(`//tr[.//input[@aria-label="Prefix"]]/td[3][normalize-space(.)="1"]`),
	)

	// A described item is deleted from the disk; after a scan its note is
	// listed as missing and can be deleted.
	if err := os.Remove(filepath.Join(root, "media/movies/alien.mkv")); err != nil {
		t.Fatal(err)
	}
	p.do("annotations 05 a scan after the item has gone",
		chromedp.Click(has("button", "Scan now")),
		chromedp.Click(has("button", "Start scan")),
		chromedp.WaitVisible(`//td[.//span[.="media/movies/alien.mkv"]]`),
	)
	p.do("annotations 06 the missing note deleted",
		chromedp.Click(`//tr[.//span[.="media/movies/alien.mkv"]]//button[.="Delete"]`),
		chromedp.WaitNotPresent(`//td[.//span[.="media/movies/alien.mkv"]]`),
	)
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

func TestFind(t *testing.T) {
	p := open(t)
	p.signIn("find 01 signed in", "/find", `.vl-row`)

	// A view is a preset of filters; the summary counts everything that
	// matches, not just the rows on screen.
	p.do("find 02 largest folders",
		chromedp.Click(has("button", "Largest folders")),
		until(`location.search.includes('kind=dir') && document.querySelector('.vl-row .fs')?.textContent.endsWith('/')`),
	)
	p.do("find 03 a search by name",
		chromedp.Navigate(base+"/find"), chromedp.WaitVisible(`input[type=search]`),
		chromedp.SendKeys(`input[type=search]`, "file-29"),
		chromedp.WaitVisible(`//span[@role="status"][starts-with(., "100 items")]`),
		until(`[...document.querySelectorAll('.vl-row .nm .fs')].every((e) => e.textContent.startsWith('file-29'))`),
	)

	// The nearest match comes first while searching: the file called "art"
	// before the larger files that merely contain those letters. A column
	// header still orders by that column.
	p.do("find 03b best match first",
		chromedp.Navigate(base+"/find?kind=file&q=art"),
		chromedp.WaitVisible(row("art.txt")),
		until(`[...document.querySelectorAll('.vl-row .nm .fs')].map((e) => e.textContent).join('|') === 'art.txt|the art of war.txt|party plans.txt'`),
		chromedp.WaitVisible(`//button[.="Best match first"][@aria-pressed="true"]`),
		chromedp.Click(`//div[@role="columnheader"]/button[starts-with(., "Size")]`),
		until(`document.querySelector('.vl-row .nm .fs')?.textContent === 'party plans.txt'`),
		chromedp.Click(has("button", "Best match first")),
		until(`document.querySelector('.vl-row .nm .fs')?.textContent === 'art.txt'`),
	)
	p.do("find 03c words in any order",
		chromedp.Navigate(base+"/find?q=war+art"),
		chromedp.WaitVisible(row("the art of war.txt")),
		chromedp.WaitVisible(`//span[@role="status"][starts-with(., "1 item")]`),
	)

	// From a result to where it lives, and from a set of results to a review.
	p.do("find 04 show in space",
		chromedp.Navigate(base+"/find?kind=file&shares=docs&q=notes"),
		chromedp.WaitVisible(row("notes.txt")),
		chromedp.Click(row("notes.txt")),
		chromedp.Click(has("button", "Show in Space")),
		chromedp.WaitVisible(row("notes.txt")+`[contains(@class,"sel")]`),
		until(`location.pathname === '/space' && location.search.includes('docs')`),
	)
	p.do("find 05 review these results",
		chromedp.Navigate(base+"/find?kind=file&shares=docs&q=notes"),
		chromedp.WaitVisible(row("notes.txt")),
		chromedp.Click(has("button", "Review these results")),
		heading("notes.txt"),
		until(`location.pathname === '/review' && location.search.includes('q=notes')`),
	)

	// A view of one's own is kept in the browser.
	p.do("find 06 a saved view",
		chromedp.Navigate(base+"/find?kind=file&types=text"),
		chromedp.WaitVisible(row("notes.txt")),
		chromedp.Click(has("button", "Save this view")),
		chromedp.SendKeys(`input[aria-label="Name for this view"]`, "Text files"),
		chromedp.Click(has("button", "Save")),
		chromedp.Reload(),
		chromedp.WaitVisible(`//span[contains(@class,"scope")]/button[.="Text files"]`),
	)

	// Space hands its folder over.
	p.do("find 07 inside a folder, from space",
		chromedp.Navigate(base+"/space?path="+root+"/docs"),
		chromedp.Click(has("button", "Find inside this folder")),
		chromedp.WaitVisible(`//span[contains(@class,"scope")][contains(., "/docs")]`),
		chromedp.WaitVisible(`.vl-row`),
	)
	if got := p.text(`span[role=status]`); !strings.Contains(got, "3,004 items") && !strings.Contains(got, "3,005 items") && !strings.Contains(got, "3,006 items") {
		t.Errorf("everything inside docs = %q", got)
	}
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

func TestOverview(t *testing.T) {
	p := open(t)
	// The address with nothing after it opens on Overview.
	p.signIn("overview 01 signed in", "/", has("h1", "Overview"))
	p.do("overview 02 capacity and shares",
		chromedp.WaitVisible(`.capbar`),
		chromedp.WaitVisible(`//table//a[.="docs"]`),
		chromedp.WaitVisible(`//div[contains(@class,"card")][.//h2[.="Last scan"]]//span[.="Files"]`),
	)
	// The largest share comes first, and its name leads into Space.
	if got := p.text(`table.list tbody tr`); !strings.Contains(got, "docs") {
		t.Errorf("first share on the overview = %q", got)
	}
	p.do("overview 03 into a share",
		chromedp.Click(`//table//a[.="inbox"]`),
		chromedp.WaitVisible(row("a.mkv")),
	)
	// The review stopped earlier in this browser is offered again... in this
	// fresh browser there is none, so the card offers to start one.
	p.do("overview 04 start a review",
		chromedp.Click(has("a", "Overview")),
		chromedp.Click(has("button", "Start a review")),
		chromedp.WaitVisible(`.rv-top`),
	)
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

func TestStorage(t *testing.T) {
	p := open(t)
	p.signIn("storage 01 signed in", "/storage", has("h1", "Storage"))
	// The test runs on an ordinary filesystem: one row, no ZFS figures.
	p.do("storage 02 the filesystem under the pool", chromedp.WaitVisible(`table.list tbody tr`))
	if got := p.text(`table.list tbody tr`); !strings.Contains(got, "B") {
		t.Errorf("dataset row = %q", got)
	}
	if got := p.text(`.sh .sub`); !strings.Contains(got, "free of") {
		t.Errorf("summary = %q", got)
	}
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

func TestMap(t *testing.T) {
	p := open(t)
	p.signIn("map 01 signed in", "/space", `.map`)
	// The pool's shares as areas, with what is inside the large ones drawn
	// within them. Everything drawn stays inside the map.
	p.do("map 02 areas", until(`document.querySelectorAll('.map .cell').length >= 3 && document.querySelectorAll('.map .grp').length >= 1`))
	if out := eval[int](p, `(() => { const m = document.querySelector('.map').getBoundingClientRect(); return [...document.querySelectorAll('.map .cell, .map .grp')].filter((c) => { const b = c.getBoundingClientRect(); return b.left < m.left - 1 || b.right > m.right + 1 || b.top < m.top - 1 || b.bottom > m.bottom + 1; }).length; })()`); out != 0 {
		t.Errorf("%d areas are drawn outside the map", out)
	}
	// Areas are in proportion: the largest share covers the part of the map
	// that the list says it holds of the pool.
	area := eval[float64](p, `(() => { const m = document.querySelector('.map').getBoundingClientRect(); const g = [...document.querySelectorAll('.map .grp')].map((e) => e.getBoundingClientRect()).sort((a, b) => b.width * b.height - a.width * a.height)[0]; return g.width * g.height / (m.width * m.height); })()`)
	share := eval[float64](p, `parseFloat(document.querySelector('.vl-row .c-pct').textContent) / 100`)
	if share < 0.3 || area < share-0.06 || area > share+0.06 {
		t.Errorf("the largest share covers %.2f of the map, and the list says it holds %.2f of the pool", area, share)
	}
	// Clicking selects, in the map and in the item column alike; a double click opens.
	p.do("map 03 select and open",
		chromedp.Click(`//div[contains(@class,"grp-h")][starts-with(., "docs")]`),
		chromedp.WaitVisible(`//aside//h3[.="docs"]`),
		chromedp.DoubleClick(`//div[contains(@class,"grp-h")][starts-with(., "docs")]`),
		chromedp.WaitVisible(row("many")),
		until(`location.search.includes('docs')`),
	)
	// The other form, and off; the choice is remembered.
	p.do("map 04 bars",
		chromedp.Click(has("button", "Bars")),
		until(`document.querySelector('.map').getBoundingClientRect().height < 120 && document.querySelectorAll('.map .cell').length >= 1`),
	)
	p.do("map 05 off, and still off after a reload",
		chromedp.Click(has("button", "Off")),
		chromedp.WaitNotPresent(`.map`),
		chromedp.Reload(),
		chromedp.WaitVisible(row("many")),
	)
	if eval[bool](p, `!!document.querySelector('.map')`) {
		t.Error("the map came back after a reload although it was turned off")
	}
	p.do("map 06 on again", chromedp.Click(has("button", "Areas")), chromedp.WaitVisible(`.map .cell`))
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

// Folders are reviewed as readily as files: a switch beside the order, and a
// button on a file's form for describing its folder instead.
func TestReviewFolders(t *testing.T) {
	p := open(t)
	p.signIn("folders 01 signed in", "/review?shares=docs&groups=none", `#rv-note`)
	p.do("folders 02 the switch",
		chromedp.Click(`//div[@aria-label="Review files or folders"]/button[.="Folders"]`),
		until(`location.search.includes('kind=dir') && document.querySelector('label[for=rv-note]')?.textContent === 'What is this folder for?'`),
	)
	p.do("folders 03 the folder button on a file",
		chromedp.Navigate(base+"/review?shares=docs&groups=none&q=receipt"),
		heading("receipt.pdf"),
		chromedp.Click(`//button[starts-with(normalize-space(.), "Describe the folder instead")]`),
		heading("taxes"),
		chromedp.Click(`//button[starts-with(normalize-space(.), "Back to the item")]`),
		heading("receipt.pdf"),
	)
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

// A folder's bar shows what the folder is made of, and the item column gives
// the figures.
func TestFoldersShowWhatTheyHold(t *testing.T) {
	p := open(t)
	p.signIn("types 01 signed in", "/space?path="+root+"/inbox", `.vl-row`)
	// inbox holds films (most of its bytes) and text files: its row in the
	// pool's list has a blue part for video, and the map colours it as video.
	p.do("types 02 the pool",
		chromedp.Navigate(base+"/space?path="+root),
		chromedp.WaitVisible(row("inbox")+`//i[contains(@class,"k-video")]`),
		chromedp.Click(row("inbox")),
		chromedp.WaitVisible(`//aside//div[.="What is in it"]`),
	)
	if got := p.text(`//aside//div[.="What is in it"]/following-sibling::table`); !strings.Contains(got, "video") || !strings.Contains(got, "text") {
		t.Errorf("what inbox holds = %q", got)
	}
	// docs is almost all small binary files: no single colour but the neutral one.
	if n := eval[int](p, `document.querySelectorAll('.capbar i:not(.free)').length`); n != 0 {
		t.Errorf("unexpected capacity bar on Space: %d", n)
	}
	p.do("types 03 overview by kind",
		chromedp.Click(has("a", "Overview")),
		chromedp.WaitVisible(`.capbar i.k-backup`), // the test pool is mostly .bin files, which count as disk images
		chromedp.WaitVisible(`//span[contains(@class,"legend-i")][starts-with(., "Archives and disk images")]`),
	)
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

// The folder tree: out with a button, a level at a time, straight to a folder,
// and open along the way to wherever the list is.
func TestFolderTree(t *testing.T) {
	p := open(t)
	p.signIn("tree 01 signed in", "/space", `.vl-row`)
	p.do("tree 02 out, with the shares in it",
		chromedp.Click(has("button", "Folder tree")),
		chromedp.WaitVisible(`//nav[@aria-label="Folders"]//button[contains(@class,"tree-name")][.="docs"]`),
	)
	p.do("tree 03 a level opened, and a folder gone to",
		chromedp.Click(`button[aria-label="Open docs"]`),
		chromedp.WaitVisible(`//nav[@aria-label="Folders"]//button[contains(@class,"tree-name")][.="taxes"]`),
		chromedp.Click(`//nav[@aria-label="Folders"]//button[contains(@class,"tree-name")][.="taxes"]`),
		chromedp.WaitVisible(row("receipt.pdf")),
		chromedp.WaitVisible(`//div[contains(@class,"tree-row")][contains(@class,"cur")]/button[.="taxes"]`),
	)
	// Arriving somewhere by its address opens the tree down to it; the tree stays out across a reload.
	p.do("tree 04 open along the way",
		chromedp.Navigate(base+"/space?path="+root+"/inbox/sub"),
		chromedp.WaitVisible(row("deep1.txt")),
		chromedp.WaitVisible(`//div[contains(@class,"tree-row")][contains(@class,"cur")]/button[.="sub"]`),
	)
	p.do("tree 05 hidden again",
		chromedp.Click(has("button", "Hide the folder tree")),
		chromedp.WaitNotPresent(`nav[aria-label="Folders"]`),
	)
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}

// A film in a format no browser plays is shown as stills made on the server,
// which open large like any picture.
func TestPreviews(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed here, so there is no film to preview")
	}
	p := open(t)
	p.signIn("previews 01 signed in", "/space?path="+root+"/media/movies/clip.mpg", `.insp`)
	p.do("previews 02 stills from a film",
		chromedp.WaitVisible(`.insp .pv img`),
		until(`(() => { const i = document.querySelector('.insp .pv img'); return i && i.complete && i.naturalWidth === 1292; })()`),
		chromedp.WaitVisible(`//div[contains(@class,"pv-cap")][contains(., "8 stills")][contains(., "640×360")][contains(., "mpeg2video")]`),
	)
	if eval[bool](p, `!!document.evaluate('//button[.="Play it here"]', document).iterateNext()`) {
		t.Error("a player is offered for a format the browser cannot play")
	}
	p.do("previews 03 large",
		chromedp.Click(`.insp .pv-zoom`),
		chromedp.WaitVisible(`dialog.lightbox[open] img`),
		chromedp.KeyEvent(kb.Escape),
		chromedp.WaitNotPresent(`dialog.lightbox`),
	)
	if c := p.complaints(); len(c) > 0 {
		t.Errorf("the page complained:\n%s", strings.Join(c, "\n"))
	}
}
