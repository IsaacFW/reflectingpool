// Package scan walks directory trees in parallel, reading metadata only.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type Kind uint8

const (
	KindFile Kind = iota
	KindDir
	KindSymlink
	KindOther
)

const (
	// FlagMount marks the root of a filesystem (a ZFS dataset) or of the scan.
	FlagMount uint8 = 1 << iota
	// FlagExcluded marks a directory that was recorded but not entered.
	FlagExcluded
	// FlagError marks a directory that could not be read completely.
	FlagError
)

// Entry is one file, directory, symlink or special file.
//
// For a directory, Size and Disk are the totals of everything beneath it with
// hardlinked files counted once, and Files, Dirs and the Max times are rollups
// of its descendants. For anything else they describe the entry itself.
type Entry struct {
	ID, Parent int64
	Name       string // for a scan root, its absolute path
	Kind       Kind
	Flags      uint8
	Size       int64 // apparent bytes
	Disk       int64 // allocated bytes
	Mtime      int64
	Btime      int64 // creation time, 0 when the filesystem does not report one
	// BtimeNsec is the sub-second part of Btime. Together with the inode
	// number it tells a file apart from a later one that reused the number.
	BtimeNsec uint32
	Atime     int64
	Ino       uint64
	Dev       uint64
	Nlink     uint32
	Share     int64 // ID of the top-level directory under the root, 0 above that
	Counted   bool  // false for all but one link of a hardlinked file

	Files, Dirs                  int64
	MaxMtime, MaxBtime, MaxAtime int64
}

// Sink receives scan output. Entries is called from a single goroutine;
// directories arrive after every file. Counted is called once at the end with
// the hardlinked files whose size was attributed to their directory.
type Sink interface {
	Entries(batch []Entry) error
	Counted(ids []int64) error
}

// Progress is updated while a scan runs and may be read concurrently.
type Progress struct {
	Entries, Dirs, Bytes, Errors atomic.Int64
	// Rested is the total time, in nanoseconds, walkers spent resting to
	// leave the disks to other work.
	Rested atomic.Int64
}

type Options struct {
	Roots []string
	// Workers is the number of parallel walkers for an aggressive scan.
	// The gentler intensities choose their own.
	Workers   int
	Intensity Intensity
	// Exclude lists absolute directory paths that are recorded but not entered.
	// A directory is also excluded when it is the same directory as one of
	// these reached by another path, as happens when a folder inside the
	// pool is also mapped into the container somewhere else.
	Exclude []string
	// CrossMount decides whether to enter a directory on a different
	// filesystem from its parent. Nil means always enter.
	CrossMount func(dev uint64) bool
	// ShareMetaDirs names directories that, directly inside a share, hold
	// this program's own files. They are recorded but not entered, so the
	// program's bookkeeping never shows up as data to review.
	ShareMetaDirs []string
	Progress      *Progress

	// Test hooks: called on each walker's thread as it starts, and in place
	// of sleeping.
	onWalkerStart func()
	sleep         func(time.Duration)
}

type Result struct {
	Files, Dirs, Errors int64
	Size, Disk          int64
	Hardlinked          int64 // distinct files with more than one link
	Elapsed             time.Duration
}

const (
	statxMask = unix.STATX_BASIC_STATS | unix.STATX_BTIME
	readChunk = 1024
	flushAt   = 2048
)

// DefaultWorkers favours more walkers than cores: on a cold cache the walk
// waits on disks, and concurrent reads let a multi-disk pool serve several.
func DefaultWorkers() int {
	return min(32, max(8, 4*runtime.NumCPU()))
}

type node struct {
	id       int64
	parent   *node
	name     string
	dev, ino uint64
	share    int64
	flags    uint8
	nlink    uint32

	mtime, btime, atime int64
	btimeNsec           uint32

	// Own values while walking, cumulative after rollup.
	size, disk, files, dirs int64
	maxM, maxB, maxA        int64
}

type work struct {
	n    *node
	path string
}

type linkKey struct{ dev, ino uint64 }

type link struct {
	id         int64
	dir        *node
	size, disk int64
}

type walker struct {
	opts      Options
	exclude   map[string]struct{}
	excludeID map[linkKey]struct{} // the excluded directories by device and inode
	prog      *Progress
	prof      profile
	nextID    atomic.Int64
	out       chan []Entry

	mu      sync.Mutex
	cond    *sync.Cond
	stack   []work
	pending int // directories queued or being read
	stop    bool
	quit    chan struct{} // closed when stop is set, to cut rests short

	nodesMu sync.Mutex
	nodes   []*node // every parent precedes its children

	linksMu sync.Mutex
	links   map[linkKey]link
}

