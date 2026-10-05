package meta

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// These audit tests assert vulnerable behavior, not the desired fixed behavior.
// Every target is a disposable fixture under t.TempDir().
func TestAuditMetadataDirectorySymlink(t *testing.T) {
	share, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(share, Dir)); err != nil {
		t.Fatal(err)
	}
	if err := Open(share).Put(Annotation{Path: "file.txt", Kind: "file", Note: "audit"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outside, fileName))
	if err != nil || !bytes.Contains(data, []byte("audit")) {
		t.Fatalf("outside write: %q, %v", data, err)
	}
	t.Log("confirmed annotation, summary and README writes escape the share through .reflection symlink")
}

func TestAuditSkipLeafSymlink(t *testing.T) {
	share, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(share, Dir), 0700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(share, Dir, skipName)); err != nil {
		t.Fatal(err)
	}
	if err := Open(share).AddSkips([]string{"attacker-chosen-name"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "original\n\"attacker-chosen-name\"\n" {
		t.Fatalf("outside append: %q, %v", data, err)
	}
	t.Log("confirmed skipped.jsonl leaf symlink appends JSON-encoded paths to arbitrary accessible target")
}

func TestAuditMetadataReadSymlink(t *testing.T) {
	share, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(share, Dir), 0700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "private.jsonl")
	if err := os.WriteFile(victim, []byte("{\"path\":\"private\",\"note\":\"outside secret\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(share, Dir, fileName)); err != nil {
		t.Fatal(err)
	}
	all, err := Open(share).All()
	if err != nil || len(all) != 1 || all[0].Note != "outside secret" {
		t.Fatalf("outside read: %+v, %v", all, err)
	}
	t.Log("confirmed metadata reads also follow leaf symlinks outside the share")
}
