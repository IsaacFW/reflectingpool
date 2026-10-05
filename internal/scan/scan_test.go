package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memSink struct {
	entries []Entry
	counted map[int64]bool
}

func (m *memSink) Entries(b []Entry) error {
	m.entries = append(m.entries, b...)
	return nil
}

func (m *memSink) Counted(ids []int64) error {
	m.counted = make(map[int64]bool, len(ids))
	for _, id := range ids {
		m.counted[id] = true
	}
	return nil
}

// paths maps each entry's path relative to the root to the entry.
func (m *memSink) paths(t *testing.T) map[string]Entry {
	t.Helper()
	byID := make(map[int64]Entry, len(m.entries))
	for _, e := range m.entries {
		byID[e.ID] = e
	}
	out := make(map[string]Entry, len(m.entries))
	for _, e := range m.entries {
		var parts []string
		for cur := e; cur.Parent != 0; cur = byID[cur.Parent] {
			parts = append([]string{cur.Name}, parts...)
		}
		out[strings.Join(parts, "/")] = e
	}
	return out
}

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "media/movies/a.mkv"), 5000)
	write(t, filepath.Join(root, "media/movies/b.mkv"), 3000)
	write(t, filepath.Join(root, "media/notes.txt"), 100)
	write(t, filepath.Join(root, "downloads/a.mkv.part"), 700)
	write(t, filepath.Join(root, "skipme/big.bin"), 9000)
	write(t, filepath.Join(root, "top.txt"), 10)
	// a.mkv is also linked from downloads: its bytes must be counted once.
	if err := os.Link(filepath.Join(root, "media/movies/a.mkv"), filepath.Join(root, "downloads/a.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("movies/a.mkv", filepath.Join(root, "media/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "media/empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	sink := &memSink{}
	res, err := Walk(context.Background(), Options{
		Roots:   []string{root},
		Workers: 4,
		Exclude: []string{filepath.Join(root, "skipme")},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	p := sink.paths(t)

	// 7 non-directories: a.mkv, b.mkv, notes.txt, link, a.mkv.part, a.mkv (link), top.txt.
	if res.Files != 7 {
		t.Errorf("files = %d, want 7", res.Files)
	}
	// root, media, movies, empty, downloads, skipme.
	if res.Dirs != 6 {
		t.Errorf("dirs = %d, want 6", res.Dirs)
	}
	linkLen := int64(len("movies/a.mkv"))
	if want := 5000 + 3000 + 100 + 700 + 10 + linkLen; res.Size != want {
		t.Errorf("total size = %d, want %d (hardlink counted once, excluded dir not counted)", res.Size, want)
	}
	if res.Hardlinked != 1 {
		t.Errorf("hardlinked = %d, want 1", res.Hardlinked)
	}

	media, downloads := p["media"], p["downloads"]
	if got := media.Size + downloads.Size; got != 5000+3000+100+700+linkLen {
		t.Errorf("media+downloads = %d: the hardlinked file must be attributed to exactly one of them", got)
	}
	if media.Files != 4 || media.Dirs != 2 {
		t.Errorf("media files/dirs = %d/%d, want 4/2", media.Files, media.Dirs)
	}
	if media.MaxMtime == 0 {
		t.Error("media has no rolled-up modified time")
	}

	a1, a2 := p["media/movies/a.mkv"], p["downloads/a.mkv"]
	if a1.Ino != a2.Ino || a1.Nlink != 2 {
		t.Errorf("hardlink not detected: ino %d/%d nlink %d", a1.Ino, a2.Ino, a1.Nlink)
	}
	if sink.counted[a1.ID] == sink.counted[a2.ID] {
		t.Error("exactly one link of a hardlinked file must be counted")
	}
	if a1.Counted || a2.Counted {
		t.Error("hardlinked files are emitted uncounted and resolved through Counted")
	}

	if e := p["media/link"]; e.Kind != KindSymlink {
		t.Errorf("symlink kind = %d", e.Kind)
	}
	if e := p["skipme"]; e.Flags&FlagExcluded == 0 || e.Files != 0 {
		t.Errorf("excluded dir: flags=%d files=%d", e.Flags, e.Files)
	}
	if _, ok := p["skipme/big.bin"]; ok {
		t.Error("excluded directory was entered")
	}

	// Shares are the top-level directories; everything beneath carries their ID.
	if media.Share != media.ID || a1.Share != media.ID || p["media/movies"].Share != media.ID {
		t.Error("share IDs not propagated")
	}
	if p["top.txt"].Share != 0 {
		t.Error("a file at the root belongs to no share")
	}
}

func TestWalkSkipsZFSControlDir(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".zfs/snapshot/daily/file"), 10)
	write(t, filepath.Join(root, "share/.zfs/kept"), 10)
	sink := &memSink{}
	if _, err := Walk(context.Background(), Options{Roots: []string{root}}, sink); err != nil {
		t.Fatal(err)
	}
	p := sink.paths(t)
	if _, ok := p[".zfs"]; ok {
		t.Error(".zfs at a filesystem root must be skipped")
	}
	if _, ok := p["share/.zfs/kept"]; !ok {
		t.Error("a directory named .zfs elsewhere is ordinary data")
	}
}

// The data directory is usually a folder inside the pool that the container
// also sees at another path. It must be skipped whichever path names it.
func TestWalkExcludesSameDirectoryByAnotherPath(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "appdata/reflectingpool/index/scan.db"), 5000)
	write(t, filepath.Join(root, "appdata/other/config"), 10)
	alias := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(filepath.Join(root, "appdata/reflectingpool"), alias); err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	res, err := Walk(context.Background(), Options{Roots: []string{root}, Exclude: []string{alias, "/does/not/exist"}}, sink)
	if err != nil {
		t.Fatal(err)
	}
	p := sink.paths(t)
	if e := p["appdata/reflectingpool"]; e.Flags&FlagExcluded == 0 {
		t.Error("the directory was not recognised through its other path")
	}
	if _, ok := p["appdata/reflectingpool/index"]; ok || res.Size != 10 {
		t.Errorf("excluded directory was entered: total size %d", res.Size)
	}
}

func TestWalkCancel(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a/b"), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Walk(ctx, Options{Roots: []string{root}}, &memSink{}); err == nil {
		t.Error("cancelled walk returned no error")
	}
}
