package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/IsaacFW/reflectingpool/internal/index"
	"github.com/IsaacFW/reflectingpool/internal/meta"
)

func (e *env) skip(path string) {
	e.t.Helper()
	an, err := e.app.PutAnnotation(context.Background(), e.id(path), AnnotationInput{})
	if err != nil || !an.Skipped {
		e.t.Fatalf("skip %s: %+v, %v", path, an, err)
	}
}

func (e *env) unreview(path string) {
	e.t.Helper()
	if err := e.app.DeleteAnnotation(context.Background(), e.id(path)); err != nil {
		e.t.Fatalf("delete %s: %v", path, err)
	}
}

func (e *env) skips(share string) []string {
	e.t.Helper()
	list, err := meta.Open(filepath.Join(e.root, share)).Skips()
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func (e *env) queue(spec index.QueueSpec) index.QueueResult {
	e.t.Helper()
	var res index.QueueResult
	err := e.app.View(func(ix *index.Index) (err error) {
		res, err = ix.Queue(context.Background(), spec)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func itemNames(q index.QueueResult) []string {
	var out []string
	for _, it := range q.Items {
		out = append(out, it.Name)
	}
	return out
}

func TestMetaDirNamesAgree(t *testing.T) {
	if index.MetaDir != meta.Dir {
		t.Fatalf("the index hides %q from queues but the metadata lives in %q", index.MetaDir, meta.Dir)
	}
}

func TestSkipsAreKeptApartFromAnnotations(t *testing.T) {
	e := newEnv(t, false, map[string]string{"share/a.mkv": "aaaa", "share/b.mkv": "bbb", "share/c.mkv": "cc", "share/d.mkv": "d"})
	e.scan()
	annotations := filepath.Join(e.root, "share", meta.Dir, "annotations.jsonl")

	e.skip("share/a.mkv")
	e.skip("share/b.mkv")
	e.skip("share/b.mkv") // skipping twice records it once
	if got := e.skips("share"); !slices.Equal(got, []string{"a.mkv", "b.mkv"}) {
		t.Fatalf("skipped list = %q", got)
	}
	if _, err := os.Stat(annotations); !os.IsNotExist(err) {
		t.Error("a skip was written among the annotations")
	}
	if an, found, err := e.app.Annotation(context.Background(), e.id("share/a.mkv")); err != nil || !found || !an.Skipped || an.Path != "a.mkv" {
		t.Errorf("a skipped item reads back as %+v, %v, %v", an, found, err)
	}

	// A blank save over an annotation withdraws what was recorded.
	e.annotate("share/c.mkv", "recorded, then thought better of")
	e.skip("share/c.mkv")
	if _, kept := e.stored("share")["c.mkv"]; kept || e.state("share/c.mkv") != index.StateSkipped {
		t.Errorf("blank save left the annotation: kept=%v state=%d", kept, e.state("share/c.mkv"))
	}
	// An annotation outranks an earlier skip, at once and after a scan.
	e.annotate("share/b.mkv", "worth describing after all")
	if e.state("share/b.mkv") != index.StateNote {
		t.Errorf("annotated after a skip: state %d", e.state("share/b.mkv"))
	}

	// A skip does not follow a rename: the item returns to review.
	e.mv("share/a.mkv", "share/renamed.mkv")
	e.scan()
	if e.state("share/renamed.mkv") != 0 || e.state("share/b.mkv") != index.StateNote || e.state("share/c.mkv") != index.StateSkipped {
		t.Errorf("after a scan: renamed=%d b=%d c=%d", e.state("share/renamed.mkv"), e.state("share/b.mkv"), e.state("share/c.mkv"))
	}
	// The scan tidied the list: the renamed item and the annotated one are gone from it.
	if got := e.skips("share"); !slices.Equal(got, []string{"c.mkv"}) {
		t.Errorf("skipped list after a scan = %q", got)
	}

	data := e.app.Config().DataDir
	e.app.Close()
	again := openEnv(t, e.root, data, false)
	if again.state("share/c.mkv") != index.StateSkipped {
		t.Error("the skip did not survive a restart")
	}

	// Returning an item to review removes its skip for good, also the one
	// left behind when a skipped item was annotated later.
	again.unreview("share/c.mkv")
	again.skip("share/d.mkv")
	again.annotate("share/d.mkv", "x")
	again.unreview("share/d.mkv")
	if got := again.skips("share"); len(got) != 0 {
		t.Errorf("skipped list after returning items to review = %q", got)
	}
	again.scan()
	if again.state("share/c.mkv") != 0 || again.state("share/d.mkv") != 0 {
		t.Errorf("a scan brought skips back: c=%d d=%d", again.state("share/c.mkv"), again.state("share/d.mkv"))
	}
}

// Skips used to be stored among the annotations. They move to the skipped
// list at the first scan, or are honoured where they are in read-only mode.
func TestOldSkipRecordsMoveToTheSkippedList(t *testing.T) {
	files := map[string]string{"share/old-skip.mkv": "1", "share/empty.mkv": "2", "share/noted.mkv": "3"}
	old := `{"path":"old-skip.mkv","kind":"file","skipped":true,"identity":{},"updated":"2026-01-01T00:00:00Z"}
{"path":"empty.mkv","kind":"file","identity":{},"updated":"2026-01-01T00:00:00Z"}
{"path":"gone.mkv","kind":"file","skipped":true,"identity":{},"updated":"2026-01-01T00:00:00Z"}
{"path":"noted.mkv","kind":"file","note":"keep","skipped":true,"identity":{},"updated":"2026-01-01T00:00:00Z"}
`
	plant := func(e *env) string {
		dir := filepath.Join(e.root, "share", meta.Dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "annotations.jsonl")
		if err := os.WriteFile(file, []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}

	e := newEnv(t, false, files)
	file := plant(e)
	e.scan()
	if got := e.skips("share"); !slices.Equal(got, []string{"empty.mkv", "old-skip.mkv"}) {
		t.Errorf("skipped list = %q", got)
	}
	stored := e.stored("share")
	if len(stored) != 1 || stored["noted.mkv"].Note != "keep" || stored["noted.mkv"].Skipped {
		t.Errorf("annotations left = %+v", stored)
	}
	if e.state("share/old-skip.mkv") != index.StateSkipped || e.state("share/empty.mkv") != index.StateSkipped || e.state("share/noted.mkv") != index.StateNote {
		t.Errorf("states: %d %d %d", e.state("share/old-skip.mkv"), e.state("share/empty.mkv"), e.state("share/noted.mkv"))
	}
	// A second scan finds nothing left to move.
	before, _ := os.ReadFile(file)
	e.scan()
	if after, _ := os.ReadFile(file); string(after) != string(before) || len(e.skips("share")) != 2 {
		t.Error("the second scan changed the files again")
	}

	ro := newEnv(t, true, files)
	file = plant(ro)
	ro.scan()
	if after, _ := os.ReadFile(file); string(after) != old || len(ro.skips("share")) != 0 {
		t.Error("a read-only scan changed the files")
	}
	if ro.state("share/old-skip.mkv") != index.StateSkipped || ro.state("share/noted.mkv")&index.StateNote == 0 {
		t.Errorf("read-only states: %d %d", ro.state("share/old-skip.mkv"), ro.state("share/noted.mkv"))
	}
}

func TestSkipGroup(t *testing.T) {
	ctx := context.Background()
	defer func(n int) { skipChunk = n }(skipChunk)
	skipChunk = 1 // every item takes its own round
	files := map[string]string{
		"media/a.mkv": "aaaaa", "media/b.mkv": "bbbb", "media/c.mp3": "ccc",
		"docs/d.mkv": "dd", "docs/e.pdf": "e",
	}
	e := newEnv(t, false, files)
	e.scan()
	spec := index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"type"}}
	e.annotate("media/b.mkv", "already described")
	q := e.queue(spec)
	if q.Groups[0].Values[0].Value != "video" || q.Groups[0].Remaining != 2 {
		t.Fatalf("first group = %+v", q.Groups[0])
	}

	// The group runs across two shares; each share's list gets its own items.
	n, err := e.app.SkipGroup(ctx, spec, []any{q.Groups[0].Values[0].Value})
	if err != nil || n != 2 {
		t.Fatalf("SkipGroup = %d, %v", n, err)
	}
	if got := e.skips("media"); !slices.Equal(got, []string{"a.mkv"}) {
		t.Errorf("media skipped list = %q", got)
	}
	if got := e.skips("docs"); !slices.Equal(got, []string{"d.mkv"}) {
		t.Errorf("docs skipped list = %q", got)
	}
	if e.note("media/b.mkv") != "already described" {
		t.Error("the bulk skip touched an annotated item")
	}
	if q := e.queue(spec); q.Remaining != 2 || q.GroupPosition != 2 || q.Groups[0].Values[0].Value == "video" {
		t.Errorf("queue after the skip: %+v", q)
	}
	shares, err := e.app.Shares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range shares {
		if sh.Skipped != 1 {
			t.Errorf("%s: skipped = %d, want 1", sh.Name, sh.Skipped)
		}
	}
	// Skipping an empty group does nothing; a group that does not exist is refused.
	if n, err := e.app.SkipGroup(ctx, spec, []any{"video"}); err != nil || n != 0 {
		t.Errorf("skipping a finished group = %d, %v", n, err)
	}
	var bad index.QueryError
	if _, err := e.app.SkipGroup(ctx, spec, []any{"nope"}); !errors.As(err, &bad) {
		t.Errorf("unknown group: %v", err)
	}
	if _, err := e.app.SkipGroup(AtIndex(ctx, "scan-of-another-day"), spec, []any{"audio"}); !errors.Is(err, ErrIndexChanged) {
		t.Errorf("group named against another index: %v", err)
	}

	e.scan()
	if e.state("media/a.mkv") != index.StateSkipped || e.state("docs/d.mkv") != index.StateSkipped {
		t.Error("the bulk skip did not survive a scan")
	}

	ro := newEnv(t, true, files)
	ro.scan()
	if _, err := ro.app.SkipGroup(ctx, spec, []any{"video"}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only bulk skip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ro.root, "media", meta.Dir)); !os.IsNotExist(err) {
		t.Error("read-only mode created a metadata folder")
	}
}

func TestDescribedFolderCoversItsContents(t *testing.T) {
	e := newEnv(t, false, map[string]string{
		"share/album/01.flac": "one", "share/album/02.flac": "two", "share/loose.flac": "three!",
	})
	e.scan()
	spec := index.QueueSpec{Filter: index.Filter{Kind: "file"}, Order: index.Sort{Key: "name"}, ExcludeCovered: true}
	if got := itemNames(e.queue(spec)); len(got) != 3 {
		t.Fatalf("before: %v", got)
	}
	e.annotate("share/album", "Ripped from the CD shelf")
	covered := func(e *env, when string) {
		t.Helper()
		if q := e.queue(spec); !slices.Equal(itemNames(q), []string{"loose.flac"}) || q.Total != 3 || q.Remaining != 1 {
			t.Errorf("%s: items %v, %d of %d left", when, itemNames(q), q.Remaining, q.Total)
		}
	}
	covered(e, "after describing the folder")
	e.scan()
	covered(e, "after a scan")
	data := e.app.Config().DataDir
	e.app.Close()
	again := openEnv(t, e.root, data, false)
	covered(again, "after a restart")

	again.unreview("share/album")
	if got := itemNames(again.queue(spec)); len(got) != 3 {
		t.Errorf("after removing the description: %v", got)
	}
}

// A measurement, not a check: what tens of thousands of skips cost when they
// are made, at the next scan and at the next start. Run it with
//
//	scripts/go.sh env RP_SCALE=200000 go test -run TestSkipsAtScale -v ./internal/core/
func TestSkipsAtScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("RP_SCALE"))
	if n == 0 {
		t.Skip("set RP_SCALE to a number of files to run this measurement")
	}
	ctx := context.Background()
	root := t.TempDir()
	for i := range n {
		dir := filepath.Join(root, "share", fmt.Sprintf("t%02d/d%05d", (i/100)%40, i/100))
		if i%100 == 0 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%07d.mkv", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := openEnv(t, root, t.TempDir(), false)
	e.scan()
	spec := index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"type"}}

	start := time.Now()
	skipped, err := e.app.SkipGroup(ctx, spec, []any{"video"})
	if err != nil || skipped != int64(n) {
		t.Fatalf("SkipGroup = %d, %v", skipped, err)
	}
	t.Logf("skipping %d items: %s", n, time.Since(start).Round(time.Millisecond))
	if st, err := os.Stat(filepath.Join(root, "share", meta.Dir, "skipped.jsonl")); err == nil {
		t.Logf("skipped.jsonl: %d bytes", st.Size())
	}

	start = time.Now()
	e.scan()
	t.Logf("scan of %d files, matching the skips to the new index: %s", n, time.Since(start).Round(time.Millisecond))

	data := e.app.Config().DataDir
	e.app.Close()
	start = time.Now()
	again := openEnv(t, root, data, false)
	t.Logf("start-up with %d skips: %s", n, time.Since(start).Round(time.Millisecond))
	if q := again.queue(spec); q.Remaining != 0 || q.Total != int64(n) {
		t.Errorf("after a restart: %d of %d left", q.Remaining, q.Total)
	}
}

