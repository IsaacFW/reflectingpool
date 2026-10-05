package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/appdb"
	"github.com/IsaacFW/reflectingpool/internal/index"
	"github.com/IsaacFW/reflectingpool/internal/meta"
	"github.com/IsaacFW/reflectingpool/internal/scan"
	"github.com/IsaacFW/reflectingpool/internal/storage"
)

type env struct {
	t    *testing.T
	app  *App
	root string
}

func newEnv(t *testing.T, readOnly bool, files map[string]string) *env {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return openEnv(t, root, t.TempDir(), readOnly)
}

func openEnv(t *testing.T, root, data string, readOnly bool) *env {
	t.Helper()
	db, err := appdb.Open(filepath.Join(data, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(context.Background(), Config{Roots: []string{root}, DataDir: data, ReadOnly: readOnly}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(); db.Close() })
	return &env{t: t, app: app, root: app.Config().Roots[0]}
}

func (e *env) scan() {
	e.t.Helper()
	if _, err := e.app.Scan(context.Background(), scan.Aggressive); err != nil {
		e.t.Fatal(err)
	}
	if st := e.app.ScanStatus(); len(st.Warnings) > 0 {
		e.t.Fatalf("scan warnings: %v", st.Warnings)
	}
}

func (e *env) id(path string) int64 {
	e.t.Helper()
	var id int64
	err := e.app.View(func(ix *index.Index) error {
		row, err := ix.Lookup(context.Background(), filepath.Join(e.root, path))
		id = row.ID
		return err
	})
	if err != nil {
		e.t.Fatalf("%s: %v", path, err)
	}
	return id
}

func (e *env) annotate(path, note string) {
	e.t.Helper()
	if _, err := e.app.PutAnnotation(context.Background(), e.id(path), AnnotationInput{Note: note}); err != nil {
		e.t.Fatalf("annotate %s: %v", path, err)
	}
}

// note returns the note recorded for the item now at path, or "".
func (e *env) note(path string) string {
	e.t.Helper()
	an, _, err := e.app.Annotation(context.Background(), e.id(path))
	if err != nil {
		e.t.Fatal(err)
	}
	return an.Note
}

func (e *env) stored(share string) map[string]meta.Annotation {
	e.t.Helper()
	all, err := meta.Open(filepath.Join(e.root, share)).All()
	if err != nil {
		e.t.Fatal(err)
	}
	out := make(map[string]meta.Annotation)
	for _, a := range all {
		out[a.Path] = a
	}
	return out
}

func (e *env) mv(from, to string) {
	e.t.Helper()
	if err := os.Rename(filepath.Join(e.root, from), filepath.Join(e.root, to)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) state(path string) int {
	e.t.Helper()
	var state int
	e.app.View(func(ix *index.Index) error {
		row, err := ix.Entry(context.Background(), e.id(path))
		state = row.State
		return err
	})
	return state
}

func TestAnnotationFollowsRenames(t *testing.T) {
	e := newEnv(t, false, map[string]string{
		"media/x7f3a9.mkv":     "movie bytes",
		"media/old-name/a.txt": "a",
		"docs/keep.txt":        "k",
	})
	e.scan()
	e.annotate("media/x7f3a9.mkv", "Wedding video")
	e.annotate("media/old-name", "Scans of receipts")
	e.annotate("media", "The share itself")

	if got := e.stored("media"); got["x7f3a9.mkv"].Identity.Ino == 0 || got["x7f3a9.mkv"].Identity.Hash == "" || got["."].Note != "The share itself" {
		t.Fatalf("stored annotations: %+v", got)
	}

	e.mv("media/x7f3a9.mkv", "media/wedding.mkv")
	e.mv("media/old-name", "media/receipts")
	e.scan()

	if got := e.note("media/wedding.mkv"); got != "Wedding video" {
		t.Errorf("file annotation did not follow the rename: %q", got)
	}
	if got := e.note("media/receipts"); got != "Scans of receipts" {
		t.Errorf("folder annotation did not follow the rename: %q", got)
	}
	stored := e.stored("media")
	if _, stale := stored["x7f3a9.mkv"]; stale || stored["wedding.mkv"].Orphaned {
		t.Errorf("annotation file not updated: %+v", stored)
	}

	// The metadata folder is bookkeeping, not data: it is listed but its
	// files are never offered for review.
	var inside int
	e.app.View(func(ix *index.Index) error {
		rows, err := ix.Find(context.Background(), index.Filter{Name: "annotations.jsonl"}, index.Sort{}, 10, 0)
		inside = len(rows)
		return err
	})
	if inside != 0 {
		t.Error("the scan indexed the contents of .reflection")
	}
	// A folder of the same name deeper in a share is the user's own.
	os.MkdirAll(filepath.Join(e.root, "media/receipts", meta.Dir), 0o755)
	os.WriteFile(filepath.Join(e.root, "media/receipts", meta.Dir, "mine.txt"), []byte("x"), 0o644)
	e.scan()
	if e.id(filepath.Join("media/receipts", meta.Dir, "mine.txt")) == 0 {
		t.Error("a .reflection folder below the share root must be scanned normally")
	}
	if e.state("media/wedding.mkv")&index.StateNote == 0 {
		t.Error("index does not show the renamed file as annotated")
	}
}

func TestAnnotationFollowsCopyToAnotherShare(t *testing.T) {
	e := newEnv(t, false, map[string]string{
		"inbox/report.pdf": strings.Repeat("pdf content ", 1000),
		"inbox/decoy.pdf":  strings.Repeat("other stuff ", 1000), // same size, different content
		"docs/placeholder": "p",
	})
	// A copy keeps the modified time, as cp -p and rsync -a do.
	src := filepath.Join(e.root, "inbox/report.pdf")
	st, _ := os.Stat(src)
	os.Chtimes(filepath.Join(e.root, "inbox/decoy.pdf"), st.ModTime(), st.ModTime())
	e.scan()
	e.annotate("inbox/report.pdf", "Q3 board report")

	data, _ := os.ReadFile(src)
	dst := filepath.Join(e.root, "docs/q3-report.pdf")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(dst, st.ModTime(), st.ModTime())
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	e.scan()

	if got := e.note("docs/q3-report.pdf"); got != "Q3 board report" {
		t.Errorf("annotation did not follow the copy: %q", got)
	}
	if got := e.note("inbox/decoy.pdf"); got != "" {
		t.Errorf("annotation attached to a same-sized file with different content: %q", got)
	}
	if got := e.stored("inbox"); len(got) != 0 {
		t.Errorf("annotation left behind in the old share: %+v", got)
	}
}

func TestAnnotationOrphanedAndReplaced(t *testing.T) {
	e := newEnv(t, false, map[string]string{
		"share/gone.bin":     "will be deleted",
		"share/replaced.txt": "version one",
		"share/a.txt":        "file a",
		"share/b.txt":        "file b!",
	})
	e.scan()
	e.annotate("share/gone.bin", "Deleted later")
	e.annotate("share/replaced.txt", "Edited in place")
	e.annotate("share/a.txt", "A's note")
	e.annotate("share/b.txt", "B's note")

	os.Remove(filepath.Join(e.root, "share/gone.bin"))
	// An editor's save: a new file takes the old one's name.
	os.Remove(filepath.Join(e.root, "share/replaced.txt"))
	os.WriteFile(filepath.Join(e.root, "share/replaced.txt"), []byte("version two, longer"), 0o644)
	// b is deleted and a takes its name.
	os.Remove(filepath.Join(e.root, "share/b.txt"))
	e.mv("share/a.txt", "share/b.txt")
	e.scan()

	stored := e.stored("share")
	if !stored["gone.bin"].Orphaned || stored["gone.bin"].Note != "Deleted later" {
		t.Errorf("deleted item's annotation must be kept and marked orphaned: %+v", stored["gone.bin"])
	}
	if got := e.note("share/replaced.txt"); got != "Edited in place" {
		t.Errorf("annotation lost when the file was replaced in place: %q", got)
	}
	if got := e.note("share/b.txt"); got != "A's note" {
		t.Errorf("the renamed file must keep its own annotation, got %q", got)
	}
	if orphan := stored["b.txt (orphaned)"]; !orphan.Orphaned || orphan.Note != "B's note" {
		t.Errorf("the deleted file's annotation must survive as an orphan: %+v", stored)
	}

	shares, err := e.app.Shares(context.Background())
	if err != nil || len(shares) != 1 {
		t.Fatalf("shares: %v, %v", shares, err)
	}
	if s := shares[0]; s.Annotated != 2 || s.Orphaned != 2 || s.CoveredBytes != int64(len("version two, longer")+len("file a")) {
		t.Errorf("share summary = annotated %d, orphaned %d, covered %d", s.Annotated, s.Orphaned, s.CoveredBytes)
	}

	// An orphan that reappears is picked up again.
	os.WriteFile(filepath.Join(e.root, "share/gone.bin"), []byte("back"), 0o644)
	e.scan()
	if got := e.note("share/gone.bin"); got != "Deleted later" || e.stored("share")["gone.bin"].Orphaned {
		t.Errorf("reappeared item not re-attached: %q", got)
	}
}

func TestAnnotationSurvivesRestart(t *testing.T) {
	e := newEnv(t, false, map[string]string{"share/a.txt": "a"})
	e.scan()
	e.annotate("share/a.txt", "kept")
	data := e.app.Config().DataDir
	e.app.Close()

	again := openEnv(t, e.root, data, false)
	if got := again.note("share/a.txt"); got != "kept" {
		t.Errorf("after reopening: %q", got)
	}
	if again.state("share/a.txt")&index.StateNote == 0 {
		t.Error("annotation state lost from the reopened index")
	}
}

// After the container's mappings change, the saved index describes paths that
// no longer exist. It must not be served; the next scan replaces it.
func TestIndexOfOtherRootsIsNotUsed(t *testing.T) {
	e := newEnv(t, false, map[string]string{"share/a.txt": "a"})
	e.scan()
	data := e.app.Config().DataDir
	e.app.Close()

	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "elsewhere"), 0o755)
	os.WriteFile(filepath.Join(other, "elsewhere/b.txt"), []byte("bb"), 0o644)
	moved := openEnv(t, other, data, false)
	if err := moved.app.View(func(*index.Index) error { return nil }); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("an index of the old roots is being served: %v", err)
	}
	if moved.app.ScanStatus().Index != nil {
		t.Error("status reports the old index")
	}
	moved.scan()
	if st := moved.app.ScanStatus(); st.Index == nil || st.Index.Size != 2 {
		t.Errorf("after rescanning the new roots: %+v", st.Index)
	}

	// The same roots still pick the index up again.
	moved.app.Close()
	again := openEnv(t, other, data, false)
	if again.app.ScanStatus().Index == nil {
		t.Error("an index of the same roots was discarded")
	}
}