// Walk scans opts.Roots and streams what it finds to sink.
func Walk(ctx context.Context, opts Options, sink Sink) (Result, error) {
	start := time.Now()
	prof := opts.Intensity.profile()
	if prof.workers > 0 {
		opts.Workers = prof.workers
	} else if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers()
	}
	if opts.Progress == nil {
		opts.Progress = &Progress{}
	}
	w := &walker{
		opts:      opts,
		exclude:   make(map[string]struct{}, len(opts.Exclude)),
		excludeID: make(map[linkKey]struct{}, len(opts.Exclude)),
		prog:      opts.Progress,
		prof:      prof,
		out:       make(chan []Entry, 4*opts.Workers),
		links:     make(map[linkKey]link),
		quit:      make(chan struct{}),
	}
	w.cond = sync.NewCond(&w.mu)
	for _, p := range opts.Exclude {
		w.exclude[filepath.Clean(p)] = struct{}{}
		var stx unix.Statx_t
		if statx(unix.AT_FDCWD, p, 0, &stx) == nil && stx.Mode&unix.S_IFMT == unix.S_IFDIR {
			w.excludeID[linkKey{devOf(&stx), stx.Ino}] = struct{}{}
		}
	}

	var roots []*node
	for _, root := range opts.Roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return Result{}, fmt.Errorf("scan root %s: %w", root, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return Result{}, fmt.Errorf("scan root %s: %w", root, err)
		}
		var stx unix.Statx_t
		if err := statx(unix.AT_FDCWD, real, 0, &stx); err != nil {
			return Result{}, fmt.Errorf("scan root %s: %w", root, err)
		}
		if stx.Mode&unix.S_IFMT != unix.S_IFDIR {
			return Result{}, fmt.Errorf("scan root %s: not a directory", root)
		}
		n := w.newNode(nil, real, &stx, FlagMount)
		roots = append(roots, n)
		w.push(work{n, real})
	}

	var sinkErr error
	written := make(chan struct{})
	go func() {
		defer close(written)
		for batch := range w.out {
			if sinkErr != nil {
				continue
			}
			if err := sink.Entries(batch); err != nil {
				sinkErr = err
				w.abort()
			}
		}
	}()

	stopWatch := context.AfterFunc(ctx, w.abort)
	var wg sync.WaitGroup
	for range opts.Workers {
		wg.Go(func() {
			prof.runWalker(func() {
				if opts.onWalkerStart != nil {
					opts.onWalkerStart()
				}
				w.run()
			})
		})
	}
	wg.Wait()
	stopWatch()

	if ctx.Err() != nil {
		close(w.out)
		<-written
		return Result{}, ctx.Err()
	}

	// Attribute each hardlinked file's size to one directory, then fold every
	// directory into its parent. Children always follow parents in w.nodes.
	counted := make([]int64, 0, len(w.links))
	for _, l := range w.links {
		l.dir.size += l.size
		l.dir.disk += l.disk
		counted = append(counted, l.id)
	}
	for i := len(w.nodes) - 1; i >= 0; i-- {
		n := w.nodes[i]
		if p := n.parent; p != nil {
			p.size += n.size
			p.disk += n.disk
			p.files += n.files
			p.dirs += n.dirs
			p.maxM = max(p.maxM, n.maxM)
			p.maxB = max(p.maxB, n.maxB)
			p.maxA = max(p.maxA, n.maxA)
		}
	}
	for i := 0; i < len(w.nodes); i += flushAt {
		chunk := w.nodes[i:min(i+flushAt, len(w.nodes))]
		batch := make([]Entry, len(chunk))
		for j, n := range chunk {
			batch[j] = n.entry()
		}
		w.out <- batch
	}
	close(w.out)
	<-written
	if sinkErr != nil {
		return Result{}, sinkErr
	}
	if err := sink.Counted(counted); err != nil {
		return Result{}, err
	}

	res := Result{
		Errors:     w.prog.Errors.Load(),
		Hardlinked: int64(len(w.links)),
		Elapsed:    time.Since(start),
	}
	for _, n := range roots {
		res.Files += n.files
		res.Dirs += n.dirs + 1
		res.Size += n.size
		res.Disk += n.disk
	}
	return res, nil
}