// A pool mapped into the container read-only must say so in words. It used
// to surface as "internal error; see the log".
func TestAShareThatCannotBeWrittenSaysSo(t *testing.T) {
	cause := &fs.PathError{Op: "mkdir", Path: "/mnt/pool/media/" + meta.Dir, Err: unix.EROFS}
	var ro *ShareReadOnlyError
	if err := writeFailure(fmt.Errorf("saving: %w", cause)); !errors.As(err, &ro) || ro.Path != "/mnt/pool/media" || !strings.Contains(err.Error(), "Read/Write") {
		t.Errorf("read-only filesystem: %v", err)
	}
	// A failure to read an item, or any other failure, is left as it is.
	other := &fs.PathError{Op: "open", Path: "/mnt/pool/media/film.mkv", Err: unix.EACCES}
	if err := writeFailure(other); err != error(other) {
		t.Errorf("an unrelated failure was rewritten: %v", err)
	}
	if writeFailure(nil) != nil {
		t.Error("no failure became one")
	}

	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere, so the rest needs an ordinary user")
	}
	e := newEnv(t, false, map[string]string{"share/a.txt": "a", "open/b.txt": "b"})
	e.scan()
	locked := filepath.Join(e.root, "share")
	os.Chmod(locked, 0o555)
	defer os.Chmod(locked, 0o755)
	if _, err := e.app.PutAnnotation(context.Background(), e.id("share/a.txt"), AnnotationInput{Note: "x"}); !errors.As(err, &ro) || ro.Path != locked {
		t.Errorf("saving into a share that cannot be written: %v", err)
	}
	if _, err := e.app.PutAnnotation(context.Background(), e.id("share/a.txt"), AnnotationInput{}); !errors.As(err, &ro) {
		t.Errorf("skipping in a share that cannot be written: %v", err)
	}
	shares, err := e.app.Shares(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shares {
		if s.Writable != (s.Name == "open") {
			t.Errorf("%s: writable = %v", s.Name, s.Writable)
		}
	}
}
