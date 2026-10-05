// Package preview makes a picture of a file that a browser cannot show by
// itself: a sheet of stills for a film, and a JPEG for a HEIC or RAW photo.
//
// The files are whatever is on the pool, so they are treated as hostile.
// Film and HEIC decoding is done by ffmpeg, which is run without root
// rights, is handed the file as an already-open descriptor and no path, and
// is allowed no other input, so a crafted file cannot make it open anything
// else. RAW photos are not decoded at all: the JPEG the camera embedded is
// found and copied out, in Go.
package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Kind is how a file's preview is made.
type Kind int

const (
	None  Kind = iota
	Video      // a sheet of stills from across the film
	Photo      // a format ffmpeg decodes to one picture
	Raw        // a camera file with a JPEG inside it
)

var (
	rawExts   = map[string]bool{"cr2": true, "cr3": true, "nef": true, "arw": true, "dng": true, "orf": true, "rw2": true, "raw": true, "raf": true, "pef": true, "srw": true}
	photoExts = map[string]bool{"heic": true, "heif": true}
)

// KindOf says how a file of the given type and extension is previewed.
func KindOf(fileType, ext string) Kind {
	switch {
	case rawExts[ext]:
		return Raw
	case photoExts[ext]:
		return Photo
	case fileType == "video":
		return Video
	}
	return None
}

var (
	// ErrUnsupported means there is no way to preview this kind of file.
	ErrUnsupported = errors.New("there is no preview for this kind of file")
	// ErrNoTool means ffmpeg is not installed where the program runs.
	ErrNoTool = errors.New("previews of films and HEIC photos need ffmpeg, which is not installed here")
)

// FailedError reports that a preview could not be made from this file.
type FailedError struct{ Reason string }

func (e *FailedError) Error() string { return e.Reason }