func TestInputValidation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, false, map[string]string{"share/a.txt": "a", "loose.txt": "l"})
	e.scan()
	id := e.id("share/a.txt")

	if err := e.app.SetPrefixes([]Prefix{{Name: "ARCHIVE", Meaning: "Cold storage"}, {Name: "TMP", Meaning: "Safe to delete"}}); err != nil {
		t.Fatal(err)
	}
	var ie InputError
	for name, in := range map[string]AnnotationInput{
		"unknown prefix": {Prefixes: []string{"NOPE"}},
		"bad date":       {ReviewAfter: "next tuesday"},
		"display name /": {DisplayName: "a/b"},
		"huge note":      {Note: strings.Repeat("x", 10_001)},
	} {
		if _, err := e.app.PutAnnotation(ctx, id, in); !errors.As(err, &ie) {
			t.Errorf("%s: %v", name, err)
		}
	}
	an, err := e.app.PutAnnotation(ctx, id, AnnotationInput{Prefixes: []string{"TMP", "TMP", "ARCHIVE"}, ReviewAfter: "2027-01-31"})
	if err != nil || len(an.Prefixes) != 2 {
		t.Errorf("valid input: %+v, %v", an, err)
	}
	for name, list := range map[string][]Prefix{
		"duplicate": {{Name: "A"}, {Name: "A"}},
		"pipe":      {{Name: "A|B"}},
		"space":     {{Name: "A B"}},
		"empty":     {{Name: ""}},
	} {
		if err := e.app.SetPrefixes(list); !errors.As(err, &ie) {
			t.Errorf("prefix list %s: %v", name, err)
		}
	}
	if got, _ := e.app.Prefixes(); len(got) != 2 || got[0].Name != "ARCHIVE" {
		t.Errorf("a rejected list must not replace the stored one: %+v", got)
	}

	if _, err := e.app.PutAnnotation(ctx, e.id("loose.txt"), AnnotationInput{Note: "n"}); !errors.Is(err, ErrNoShare) {
		t.Errorf("file outside any share: %v", err)
	}
	if _, err := e.app.PutAnnotation(ctx, 999999, AnnotationInput{}); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	os.Remove(filepath.Join(e.root, "share/a.txt"))
	if _, err := e.app.PutAnnotation(ctx, id, AnnotationInput{Note: "n"}); !errors.Is(err, ErrGone) {
		t.Errorf("item deleted since the scan: %v", err)
	}
}

func TestReadOnlyChangesNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, false, map[string]string{"share/a.txt": "a"})
	e.scan()
	e.annotate("share/a.txt", "before")
	data := e.app.Config().DataDir
	e.app.Close()

	ro := openEnv(t, e.root, data, true)
	ro.mv("share/a.txt", "share/renamed.txt")
	file := filepath.Join(ro.root, "share", meta.Dir, "annotations.jsonl")
	before, _ := os.ReadFile(file)
	ro.scan()

	if _, err := ro.app.PutAnnotation(ctx, ro.id("share/renamed.txt"), AnnotationInput{Note: "x"}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("put: %v", err)
	}
	if err := ro.app.DeleteAnnotation(ctx, ro.id("share/renamed.txt")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("delete: %v", err)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Error("a read-only scan rewrote the annotation file")
	}
	// The index still shows the renamed item as annotated.
	if ro.state("share/renamed.txt")&index.StateNote == 0 {
		t.Error("read-only scan lost track of the annotation")
	}
}

func TestOpenContentCannotEscapeTheRoot(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, false, map[string]string{"share/dir/secret.txt": "inside", "share/plain.txt": "hello"})
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE"), 0o644)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(e.root, "share/link.txt"))
	e.scan()

	f, row, err := e.app.OpenContent(ctx, e.id("share/plain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(f)
	f.Close()
	if string(body) != "hello" || row.Name != "plain.txt" {
		t.Errorf("content = %q", body)
	}

	if _, _, err := e.app.OpenContent(ctx, e.id("share/link.txt")); !errors.Is(err, ErrNotFile) {
		t.Errorf("symlink: %v", err)
	}
	if _, _, err := e.app.OpenContent(ctx, e.id("share/dir")); !errors.Is(err, ErrNotFile) {
		t.Errorf("directory: %v", err)
	}

	// After the scan, a directory on the path is swapped for a symlink that
	// points outside the root. The open must not follow it.
	id := e.id("share/dir/secret.txt")
	e.mv("share/dir", "share/dir-moved")
	os.Symlink(outside, filepath.Join(e.root, "share/dir"))
	if f, _, err := e.app.OpenContent(ctx, id); err == nil {
		body, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("followed a symlinked directory out of the root and read %q", body)
	}
	// The same for the file itself.
	id = e.id("share/plain.txt")
	os.Remove(filepath.Join(e.root, "share/plain.txt"))
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(e.root, "share/plain.txt"))
	if _, _, err := e.app.OpenContent(ctx, id); err == nil {
		t.Fatal("followed a symlink that replaced the file")
	}
}

