// Package core ties the scanner, the index, the annotation files and the
// storage readers together into the operations the API exposes.
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/index"
	"github.com/IsaacFW/reflectingpool/internal/meta"
	"github.com/IsaacFW/reflectingpool/internal/scan"
	"github.com/IsaacFW/reflectingpool/internal/storage"
)

var (
	ErrReadOnly    = errors.New("read-only mode is on: changes are disabled")
	ErrNoIndex     = errors.New("no scan has finished yet")
	ErrScanRunning = errors.New("a scan is already running")
	ErrNoShare     = errors.New("this item is not inside a share, so there is nowhere to keep its annotation")
	ErrGone        = errors.New("the item is no longer where the last scan found it; scan again")
	ErrNotFile     = errors.New("only regular files have contents to show")
)

// InputError reports a request the user can correct.
type InputError string

func (e InputError) Error() string { return string(e) }

type Config struct {
	Roots       []string // directories to scan, normally one per pool
	DataDir     string   // where the index, the app database and TLS keys live
	Exclude     []string // absolute directories to record but not enter
	Workers     int
	ReadOnly    bool
	KeepIndexes int
	ZFSListFile string // output of a host script, used when /dev/zfs is not passed in
}

type App struct {
	cfg Config
	db  *sql.DB
	bg  context.Context

	ixMu sync.RWMutex
	ix   *index.Index

	scanMu   sync.Mutex
	scanning bool
	progress *scan.Progress
	started  time.Time
	expected int64
	lastErr  string
	warnings []string

	storesMu sync.Mutex
	stores   map[string]*meta.Store

	zfsMu sync.Mutex
	zfs   storage.ZFSListing
	zfsAt time.Time
}

// New prepares the data directory and opens the most recent index, if any.
// bg bounds background work such as scans started through the API.
func New(bg context.Context, cfg Config, db *sql.DB) (*App, error) {
	if len(cfg.Roots) == 0 {
		return nil, errors.New("no directories to scan are configured")
	}
	seen := make(map[string]bool)
	var roots []string
	for _, r := range cfg.Roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("scan root %s: %w", r, err)
		}
		if !seen[real] {
			seen[real] = true
			roots = append(roots, real)
		}
	}
	cfg.Roots = roots
	if cfg.KeepIndexes <= 0 {
		cfg.KeepIndexes = 3
	}
	a := &App{cfg: cfg, db: db, bg: bg, stores: make(map[string]*meta.Store)}
	if err := os.MkdirAll(a.indexDir(), 0o700); err != nil {
		return nil, err
	}
	index.Prune(a.indexDir(), cfg.KeepIndexes)
	if path := index.Latest(a.indexDir()); path != "" {
		ix, err := index.Open(path)
		if err != nil {
			return nil, err
		}
		a.ix = ix
	}
	return a, nil
}

func (a *App) Close() {
	a.ixMu.Lock()
	defer a.ixMu.Unlock()
	if a.ix != nil {
		a.ix.Close()
		a.ix = nil
	}
}

func (a *App) Config() Config   { return a.cfg }
func (a *App) indexDir() string { return filepath.Join(a.cfg.DataDir, "index") }

// View runs fn against the current index. The index cannot be replaced by a
// finishing scan while fn runs.
func (a *App) View(fn func(ix *index.Index) error) error {
	a.ixMu.RLock()
	defer a.ixMu.RUnlock()
	if a.ix == nil {
		return ErrNoIndex
	}
	return fn(a.ix)
}

func (a *App) store(sharePath string) *meta.Store {
	a.storesMu.Lock()
	defer a.storesMu.Unlock()
	s := a.stores[sharePath]
	if s == nil {
		s = meta.Open(sharePath)
		a.stores[sharePath] = s
	}
	return s
}

