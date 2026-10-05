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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
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
			// is tried on purpose too.
			if strings.Contains(m.Text, "status of 401") {
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
		chromedp.WaitVisible(`//div[contains(@class,"pv-msg")][contains(., "could not be shown")]`),
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
