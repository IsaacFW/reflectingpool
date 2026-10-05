package scan

import (
	"os"
	"path/filepath"
	"testing"
)

// Simulate a directory already queued by subdir, then replace its ancestor.
func TestAuditQueuedDirectoryAncestorSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	share := filepath.Join(root, "share")
	if err := os.MkdirAll(filepath.Join(share, "queued"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "queued"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "queued", "private-name.txt"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	w := &walker{prog: &Progress{}, out: make(chan []Entry, 1), links: make(map[linkKey]link)}
	n := &node{id: 3, name: "queued", share: 2}
	it := work{n: n, path: filepath.Join(share, "queued")}
	if err := os.Rename(share, share+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, share); err != nil {
		t.Fatal(err)
	}
	w.readDir(it)
	select {
	case batch := <-w.out:
		if len(batch) != 1 || batch[0].Name != "private-name.txt" {
			t.Fatalf("unexpected outside entries: %+v", batch)
		}
		t.Log("confirmed queued scan follows symlinked ancestor and indexes outside file metadata")
	default:
		t.Fatal("outside directory was not indexed")
	}
}
