package index

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/scan"
)

func lookup(t *testing.T, ix *Index, root, rel string) Row {
	t.Helper()
	row, err := ix.Lookup(context.Background(), filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return row
}

func queue(t *testing.T, ix *Index, spec QueueSpec) QueueResult {
	t.Helper()
	q, err := ix.Queue(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func annotate(t *testing.T, ix *Index, id int64, state int) {
	t.Helper()
	if err := ix.SetAnnot(context.Background(), Annot{Entry: id, State: state}); err != nil {
		t.Fatal(err)
	}
}

func labels(q QueueResult) []string {
	var out []string
	for _, g := range q.Groups {
		label := ""
		for i, v := range g.Values {
			if i > 0 {
				label += "/"
			}
			label += v.Label
		}
		out = append(out, fmt.Sprintf("%s %d of %d", label, g.Remaining, g.Total))
	}
	return out
}

// A queue must only offer what can be annotated or skipped. The scan root and
// anything loose in it belong to no share, and would otherwise sit at the
// head of a queue for ever.
func TestQueueOffersOnlyWhatCanBeReviewed(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "media/movies/a.mkv"), 10)
	write(t, filepath.Join(root, "media/"+MetaDir+"/annotations.jsonl"), 10)
	write(t, filepath.Join(root, "loose-in-the-root.iso"), 5000)
	ix := build(t, root)

	q := queue(t, ix, QueueSpec{Filter: Filter{Kind: "dir"}, Order: Sort{Key: "name"}})
	if got := names(q.Items); !equal(got, []string{"media", "movies"}) {
		t.Errorf("folders offered = %v", got)
	}
	q = queue(t, ix, QueueSpec{Order: Sort{Key: "name"}})
	if got := names(q.Items); slices.Contains(got, "loose-in-the-root.iso") || slices.Contains(got, MetaDir) || !slices.Contains(got, "a.mkv") {
		t.Errorf("items offered = %v", got)
	}
}

func TestUnder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "media/tv/show/s01/e01.mkv"), 300)
	write(t, filepath.Join(root, "media/tv/show/s01/e02.mkv"), 200)
	write(t, filepath.Join(root, "media/tv/show/notes.txt"), 10)
	write(t, filepath.Join(root, "media/tv/other.mkv"), 900)
	write(t, filepath.Join(root, "media/tvx/near-miss.mkv"), 50)
	ix := build(t, root)
	show := lookup(t, ix, root, "media/tv/show")

	rows, err := ix.Find(ctx, Filter{Under: show.ID}, Sort{Key: "name"}, 0, 0)
	if err != nil || !equal(names(rows), []string{"e01.mkv", "e02.mkv", "notes.txt", "s01"}) {
		t.Fatalf("under show = %v, %v", names(rows), err)
	}
	totals, err := ix.Count(ctx, Filter{Kind: "file", Under: show.ID})
	if err != nil || totals.Count != 3 || *totals.Size != 510 {
		t.Errorf("totals under show = %+v, %v", totals, err)
	}
	// A file holds nothing.
	rows, err = ix.Find(ctx, Filter{Under: lookup(t, ix, root, "media/tv/other.mkv").ID}, Sort{}, 0, 0)
	if err != nil || len(rows) != 0 {
		t.Errorf("under a file = %v, %v", names(rows), err)
	}

	// A queue over a subtree counts only what is in it, also when the count
	// is adjusted one annotation at a time.
	spec := QueueSpec{Filter: Filter{Kind: "file", Under: show.ID}, Groups: []string{"type"}}
	if q := queue(t, ix, spec); q.Remaining != 3 || q.Total != 3 {
		t.Fatalf("queue under show: %d of %d", q.Remaining, q.Total)
	}
	annotate(t, ix, lookup(t, ix, root, "media/tv/other.mkv").ID, StateNote)
	annotate(t, ix, lookup(t, ix, root, "media/tv/show/s01/e01.mkv").ID, StateNote)
	if q := queue(t, ix, spec); q.Remaining != 2 || !equal(names(q.Items), []string{"e02.mkv"}) {
		t.Errorf("after annotating one inside and one outside: %d left, %v", q.Remaining, names(q.Items))
	}
}

