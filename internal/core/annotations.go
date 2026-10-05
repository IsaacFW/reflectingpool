package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/IsaacFW/reflectingpool/internal/index"
	"github.com/IsaacFW/reflectingpool/internal/meta"
)

// AnnotationInput is what a user can record about an item.
type AnnotationInput struct {
	Note        string   `json:"note"`
	DisplayName string   `json:"display_name"`
	Prefixes    []string `json:"prefixes"`
	Owner       string   `json:"owner"`
	ReviewAfter string   `json:"review_after"`
	Skipped     bool     `json:"skipped"`
}

func (a *App) validate(in *AnnotationInput) error {
	in.Note = strings.TrimSpace(in.Note)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	in.Owner = strings.TrimSpace(in.Owner)
	in.ReviewAfter = strings.TrimSpace(in.ReviewAfter)
	switch {
	case len(in.Note) > 10_000:
		return InputError("the note is longer than 10,000 characters")
	case len(in.DisplayName) > 255 || strings.ContainsAny(in.DisplayName, "/\x00\n"):
		return InputError("a display name must be at most 255 characters on one line, without /")
	case len(in.Owner) > 100:
		return InputError("the owner is longer than 100 characters")
	}
	if in.ReviewAfter != "" {
		if _, err := time.Parse("2006-01-02", in.ReviewAfter); err != nil {
			return InputError("the review date must look like 2027-01-31")
		}
	}
	known, err := a.Prefixes()
	if err != nil {
		return err
	}
	var prefixes []string
	for _, p := range in.Prefixes {
		if !slices.ContainsFunc(known, func(k Prefix) bool { return k.Name == p }) {
			return InputError(fmt.Sprintf("prefix %q is not in the prefix list", p))
		}
		if !slices.Contains(prefixes, p) {
			prefixes = append(prefixes, p)
		}
	}
	in.Prefixes = prefixes
	return nil
}

func stateOf(a meta.Annotation) int {
	state := 0
	if a.Note != "" {
		state |= index.StateNote
	}
	if len(a.Prefixes) > 0 {
		state |= index.StatePrefix
	}
	if a.Skipped {
		state |= index.StateSkipped
	}
	if a.DisplayName != "" {
		state |= index.StateDisplayName
	}
	return state
}

// target is an indexed item together with where its annotation is kept.
type target struct {
	row       index.Row
	path      string
	sharePath string
	rel       string // path within the share; "." for the share itself
}

func (a *App) target(ctx context.Context, ix *index.Index, id int64) (target, error) {
	row, err := ix.Entry(ctx, id)
	if err != nil {
		return target{}, err
	}
	if row.Share == 0 {
		return target{}, ErrNoShare
	}
	t := target{row: row}
	if t.path, err = ix.EntryPath(ctx, id); err != nil {
		return target{}, err
	}
	if t.sharePath, err = ix.EntryPath(ctx, row.Share); err != nil {
		return target{}, err
	}
	t.rel = relTo(t.sharePath, t.path)
	return t, nil
}

func relTo(sharePath, path string) string {
	if path == sharePath {
		return "."
	}
	return strings.TrimPrefix(path, sharePath+"/")
}