// ScanStatus describes the running scan, if any, and the index in use.
type ScanStatus struct {
	Running bool       `json:"running"`
	Started *time.Time `json:"started,omitempty"`
	Entries int64      `json:"entries"`
	// Expected is the filesystems' own count of files and directories, or 0
	// when they do not report one; Percent is only meaningful when it is set.
	Expected  int64       `json:"expected"`
	Percent   float64     `json:"percent"`
	Errors    int64       `json:"errors"`
	LastError string      `json:"last_error,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
	Index     *index.Info `json:"index,omitempty"`
}

func (a *App) ScanStatus() ScanStatus {
	a.scanMu.Lock()
	st := ScanStatus{Running: a.scanning, LastError: a.lastErr, Warnings: a.warnings}
	if a.scanning {
		started := a.started
		st.Started = &started
		st.Entries = a.progress.Entries.Load()
		st.Errors = a.progress.Errors.Load()
		st.Expected = a.expected
		if a.expected > 0 {
			st.Percent = min(100, 100*float64(st.Entries)/float64(a.expected))
		}
	}
	a.scanMu.Unlock()
	a.View(func(ix *index.Index) error {
		info := ix.Info
		st.Index = &info
		return nil
	})
	return st
}

// StartScan begins a scan in the background.
func (a *App) StartScan() error {
	a.scanMu.Lock()
	if a.scanning {
		a.scanMu.Unlock()
		return ErrScanRunning
	}
	a.scanMu.Unlock()
	go a.Scan(a.bg)
	return nil
}

// Scan walks every root, builds a new index and switches to it.
func (a *App) Scan(ctx context.Context) (index.Info, error) {
	a.scanMu.Lock()
	if a.scanning {
		a.scanMu.Unlock()
		return index.Info{}, ErrScanRunning
	}
	prog := &scan.Progress{}
	a.scanning, a.progress, a.started, a.expected, a.lastErr = true, prog, time.Now(), 0, ""
	a.scanMu.Unlock()

	info, warnings, err := a.scanOnce(ctx, prog)

	a.scanMu.Lock()
	a.scanning = false
	if err != nil {
		a.lastErr = err.Error()
	} else {
		a.warnings = warnings
	}
	a.scanMu.Unlock()
	return info, err
}

func (a *App) scanOnce(ctx context.Context, prog *scan.Progress) (index.Info, []string, error) {
	started := time.Now()
	var table *storage.Table
	if mounts, err := storage.Mounts(); err == nil {
		table = storage.NewTable(mounts)
	}

	var datasets []index.Dataset
	rootFS := make(map[string]bool)
	if table != nil {
		seen := make(map[uint64]bool)
		var expected int64
		add := func(m storage.Mount, counts bool) {
			if seen[m.Dev] {
				return
			}
			seen[m.Dev] = true
			datasets = append(datasets, index.Dataset{Dev: m.Dev, Name: m.Source, Mountpoint: m.MountPoint, FSType: m.FSType})
			if counts {
				if u, err := storage.StatUsage(m.MountPoint); err == nil {
					expected += u.Objects
				}
			}
		}
		known := true
		for _, root := range a.cfg.Roots {
			if m, ok := table.Containing(root); ok {
				rootFS[m.FSType] = true
				// A root that is only part of a filesystem says nothing
				// about how many files the scan will find.
				known = known && m.MountPoint == root
				add(m, m.MountPoint == root)
			}
			for _, m := range table.Under(root) {
				add(m, true)
			}
		}
		if known {
			a.scanMu.Lock()
			a.expected = expected
			a.scanMu.Unlock()
		}
	}

	b, err := index.NewBuilder(a.indexDir(), started)
	if err != nil {
		return index.Info{}, nil, err
	}
	res, err := scan.Walk(ctx, scan.Options{
		Roots:   a.cfg.Roots,
		Workers: a.cfg.Workers,
		// The data directory often lives on the pool being scanned; indexing
		// our own index would only add noise.
		Exclude:       append(slices.Clone(a.cfg.Exclude), a.cfg.DataDir),
		ShareMetaDirs: []string{meta.Dir},
		Progress:      prog,
		// Follow the pool into its child datasets, but not into unrelated
		// filesystems that happen to be mounted inside it.
		CrossMount: func(dev uint64) bool {
			if table == nil {
				return true
			}
			m, ok := table.ByDev(dev)
			return !ok || rootFS[m.FSType]
		},
	}, b)
	if err != nil {
		b.Abort()
		return index.Info{}, nil, err
	}
	info := index.Info{
		Started: started, Finished: time.Now(), Roots: a.cfg.Roots,
		Files: res.Files, Dirs: res.Dirs, Size: res.Size, Disk: res.Disk,
		Hardlinked: res.Hardlinked, Errors: res.Errors,
	}
	path, err := b.Finish(info, datasets)
	if err != nil {
		return index.Info{}, nil, err
	}
	ix, err := index.Open(path)
	if err != nil {
		return index.Info{}, nil, err
	}
	warnings := a.reconcile(ctx, ix)

	a.ixMu.Lock()
	old := a.ix
	a.ix = ix
	a.ixMu.Unlock()
	if old != nil {
		old.Close()
	}
	index.Prune(a.indexDir(), a.cfg.KeepIndexes)
	return info, warnings, nil
}

// DatasetReport is one filesystem under the scan roots, with ZFS's own
// accounting when that is available.
type DatasetReport struct {
	Name       string `json:"name"`
	Mountpoint string `json:"mountpoint"`
	FSType     string `json:"fstype"`
	storage.Usage
	ZFS       *storage.ZFSDataset `json:"zfs,omitempty"`
	Snapshots int                 `json:"snapshots"`
}

type StorageReport struct {
	Datasets []DatasetReport `json:"datasets"`
	// ZFSOrigin is "zfs", "file" or "none"; see storage.ZFSListing.
	ZFSOrigin string     `json:"zfs_origin"`
	ZFSAsOf   *time.Time `json:"zfs_as_of,omitempty"`
	ZFSError  string     `json:"zfs_error,omitempty"`
}

const zfsCacheFor = 5 * time.Minute

// Storage reports the live state of the filesystems under the scan roots.
func (a *App) Storage(ctx context.Context) (StorageReport, error) {
	mounts, err := storage.Mounts()
	if err != nil {
		return StorageReport{}, err
	}
	table := storage.NewTable(mounts)

	a.zfsMu.Lock()
	if a.zfsAt.IsZero() || time.Since(a.zfsAt) > zfsCacheFor {
		a.zfs, a.zfsAt = storage.ListZFS(ctx, a.cfg.ZFSListFile), time.Now()
	}
	listing := a.zfs
	a.zfsMu.Unlock()

	byName := make(map[string]*storage.ZFSDataset)
	snapshots := make(map[string]int)
	for i := range listing.Datasets {
		d := &listing.Datasets[i]
		if d.Type == "snapshot" {
			name, _, _ := strings.Cut(d.Name, "@")
			snapshots[name]++
		} else {
			byName[d.Name] = d
		}
	}

	rep := StorageReport{Datasets: []DatasetReport{}, ZFSOrigin: listing.Origin, ZFSError: listing.Err}
	if !listing.AsOf.IsZero() {
		rep.ZFSAsOf = &listing.AsOf
	}
	seen := make(map[uint64]bool)
	add := func(m storage.Mount) {
		if seen[m.Dev] {
			return
		}
		seen[m.Dev] = true
		d := DatasetReport{Name: m.Source, Mountpoint: m.MountPoint, FSType: m.FSType}
		d.Usage, _ = storage.StatUsage(m.MountPoint)
		if m.FSType == "zfs" {
			d.ZFS = byName[m.Source]
			d.Snapshots = snapshots[m.Source]
		}
		rep.Datasets = append(rep.Datasets, d)
	}
	for _, root := range a.cfg.Roots {
		if m, ok := table.Containing(root); ok {
			add(m)
		}
		for _, m := range table.Under(root) {
			add(m)
		}
	}
	return rep, nil
}

// Prefix is one entry in the user's list of prefixes.
type Prefix struct {
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
}

func (a *App) Prefixes() ([]Prefix, error) {
	rows, err := a.db.Query(`SELECT name, meaning FROM prefixes ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Prefix{}
	for rows.Next() {
		var p Prefix
		if err := rows.Scan(&p.Name, &p.Meaning); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPrefixes replaces the prefix list. Removing a prefix from the list does
// not strip it from items that already carry it.
func (a *App) SetPrefixes(list []Prefix) error {
	seen := make(map[string]bool)
	for i := range list {
		p := &list[i]
		p.Name, p.Meaning = strings.TrimSpace(p.Name), strings.TrimSpace(p.Meaning)
		if err := validPrefix(p.Name); err != nil {
			return err
		}
		if seen[p.Name] {
			return InputError(fmt.Sprintf("prefix %q is listed twice", p.Name))
		}
		seen[p.Name] = true
		if len(p.Meaning) > 500 {
			return InputError(fmt.Sprintf("the meaning of prefix %q is longer than 500 characters", p.Name))
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM prefixes`); err != nil {
		return err
	}
	for i, p := range list {
		if _, err := tx.Exec(`INSERT INTO prefixes(name, meaning, position) VALUES(?,?,?)`, p.Name, p.Meaning, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validPrefix(name string) error {
	if name == "" || len(name) > 32 {
		return InputError("a prefix must be 1 to 32 characters")
	}
	for _, r := range name {
		// "|" separates prefixes in the index's annotation cache.
		if r <= ' ' || r == '|' || r == 0x7f {
			return InputError(fmt.Sprintf("prefix %q may not contain spaces, control characters or |", name))
		}
	}
	return nil
}
