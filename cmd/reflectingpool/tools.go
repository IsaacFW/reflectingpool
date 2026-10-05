package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/IsaacFW/reflectingpool/internal/appdb"
	"github.com/IsaacFW/reflectingpool/internal/core"
	"github.com/IsaacFW/reflectingpool/internal/index"
	"github.com/IsaacFW/reflectingpool/internal/scan"
	"github.com/IsaacFW/reflectingpool/internal/storage"
)

type countSink struct{ entries int64 }

func (c *countSink) Entries(b []scan.Entry) error { c.entries += int64(len(b)); return nil }
func (c *countSink) Counted([]int64) error        { return nil }

// cmdBench measures how fast this machine walks and indexes a tree. It reads
// metadata only and writes nothing outside a temporary directory.
func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	intensityName := fs.String("intensity", "aggressive", "scan intensity to measure: aggressive, balanced or low")
	limit := fs.Duration("for", 0, "walk for this long and report the rate, e.g. 5m; 0 runs the full benchmark")
	if err := fs.Parse(args); err != nil {
		return err
	}
	intensity, err := scan.ParseIntensity(*intensityName)
	if err != nil {
		return err
	}
	roots := fs.Args()
	if len(roots) == 0 {
		roots = envList("RP_ROOTS")
	}
	if len(roots) == 0 {
		return fmt.Errorf("give a directory to measure, or set RP_ROOTS")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	workers := scan.DefaultWorkers()
	if v, err := strconv.Atoi(os.Getenv("RP_WORKERS")); err == nil && v > 0 {
		workers = v
	}
	fmt.Printf("Reflecting Pool %s benchmark\nroots: %s\nintensity: %s, cores: %d\n\n", version, strings.Join(roots, ", "), intensity, runtime.NumCPU())
	if *limit > 0 {
		return benchTimed(ctx, roots, workers, intensity, *limit)
	}

	// Two walks with no index: the first shows the cache as it was found,
	// the second shows the speed once the filesystem's metadata is in memory.
	for _, label := range []string{"walk 1 (cache as found)", "walk 2 (cache warm)"} {
		sink := &countSink{}
		res, err := scan.Walk(ctx, scan.Options{Roots: roots, Workers: workers, Intensity: intensity}, sink)
		if err != nil {
			return err
		}
		fmt.Printf("%-26s %10d entries in %8s  %9.0f entries/s  (%d errors)\n", label, sink.entries,
			res.Elapsed.Round(time.Millisecond), float64(sink.entries)/res.Elapsed.Seconds(), res.Errors)
	}

	tmp, err := os.MkdirTemp(envOr("RP_DATA", os.TempDir()), "bench-*")
	if err != nil {
		tmp, err = os.MkdirTemp("", "bench-*")
		if err != nil {
			return err
		}
	}
	defer os.RemoveAll(tmp)
	db, err := appdb.Open(filepath.Join(tmp, "app.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	// Read-only, so that reconciling existing annotations cannot rewrite them.
	app, err := core.New(ctx, core.Config{Roots: roots, DataDir: tmp, Workers: workers, ReadOnly: true}, db)
	if err != nil {
		return err
	}
	defer app.Close()

	start := time.Now()
	info, err := app.Scan(ctx, intensity)
	if err != nil {
		return err
	}
	total := time.Since(start)
	walked := info.Finished.Sub(info.Started)
	var indexBytes int64
	if st, err := os.Stat(index.Latest(filepath.Join(tmp, "index"))); err == nil {
		indexBytes = st.Size()
	}
	entries := info.Files + info.Dirs
	fmt.Printf("%-26s %10d entries in %8s  %9.0f entries/s\n", "walk + write index", entries, walked.Round(time.Millisecond), float64(entries)/walked.Seconds())
	fmt.Printf("%-26s %21s %8s\n", "build lookup indexes", "", (total - walked).Round(time.Millisecond))
	fmt.Printf("%-26s %21s %8s\n\n", "full scan, end to end", "", total.Round(time.Millisecond))
	fmt.Printf("files %d, folders %d, hardlinked files %d\n", info.Files, info.Dirs, info.Hardlinked)
	fmt.Printf("apparent size %s, on disk %s\n", human(info.Size), human(info.Disk))
	if entries > 0 {
		fmt.Printf("index file %s (%d bytes per entry)\n", human(indexBytes), indexBytes/entries)
	}
	fmt.Printf("peak memory %s\n\n", peakMemory())

	fmt.Printf("query timings %31s %10s\n", "first call", "repeat")
	return app.View(func(ix *index.Index) error {
		// The first call pays for anything cached; the repeat (best of 3) is
		// what a user feels while paging or working through a review.
		timeIt := func(label string, fn func() error) error {
			var first, best time.Duration
			for i := range 4 {
				t := time.Now()
				if err := fn(); err != nil {
					return fmt.Errorf("%s: %w", label, err)
				}
				switch d := time.Since(t); {
				case i == 0:
					first = d
				case i == 1 || d < best:
					best = d
				}
			}
			fmt.Printf("  %-42s %10s %10s\n", label, first.Round(10*time.Microsecond), best.Round(10*time.Microsecond))
			return nil
		}
		rootRows, _, err := ix.Children(ctx, 0, index.Sort{}, 0, 0)
		if err != nil || len(rootRows) == 0 {
			return err
		}
		biggest, err := ix.Find(ctx, index.Filter{Kind: "dir"}, index.Sort{Key: "files", Desc: true}, 1, 0)
		if err != nil || len(biggest) == 0 {
			return err
		}
		steps := []struct {
			label string
			fn    func() error
		}{
			{"children of the first root, by size", func() error {
				_, _, err := ix.Children(ctx, rootRows[0].ID, index.Sort{Key: "size", Desc: true}, 500, 0)
				return err
			}},
			{fmt.Sprintf("children of the fullest folder (%d files)", biggest[0].Files), func() error {
				_, _, err := ix.Children(ctx, biggest[0].ID, index.Sort{Key: "size", Desc: true}, 500, 0)
				return err
			}},
			{"100 largest files, with paths", func() error {
				rows, err := ix.Find(ctx, index.Filter{Kind: "file"}, index.Sort{Key: "size", Desc: true}, 100, 0)
				if err != nil {
					return err
				}
				return ix.FillPaths(ctx, rows)
			}},
			{"100 largest folders", func() error {
				_, err := ix.Find(ctx, index.Filter{Kind: "dir"}, index.Sort{Key: "size", Desc: true}, 100, 0)
				return err
			}},
			{"name search across everything", func() error {
				_, err := ix.Find(ctx, index.Filter{Name: "report"}, index.Sort{Key: "size", Desc: true}, 100, 0)
				return err
			}},
			{"files over 1 GiB not modified in 2 years", func() error {
				_, err := ix.Find(ctx, index.Filter{Kind: "file", MinSize: 1 << 30, ModifiedBefore: time.Now().AddDate(-2, 0, 0).Unix()}, index.Sort{Key: "size", Desc: true}, 100, 0)
				return err
			}},
			{"review queue: share, type, largest first", func() error {
				_, err := ix.Queue(ctx, index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"share", "type"}, Order: index.Sort{Key: "size", Desc: true}})
				return err
			}},
			{"review queue: folders by share", func() error {
				_, err := ix.Queue(ctx, index.QueueSpec{Filter: index.Filter{Kind: "dir"}, Groups: []string{"share"}, Order: index.Sort{Key: "size", Desc: true}})
				return err
			}},
			{"review queue: share, age, oldest first", func() error {
				_, err := ix.Queue(ctx, index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"share", "age"}, Order: index.Sort{Key: "age"}})
				return err
			}},
			{"review queue: by extension", func() error {
				_, err := ix.Queue(ctx, index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"ext"}, Order: index.Sort{Key: "size", Desc: true}})
				return err
			}},
		}
		for _, s := range steps {
			if err := timeIt(s.label, s.fn); err != nil {
				return err
			}
		}
		return benchReview(ctx, ix, timeIt)
	})
}