// absPath resolves an annotation's path. The file can be edited by hand, so a
// path that climbs out of the share is treated as pointing nowhere.
func absPath(sharePath, rel string) (string, bool) {
	clean := filepath.Clean(rel)
	if clean == "." {
		return sharePath, true
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return sharePath + "/" + clean, true
}

func datasetNames(ctx context.Context, ix *index.Index) (byDev map[uint64]string, byName map[string][]uint64, err error) {
	datasets, err := ix.Datasets(ctx)
	if err != nil {
		return nil, nil, err
	}
	byDev, byName = make(map[uint64]string), make(map[string][]uint64)
	for _, d := range datasets {
		byDev[d.Dev] = d.Name
		byName[d.Name] = append(byName[d.Name], d.Dev)
	}
	return byDev, byName, nil
}

// Annotation returns what is recorded about an item, if anything.
func (a *App) Annotation(ctx context.Context, id int64) (an meta.Annotation, found bool, err error) {
	err = a.View(func(ix *index.Index) error {
		t, err := a.target(ctx, ix, id)
		if errors.Is(err, ErrNoShare) {
			return nil
		}
		if err != nil {
			return err
		}
		an, found, err = a.store(t.sharePath).Get(t.rel)
		return err
	})
	return an, found, err
}

// PutAnnotation records or replaces an item's annotation.
func (a *App) PutAnnotation(ctx context.Context, id int64, in AnnotationInput) (meta.Annotation, error) {
	if a.cfg.ReadOnly {
		return meta.Annotation{}, ErrReadOnly
	}
	if err := a.validate(&in); err != nil {
		return meta.Annotation{}, err
	}
	var an meta.Annotation
	err := a.View(func(ix *index.Index) error {
		t, err := a.target(ctx, ix, id)
		if err != nil {
			return err
		}
		ident, err := a.liveIdentity(ctx, ix, t)
		if err != nil {
			return err
		}
		an = meta.Annotation{
			Path: t.rel, Kind: t.row.Kind,
			Note: in.Note, DisplayName: in.DisplayName, Prefixes: in.Prefixes,
			Owner: in.Owner, ReviewAfter: in.ReviewAfter, Skipped: in.Skipped,
			Identity: ident, Updated: time.Now().UTC().Truncate(time.Second),
		}
		if err := a.store(t.sharePath).Put(an); err != nil {
			return err
		}
		return ix.SetAnnot(ctx, index.Annot{Entry: id, State: stateOf(an), Prefixes: an.Prefixes})
	})
	return an, err
}

// DeleteAnnotation removes an item's annotation, returning it to review queues.
func (a *App) DeleteAnnotation(ctx context.Context, id int64) error {
	if a.cfg.ReadOnly {
		return ErrReadOnly
	}
	return a.View(func(ix *index.Index) error {
		t, err := a.target(ctx, ix, id)
		if err != nil {
			return err
		}
		if err := a.store(t.sharePath).Delete(t.rel); err != nil {
			return err
		}
		return ix.ClearAnnot(ctx, id)
	})
}

// liveIdentity reads the item's identity from disk as it is now, which may be
// newer than the scan.
func (a *App) liveIdentity(ctx context.Context, ix *index.Index, t target) (meta.Identity, error) {
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, t.path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &stx)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return meta.Identity{}, ErrGone
	}
	if err != nil {
		return meta.Identity{}, err
	}
	isDir := stx.Mode&unix.S_IFMT == unix.S_IFDIR
	if isDir != t.row.IsDir() {
		return meta.Identity{}, ErrGone
	}
	byDev, _, err := datasetNames(ctx, ix)
	if err != nil {
		return meta.Identity{}, err
	}
	id := meta.Identity{Dataset: byDev[unix.Mkdev(stx.Dev_major, stx.Dev_minor)], Ino: stx.Ino}
	if stx.Mask&unix.STATX_BTIME != 0 {
		id.Btime, id.BtimeNs = stx.Btime.Sec, int64(stx.Btime.Nsec)
	}
	if stx.Mode&unix.S_IFMT == unix.S_IFREG {
		id.Size, id.Mtime = int64(stx.Size), stx.Mtime.Sec
		// Without a hash the annotation still follows renames; it only
		// loses the ability to follow a copy to another filesystem.
		id.Hash, _ = a.fingerprint(t.path)
	}
	return id, nil
}

func (a *App) fingerprint(path string) (string, error) {
	f, err := a.openFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	return meta.Fingerprint(f, st.Size())
}

// OpenContent opens a regular file from the index for reading, for previews.
func (a *App) OpenContent(ctx context.Context, id int64) (*os.File, index.Row, error) {
	var f *os.File
	var row index.Row
	err := a.View(func(ix *index.Index) error {
		var err error
		if row, err = ix.Entry(ctx, id); err != nil {
			return err
		}
		if row.Kind != "file" {
			return ErrNotFile
		}
		path, err := ix.EntryPath(ctx, id)
		if err != nil {
			return err
		}
		f, err = a.openFile(path)
		return err
	})
	return f, row, err
}

// ShareInfo is a share with a summary of how much of it is annotated.
type ShareInfo struct {
	index.Row
	Annotated int `json:"annotated"`
	Skipped   int `json:"skipped"`
	Orphaned  int `json:"orphaned"`
	// CoveredBytes counts everything inside annotated items; an annotated
	// folder covers all it contains.
	CoveredBytes int64  `json:"covered_bytes"`
	Error        string `json:"error,omitempty"`
}