// Closing must stop a scan in progress and wait until it has cleaned up, so
// nothing is still writing to the data directory afterwards.
func TestCloseStopsAndWaitsForAScan(t *testing.T) {
	files := make(map[string]string)
	for d := range 40 {
		for f := range 50 {
			files[filepath.Join("share", "d"+strconv.Itoa(d), "f"+strconv.Itoa(f))] = "x"
		}
	}
	for range 20 { // the race this guards against is a matter of timing
		e := newEnv(t, false, files)
		if err := e.app.StartScan(scan.Aggressive); err != nil {
			t.Fatal(err)
		}
		e.app.Close()
		if st := e.app.ScanStatus(); st.Running {
			t.Fatal("Close returned while a scan was still running")
		}
		left, _ := filepath.Glob(filepath.Join(e.app.Config().DataDir, "index", "*.tmp*"))
		if len(left) != 0 {
			t.Fatalf("a stopped scan left its unfinished index behind: %v", left)
		}
		if _, err := e.app.Scan(context.Background(), scan.Aggressive); !errors.Is(err, ErrClosed) {
			t.Fatalf("scan after Close: %v", err)
		}
	}
}

func TestScheduleDueAndNext(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	on := Schedule{Enabled: true, Time: "03:00", Intensity: "low"}
	for _, tc := range []struct {
		name    string
		s       Schedule
		now     string
		lastDay string
		want    bool
	}{
		{"off", Schedule{Time: "03:00"}, "2026-10-05 03:00", "", false},
		{"before the time", on, "2026-10-05 02:59", "", false},
		{"at the time", on, "2026-10-05 03:00", "2026-10-04", true},
		{"already ran today", on, "2026-10-05 03:20", "2026-10-05", false},
		{"a little late", on, "2026-10-05 03:59", "2026-10-04", true},
		// The server was off at 03:00. Starting at noon would put the load
		// where the user chose not to have it.
		{"too late to start", on, "2026-10-05 12:00", "2026-10-04", false},
		{"unreadable time", Schedule{Enabled: true, Time: "soon"}, "2026-10-05 03:00", "", false},
	} {
		if got := tc.s.due(at(tc.now), tc.lastDay); got != tc.want {
			t.Errorf("%s: due = %v, want %v", tc.name, got, tc.want)
		}
	}
	if next := on.next(at("2026-10-05 02:00")); next == nil || !next.Equal(at("2026-10-05 03:00")) {
		t.Errorf("next before the time = %v", next)
	}
	if next := on.next(at("2026-10-05 03:00")); next == nil || !next.Equal(at("2026-10-06 03:00")) {
		t.Errorf("next after the time = %v", next)
	}
	if next := (Schedule{Time: "03:00"}).next(at("2026-10-05 02:00")); next != nil {
		t.Errorf("a schedule that is off has a next run: %v", next)
	}
}

