package meta

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSkips(t *testing.T) {
	share := filepath.Join(t.TempDir(), "media")
	if err := os.Mkdir(share, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(share, 0o775); err != nil { // Mkdir is subject to the umask
		t.Fatal(err)
	}
	s := Open(share)
	if got, err := s.Skips(); err != nil || len(got) != 0 {
		t.Fatalf("no list yet: %v, %v", got, err)
	}
	if err := s.RemoveSkip("nothing.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, Dir)); !os.IsNotExist(err) {
		t.Error("reading must not create the metadata folder")
	}

	// Names are whatever the filesystem allows.
	odd := []string{"movies/a.mkv", "line\nbreak.txt", `quote"and\slash`, "ümlaut & <tag>.mkv", "."}
	if err := s.AddSkips(odd[:2]); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSkips(odd[2:]); err != nil {
		t.Fatal(err)
	}
	if got, err := Open(share).Skips(); err != nil || !slices.Equal(got, odd) {
		t.Fatalf("round trip: %q, %v", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(share, Dir, skipName))
	if strings.Count(string(raw), "\n") != len(odd) || !strings.Contains(string(raw), `"ümlaut & <tag>.mkv"`) {
		t.Errorf("one readable line per item expected:\n%s", raw)
	}
	if st, _ := os.Stat(filepath.Join(share, Dir, skipName)); st.Mode().Perm() != 0o664 {
		t.Errorf("list mode = %o, want 664 to match the share", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(share, Dir, fileName)); !os.IsNotExist(err) {
		t.Error("a skip must not create the annotation file")
	}
	if readme, _ := os.ReadFile(filepath.Join(share, Dir, "README.txt")); !strings.Contains(string(readme), skipName) {
		t.Error("the readme does not explain the list")
	}

	if err := s.RemoveSkip("line\nbreak.txt"); err != nil {
		t.Fatal(err)
	}
	err := s.EditSkips(func(paths []string) []string {
		if len(paths) != 4 {
			t.Errorf("after removing one: %q", paths)
		}
		return paths[:1]
	})
	if got, _ := s.Skips(); err != nil || !slices.Equal(got, odd[:1]) {
		t.Fatalf("after editing: %q, %v", got, err)
	}
	// An empty list leaves no file behind.
	if err := s.RemoveSkip("movies/a.mkv"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, Dir, skipName)); !os.IsNotExist(err) {
		t.Error("an empty list was left on disk")
	}
}

// A crash can cut an append short, and the list can be edited by hand. Only
// the damaged line is lost.
func TestSkipsSurviveDamage(t *testing.T) {
	share := t.TempDir()
	if err := os.Mkdir(filepath.Join(share, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	damaged := "\"a.txt\"\nnot json at all\n{\"an\":\"object\"}\n\n\"b.txt\"\n\"cut sho"
	if err := os.WriteFile(filepath.Join(share, Dir, skipName), []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(share)
	if err := s.AddSkips([]string{"c.txt"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Skips(); err != nil || !slices.Equal(got, []string{"a.txt", "b.txt", "c.txt"}) {
		t.Errorf("after a torn write: %q, %v", got, err)
	}
}