// Info describes a preview.
type Info struct {
	Kind     string  `json:"kind"` // "video" or "photo"
	Duration float64 `json:"duration,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Codec    string  `json:"codec,omitempty"`
	Stills   int     `json:"stills,omitempty"`
}

const (
	stillWidth = 320
	stillsWide = 4
	stillCount = 8
	maxStill   = 8 << 20  // bytes accepted from the tool for one picture
	maxRawRead = 64 << 20 // how far into a RAW file its embedded JPEG is looked for
	maxOutput  = 25 << 20
	toolMemory = 2 << 20 // KiB of address space the tool may use
	jobTimeout = 40 * time.Second
)

// Maker makes previews and keeps them.
type Maker struct {
	dir string
	cap int64

	ffmpeg, ffprobe string
	// drop is set when running as root: the tool then runs as "nobody".
	drop bool
	// slots bounds how many previews are being made at once, so that
	// looking through a folder of films does not occupy the whole server.
	slots chan struct{}

	mu       sync.Mutex
	inflight map[string]*job
	made     int // previews made, not served from the cache; for tests
}

type job struct {
	done chan struct{}
	err  error
}

// New returns a Maker that keeps its previews in dir, up to capBytes.
func New(dir string, capBytes int64) *Maker {
	m := &Maker{dir: dir, cap: capBytes, drop: os.Geteuid() == 0, slots: make(chan struct{}, 2), inflight: make(map[string]*job)}
	m.ffmpeg, _ = exec.LookPath("ffmpeg")
	m.ffprobe, _ = exec.LookPath("ffprobe")
	return m
}

// Get returns the path of the preview of the open file f, making it first if
// it is not in the cache. key must change whenever the file's contents may
// have: it names the preview.
func (m *Maker) Get(ctx context.Context, key string, kind Kind, f *os.File) (string, Info, error) {
	if kind == None {
		return "", Info{}, ErrUnsupported
	}
	pic, meta := filepath.Join(m.dir, key+".jpg"), filepath.Join(m.dir, key+".json")
	for {
		if info, ok := readInfo(meta); ok {
			if _, err := os.Stat(pic); err == nil {
				now := time.Now()
				os.Chtimes(pic, now, now) // recently used: the last to be cleared out
				return pic, info, nil
			}
		}
		// One request makes a given preview; the others wait for it.
		m.mu.Lock()
		j, waiting := m.inflight[key]
		if !waiting {
			j = &job{done: make(chan struct{})}
			m.inflight[key] = j
		}
		m.mu.Unlock()
		if waiting {
			select {
			case <-j.done:
			case <-ctx.Done():
				return "", Info{}, ctx.Err()
			}
			if j.err != nil {
				return "", Info{}, j.err
			}
			continue
		}
		j.err = m.makeOne(ctx, kind, f, pic, meta)
		m.mu.Lock()
		delete(m.inflight, key)
		m.mu.Unlock()
		close(j.done)
		if j.err != nil {
			return "", Info{}, j.err
		}
	}
}

func readInfo(path string) (Info, bool) {
	var info Info
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &info) != nil {
		return Info{}, false
	}
	return info, true
}

func (m *Maker) makeOne(ctx context.Context, kind Kind, f *os.File, pic, meta string) error {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	var data []byte
	var info Info
	var err error
	switch kind {
	case Video:
		data, info, err = m.sheet(ctx, f)
	case Photo:
		info.Kind = "photo"
		data, err = m.tool(ctx, f, maxStill, m.ffmpeg, "-fd", "3", "-i", "fd:", "-frames:v", "1",
			"-vf", "scale='min(1600,iw)':-2", "-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "4", "pipe:1")
		if err == nil && len(data) == 0 {
			err = &FailedError{"the picture in this file could not be decoded"}
		}
	case Raw:
		info.Kind = "photo"
		data, info.Width, info.Height, err = embeddedJPEG(f)
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return &FailedError{"making the preview took too long and was stopped"}
		}
		return err
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	blob, _ := json.Marshal(info)
	if err := writeFile(pic, data); err != nil {
		return err
	}
	if err := writeFile(meta, blob); err != nil {
		return err
	}
	m.mu.Lock()
	m.made++
	m.mu.Unlock()
	m.prune()
	return nil
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// prune keeps the cache under its cap by removing the previews that were
// used longest ago.
func (m *Maker) prune() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	type file struct {
		name string
		size int64
		used time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		if st, err := e.Info(); err == nil && !e.IsDir() {
			files = append(files, file{e.Name(), st.Size(), st.ModTime()})
			total += st.Size()
		}
	}
	if total <= m.cap {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= m.cap*9/10 {
			break
		}
		if os.Remove(filepath.Join(m.dir, f.name)) == nil {
			total -= f.size
		}
	}
}

// sheet makes one picture of stills taken at even steps through a film.
func (m *Maker) sheet(ctx context.Context, f *os.File) ([]byte, Info, error) {
	info := Info{Kind: "video"}
	out, err := m.tool(ctx, f, 1<<20, m.ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "format=duration:stream=codec_name,width,height", "-of", "json", "-fd", "3", "fd:")
	if err != nil {
		return nil, info, err
	}
	var probe struct {
		Streams []struct {
			Codec  string `json:"codec_name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &probe) != nil || len(probe.Streams) == 0 {
		return nil, info, &FailedError{"no picture was found in this file; it may not be a film, or it is in a format that cannot be read"}
	}
	info.Codec, info.Width, info.Height = probe.Streams[0].Codec, probe.Streams[0].Width, probe.Streams[0].Height
	info.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)

	// Eight moments at even steps through the first nine tenths of the film:
	// in some containers a seek close to the end lands past the last
	// picture and gives nothing. A film
	// too short for that, or of unknown length, gets its opening frame.
	var times []float64
	if info.Duration >= 2*stillCount {
		for i := range stillCount {
			times = append(times, 0.9*info.Duration*(float64(i)+0.5)/stillCount)
		}
	} else {
		times = []float64{0}
	}
	var stills []image.Image
	for _, t := range times {
		data, err := m.tool(ctx, f, maxStill, m.ffmpeg, "-ss", strconv.FormatFloat(t, 'f', 2, 64), "-fd", "3", "-i", "fd:",
			"-frames:v", "1", "-an", "-sn", "-dn", "-vf", fmt.Sprintf("scale=%d:-2", stillWidth),
			"-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "5", "pipe:1")
		if err != nil {
			if errors.Is(err, ErrNoTool) || ctx.Err() != nil {
				return nil, info, err
			}
			continue // one moment that cannot be decoded does not spoil the rest
		}
		if img, err := jpeg.Decode(bytes.NewReader(data)); err == nil {
			stills = append(stills, img)
		}
	}
	if len(stills) == 0 {
		return nil, info, &FailedError{"no still could be taken from this film"}
	}
	info.Stills = len(stills)

	const gap = 4
	w, h := stills[0].Bounds().Dx(), stills[0].Bounds().Dy()
	cols := min(stillsWide, len(stills))
	rows := (len(stills) + cols - 1) / cols
	sheet := image.NewRGBA(image.Rect(0, 0, cols*w+(cols-1)*gap, rows*h+(rows-1)*gap))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.RGBA{11, 15, 17, 255}), image.Point{}, draw.Src)
	for i, img := range stills {
		at := image.Pt((i%cols)*(w+gap), (i/cols)*(h+gap))
		draw.Draw(sheet, image.Rectangle{Min: at, Max: at.Add(image.Pt(w, h))}, img, img.Bounds().Min, draw.Src)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: 82}); err != nil {
		return nil, info, err
	}
	return buf.Bytes(), info, nil
}