// benchReview measures a review in progress: what one step costs, and whether
// it still costs the same once a folder has been described and tens of
// thousands of items have been skipped. It marks items in the temporary index
// only; nothing is written to the shares.
func benchReview(ctx context.Context, ix *index.Index, timeIt func(string, func() error) error) error {
	fmt.Printf("\nreview in progress (marks the temporary index only; the shares are not written to)\n")
	once := func(label string, fn func() error) error {
		t := time.Now()
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		fmt.Printf("  %-42s %10s\n", label, time.Since(t).Round(10*time.Microsecond))
		return nil
	}
	// One step of a review: record the item, fetch the next ones.
	steps := func(label string, spec index.QueueSpec) error {
		const n = 200
		done := 0
		t := time.Now()
		for ; done < n; done++ {
			q, err := ix.Queue(ctx, spec)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			if len(q.Items) == 0 {
				break
			}
			if err := ix.SetAnnot(ctx, index.Annot{Entry: q.Items[0].ID, State: index.StateSkipped}); err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
		}
		if done == 0 {
			return nil
		}
		fmt.Printf("  %-42s %10s  (mean of %d steps)\n", label, (time.Since(t) / time.Duration(done)).Round(10*time.Microsecond), done)
		return nil
	}
	queue := index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"share", "type"}, Order: index.Sort{Key: "size", Desc: true}}
	covered := queue
	covered.ExcludeCovered = true
	byAge := index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"share", "age"}, Order: index.Sort{Key: "age"}}
	if err := steps("one step: share, type, largest first", queue); err != nil {
		return err
	}
	if err := steps("one step: share, age, oldest first", byAge); err != nil {
		return err
	}

	// The largest folder that can be described: inside a share, not a share.
	folders, err := ix.Find(ctx, index.Filter{Kind: "dir"}, index.Sort{Key: "files", Desc: true}, 200, 0)
	if err != nil {
		return err
	}
	var folder *index.Row
	for i := range folders {
		if f := &folders[i]; f.Share != 0 && f.Share != f.ID {
			folder = f
			break
		}
	}
	if folder != nil {
		under := index.QueueSpec{Filter: index.Filter{Kind: "file", Under: folder.ID}, Groups: []string{"type"}, Order: index.Sort{Key: "size", Desc: true}}
		if err := timeIt(fmt.Sprintf("queue inside one folder (%d files)", folder.Files), func() error {
			_, err := ix.Queue(ctx, under)
			return err
		}); err != nil {
			return err
		}
		if err := steps("one step inside that folder", under); err != nil {
			return err
		}
		if _, err := ix.Queue(ctx, covered); err != nil { // so that describing the folder has a queue to update
			return err
		}
		if err := once(fmt.Sprintf("describe that folder (%d folders in it)", folder.Dirs), func() error {
			return ix.SetAnnot(ctx, index.Annot{Entry: folder.ID, State: index.StateNote})
		}); err != nil {
			return err
		}
		if err := timeIt("queue leaving out described folders", func() error {
			_, err := ix.Queue(ctx, covered)
			return err
		}); err != nil {
			return err
		}
		if err := steps("one step leaving out described folders", covered); err != nil {
			return err
		}
	}

	// Skip the rest of the current group, as the bulk action does.
	q, err := ix.Queue(ctx, queue)
	if err != nil || len(q.Groups) == 0 {
		return err
	}
	var group []any
	for _, v := range q.Groups[0].Values {
		group = append(group, v.Value)
	}
	if err := once(fmt.Sprintf("skip the rest of a group (%d items)", q.Groups[0].Remaining), func() error {
		pending, err := ix.PendingInGroup(ctx, queue, group)
		if err != nil {
			return err
		}
		for ids := range slices.Chunk(pending, 10_000) {
			rows, err := ix.Rows(ctx, ids)
			if err != nil {
				return err
			}
			if err := ix.FillPaths(ctx, rows); err != nil { // the real thing needs each item's path
				return err
			}
			if _, err := ix.MarkSkipped(ctx, queue, group, ids); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := steps("one step after that skip", queue); err != nil {
		return err
	}
	// The hardest thing to fetch: the first items of a small group that no
	// index leads to. A queue pays this once each time it changes group.
	byExt := index.QueueSpec{Filter: index.Filter{Kind: "file"}, Groups: []string{"ext"}, Order: index.Sort{Key: "size", Desc: true}}
	if q, err = ix.Queue(ctx, byExt); err != nil {
		return err
	}
	byExt.Offset = int(q.Remaining) - 1
	if err := once("change to the smallest group, by extension", func() error {
		_, err := ix.Queue(ctx, byExt)
		return err
	}); err != nil {
		return err
	}
	// Another queue over the same items has to count the skips once.
	return timeIt("another queue after that skip", func() error {
		_, err := ix.Queue(ctx, byAge)
		return err
	})
}

// benchTimed walks at one intensity for a fixed time. It exists to answer the
// question the full benchmark cannot: how much does a scan at this intensity
// slow down whatever else is using the pool? Start a transfer, run this, and
// watch the transfer's speed.
func benchTimed(ctx context.Context, roots []string, workers int, intensity scan.Intensity, limit time.Duration) error {
	expected := expectedEntries(roots)
	fmt.Printf("walking for %s; compare the speed of other work on the pool now with its speed before\n", limit)
	prog := &scan.Progress{}
	timed, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	_, err := scan.Walk(timed, scan.Options{Roots: roots, Workers: workers, Intensity: intensity, Progress: prog}, &countSink{})
	elapsed := time.Since(start)
	if err != nil && timed.Err() == nil {
		return err
	}
	entries := prog.Entries.Load()
	rate := float64(entries) / elapsed.Seconds()
	fmt.Printf("\nwalked %d entries in %s: %.0f entries/s (%d errors)\n", entries, elapsed.Round(time.Millisecond), rate, prog.Errors.Load())
	fmt.Printf("walkers rested for %s in total to leave the disks to other work\n", time.Duration(prog.Rested.Load()).Round(time.Millisecond))
	switch {
	case err == nil:
		fmt.Println("the whole tree was walked within the time, so this was a complete walk")
	case expected > 0 && rate > 0:
		fmt.Printf("the filesystems report about %d entries: a full walk at this rate would take about %s\n",
			expected, (time.Duration(float64(expected)/rate) * time.Second).Round(time.Minute))
		fmt.Println("(the rate was measured with the cache as it was found; a repeated walk can be faster)")
	}
	return nil
}

// expectedEntries is the filesystems' own count of files and folders under
// the roots, or 0 when it cannot be known.
func expectedEntries(roots []string) int64 {
	mounts, err := storage.Mounts()
	if err != nil {
		return 0
	}
	table := storage.NewTable(mounts)
	seen := make(map[uint64]bool)
	var total int64
	for _, root := range roots {
		for _, m := range table.Under(root) {
			if seen[m.Dev] {
				continue
			}
			seen[m.Dev] = true
			if u, err := storage.StatUsage(m.MountPoint); err == nil {
				total += u.Objects
			}
		}
	}
	return total
}

func peakMemory() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			if kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64); err == nil {
				return human(kb * 1024)
			}
		}
	}
	return "unknown"
}