// synthetic builds an index from entries made by hand, for what a real scan
// of a test folder cannot give: creation times in the past.
func synthetic(t *testing.T, scanned time.Time, files map[string][2]time.Time) *Index {
	t.Helper()
	b, err := NewBuilder(t.TempDir(), scanned)
	if err != nil {
		t.Fatal(err)
	}
	entries := []scan.Entry{
		{ID: 1, Name: "/pool", Kind: scan.KindDir},
		{ID: 2, Parent: 1, Name: "share", Kind: scan.KindDir, Share: 2},
	}
	id := int64(2)
	for name, times := range files {
		id++
		entries = append(entries, scan.Entry{
			ID: id, Parent: 2, Name: name, Kind: scan.KindFile, Share: 2, Size: id,
			Mtime: times[0].Unix(), Btime: times[1].Unix(), Counted: true,
		})
	}
	if err := b.Entries(entries); err != nil {
		t.Fatal(err)
	}
	path, err := b.Finish(Info{Started: scanned, Roots: []string{"/pool"}, Dirs: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func TestAgeGroupsAndOrder(t *testing.T) {
	scanned := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ago := func(years, months int) time.Time { return scanned.AddDate(-years, -months, -1) }
	ix := synthetic(t, scanned, map[string][2]time.Time{
		"ancient.mkv": {ago(9, 0), ago(8, 0)},
		// Modified long ago, but it only arrived on the pool this year: it
		// is as old as its arrival.
		"old-film-copied-recently.mkv": {ago(20, 0), ago(0, 2)},
		"three-years.mkv":              {ago(3, 0), ago(4, 0)},
		"eighteen-months.mkv":          {ago(1, 6), ago(1, 6)},
		"eight-months.mkv":             {ago(0, 8), ago(0, 9)},
		"last-month.mkv":               {ago(0, 1), ago(0, 1)},
	})

	spec := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"age"}, Order: Sort{Key: "age"}}
	q := queue(t, ix, spec)
	want := []string{"Over 5 years 1 of 1", "2 to 5 years 1 of 1", "1 to 2 years 1 of 1", "6 to 12 months 1 of 1", "Under 6 months 2 of 2"}
	if got := labels(q); !equal(got, want) {
		t.Fatalf("age groups = %v\nwant %v", got, want)
	}
	if q.Groups[0].Values[0].Value != "over-5y" || !equal(names(q.Items), []string{"ancient.mkv"}) {
		t.Errorf("first group = %+v, items %v", q.Groups[0].Values, names(q.Items))
	}

	// Oldest first inside a group, and across the whole index.
	annotate(t, ix, q.Items[0].ID, StateSkipped)
	for range 3 {
		annotate(t, ix, queue(t, ix, spec).Items[0].ID, StateSkipped)
	}
	if got := names(queue(t, ix, spec).Items); !equal(got, []string{"old-film-copied-recently.mkv", "last-month.mkv"}) {
		t.Errorf("newest group, oldest first = %v", got)
	}
	rows, err := ix.Find(context.Background(), Filter{Kind: "file"}, Sort{Key: "age"}, 2, 0)
	if err != nil || !equal(names(rows), []string{"ancient.mkv", "three-years.mkv"}) {
		t.Errorf("oldest two = %v, %v", names(rows), err)
	}
	rows, err = ix.Find(context.Background(), Filter{Kind: "file"}, Sort{Key: "created", Desc: true}, 1, 0)
	if err != nil || !equal(names(rows), []string{"last-month.mkv"}) {
		t.Errorf("newest by creation = %v, %v", names(rows), err)
	}

	// A group is named by its values when it is sent back.
	if _, err := ix.PendingInGroup(context.Background(), spec, []any{"over-9000y"}); err == nil {
		t.Error("unknown age group accepted")
	}
	pending, err := ix.PendingInGroup(context.Background(), spec, []any{"under-6m"})
	if err != nil || len(pending) != 2 {
		t.Errorf("pending in the newest group = %v, %v", pending, err)
	}
}

func coveredFixture(t *testing.T) (string, *Index) {
	root := t.TempDir()
	write(t, filepath.Join(root, "media/movies/alien/alien.mkv"), 9000)
	write(t, filepath.Join(root, "media/movies/alien/extras/making-of.mkv"), 4000)
	write(t, filepath.Join(root, "media/movies/brazil/brazil.mkv"), 8000)
	write(t, filepath.Join(root, "media/tv/pilot.mkv"), 3000)
	write(t, filepath.Join(root, "docs/taxes/2024.pdf"), 300)
	return root, build(t, root)
}

func TestCoveredFolders(t *testing.T) {
	ctx := context.Background()
	root, ix := coveredFixture(t)
	files := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"share"}, ExcludeCovered: true}
	plain := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"share"}}
	folders := QueueSpec{Filter: Filter{Kind: "dir"}, Order: Sort{Key: "name"}, ExcludeCovered: true}
	queue(t, ix, files) // remembered now, so later changes must reach it
	queue(t, ix, plain)

	// Describing a folder takes everything inside it out of the queue, at
	// any depth, and leaves the totals and the order of groups alone.
	movies := lookup(t, ix, root, "media/movies")
	annotate(t, ix, movies.ID, StateNote)
	q := queue(t, ix, files)
	if got := labels(q); !equal(got, []string{"media 1 of 4", "docs 1 of 1"}) || !equal(names(q.Items), []string{"pilot.mkv"}) {
		t.Fatalf("after describing movies: %v, items %v", got, names(q.Items))
	}
	if q.Total != 5 || q.Remaining != 2 || q.GroupTotal != 2 {
		t.Errorf("totals: %+v", q)
	}
	if got := names(queue(t, ix, folders).Items); !equal(got, []string{"docs", "media", "taxes", "tv"}) {
		t.Errorf("folders left = %v", got)
	}
	// A queue that did not ask for it is unaffected.
	if q := queue(t, ix, plain); q.Remaining != 5 {
		t.Errorf("plain queue remaining = %d", q.Remaining)
	}

	// An item annotated inside a covered folder is not counted twice.
	annotate(t, ix, lookup(t, ix, root, "media/movies/alien/alien.mkv").ID, StateNote)
	if q := queue(t, ix, files); q.Remaining != 2 {
		t.Errorf("after annotating inside a covered folder: %d remaining", q.Remaining)
	}

	// Skipping a folder says nothing about what is in it.
	annotate(t, ix, lookup(t, ix, root, "media/tv").ID, StateSkipped)
	if q := queue(t, ix, files); q.Remaining != 2 {
		t.Errorf("a skipped folder covered its contents: %d remaining", q.Remaining)
	}
	// Nor does a note on the share itself, or one note would empty its queue.
	annotate(t, ix, lookup(t, ix, root, "docs").ID, StateNote)
	if q := queue(t, ix, files); q.Remaining != 2 {
		t.Errorf("a note on a share covered its contents: %d remaining", q.Remaining)
	}

	// A folder described inside a described folder changes nothing, and
	// keeps its contents covered when the outer description goes.
	annotate(t, ix, lookup(t, ix, root, "media/movies/alien").ID, StatePrefix)
	if err := ix.ClearAnnot(ctx, movies.ID); err != nil {
		t.Fatal(err)
	}
	q = queue(t, ix, files)
	if got := labels(q); !equal(got, []string{"media 2 of 4", "docs 1 of 1"}) || !equal(names(q.Items), []string{"brazil.mkv", "pilot.mkv"}) {
		t.Errorf("after removing the outer description: %v, items %v", got, names(q.Items))
	}

	// Replacing the cache rebuilds the covered folders from it.
	if err := ix.ReplaceAnnots(ctx, []Annot{{Entry: lookup(t, ix, root, "media/tv").ID, State: StateDisplayName}}); err != nil {
		t.Fatal(err)
	}
	if got := names(queue(t, ix, files).Items); !equal(got, []string{"alien.mkv", "brazil.mkv", "making-of.mkv"}) {
		t.Errorf("after replacing the cache: %v", got)
	}
}