// A scan run from the command line builds its index in another process. The
// running server must pick it up without a restart.
func TestServerAdoptsAnIndexBuiltElsewhere(t *testing.T) {
	server := newEnv(t, false, map[string]string{"share/a.txt": "a"})
	if server.app.IndexID() != "" {
		t.Fatal("a new installation must not have an index: nothing scans by itself")
	}
	cli := openEnv(t, server.root, server.app.Config().DataDir, false)
	cli.scan()

	server.app.adoptLatest()
	if server.app.IndexID() != cli.app.IndexID() || server.app.IndexID() == "" {
		t.Fatalf("server index %q, command-line index %q", server.app.IndexID(), cli.app.IndexID())
	}
	if got := server.id("share/a.txt"); got == 0 {
		t.Error("the adopted index cannot be queried")
	}
	// A second look changes nothing.
	before := server.app.IndexID()
	server.app.adoptLatest()
	if server.app.IndexID() != before {
		t.Error("adopting twice switched indexes")
	}

	// What is annotated here while the other process scans is not in the
	// index that process builds. Adopting it must not lose sight of it.
	cli.scan()
	server.annotate("share/a.txt", "written while the scan ran")
	server.app.adoptLatest()
	if server.app.IndexID() != cli.app.IndexID() || server.state("share/a.txt")&index.StateNote == 0 {
		t.Errorf("after adopting: index %q, state %d", server.app.IndexID(), server.state("share/a.txt"))
	}
}

func TestStopScan(t *testing.T) {
	files := make(map[string]string)
	for d := range 60 {
		for f := range 60 {
			files[filepath.Join("share", "d"+strconv.Itoa(d), "f"+strconv.Itoa(f))] = "x"
		}
	}
	e := newEnv(t, false, files)
	if err := e.app.StopScan(); !errors.Is(err, ErrNoScan) {
		t.Errorf("stopping when nothing runs: %v", err)
	}
	if err := e.app.StartScan(scan.Aggressive); err != nil {
		t.Fatal(err)
	}
	stopped := false
	for range 2000 {
		if err := e.app.StopScan(); err == nil {
			stopped = true
			break
		}
		if e.app.IndexID() != "" {
			break // it finished before it could be stopped; nothing to check
		}
		time.Sleep(100 * time.Microsecond)
	}
	for range 500 {
		if !e.app.ScanStatus().Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := e.app.ScanStatus()
	if st.Running {
		t.Fatal("the scan is still running after it was stopped")
	}
	if stopped && st.Index == nil && !strings.Contains(st.LastError, "stopped") {
		t.Errorf("a stopped scan should say so: %q", st.LastError)
	}
	if left, _ := filepath.Glob(filepath.Join(e.app.Config().DataDir, "index", "*.tmp*")); len(left) != 0 {
		t.Errorf("a stopped scan left its unfinished index behind: %v", left)
	}
}

// Entry IDs belong to one index. An operation that names another is refused.
func TestOperationsAreTiedToTheirIndex(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, false, map[string]string{"share/a.txt": "a", "share/b.txt": "bb"})
	e.scan()
	old, id := e.app.IndexID(), e.id("share/a.txt")
	e.scan()
	if e.app.IndexID() == old {
		t.Fatal("two scans produced the same index ID")
	}
	stale := AtIndex(ctx, old)
	if _, err := e.app.PutAnnotation(stale, id, AnnotationInput{Note: "n"}); !errors.Is(err, ErrIndexChanged) {
		t.Errorf("put against a replaced index: %v", err)
	}
	if err := e.app.DeleteAnnotation(stale, id); !errors.Is(err, ErrIndexChanged) {
		t.Errorf("delete against a replaced index: %v", err)
	}
	if _, _, err := e.app.OpenContent(stale, id); !errors.Is(err, ErrIndexChanged) {
		t.Errorf("content against a replaced index: %v", err)
	}
	if _, err := e.app.PutAnnotation(AtIndex(ctx, e.app.IndexID()), e.id("share/a.txt"), AnnotationInput{Note: "n"}); err != nil {
		t.Errorf("put against the current index: %v", err)
	}
}

