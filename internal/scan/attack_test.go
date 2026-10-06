package scan

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A folder is queued by path while its parent is being read, and opened
// later. Between the two, someone who can write to the share can swap a
// folder above it for a link to somewhere else, and the open would land
// there. The opened folder has to be the one that was listed.
func TestAttackQueuedFolderAncestorSwappedForALink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	share := filepath.Join(root, "share")
	queued := filepath.Join(share, "queued")
	if err := os.MkdirAll(queued, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "queued"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "queued", "private-name.txt"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stx unix.Statx_t
	if err := statx(unix.AT_FDCWD, queued, 0, &stx); err != nil {
		t.Fatal(err)
	}
	w := &walker{prog: &Progress{}, out: make(chan []Entry, 1), links: make(map[linkKey]link)}
	n := &node{id: 3, name: "queued", share: 2, dev: devOf(&stx), ino: stx.Ino}
	it := work{n: n, path: queued}

	// First the honest case: the folder is where it was listed.
	if err := os.WriteFile(filepath.Join(queued, "ours.txt"), []byte("ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.readDir(it)
	select {
	case batch := <-w.out:
		if len(batch) != 1 || batch[0].Name != "ours.txt" {
			t.Fatalf("the folder's own entries: %+v", batch)
		}
	default:
		t.Fatal("the folder was not read")
	}

	// Then the swap.
	if err := os.Rename(share, share+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, share); err != nil {
		t.Fatal(err)
	}
	n.flags = 0
	w.readDir(it)
	select {
	case batch := <-w.out:
		t.Fatalf("entries from outside the share were indexed: %+v", batch)
	default:
	}
	if n.flags&FlagError == 0 || w.prog.Errors.Load() != 1 {
		t.Errorf("the swapped folder was not marked as an error: flags %b, errors %d", n.flags, w.prog.Errors.Load())
	}
}