func (a *App) Shares(ctx context.Context) ([]ShareInfo, error) {
	out := []ShareInfo{}
	err := a.View(func(ix *index.Index) error {
		shares, err := ix.Shares(ctx)
		if err != nil {
			return err
		}
		if err := ix.FillPaths(ctx, shares); err != nil {
			return err
		}
		for _, sh := range shares {
			info := ShareInfo{Row: sh}
			all, err := a.store(sh.Path).All()
			if err != nil {
				info.Error = err.Error()
			}
			var covering []string
			for _, an := range all { // ordered by path, so a folder precedes its contents
				switch {
				case an.Orphaned:
					info.Orphaned++
					continue
				case stateOf(an)&^index.StateSkipped == 0:
					if an.Skipped {
						info.Skipped++
					}
					continue
				}
				info.Annotated++
				if an.Path == "." || slices.ContainsFunc(covering, func(c string) bool { return strings.HasPrefix(an.Path, c+"/") }) {
					continue
				}
				abs, ok := absPath(sh.Path, an.Path)
				if !ok {
					continue
				}
				if row, err := ix.Lookup(ctx, abs); err == nil {
					info.CoveredBytes += row.Size
					covering = append(covering, an.Path)
				}
			}
			out = append(out, info)
		}
		return nil
	})
	return out, err
}

// ShareAnnotations lists everything recorded in a share, including
// annotations whose item has gone missing.
func (a *App) ShareAnnotations(ctx context.Context, shareID int64) ([]meta.Annotation, error) {
	var out []meta.Annotation
	err := a.View(func(ix *index.Index) error {
		row, err := ix.Entry(ctx, shareID)
		if err != nil {
			return err
		}
		if row.ID != row.Share {
			return index.ErrNotFound
		}
		path, err := ix.EntryPath(ctx, shareID)
		if err != nil {
			return err
		}
		out, err = a.store(path).All()
		return err
	})
	return out, err
}

type reconciler struct {
	a       *App
	ctx     context.Context
	ix      *index.Index
	byDev   map[uint64]string
	byName  map[string][]uint64
	claimed map[int64]bool // entries already matched to an annotation
}

// reconcile matches every stored annotation to an entry in a new index,
// following items that were renamed or moved since the last scan, and rebuilds
// the index's annotation cache. Problems are returned as warnings: a damaged
// annotation file in one share must not cost the user the scan.
func (a *App) reconcile(ctx context.Context, ix *index.Index) (warnings []string) {
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	shares, err := ix.Shares(ctx)
	if err != nil {
		warn("annotations were not loaded: %v", err)
		return
	}
	byDev, byName, err := datasetNames(ctx, ix)
	if err != nil {
		warn("annotations were not loaded: %v", err)
		return
	}
	r := &reconciler{a: a, ctx: ctx, ix: ix, byDev: byDev, byName: byName, claimed: make(map[int64]bool)}

	type item struct {
		sharePath string
		an        meta.Annotation
		row       index.Row
		found     bool
	}
	var items []*item
	sharePaths := make(map[int64]string)
	for _, sh := range shares {
		path, err := ix.EntryPath(ctx, sh.ID)
		if err != nil {
			warn("%s: %v", sh.Name, err)
			continue
		}
		sharePaths[sh.ID] = path
		all, err := a.store(path).All()
		if err != nil {
			warn("annotations in %s could not be read: %v", path, err)
			continue
		}
		for _, an := range all {
			items = append(items, &item{sharePath: path, an: an})
		}
	}

	// Identity matches go first across all shares, so that when one item has
	// taken another's old name, the annotation that truly belongs to the
	// file claims it before a path-only match can.
	for _, it := range items {
		if row, ok := r.byIdentity(it.sharePath, it.an); ok {
			it.row, it.found, r.claimed[row.ID] = row, true, true
		}
	}
	for _, it := range items {
		if it.found {
			continue
		}
		if row, ok := r.byPathOrContent(it.sharePath, it.an); ok {
			it.row, it.found, r.claimed[row.ID] = row, true, true
		}
	}

	type edit struct {
		remove []string
		put    []meta.Annotation
	}
	edits := make(map[string]*edit)
	editFor := func(sharePath string) *edit {
		if edits[sharePath] == nil {
			edits[sharePath] = &edit{}
		}
		return edits[sharePath]
	}
	var cache []index.Annot
	for _, it := range items {
		next, dest := it.an, it.sharePath
		destPath, inShare := sharePaths[it.row.Share]
		if !it.found || !inShare {
			next.Orphaned = true
		} else {
			rowPath, err := ix.EntryPath(ctx, it.row.ID)
			if err != nil {
				warn("%s: %v", it.an.Path, err)
				continue
			}
			dest = destPath
			next.Orphaned = false
			next.Path = relTo(dest, rowPath)
			next.Kind = it.row.Kind
			next.Identity = r.identity(it.row, it.an.Identity, rowPath)
			cache = append(cache, index.Annot{Entry: it.row.ID, State: stateOf(next), Prefixes: next.Prefixes})
		}
		if dest == it.sharePath && reflect.DeepEqual(it.an, next) {
			continue
		}
		editFor(it.sharePath).remove = append(editFor(it.sharePath).remove, it.an.Path)
		editFor(dest).put = append(editFor(dest).put, next)
	}

	if !a.cfg.ReadOnly {
		for sharePath, e := range edits {
			// Found items are written before orphans so that an orphan
			// never displaces the annotation of an item that exists.
			sort.SliceStable(e.put, func(i, j int) bool { return !e.put[i].Orphaned && e.put[j].Orphaned })
			err := a.store(sharePath).Update(func(m map[string]meta.Annotation) bool {
				for _, p := range e.remove {
					delete(m, p)
				}
				for _, an := range e.put {
					if cur, taken := m[an.Path]; taken {
						switch {
						case an.Orphaned:
							an.Path += " (orphaned)"
						case cur.Orphaned:
							cur.Path += " (orphaned)"
							m[cur.Path] = cur
						}
					}
					m[an.Path] = an
				}
				return true
			})
			if err != nil {
				warn("annotations in %s could not be updated: %v", sharePath, err)
			}
		}
	}
	if err := ix.ReplaceAnnots(ctx, cache); err != nil {
		warn("annotations were not loaded: %v", err)
	}
	return warnings
}

