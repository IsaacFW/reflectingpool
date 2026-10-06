package meta

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These tests do what someone who can write to a share might do to turn the
// program's own writes against other places: put a link, a pipe or a file
// of their own where the program keeps its files. Each must be refused with
// the outside file untouched, and reported as a Suspect naming the place.

// pool lays out a root with a share, and an outside folder holding a victim
// file, and returns the three paths.
func pool(t *testing.T) (root, share, victim string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "pool")
	share = filepath.Join(root, "downloads")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	victim = filepath.Join(outside, "victim.txt")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, share, victim
}

func unchanged(t *testing.T, victim string) {
	t.Helper()
	if data, err := os.ReadFile(victim); err != nil || string(data) != "original\n" {
		t.Errorf("the outside file was touched: %q, %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(victim)); len(entries) != 1 {
		t.Errorf("files appeared next to the outside file: %v", entries)
	}
}

func suspectNaming(t *testing.T, err error, what, path string) {
	t.Helper()
	var sus *Suspect
	if !errors.As(err, &sus) {
		t.Fatalf("%s: got %v, want a Suspect", what, err)
	}
	if sus.Path != path {
		t.Errorf("%s: the Suspect names %q, want %q", what, sus.Path, path)
	}
}

func TestAttackMetadataFolderIsALink(t *testing.T) {
	root, share, victim := pool(t)
	if err := os.Symlink(filepath.Dir(victim), filepath.Join(share, Dir)); err != nil {
		t.Fatal(err)
	}
	s := Open(root, "downloads")
	err := s.Put(Annotation{Path: "file.txt", Kind: "file", Note: "audit"})
	suspectNaming(t, err, "saving a note", filepath.Join(share, Dir))
	suspectNaming(t, s.AddSkips([]string{"a"}), "skipping", filepath.Join(share, Dir))
	_, err = s.All()
	suspectNaming(t, err, "reading", filepath.Join(share, Dir))
	suspectNaming(t, s.Check(), "checking", filepath.Join(share, Dir))
	unchanged(t, victim)
}

// A relative link needs no knowledge of how the pool is mapped: "../" from
// the share is the pool, and every other share is next door.
func TestAttackMetadataFolderIsARelativeLinkToAnotherShare(t *testing.T) {
	root, share, _ := pool(t)
	other := Open(root, "documents")
	if err := os.MkdirAll(filepath.Join(root, "documents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := other.Put(Annotation{Path: "taxes", Kind: "dir", Note: "keep"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "documents", Dir, fileName))
	if err := os.Symlink("../documents/"+Dir, filepath.Join(share, Dir)); err != nil {
		t.Fatal(err)
	}
	err := Open(root, "downloads").Put(Annotation{Path: "x", Kind: "file", Note: "mine now"})
	suspectNaming(t, err, "saving a note", filepath.Join(share, Dir))
	after, _ := os.ReadFile(filepath.Join(root, "documents", Dir, fileName))
	if string(after) != string(before) {
		t.Errorf("the other share's notes changed:\n%s", after)
	}
}

func TestAttackShareFolderIsALink(t *testing.T) {
	root, share, victim := pool(t)
	if err := os.Rename(share, share+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(victim), share); err != nil {
		t.Fatal(err)
	}
	s := Open(root, "downloads")
	suspectNaming(t, s.Put(Annotation{Path: "x", Kind: "file", Note: "n"}), "saving a note", share)
	_, err := s.Skips()
	suspectNaming(t, err, "reading skips", share)
	unchanged(t, victim)
}

func TestAttackAnnotationFileIsALink(t *testing.T) {
	root, share, victim := pool(t)
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte(`{"path":"private","note":"outside secret"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(share, Dir, fileName)); err != nil {
		t.Fatal(err)
	}
	s := Open(root, "downloads")
	all, err := s.All()
	if len(all) != 0 {
		t.Errorf("the outside file was read as notes: %+v", all)
	}
	suspectNaming(t, err, "reading", filepath.Join(share, Dir, fileName))
	suspectNaming(t, s.Put(Annotation{Path: "x", Kind: "file", Note: "n"}), "saving", filepath.Join(share, Dir, fileName))
	if data, _ := os.ReadFile(victim); string(data) != `{"path":"private","note":"outside secret"}`+"\n" {
		t.Errorf("the outside file was changed: %q", data)
	}
}

func TestAttackSkipFileIsALink(t *testing.T) {
	root, share, victim := pool(t)
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(share, Dir, skipName)); err != nil {
		t.Fatal(err)
	}
	s := Open(root, "downloads")
	suspectNaming(t, s.AddSkips([]string{"attacker-chosen-name"}), "skipping", filepath.Join(share, Dir, skipName))
	suspectNaming(t, s.RemoveSkip("anything"), "editing skips", filepath.Join(share, Dir, skipName))
	unchanged(t, victim)
}

// A hard link is not a symbolic link: our name and theirs are the same file,
// so appending through ours would write into theirs. Only the appending path
// writes in place; everything else replaces the name with a fresh file.
func TestAttackSkipFileHasTwoNames(t *testing.T) {
	root, share, victim := pool(t)
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, filepath.Join(share, Dir, skipName)); err != nil {
		t.Skipf("hard links are not possible here: %v", err)
	}
	s := Open(root, "downloads")
	suspectNaming(t, s.AddSkips([]string{"attacker-chosen-name"}), "skipping", filepath.Join(share, Dir, skipName))
	suspectNaming(t, s.Check(), "checking", filepath.Join(share, Dir, skipName))
	unchanged(t, victim)
}

// A pipe in place of a file would hang a plain read forever: the scan that
// loads annotations would never finish.
func TestAttackAnnotationFileIsAPipe(t *testing.T) {
	root, share, _ := pool(t)
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(share, Dir, fileName), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Open(root, "downloads").All(); done <- err }()
	select {
	case err := <-done:
		suspectNaming(t, err, "reading", filepath.Join(share, Dir, fileName))
	case <-time.After(5 * time.Second):
		t.Fatal("reading a pipe hung")
	}
}

// A file far larger than any real annotation file is not read: loading it
// whole would take the server's memory.
func TestAttackAnnotationFileIsHuge(t *testing.T) {
	root, share, _ := pool(t)
	old := maxFile
	maxFile = 64 << 10
	t.Cleanup(func() { maxFile = old })
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, Dir, fileName), make([]byte, 65<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(root, "downloads")
	_, err := s.All()
	suspectNaming(t, err, "reading", filepath.Join(share, Dir, fileName))
	suspectNaming(t, s.Check(), "checking", filepath.Join(share, Dir, fileName))
}

func TestAttackSummaryAndReadmeAreLinks(t *testing.T) {
	root, share, victim := pool(t)
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{indexName, "README.txt"} {
		if err := os.Symlink(victim, filepath.Join(share, Dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	s := Open(root, "downloads")
	err := s.Put(Annotation{Path: "x", Kind: "file", Note: "n"})
	suspectNaming(t, err, "saving", filepath.Join(share, Dir, indexName))
	unchanged(t, victim)
}

// A healthy share reports nothing, so the check can run on every listing.
func TestCheckPassesAHealthyShare(t *testing.T) {
	root, _, _ := pool(t)
	s := Open(root, "downloads")
	if err := s.Check(); err != nil {
		t.Errorf("before anything is written: %v", err)
	}
	if err := s.Put(Annotation{Path: "x", Kind: "file", Note: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSkips([]string{"y"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(); err != nil {
		t.Errorf("after writing: %v", err)
	}
}
