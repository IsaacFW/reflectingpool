package meta

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	share := filepath.Join(t.TempDir(), "media")
	if err := os.Mkdir(share, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(share, 0o775); err != nil { // Mkdir is subject to the umask
		t.Fatal(err)
	}
	s := Open(share)

	if all, err := s.All(); err != nil || len(all) != 0 {
		t.Fatalf("empty store: %v, %v", all, err)
	}
	if _, err := os.Stat(filepath.Join(share, Dir)); !os.IsNotExist(err) {
		t.Error("reading must not create the metadata folder")
	}

	a := Annotation{
		Path: "movies/home", Kind: "dir", Note: "Family videos | 2010-2019\nkeep",
		Prefixes: []string{"ARCHIVE"}, Owner: "isaac", ReviewAfter: "2027-01-01",
		Identity: Identity{Dataset: "tank/media", Ino: 42, Btime: 1700000000},
		Updated:  time.Unix(1790000000, 0).UTC(),
	}
	if err := s.Put(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Annotation{Path: "x.bin", Kind: "file", Skipped: true}); err != nil {
		t.Fatal(err)
	}

	// A second store on the same share reads what the first wrote.
	got, ok, err := Open(share).Get("movies/home")
	if err != nil || !ok || got.Note != a.Note || got.Identity != a.Identity || !got.Updated.Equal(a.Updated) {
		t.Fatalf("round trip: %+v, %v, %v", got, ok, err)
	}

	raw, err := os.ReadFile(filepath.Join(share, Dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte("\n")); n != 2 {
		t.Errorf("expected one line per annotation, got %d lines", n)
	}

	st, _ := os.Stat(filepath.Join(share, Dir))
	if st.Mode().Perm() != 0o775 {
		t.Errorf("metadata folder mode = %o, want the share's 775", st.Mode().Perm())
	}
	st, _ = os.Stat(filepath.Join(share, Dir, fileName))
	if st.Mode().Perm() != 0o664 {
		t.Errorf("annotation file mode = %o, want 664", st.Mode().Perm())
	}

	summary, err := os.ReadFile(filepath.Join(share, Dir, indexName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), `| movies/home/ | ARCHIVE | Family videos \| 2010-2019 keep | isaac | 2027-01-01 |`) {
		t.Errorf("summary row missing or unescaped:\n%s", summary)
	}
	if strings.Contains(string(summary), "x.bin") {
		t.Error("a skipped item with nothing recorded does not belong in the summary")
	}
	if _, err := os.Stat(filepath.Join(share, Dir, "README.txt")); err != nil {
		t.Error("README.txt not written")
	}
	if left, _ := filepath.Glob(filepath.Join(share, Dir, ".tmp-*")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	if err := s.Delete("x.bin"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.All(); len(all) != 1 {
		t.Errorf("after delete: %d annotations", len(all))
	}
}

func TestStoreSeesOutsideEdits(t *testing.T) {
	share := t.TempDir()
	s := Open(share)
	if err := s.Put(Annotation{Path: "a", Kind: "file", Note: "one"}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(share, Dir, fileName)
	edited := `{"path":"a","kind":"file","note":"edited by hand","identity":{},"updated":"2026-01-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(file, []byte(edited), 0o664); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(file, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	got, _, err := s.Get("a")
	if err != nil || got.Note != "edited by hand" {
		t.Errorf("outside edit not picked up: %+v, %v", got, err)
	}
}

func TestStoreRefusesToOverwriteDamagedFile(t *testing.T) {
	share := t.TempDir()
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(share, Dir, fileName)
	damaged := `{"path":"a","kind":"file","note":"good"}` + "\n" + `{"path":"b", oops` + "\n"
	if err := os.WriteFile(file, []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(share)
	if err := s.Put(Annotation{Path: "c", Kind: "file"}); err == nil {
		t.Fatal("wrote over a file it could not fully read")
	}
	after, _ := os.ReadFile(file)
	if string(after) != damaged {
		t.Error("damaged file was modified")
	}
}

func TestFingerprint(t *testing.T) {
	big := make([]byte, 300*1024)
	for i := range big {
		big[i] = byte(i * 7)
	}
	a, err := Fingerprint(bytes.NewReader(big), int64(len(big)))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := Fingerprint(bytes.NewReader(big), int64(len(big))); a != b {
		t.Error("fingerprint is not stable")
	}
	tail := bytes.Clone(big)
	tail[len(tail)-1] ^= 1
	if b, _ := Fingerprint(bytes.NewReader(tail), int64(len(tail))); a == b {
		t.Error("a change in the last block went unnoticed")
	}
	head := bytes.Clone(big)
	head[0] ^= 1
	if b, _ := Fingerprint(bytes.NewReader(head), int64(len(head))); a == b {
		t.Error("a change in the first block went unnoticed")
	}
	for _, size := range []int{0, 1, fingerprintSpan, fingerprintSpan + 1} {
		if _, err := Fingerprint(bytes.NewReader(big[:size]), int64(size)); err != nil {
			t.Errorf("size %d: %v", size, err)
		}
	}
}