func (n *node) entry() Entry {
	e := Entry{
		ID: n.id, Name: n.name, Kind: KindDir, Flags: n.flags,
		Size: n.size, Disk: n.disk,
		Mtime: n.mtime, Btime: n.btime, BtimeNsec: n.btimeNsec, Atime: n.atime,
		Ino: n.ino, Dev: n.dev, Nlink: n.nlink, Share: n.share, Counted: true,
		Files: n.files, Dirs: n.dirs,
		MaxMtime: n.maxM, MaxBtime: n.maxB, MaxAtime: n.maxA,
	}
	if n.parent != nil {
		e.Parent = n.parent.id
	}
	return e
}

func (w *walker) newNode(parent *node, name string, stx *unix.Statx_t, flags uint8) *node {
	n := &node{
		id: w.nextID.Add(1), parent: parent, name: name,
		dev: devOf(stx), ino: stx.Ino, flags: flags, nlink: stx.Nlink,
		mtime: stx.Mtime.Sec, atime: stx.Atime.Sec,
		disk: int64(stx.Blocks) * 512,
	}
	n.btime, n.btimeNsec = btimeOf(stx)
	if parent != nil {
		n.share = parent.share
		if parent.parent == nil {
			n.share = n.id
		}
	}
	w.nodesMu.Lock()
	w.nodes = append(w.nodes, n)
	w.nodesMu.Unlock()
	return n
}

func (w *walker) push(it work) {
	w.mu.Lock()
	w.stack = append(w.stack, it)
	w.pending++
	w.mu.Unlock()
	w.cond.Signal()
}

func (w *walker) abort() {
	w.mu.Lock()
	if !w.stop {
		w.stop = true
		close(w.quit)
	}
	w.mu.Unlock()
	w.cond.Broadcast()
}

// rest pauses a walker after a chunk of work, for as long as the scan's
// intensity asks.
func (w *walker) rest(worked time.Duration) {
	d := w.prof.rest(worked)
	if d <= 0 {
		return
	}
	w.prog.Rested.Add(int64(d))
	if w.opts.sleep != nil {
		w.opts.sleep(d)
		return
	}
	t := time.NewTimer(d)
	select {
	case <-t.C:
	case <-w.quit:
		t.Stop()
	}
}

func (w *walker) run() {
	for {
		w.mu.Lock()
		for len(w.stack) == 0 && w.pending > 0 && !w.stop {
			w.cond.Wait()
		}
		if w.stop || len(w.stack) == 0 {
			w.mu.Unlock()
			return
		}
		it := w.stack[len(w.stack)-1]
		w.stack = w.stack[:len(w.stack)-1]
		w.mu.Unlock()

		w.readDir(it)

		w.mu.Lock()
		w.pending--
		if w.pending == 0 {
			w.cond.Broadcast()
		}
		w.mu.Unlock()
	}
}

func (w *walker) readDir(it work) {
	n := it.n
	began := time.Now()
	fd, err := openDir(it.path)
	if err != nil {
		n.flags |= FlagError
		w.prog.Errors.Add(1)
		return
	}
	f := os.NewFile(uintptr(fd), it.path)
	defer f.Close()
	// The folder was queued by path, and a folder above it may since have
	// been swapped for a link to somewhere else; the open follows links in
	// every component but the last. So the open folder is checked against the
	// one that was listed: a different device or inode is not it, and is left
	// out rather than read.
	var self unix.Statx_t
	if err := statx(fd, "", unix.AT_EMPTY_PATH, &self); err != nil || devOf(&self) != n.dev || self.Ino != n.ino {
		n.flags |= FlagError
		w.prog.Errors.Add(1)
		return
	}

	var batch []Entry
	var bytes int64
	for {
		names, rerr := f.Readdirnames(readChunk)
		for _, name := range names {
			var stx unix.Statx_t
			if err := statx(fd, name, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, &stx); err != nil {
				if !errors.Is(err, unix.ENOENT) { // ENOENT: removed while we were reading
					w.prog.Errors.Add(1)
				}
				continue
			}
			if stx.Mode&unix.S_IFMT == unix.S_IFDIR {
				w.subdir(it, name, &stx)
				continue
			}
			e := Entry{
				ID: w.nextID.Add(1), Parent: n.id, Name: name, Kind: kindOf(stx.Mode),
				Size: int64(stx.Size), Disk: int64(stx.Blocks) * 512,
				Mtime: stx.Mtime.Sec, Atime: stx.Atime.Sec,
				Ino: stx.Ino, Dev: devOf(&stx), Nlink: stx.Nlink, Share: n.share, Counted: true,
			}
			e.Btime, e.BtimeNsec = btimeOf(&stx)
			n.files++
			n.maxM = max(n.maxM, e.Mtime)
			n.maxB = max(n.maxB, e.Btime)
			n.maxA = max(n.maxA, e.Atime)
			if e.Nlink > 1 {
				e.Counted = false
				w.addLink(&e, n)
			} else {
				n.size += e.Size
				n.disk += e.Disk
			}
			bytes += e.Size
			batch = append(batch, e)
		}
		if len(batch) >= flushAt {
			w.prog.Entries.Add(int64(len(batch)))
			w.out <- batch
			batch = nil
		}
		// Rest after every chunk, not only after the directory, so that a
		// folder with a million entries is not read in one unbroken run.
		w.rest(time.Since(began))
		began = time.Now()
		if rerr != nil {
			if rerr != io.EOF {
				n.flags |= FlagError
				w.prog.Errors.Add(1)
			}
			break
		}
	}
	if len(batch) > 0 {
		w.prog.Entries.Add(int64(len(batch)))
		w.out <- batch
	}
	w.prog.Bytes.Add(bytes)
	w.prog.Dirs.Add(1)
}