func TestQueueTotalsPositionAndOffset(t *testing.T) {
	_, ix := fixture(t)
	spec := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"share", "type"}, Limit: 1}
	q := queue(t, ix, spec)
	if q.Total != 7 || q.GroupTotal != 5 || q.GroupPosition != 1 || q.TotalSize != 9000+2000+4000+50+9000+300+20 {
		t.Fatalf("totals = %+v", q)
	}
	if v := q.Groups[0].Values; v[0].Label != "media" || v[0].Value != q.Items[0].Share || v[1].Value != "video" {
		t.Errorf("group values = %+v", v)
	}

	// Offsets count unreviewed items from the head and run on across groups.
	var seen []string
	for offset := 0; ; offset++ {
		spec.Offset = offset
		q := queue(t, ix, spec)
		if len(q.Items) == 0 {
			break
		}
		seen = append(seen, fmt.Sprintf("%s@%d", q.Items[0].Name, q.GroupPosition))
		if q.Remaining != 7 || q.GroupCount != 5 {
			t.Errorf("offset %d changed the counts: %+v", offset, q)
		}
	}
	want := []string{"big.mkv@1", "small.mkv@1", "song.flac@2", "tiny.mp3@2", "big-link.mkv@3", "report.pdf@4", "50%_done.txt@5"}
	if !equal(seen, want) {
		t.Errorf("walked by offset = %v\nwant %v", seen, want)
	}

	// Finished groups keep their place in the count.
	spec.Offset = 0
	for range 2 {
		annotate(t, ix, queue(t, ix, spec).Items[0].ID, StateSkipped)
	}
	if q := queue(t, ix, spec); q.GroupPosition != 2 || q.GroupTotal != 5 || q.GroupCount != 4 || q.Total != 7 {
		t.Errorf("after finishing the first group: %+v", q)
	}
}