const capSysAdmin = 21

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// cmdDoctor reports what the container can see and do, so that problems with
// mappings, permissions or the ZFS passthrough are found before they matter.
// It only reads.
func cmdDoctor() error {
	ctx := context.Background()
	problems := 0
	ok := func(format string, args ...any) { fmt.Printf("  ok    "+format+"\n", args...) }
	note := func(format string, args ...any) { fmt.Printf("  note  "+format+"\n", args...) }
	bad := func(format string, args ...any) {
		problems++
		fmt.Printf("  FIX   "+format+"\n", args...)
	}

	fmt.Printf("Reflecting Pool %s doctor\n\nProcess\n", version)
	var uts unix.Utsname
	if unix.Uname(&uts) == nil {
		ok("kernel %s", unix.ByteSliceToString(uts.Release[:]))
	}
	if os.Geteuid() == 0 {
		ok("running as root inside the container, so every file can be read")
	} else {
		note("running as uid %d: files this user cannot read will be missing from scans", os.Geteuid())
	}
	if caps, found := effectiveCaps(); found {
		if caps&(1<<capSysAdmin) != 0 {
			bad("the container has CAP_SYS_ADMIN (is \"Privileged\" on?). Turn it off: with it, a fault in this program could destroy datasets and snapshots.")
		} else {
			ok("not privileged (no CAP_SYS_ADMIN): ZFS will refuse destructive commands from here")
		}
	}

	fmt.Println("\nScan roots")
	roots := envList("RP_ROOTS")
	if len(roots) == 0 {
		bad("RP_ROOTS is not set")
	}
	var table *storage.Table
	if mounts, err := storage.Mounts(); err != nil {
		bad("cannot read the mount table: %v", err)
	} else {
		table = storage.NewTable(mounts)
	}
	for _, root := range roots {
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			bad("%s is not a directory in this container; map the pool to this path", root)
			continue
		}
		var stx unix.Statx_t
		if unix.Statx(unix.AT_FDCWD, root, 0, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &stx) == nil && stx.Mask&unix.STATX_BTIME == 0 {
			note("%s does not report creation times; annotations will follow renames by inode number alone", root)
		}
		if table == nil {
			continue
		}
		m, found := table.Containing(root)
		if !found {
			note("%s: no mount found", root)
			continue
		}
		// A root is either a mapped filesystem itself (a whole pool), or a
		// folder of the container holding one mapping per share.
		type mapping struct {
			path string
			m    storage.Mount
		}
		mapped := []mapping{{root, m}}
		if m.MountPoint != root {
			var inside []mapping
			for _, c := range table.Under(root) {
				if filepath.Dir(c.MountPoint) == root {
					inside = append(inside, mapping{c.MountPoint, c})
				}
			}
			if len(inside) > 0 {
				ok("%s holds %d mapped folder(s); each is scanned as a share", root, len(inside))
				mapped = inside
			} else {
				note("%s is a folder inside %s %q with nothing mapped into it; the folders it contains are scanned as shares", root, m.FSType, m.Source)
			}
		}
		scanned := make(map[uint64]storage.Mount)
		for _, c := range table.Under(root) {
			scanned[c.Dev] = c
		}
		for _, t := range mapped {
			scanned[t.m.Dev] = t.m
			ok("%s is %s %q", t.path, t.m.FSType, t.m.Source)
			if u, err := storage.StatUsage(t.path); err == nil {
				ok("%s: %s used, %s free, %d files and folders by the filesystem's count", t.path, human(u.Used), human(u.Avail), u.Objects)
			}
			if hasOption(t.m.Options, "ro") {
				note("%s is mounted read-only: browsing works, annotations and quarantine do not", t.path)
			} else if unix.Access(t.path, unix.W_OK) != nil {
				note("%s is not writable by this process: annotations cannot be saved", t.path)
			} else {
				ok("%s is writable, so annotations can be saved", t.path)
			}
			if !strings.Contains(t.m.Propagation, "master:") && !strings.Contains(t.m.Propagation, "shared:") {
				note("%s does not receive new mounts from the host. Datasets created or mounted after the container starts stay invisible until it restarts; set the path's access mode to \"Read/Write - Slave\" to fix that.", t.path)
			} else {
				ok("%s receives new mounts from the host (%s)", t.path, t.m.Propagation)
			}
		}
		noatime := 0
		for _, c := range scanned {
			if hasOption(c.Options, "noatime") {
				noatime++
			}
		}
		ok("%d filesystem(s) in all will be scanned under %s", len(scanned), root)
		if noatime == len(scanned) {
			note("read times are off (noatime) on all of them: staleness can only use modified and creation times")
		} else if noatime > 0 {
			note("read times are off (noatime) on %d of them", noatime)
		}
	}

	fmt.Println("\nZFS")
	kmod := ""
	if data, err := os.ReadFile("/sys/module/zfs/version"); err == nil {
		kmod = strings.TrimSpace(string(data))
		ok("host ZFS kernel module %s", kmod)
	} else {
		note("no ZFS kernel module is visible; this host does not appear to use ZFS")
	}
	_, devErr := os.Stat("/dev/zfs")
	bin, binErr := exec.LookPath("zfs")
	switch {
	case devErr != nil:
		note("/dev/zfs is not passed in: snapshot sizes will be unavailable unless the host script is set up")
	case binErr != nil:
		bad("/dev/zfs is present but the zfs command is missing from this image")
	default:
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, "version")
		cmd.Stdout = &out
		cmd.Run()
		tools := firstMatch(out.String())
		ok("zfs tools in this image: %s", strings.Join(strings.Fields(out.String()), " "))
		if a, b := versionRE.FindStringSubmatch(tools), versionRE.FindStringSubmatch(kmod); a != nil && b != nil && (a[1] != b[1] || a[2] != b[2]) {
			note("the tools (%s) and the host module (%s) are different release series; if the listing below fails, use an image built for %s.%s", a[0], b[0], b[1], b[2])
		}
	}
	listing := storage.ListZFS(ctx, envOr("RP_ZFS_LIST_FILE", filepath.Join(envOr("RP_DATA", "/data"), "zfs-list.txt")))
	filesystems, snapshots := 0, 0
	var snapBytes int64
	for _, d := range listing.Datasets {
		if d.Type == "snapshot" {
			snapshots++
		} else {
			filesystems++
			if d.UsedSnap > 0 {
				snapBytes += d.UsedSnap
			}
		}
	}
	switch listing.Origin {
	case "zfs":
		ok("zfs list works: %d datasets, %d snapshots holding %s", filesystems, snapshots, human(snapBytes))
	case "file":
		age := time.Since(listing.AsOf).Round(time.Minute)
		ok("read the host script's output (%s old): %d datasets, %d snapshots holding %s", age, filesystems, snapshots, human(snapBytes))
		if age > 6*time.Hour {
			note("that file is %s old; check the host script is still scheduled", age)
		}
	default:
		note("no ZFS accounting available")
	}
	if listing.Err != "" {
		bad("%s", listing.Err)
	}

	fmt.Println("\nData directory")
	data := envOr("RP_DATA", "/data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		bad("%s cannot be created: %v", data, err)
	} else if f, err := os.CreateTemp(data, ".doctor-*"); err != nil {
		bad("%s is not writable: %v", data, err)
	} else {
		f.Close()
		os.Remove(f.Name())
		if u, err := storage.StatUsage(data); err == nil {
			ok("%s is writable, %s free", data, human(u.Avail))
		}
		if table != nil {
			if m, found := table.Containing(data); found && (m.FSType == "overlay" || m.FSType == "tmpfs") {
				bad("%s is inside the container, not a mapped folder: the index, the admin account and the TLS key will be lost when the container is recreated", data)
			}
		}
	}

	if problems > 0 {
		fmt.Printf("\n%d item(s) marked FIX need attention.\n", problems)
		os.Exit(1)
	}
	fmt.Println("\nNothing needs fixing.")
	return nil
}

func firstMatch(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func hasOption(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == want {
			return true
		}
	}
	return false
}

func effectiveCaps() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "CapEff:"); ok {
			v, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
			return v, err == nil
		}
	}
	return 0, false
}