// A gentler scan must produce the same index as an aggressive one, and say
// which intensity it ran at.
func TestScanIntensityIsRecordedAndChangesNothingElse(t *testing.T) {
	e := newEnv(t, false, map[string]string{"share/a.txt": "aaa", "share/sub/b.txt": "bb", "other/c.bin": "c"})
	e.scan()
	fast := *e.app.ScanStatus().Index
	if fast.Intensity != "aggressive" {
		t.Errorf("intensity recorded as %q", fast.Intensity)
	}
	for _, intensity := range []scan.Intensity{scan.Balanced, scan.LowImpact} {
		info, err := e.app.Scan(context.Background(), intensity)
		if err != nil {
			t.Fatal(err)
		}
		if info.Intensity != intensity.String() {
			t.Errorf("%v scan recorded as %q", intensity, info.Intensity)
		}
		if info.Files != fast.Files || info.Dirs != fast.Dirs || info.Size != fast.Size {
			t.Errorf("%v scan found %d files, %d folders, %d bytes; aggressive found %d, %d, %d",
				intensity, info.Files, info.Dirs, info.Size, fast.Files, fast.Dirs, fast.Size)
		}
	}
	if st := e.app.ScanStatus(); st.Running || st.Intensity != "" {
		t.Errorf("an idle status reports intensity %q", st.Intensity)
	}
}

func TestScannedFSTypes(t *testing.T) {
	const mountinfo = `1 0 0:30 / / rw - overlay overlay rw
2 1 0:5 / /proc rw - proc proc rw
3 1 0:40 / /pool/fleetdevices rw master:1 - zfs tank/fleetdevices rw
4 3 0:41 / /pool/fleetdevices/archive rw master:2 - zfs tank/fleetdevices/archive rw
5 1 0:50 / /mnt/tank rw master:3 - zfs tank rw
6 5 0:51 / /mnt/tank/media rw master:4 - zfs tank/media rw
7 5 0:60 / /mnt/tank/remote rw - nfs4 nas:/export rw
8 1 0:70 / /data rw - xfs /dev/sdb1 rw
`
	mounts, err := storage.ParseMountinfo(strings.NewReader(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	table := storage.NewTable(mounts)
	for _, tc := range []struct {
		name  string
		roots []string
		want  map[string]bool
	}{
		// One share mapped into a folder the container made: the share and
		// its child datasets are scanned although the folder is not ZFS.
		{"share mapped into a folder", []string{"/pool"}, map[string]bool{"overlay": true, "zfs": true}},
		// A whole pool: its datasets are followed, the NFS mount inside is not.
		{"whole pool", []string{"/mnt/tank"}, map[string]bool{"zfs": true}},
		{"both", []string{"/pool", "/mnt/tank"}, map[string]bool{"overlay": true, "zfs": true}},
	} {
		got := scannedFSTypes(table, tc.roots)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		for kind := range tc.want {
			if !got[kind] {
				t.Errorf("%s: %s not scanned", tc.name, kind)
			}
		}
	}
}

func TestScanStatusAndStorage(t *testing.T) {
	e := newEnv(t, false, map[string]string{"share/a.txt": "abc"})
	if err := e.app.View(func(*index.Index) error { return nil }); !errors.Is(err, ErrNoIndex) {
		t.Errorf("before any scan: %v", err)
	}
	e.scan()
	st := e.app.ScanStatus()
	if st.Running || st.Index == nil || st.Index.Files != 1 || st.Index.Size != 3 {
		t.Errorf("status = %+v", st)
	}
	rep, err := e.app.Storage(context.Background())
	if err != nil || len(rep.Datasets) == 0 || rep.Datasets[0].Total <= 0 {
		t.Errorf("storage = %+v, %v", rep, err)
	}

	// Old indexes are pruned down to the configured number.
	for range 4 {
		e.scan()
	}
	left, _ := filepath.Glob(filepath.Join(e.app.Config().DataDir, "index", "scan-*.db"))
	if len(left) != 3 {
		t.Errorf("%d indexes kept, want 3", len(left))
	}
}