// A group larger than what is held ready is served to the end, and an item
// that comes back into review reappears in its place.
func TestQueueBeyondWhatIsHeldReady(t *testing.T) {
	defer func(n int) { headSize = n }(headSize)
	headSize = 4
	ctx := context.Background()
	root := t.TempDir()
	for i := range 11 {
		write(t, filepath.Join(root, fmt.Sprintf("share/f%02d.bin", i)), 100-i)
	}
	ix := build(t, root)
	spec := QueueSpec{Filter: Filter{Kind: "file"}, Limit: 2}

	var served []string
	var first int64
	for {
		q := queue(t, ix, spec)
		if len(q.Items) == 0 {
			break
		}
		if first == 0 {
			first = q.Items[0].ID
		}
		served = append(served, q.Items[0].Name)
		annotate(t, ix, q.Items[0].ID, StateSkipped)
	}
	if len(served) != 11 || served[0] != "f00.bin" || served[10] != "f10.bin" {
		t.Fatalf("served = %v", served)
	}
	if err := ix.ClearAnnot(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := names(queue(t, ix, spec).Items); !equal(got, []string{"f00.bin"}) {
		t.Errorf("after returning an item to review: %v", got)
	}

	// Paging past what is held ready reads the index directly.
	if err := ix.ReplaceAnnots(ctx, nil); err != nil {
		t.Fatal(err)
	}
	spec.Offset = 7
	if got := names(queue(t, ix, spec).Items); !equal(got, []string{"f07.bin", "f08.bin"}) {
		t.Errorf("offset 7 = %v", got)
	}
}

func TestSkipAGroup(t *testing.T) {
	ctx := context.Background()
	root, ix := fixture(t)
	spec := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"share", "type"}}
	other := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"type"}}
	q := queue(t, ix, spec)
	queue(t, ix, other)
	annotate(t, ix, lookup(t, ix, root, "media/movies/small.mkv").ID, StateNote)

	group := []any{q.Groups[0].Values[0].Value, q.Groups[0].Values[1].Value}
	pending, err := ix.PendingInGroup(ctx, spec, group)
	if err != nil || len(pending) != 1 || pending[0] != lookup(t, ix, root, "media/movies/big.mkv").ID {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	// An ID that is already annotated is left as it is.
	small := lookup(t, ix, root, "media/movies/small.mkv")
	n, err := ix.MarkSkipped(ctx, spec, group, []int64{pending[0], small.ID})
	if err != nil || n != 1 {
		t.Fatalf("MarkSkipped = %d, %v", n, err)
	}
	if row, _ := ix.Entry(ctx, small.ID); row.State != StateNote {
		t.Errorf("a bulk skip overwrote an annotation: state %d", row.State)
	}
	if got := labels(queue(t, ix, spec)); got[0] != "media/audio 2 of 2" {
		t.Errorf("after skipping the group: %v", got)
	}
	// Another queue over the same items sees the skip too.
	if got := labels(queue(t, ix, other)); !slices.Contains(got, "video 1 of 3") {
		t.Errorf("other queue: %v", got)
	}
	// JSON numbers arrive as float64.
	if _, err := ix.PendingInGroup(ctx, spec, []any{float64(small.Share), "audio"}); err != nil {
		t.Errorf("group named with a JSON number: %v", err)
	}
	for _, bad := range [][]any{{"media", "video"}, {1.5, "video"}, {small.Share}, {small.Share, "nope"}} {
		if _, err := ix.PendingInGroup(ctx, spec, bad); err == nil {
			t.Errorf("group %v accepted", bad)
		}
	}
}

