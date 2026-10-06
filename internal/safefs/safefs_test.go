package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinksAreRefusedAtEveryStep(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "share/.reflection"), 0o755)
	os.WriteFile(filepath.Join(root, "share/.reflection/notes"), []byte("ours"), 0o644)
	os.WriteFile(filepath.Join(outside, "victim"), []byte("theirs"), 0o644)
	os.Symlink(outside, filepath.Join(root, "share/linked-dir"))
	os.Symlink(filepath.Join(outside, "victim"), filepath.Join(root, "share/.reflection/linked-file"))
	os.Symlink(filepath.Join(outside, "victim"), filepath.Join(root, "share/.reflection/linked-file"))
	if err := unix.Mkfifo(filepath.Join(root, "share/.reflection/pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Link(filepath.Join(outside, "victim"), filepath.Join(root, "share/.reflection/twin")) // same filesystem under t.TempDir()

	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var refused *Refused
	if _, err := d.Sub("share/linked-dir"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("a name with a separator: %v", err)
	}
	share, err := d.Walk("share")
	if err != nil {
		t.Fatal(err)
	}
	defer share.Close()
	if _, err := share.Sub("linked-dir"); !errors.As(err, &refused) || refused.Why != IsLink {
		t.Errorf("a linked folder: %v", err)
	}
	if _, err := share.Walk("linked-dir/anything"); !errors.As(err, &refused) {
		t.Errorf("walking through a linked folder: %v", err)
	}
	meta, err := share.Sub(".reflection")
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	if _, err := meta.OpenFile("linked-file", os.O_RDWR, 0, false); !errors.As(err, &refused) || refused.Why != IsLink {
		t.Errorf("a linked file: %v", err)
	}
	if _, err := meta.OpenFile("pipe", os.O_RDWR, 0, false); !errors.As(err, &refused) || refused.Why != NotRegular {
		t.Errorf("a pipe (must not hang either): %v", err)
	}
	if _, err := meta.OpenFile("twin", os.O_RDWR|os.O_APPEND, 0, true); !errors.As(err, &refused) || refused.Why != ManyNames {
		t.Errorf("a file with two names, for appending: %v", err)
	}
	if f, err := meta.OpenFile("twin", os.O_RDONLY, 0, false); err != nil {
		t.Errorf("a file with two names, for reading: %v", err)
	} else {
		f.Close()
	}
	if _, err := meta.Sub("notes"); !errors.As(err, &refused) || refused.Why != NotRegular {
		t.Errorf("a file where a folder was expected: %v", err)
	}
	if _, err := meta.OpenFile("missing", os.O_RDONLY, 0, false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "victim")); string(got) != "theirs" {
		t.Errorf("the outside file changed: %q", got)
	}
}

func TestWritingThroughAFolder(t *testing.T) {
	root := t.TempDir()
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if made, err := d.Mkdir("m", 0o750); err != nil || !made {
		t.Fatalf("Mkdir = %v, %v", made, err)
	}
	if made, err := d.Mkdir("m", 0o750); err != nil || made {
		t.Fatalf("second Mkdir = %v, %v", made, err)
	}
	m, err := d.Sub("m")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	tmp, err := m.CreateTemp(".tmp-", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	tmp.WriteString("hello")
	tmp.Close()
	if err := m.Rename(filepath.Base(tmp.Name()), "file"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "m/file")); string(got) != "hello" {
		t.Errorf("file holds %q", got)
	}
	st, err := m.Lstat("file")
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		t.Errorf("Lstat: %o, %v", st.Mode, err)
	}
	same, err := d.Walk("")
	if err != nil {
		t.Fatal(err)
	}
	defer same.Close()
	if _, err := same.Lstat("m"); err != nil {
		t.Errorf("a handle from Walk(\"\"): %v", err)
	}
	if err := m.Remove("file"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Lstat("file"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after Remove: %v", err)
	}
}
