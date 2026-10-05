package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/scan"
)

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// build scans root into a fresh index and opens it.
func build(t *testing.T, root string) *Index {
	t.Helper()
	dir := t.TempDir()
	b, err := NewBuilder(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan.Walk(context.Background(), scan.Options{Roots: []string{root}}, b)
	if err != nil {
		t.Fatal(err)
	}
	path, err := b.Finish(Info{Roots: []string{root}, Files: res.Files, Size: res.Size}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if Latest(dir) != path {
		t.Fatalf("Latest = %q, want %q", Latest(dir), path)
	}
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func fixture(t *testing.T) (string, *Index) {
	root := t.TempDir()
	write(t, filepath.Join(root, "media/movies/big.mkv"), 9000)
	write(t, filepath.Join(root, "media/movies/small.mkv"), 2000)
	write(t, filepath.Join(root, "media/music/song.flac"), 4000)
	write(t, filepath.Join(root, "media/music/tiny.mp3"), 50)
	write(t, filepath.Join(root, "docs/report.pdf"), 300)
	write(t, filepath.Join(root, "docs/50%_done.txt"), 20)
	if err := os.Link(filepath.Join(root, "media/movies/big.mkv"), filepath.Join(root, "docs/big-link.mkv")); err != nil {
		t.Fatal(err)
	}
	return root, build(t, root)
}

func names(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTreeAndPaths(t *testing.T) {
	ctx := context.Background()
	root, ix := fixture(t)

	roots, total, err := ix.Children(ctx, 0, Sort{}, 0, 0)
	if err != nil || total != 1 || roots[0].Name != root {
		t.Fatalf("roots = %v, %d, %v", names(roots), total, err)
	}
	if ix.Info.Files != 7 {
		t.Errorf("info.files = %d", ix.Info.Files)
	}

	shares, err := ix.Shares(ctx)
	if err != nil || !equal(names(shares), []string{"docs", "media"}) {
		t.Fatalf("shares = %v, %v", names(shares), err)
	}

	// The hardlinked 9000-byte file is counted once across the whole tree.
	if want := int64(9000 + 2000 + 4000 + 50 + 300 + 20); roots[0].Size != want {
		t.Errorf("root size = %d, want %d", roots[0].Size, want)
	}

	media, err := ix.Lookup(ctx, filepath.Join(root, "media"))
	if err != nil {
		t.Fatal(err)
	}
	kids, total, err := ix.Children(ctx, media.ID, Sort{Key: "size", Desc: true}, 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("children: %d, %v", total, err)
	}
	if kids[1].Name != "music" || kids[1].Size != 4050 || kids[1].Files != 2 {
		t.Errorf("music = %+v", kids[1])
	}

	big, err := ix.Lookup(ctx, filepath.Join(root, "media/movies/big.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if big.Type != "video" || big.Ext != "mkv" || big.Nlink != 2 {
		t.Errorf("big = %+v", big)
	}
	path, err := ix.EntryPath(ctx, big.ID)
	if err != nil || path != filepath.Join(root, "media/movies/big.mkv") {
		t.Errorf("path = %q, %v", path, err)
	}
	links, err := ix.ByInode(ctx, big.Dev, big.Ino)
	if err != nil || len(links) != 2 {
		t.Fatalf("links = %d, %v", len(links), err)
	}
	if links[0].Counted == links[1].Counted {
		t.Error("exactly one hardlink must be counted")
	}

	if _, err := ix.Lookup(ctx, filepath.Join(root, "media/nope")); err != ErrNotFound {
		t.Errorf("missing path: %v", err)
	}
	if _, err := ix.Lookup(ctx, root+"-sibling/x"); err != ErrNotFound {
		t.Errorf("path outside root: %v", err)
	}
}

func TestFind(t *testing.T) {
	ctx := context.Background()
	_, ix := fixture(t)

	rows, err := ix.Find(ctx, Filter{Kind: "file", Types: []string{"video"}}, Sort{Key: "size", Desc: true}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(rows); len(got) != 3 || got[2] != "small.mkv" {
		t.Errorf("videos by size = %v", got)
	}
	if err := ix.FillPaths(ctx, rows); err != nil || filepath.Base(rows[2].Path) != "small.mkv" || !filepath.IsAbs(rows[2].Path) {
		t.Errorf("FillPaths: %q, %v", rows[2].Path, err)
	}

	rows, err = ix.Find(ctx, Filter{Kind: "file", MinSize: 3000, MaxSize: 5000}, Sort{}, 10, 0)
	if err != nil || !equal(names(rows), []string{"song.flac"}) {
		t.Errorf("size range = %v, %v", names(rows), err)
	}

	// % and _ in a search are literal characters.
	rows, err = ix.Find(ctx, Filter{Name: "50%_D"}, Sort{}, 10, 0)
	if err != nil || !equal(names(rows), []string{"50%_done.txt"}) {
		t.Errorf("literal search = %v, %v", names(rows), err)
	}
	rows, err = ix.Find(ctx, Filter{Name: "%"}, Sort{}, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Errorf("%% must not match everything: %v, %v", names(rows), err)
	}

	if _, err := ix.Find(ctx, Filter{}, Sort{Key: "name; DROP TABLE entries"}, 10, 0); err == nil {
		t.Error("unknown sort key accepted")
	}
	if _, err := ix.Find(ctx, Filter{Types: []string{"nope"}}, Sort{}, 10, 0); err == nil {
		t.Error("unknown type accepted")
	}
}

func TestQueue(t *testing.T) {
	ctx := context.Background()
	_, ix := fixture(t)
	spec := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"share", "type"}, Order: Sort{Key: "size", Desc: true}}

	q, err := ix.Queue(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	// media (15050 bytes) outranks docs (9320); within media, video outranks audio.
	if q.Remaining != 7 || q.GroupCount != 5 {
		t.Fatalf("remaining=%d groups=%d", q.Remaining, q.GroupCount)
	}
	var order []string
	for _, g := range q.Groups {
		order = append(order, g.Values[0].Label+"/"+g.Values[1].Label)
	}
	if want := []string{"media/video", "media/audio", "docs/video", "docs/document", "docs/text"}; !equal(order, want) {
		t.Fatalf("group order = %v, want %v", order, want)
	}
	if !equal(names(q.Items), []string{"big.mkv", "small.mkv"}) {
		t.Fatalf("items = %v", names(q.Items))
	}

	// Annotating or skipping an item removes it; the group is served until empty.
	if err := ix.SetAnnot(ctx, Annot{Entry: q.Items[0].ID, State: StateNote, Prefixes: []string{"KEEP"}}); err != nil {
		t.Fatal(err)
	}
	q, _ = ix.Queue(ctx, spec)
	if !equal(names(q.Items), []string{"small.mkv"}) || q.Groups[0].Remaining != 1 {
		t.Fatalf("after annotating: items=%v remaining=%d", names(q.Items), q.Groups[0].Remaining)
	}
	if err := ix.SetAnnot(ctx, Annot{Entry: q.Items[0].ID, State: StateSkipped}); err != nil {
		t.Fatal(err)
	}
	q, _ = ix.Queue(ctx, spec)
	if q.Groups[0].Values[1].Label != "audio" || !equal(names(q.Items), []string{"song.flac", "tiny.mp3"}) {
		t.Fatalf("next group: %v %v", q.Groups[0].Values, names(q.Items))
	}

	rows, err := ix.Find(ctx, Filter{Prefix: "KEEP"}, Sort{}, 10, 0)
	if err != nil || !equal(names(rows), []string{"big.mkv"}) || rows[0].State != StateNote {
		t.Errorf("prefix filter = %v, %v", names(rows), err)
	}
	rows, _ = ix.Find(ctx, Filter{State: "skipped"}, Sort{}, 10, 0)
	if !equal(names(rows), []string{"small.mkv"}) {
		t.Errorf("skipped = %v", names(rows))
	}

	if _, err := ix.Queue(ctx, QueueSpec{Groups: []string{"bogus"}}); err == nil {
		t.Error("unknown group key accepted")
	}

	// With no grouping the queue is one flat list.
	q, err = ix.Queue(ctx, QueueSpec{Filter: Filter{Kind: "file"}, Order: Sort{Key: "name"}, Limit: 2})
	if err != nil || len(q.Items) != 2 || q.Remaining != 5 {
		t.Errorf("flat queue: %d items, %d remaining, %v", len(q.Items), q.Remaining, err)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"scan-20260101T000000.000Z.db", "scan-20260102T000000.000Z.db", "scan-20260103T000000.000Z.db", "scan-20260104T000000.000Z.db.tmp"} {
		write(t, filepath.Join(dir, n), 1)
	}
	Prune(dir, 2)
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 2 || filepath.Base(left[0]) != "scan-20260102T000000.000Z.db" {
		t.Errorf("after prune: %v", left)
	}
}

func TestClassify(t *testing.T) {
	for name, want := range map[string]string{
		"Movie.MKV": "video", "a.tar.gz": "archive", ".bashrc": "other", "noext": "other",
		"trailing.": "other", "x.ts": "video", "weird.ext-with-dash": "other",
	} {
		if _, cat := classify(name); CatName(cat) != want {
			t.Errorf("classify(%q) = %s, want %s", name, CatName(cat), want)
		}
	}
}