// The counts a queue keeps as annotations come and go must equal a count made
// from scratch. Every operation that touches the annotation cache is mixed
// here, against queues of every shape, and after each one the remembered
// answer is compared with that of a freshly opened index.
func TestQueueCountsStayExact(t *testing.T) {
	ctx := context.Background()
	defer func(n int) { headSize = n }(headSize)
	headSize = 6
	root := t.TempDir()
	rng := rand.New(rand.NewSource(17))
	exts := []string{"mkv", "mp3", "jpg", "pdf", "txt", "iso"}
	for i := range 90 {
		dir := fmt.Sprintf("share%d/d%d", i%2, i%5)
		if i%3 == 0 {
			dir += fmt.Sprintf("/sub%d", i%4)
		}
		if i%9 == 0 {
			dir += "/deep"
		}
		path := fmt.Sprintf("%s/f%02d.%s", dir, i, exts[rng.Intn(len(exts))])
		write(t, filepath.Join(root, path), 1+rng.Intn(5000))
	}
	ix := build(t, root)

	var all []Row // every file and folder inside a share
	for _, kind := range []string{"file", "dir"} {
		rows, err := ix.Find(ctx, Filter{Kind: kind}, Sort{Key: "name"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Share != 0 {
				all = append(all, r)
			}
		}
	}
	under := lookup(t, ix, root, "share0/d0").ID
	specs := []QueueSpec{
		{Filter: Filter{Kind: "file"}, Groups: []string{"share", "type"}},
		{Filter: Filter{Kind: "file"}, Groups: []string{"share", "type"}, ExcludeCovered: true},
		{Filter: Filter{Kind: "dir"}, Groups: []string{"share"}, ExcludeCovered: true},
		{Filter: Filter{Kind: "file", Under: under}, Groups: []string{"ext"}, ExcludeCovered: true},
		{Filter: Filter{Kind: "file"}, Groups: []string{"folder"}, Order: Sort{Key: "name"}},
		{Filter: Filter{Kind: "file", MinSize: 1000}, Order: Sort{Key: "name"}, ExcludeCovered: true},
		{Groups: []string{"age", "type"}, ExcludeCovered: true},
	}
	states := []int{StateNote, StateSkipped, StatePrefix, StateNote | StateDisplayName}

	check := func(step int, what string) {
		t.Helper()
		fresh, err := Open(ix.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer fresh.Close()
		for i, spec := range specs {
			spec.Limit, spec.Offset = 3, step%3
			got, want := queue(t, ix, spec), queue(t, fresh, spec)
			if !reflect.DeepEqual(labels(got), labels(want)) || !equal(names(got.Items), names(want.Items)) ||
				got.Remaining != want.Remaining || got.GroupCount != want.GroupCount || got.GroupPosition != want.GroupPosition {
				t.Fatalf("step %d (%s), queue %d:\nremembered %v %v (%d left)\nrecounted  %v %v (%d left)",
					step, what, i, labels(got), names(got.Items), got.Remaining, labels(want), names(want.Items), want.Remaining)
			}
		}
	}
	check(0, "start")
	for step := 1; step <= 400; step++ {
		entry := all[rng.Intn(len(all))]
		var what string
		switch n := rng.Intn(20); {
		case n < 10:
			what = "annotate " + entry.Name
			annotate(t, ix, entry.ID, states[rng.Intn(len(states))])
		case n < 16:
			what = "clear " + entry.Name
			if err := ix.ClearAnnot(ctx, entry.ID); err != nil {
				t.Fatal(err)
			}
		case n < 19:
			spec := specs[rng.Intn(len(specs))]
			q := queue(t, ix, spec)
			if len(q.Groups) == 0 {
				continue
			}
			what = "skip a group"
			g := q.Groups[rng.Intn(len(q.Groups))]
			var group []any
			for _, v := range g.Values {
				group = append(group, v.Value)
			}
			ids, err := ix.PendingInGroup(ctx, spec, group)
			if err != nil {
				t.Fatal(err)
			}
			ids = ids[:min(len(ids), 4)]
			if _, err := ix.MarkSkipped(ctx, spec, group, ids); err != nil {
				t.Fatal(err)
			}
		default:
			what = "replace the cache"
			var keep []Annot
			for _, r := range all {
				if rng.Intn(6) == 0 {
					keep = append(keep, Annot{Entry: r.ID, State: states[rng.Intn(len(states))]})
				}
			}
			if err := ix.ReplaceAnnots(ctx, keep); err != nil {
				t.Fatal(err)
			}
		}
		check(step, what)
	}
}

// An index built before the covered folders existed gains them when opened.
func TestOpenUpgradesAnOlderIndex(t *testing.T) {
	ctx := context.Background()
	root, ix := coveredFixture(t)
	path := ix.Path
	if _, err := ix.db.Exec(`DROP TABLE covered; DROP INDEX entries_dirs`); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := old.SetAnnot(ctx, Annot{Entry: lookup(t, old, root, "media/movies").ID, State: StateNote}); err != nil {
		t.Fatal(err)
	}
	q := queue(t, old, QueueSpec{Filter: Filter{Kind: "file"}, ExcludeCovered: true, Order: Sort{Key: "name"}})
	if !equal(names(q.Items), []string{"2024.pdf", "pilot.mkv"}) {
		t.Errorf("items = %v", names(q.Items))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// Only a few queues are remembered. One that was dropped and is asked for
// again must count from scratch, not carry on from what it knew.
func TestForgottenQueueIsCountedAgain(t *testing.T) {
	root, ix := fixture(t)
	first := QueueSpec{Filter: Filter{Kind: "file"}, Groups: []string{"share"}}
	if q := queue(t, ix, first); q.Remaining != 7 {
		t.Fatalf("remaining = %d", q.Remaining)
	}
	for size := range maxCachedQueues + 3 {
		queue(t, ix, QueueSpec{Filter: Filter{Kind: "file", MinSize: int64(size + 1)}})
	}
	if len(ix.queues) != maxCachedQueues {
		t.Errorf("%d queues remembered, want %d", len(ix.queues), maxCachedQueues)
	}
	if _, kept := ix.queues[queueKey(first)]; kept {
		t.Fatal("the least recently used queue was not the one dropped")
	}
	annotate(t, ix, lookup(t, ix, root, "docs/report.pdf").ID, StateNote)
	if q := queue(t, ix, first); q.Remaining != 6 {
		t.Errorf("after an annotation it did not see: %d remaining", q.Remaining)
	}
}