func (w *walker) subdir(it work, name string, stx *unix.Statx_t) {
	parent := it.n
	// With snapdir=visible every dataset root lists .zfs, which holds a full
	// copy of the dataset per snapshot.
	if name == ".zfs" && parent.flags&FlagMount != 0 {
		return
	}
	path := it.path + "/" + name
	if it.path == "/" {
		path = "/" + name
	}
	var flags uint8
	if dev := devOf(stx); dev != parent.dev {
		flags |= FlagMount
		if w.opts.CrossMount != nil && !w.opts.CrossMount(dev) {
			flags |= FlagExcluded
		}
	}
	if _, ok := w.exclude[path]; ok {
		flags |= FlagExcluded
	} else if _, ok := w.excludeID[linkKey{devOf(stx), stx.Ino}]; ok {
		flags |= FlagExcluded
	} else if parent.id == parent.share && slices.Contains(w.opts.ShareMetaDirs, name) {
		flags |= FlagExcluded
	}
	c := w.newNode(parent, name, stx, flags)
	parent.dirs++
	w.prog.Entries.Add(1)
	if flags&FlagExcluded == 0 {
		w.push(work{c, path})
	}
}

// addLink records one link of a hardlinked file. The file's size is later
// attributed to the directory with the lowest (dev, ino) among its links, so
// directory totals do not depend on the order the walk happened to take.
func (w *walker) addLink(e *Entry, dir *node) {
	k := linkKey{e.Dev, e.Ino}
	w.linksMu.Lock()
	cur, ok := w.links[k]
	if !ok || dir.dev < cur.dir.dev || (dir.dev == cur.dir.dev && dir.ino < cur.dir.ino) {
		w.links[k] = link{id: e.ID, dir: dir, size: e.Size, disk: e.Disk}
	}
	w.linksMu.Unlock()
}

func openDir(path string) (int, error) {
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	for {
		// O_NOATIME keeps the scan from disturbing read times; it is refused
		// for directories we neither own nor have CAP_FOWNER over.
		fd, err := unix.Open(path, flags|unix.O_NOATIME, 0)
		if err == unix.EPERM {
			fd, err = unix.Open(path, flags, 0)
		}
		if err != unix.EINTR {
			return fd, err
		}
	}
}

func statx(dirfd int, name string, flags int, stx *unix.Statx_t) error {
	for {
		err := unix.Statx(dirfd, name, flags, statxMask, stx)
		if err != unix.EINTR {
			return err
		}
	}
}

func devOf(stx *unix.Statx_t) uint64 {
	return unix.Mkdev(stx.Dev_major, stx.Dev_minor)
}

func btimeOf(stx *unix.Statx_t) (sec int64, nsec uint32) {
	if stx.Mask&unix.STATX_BTIME == 0 {
		return 0, 0
	}
	return stx.Btime.Sec, stx.Btime.Nsec
}

func kindOf(mode uint16) Kind {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return KindFile
	case unix.S_IFDIR:
		return KindDir
	case unix.S_IFLNK:
		return KindSymlink
	}
	return KindOther
}
