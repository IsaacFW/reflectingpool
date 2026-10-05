package preview

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func picture(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fileWith(t *testing.T, name string, data []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestKindOf(t *testing.T) {
	for _, c := range []struct {
		typ, ext string
		want     Kind
	}{{"video", "mkv", Video}, {"video", "avi", Video}, {"image", "heic", Photo}, {"image", "cr2", Raw}, {"image", "nef", Raw}, {"image", "jpg", None}, {"text", "txt", None}} {
		if got := KindOf(c.typ, c.ext); got != c.want {
			t.Errorf("KindOf(%s, %s) = %d, want %d", c.typ, c.ext, got, c.want)
		}
	}
}

// A RAW file is previewed by copying out the JPEG the camera put in it. The
// largest one is the preview; the others are a thumbnail, or bytes that only
// look like the start of a picture.
func TestRawPreviewIsTheEmbeddedJPEG(t *testing.T) {
	small, large := picture(t, 160, 120), picture(t, 800, 600)
	var raw bytes.Buffer
	raw.WriteString("II*\x00")
	raw.Write(bytes.Repeat([]byte{0x12, 0xFF, 0xD8, 0x00}, 50)) // not pictures
	raw.Write(small)
	raw.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}) // a start with nothing after it
	raw.Write(bytes.Repeat([]byte{7}, 4096))
	raw.Write(large)
	raw.Write(bytes.Repeat([]byte{9}, 1000))

	m := New(t.TempDir(), 1<<20)
	path, info, err := m.Get(context.Background(), "raw-1", Raw, fileWith(t, "shot.cr2", raw.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, large) || info.Width != 800 || info.Height != 600 || info.Kind != "photo" {
		t.Errorf("preview is %d bytes (%dx%d, %s); want the %d-byte 800x600 picture", len(got), info.Width, info.Height, info.Kind, len(large))
	}
	// Asked for again, it comes from the cache.
	if _, _, err := m.Get(context.Background(), "raw-1", Raw, fileWith(t, "other.cr2", nil)); err != nil || m.made != 1 {
		t.Errorf("second request: made %d previews, %v", m.made, err)
	}

	var failed *FailedError
	if _, _, err := m.Get(context.Background(), "raw-2", Raw, fileWith(t, "empty.cr2", []byte("II*\x00 nothing here"))); !errors.As(err, &failed) {
		t.Errorf("a RAW file with no picture in it: %v", err)
	}
	if _, _, err := m.Get(context.Background(), "x", None, fileWith(t, "a.txt", nil)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a kind with no preview: %v", err)
	}
}

func TestTheCacheKeepsToItsSize(t *testing.T) {
	dir := t.TempDir()
	large := picture(t, 800, 600)
	m := New(dir, int64(len(large))*2+len64("{}")*8)
	for i, key := range []string{"a", "b", "c", "d"} {
		if _, _, err := m.Get(context.Background(), key, Raw, fileWith(t, key+".dng", large)); err != nil {
			t.Fatal(err)
		}
		// Each is used a little later than the one before.
		when := time.Now().Add(time.Duration(i-10) * time.Hour)
		os.Chtimes(filepath.Join(dir, key+".jpg"), when, when)
		os.Chtimes(filepath.Join(dir, key+".json"), when, when)
	}
	m.prune()
	if _, err := os.Stat(filepath.Join(dir, "a.jpg")); !os.IsNotExist(err) {
		t.Error("the preview used longest ago is still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "d.jpg")); err != nil {
		t.Errorf("the newest preview was cleared out: %v", err)
	}
}

func len64(s string) int64 { return int64(len(s)) }

// film makes a test film with ffmpeg, or skips the test where there is none.
func film(t *testing.T, name string, seconds int, codec string) *os.File {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed here")
	}
	path := filepath.Join(t.TempDir(), name)
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "testsrc=duration="+string(rune('0'+seconds/10))+string(rune('0'+seconds%10))+":size=640x360:rate=5", "-c:v", codec, path).CombinedOutput()
	if err != nil {
		t.Fatalf("making a test film: %v\n%s", err, out)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestAFilmBecomesASheetOfStills(t *testing.T) {
	f := film(t, "film.mpg", 24, "mpeg2video")
	m := New(t.TempDir(), 1<<30)
	path, info, err := m.Get(context.Background(), "film-1", Video, f)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	sheet, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the sheet is not a JPEG: %v", err)
	}
	// Eight stills, four across, each 320 wide.
	if b := sheet.Bounds(); b.Dx() != 4*320+3*4 || b.Dy() != 2*180+4 || info.Stills != 8 {
		t.Errorf("sheet is %dx%d with %d stills", b.Dx(), b.Dy(), info.Stills)
	}
	if info.Kind != "video" || info.Codec != "mpeg2video" || info.Width != 640 || info.Height != 360 || info.Duration < 23 || info.Duration > 25 {
		t.Errorf("info = %+v", info)
	}
	if _, again, err := m.Get(context.Background(), "film-1", Video, f); err != nil || m.made != 1 || again != info {
		t.Errorf("second request: made %d, %+v, %v", m.made, again, err)
	}

	// A film too short to take eight moments from gets its opening frame.
	_, info, err = m.Get(context.Background(), "film-2", Video, film(t, "short.mpg", 2, "mpeg2video"))
	if err != nil || info.Stills != 1 {
		t.Errorf("short film: %+v, %v", info, err)
	}
}

// The tool is given the file and nothing else. A playlist dressed up as a
// film, naming another file, must not be followed.
func TestAFileCannotMakeTheToolOpenAnother(t *testing.T) {
	secret := film(t, "secret.mpg", 2, "mpeg2video")
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\n" + secret.Name() + "\n#EXT-X-ENDLIST\n"
	concat := "ffconcat version 1.0\nfile '" + secret.Name() + "'\n"
	m := New(t.TempDir(), 1<<30)
	for name, body := range map[string]string{"trick.mkv": playlist, "trick.mp4": concat, "junk.avi": "not a film at all"} {
		var failed *FailedError
		if _, _, err := m.Get(context.Background(), name, Video, fileWith(t, name, []byte(body))); !errors.As(err, &failed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if m.made != 0 {
		t.Errorf("%d previews were made from files that are not films", m.made)
	}
}