// capped keeps what a tool writes, up to a limit.
type capped struct {
	buf   bytes.Buffer
	limit int64
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - int64(c.buf.Len()); room > 0 {
		c.buf.Write(p[:min(int64(len(p)), room)])
	}
	return len(p), nil // the rest is dropped; the tool is not held up
}

// tool runs ffmpeg or ffprobe on the open file and returns what it wrote.
//
// The file is the tool's descriptor 3 and its only way to any data: the
// "fd" and "pipe" protocols alone are allowed, so a playlist or a
// concatenation script dressed up as a film cannot name other files or
// addresses. The arguments are fixed strings and numbers, never anything
// taken from the file or its name.
func (m *Maker) tool(ctx context.Context, f *os.File, limit int64, path string, args ...string) ([]byte, error) {
	if path == "" {
		return nil, ErrNoTool
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	full := append([]string{"-c", fmt.Sprintf(`ulimit -v %d 2>/dev/null; exec "$@"`, toolMemory), "sh", path,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "fd,pipe"}, args...)
	if filepath.Base(path) == "ffprobe" {
		// ffprobe takes neither -nostdin nor -loglevel placed before its own options.
		full = append([]string{"-c", fmt.Sprintf(`ulimit -v %d 2>/dev/null; exec "$@"`, toolMemory), "sh", path, "-protocol_whitelist", "fd"}, args...)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", full...)
	cmd.ExtraFiles = []*os.File{f}
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Dir = "/"
	cmd.WaitDelay = 2 * time.Second
	out, errs := &capped{limit: limit}, &capped{limit: 2048}
	cmd.Stdout, cmd.Stderr = out, errs
	if m.drop {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	}
	if err := cmd.Start(); err != nil {
		if m.drop && errors.Is(err, syscall.EPERM) {
			return nil, &FailedError{"previews are off: the container may not hand work to an unprivileged user, which needs Docker's default capabilities (SETUID and SETGID)"}
		}
		return nil, err
	}
	unix.Setpriority(unix.PRIO_PROCESS, cmd.Process.Pid, 19) // behind everything else on the server
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &FailedError{"this file could not be read as a film or picture"}
	}
	return out.buf.Bytes(), nil
}

// embeddedJPEG finds the largest JPEG inside a camera's RAW file. Cameras
// store a developed preview there, often at full size, and copying it out
// needs none of the RAW data to be decoded.
func embeddedJPEG(f *os.File) (data []byte, width, height int, err error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxRawRead))
	if err != nil {
		return nil, 0, 0, err
	}
	best, bestArea := []byte(nil), 0
	for i := 0; i+3 < len(buf); i++ {
		if buf[i] != 0xFF || buf[i+1] != 0xD8 || buf[i+2] != 0xFF {
			continue
		}
		end, ok := jpegEnd(buf, i)
		if !ok || end-i > maxOutput {
			continue
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(buf[i:end]))
		if err != nil || cfg.Width*cfg.Height <= bestArea { // RAW sensor data is lossless JPEG in some formats; that is not a picture to show
			continue
		}
		best, bestArea, width, height = buf[i:end], cfg.Width*cfg.Height, cfg.Width, cfg.Height
	}
	if best == nil || width < 160 {
		return nil, 0, 0, &FailedError{"no preview picture was found inside this RAW file"}
	}
	return best, width, height, nil
}

// jpegEnd returns the offset just past the JPEG that starts at start, by
// walking its segments, without decoding it.
func jpegEnd(buf []byte, start int) (int, bool) {
	i := start + 2
	for i+2 <= len(buf) {
		if buf[i] != 0xFF {
			return 0, false
		}
		marker := buf[i+1]
		switch {
		case marker == 0xD9:
			return i + 2, true
		case marker == 0xFF: // padding
			i++
			continue
		case marker >= 0xD0 && marker <= 0xD7, marker == 0x01:
			i += 2
			continue
		}
		if i+4 > len(buf) {
			return 0, false
		}
		size := int(buf[i+2])<<8 | int(buf[i+3])
		if size < 2 {
			return 0, false
		}
		i += 2 + size
		if marker != 0xDA {
			continue
		}
		// After a start-of-scan come the picture's coded bytes, in which FF
		// is always followed by 00 or a restart marker; anything else is
		// the next segment.
		for i+1 < len(buf) {
			if buf[i] == 0xFF && buf[i+1] != 0x00 && !(buf[i+1] >= 0xD0 && buf[i+1] <= 0xD7) && buf[i+1] != 0xFF {
				break
			}
			i++
		}
	}
	return 0, false
}