// same reports whether an indexed entry is the item an annotation was made on.
func (r *reconciler) same(row index.Row, an meta.Annotation) bool {
	id := an.Identity
	if row.Kind != an.Kind || row.Ino != id.Ino {
		return false
	}
	if id.Dataset != "" && r.byDev[row.Dev] != id.Dataset {
		return false
	}
	if id.Btime != 0 && row.Btime != 0 {
		return id.Btime == row.Btime && id.BtimeNs == row.BtimeNsec
	}
	// Without a creation time, an inode number alone could be one that was
	// freed and reused by a different file.
	if row.Kind == "file" {
		return id.Size == row.Size && id.Mtime == row.Mtime
	}
	return true
}

func (r *reconciler) byIdentity(sharePath string, an meta.Annotation) (index.Row, bool) {
	if an.Identity.Ino == 0 {
		return index.Row{}, false
	}
	if abs, ok := absPath(sharePath, an.Path); ok {
		if row, err := r.ix.Lookup(r.ctx, abs); err == nil && r.same(row, an) && !r.claimed[row.ID] {
			return row, true
		}
	}
	for _, dev := range r.byName[an.Identity.Dataset] {
		rows, err := r.ix.ByInode(r.ctx, dev, an.Identity.Ino)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if r.same(row, an) && !r.claimed[row.ID] {
				return row, true
			}
		}
	}
	return index.Row{}, false
}

// byPathOrContent handles what identity cannot: a file replaced in place keeps
// its annotation by path, and a file copied to another filesystem is
// recognised by its size, modified time and fingerprint.
func (r *reconciler) byPathOrContent(sharePath string, an meta.Annotation) (index.Row, bool) {
	if abs, ok := absPath(sharePath, an.Path); ok {
		if row, err := r.ix.Lookup(r.ctx, abs); err == nil && row.Kind == an.Kind && !r.claimed[row.ID] {
			return row, true
		}
	}
	if an.Kind != "file" || an.Identity.Hash == "" {
		return index.Row{}, false
	}
	candidates, err := r.ix.SameSizeAndTime(r.ctx, an.Identity.Size, an.Identity.Mtime, 50)
	if err != nil {
		return index.Row{}, false
	}
	for _, row := range candidates {
		if r.claimed[row.ID] {
			continue
		}
		path, err := r.ix.EntryPath(r.ctx, row.ID)
		if err != nil {
			continue
		}
		if hash, err := r.a.fingerprint(path); err == nil && hash == an.Identity.Hash {
			return row, true
		}
	}
	return index.Row{}, false
}

func (r *reconciler) identity(row index.Row, old meta.Identity, path string) meta.Identity {
	id := meta.Identity{Dataset: r.byDev[row.Dev], Ino: row.Ino, Btime: row.Btime, BtimeNs: row.BtimeNsec}
	if row.Kind == "file" {
		id.Size, id.Mtime, id.Hash = row.Size, row.Mtime, old.Hash
		if old.Hash == "" || old.Size != row.Size || old.Mtime != row.Mtime {
			id.Hash, _ = r.a.fingerprint(path)
		}
	}
	return id
}
