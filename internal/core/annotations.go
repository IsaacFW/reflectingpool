package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/IsaacFW/reflectingpool/internal/preview"
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
	err = a.viewAt(ctx, func(ix *index.Index) error {
		t, err := a.target(ctx, ix, id)
		if errors.Is(err, ErrNoShare) {
			return nil
		}
		if err != nil {
			return err
		}
		an, found, err = a.store(t.sharePath).Get(t.rel)
		if isSuspect(err) {
			// Browsing goes on when a share's notes cannot be read; the share
			// listing and the scan warnings say what is wrong with them.
			an, found, err = meta.Annotation{}, false, nil
		}
		if err == nil && !found && t.row.State&index.StateSkipped != 0 {
			an, found = meta.Annotation{Path: t.rel, Kind: t.row.Kind, Skipped: true}, true
		}
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
	a.writeMu.RLock()
	defer a.writeMu.RUnlock()
	// Saving a form with nothing in it is how an item is passed over.
	blank := in.Note == "" && in.DisplayName == "" && len(in.Prefixes) == 0 && in.Owner == "" && in.ReviewAfter == ""
	var an meta.Annotation
	err := a.viewAt(ctx, func(ix *index.Index) error {
		t, err := a.target(ctx, ix, id)
		if err != nil {
			return err
		}
		store := a.store(t.sharePath)
		now := time.Now().UTC().Truncate(time.Second)
		if blank {
			// A skip records nothing about the item, so nothing is read
			// from it. An item that has left the disk can still be passed
			// over, and does not sit at the head of a queue until the next
			// scan.
			an = meta.Annotation{Path: t.rel, Kind: t.row.Kind, Skipped: true, Updated: now}
			if err := store.Delete(t.rel); err != nil { // whatever was recorded before is withdrawn
				return err
			}
			if t.row.State != index.StateSkipped {
				if err := store.AddSkips([]string{t.rel}); err != nil {
					return err
				}
			}
			return ix.SetAnnot(ctx, index.Annot{Entry: id, State: index.StateSkipped})
		}
		ident, err := a.liveIdentity(ctx, ix, t)
		if err != nil {
			return err
		}
		an = meta.Annotation{
			Path: t.rel, Kind: t.row.Kind,
			Note: in.Note, DisplayName: in.DisplayName, Prefixes: in.Prefixes,
			Owner: in.Owner, ReviewAfter: in.ReviewAfter,
			Identity: ident, Updated: now,
		}
		// If the item was skipped before, its line in the skipped list is
		// left behind. It does no harm: an annotation outranks a skip, and
		// the next scan tidies the list.
		if err := store.Put(an); err != nil {
			return err
		}
		return ix.SetAnnot(ctx, index.Annot{Entry: id, State: stateOf(an), Prefixes: an.Prefixes})
	})
	return an, writeFailure(err)
}

// skipChunk bounds the memory a bulk skip uses, however large the group. It
// is a variable so that a test can make a small group take several rounds.
var skipChunk = 10_000

// SkipGroup skips every unreviewed item left in one group of a review queue
// and reports how many there were. group names the group by one value for
// each of the queue's group keys.
func (a *App) SkipGroup(ctx context.Context, spec index.QueueSpec, group []any) (int64, error) {
	if a.cfg.ReadOnly {
		return 0, ErrReadOnly
	}
	a.writeMu.RLock()
	defer a.writeMu.RUnlock()
	var total int64
	err := a.viewAt(ctx, func(ix *index.Index) error {
		pending, err := ix.PendingInGroup(ctx, spec, group)
		if err != nil {
			return err
		}
		sharePaths := make(map[int64]string)
		for ids := range slices.Chunk(pending, skipChunk) {
			rows, err := ix.Rows(ctx, ids)
			if err != nil {
				return err
			}
			if err := ix.FillPaths(ctx, rows); err != nil {
				return err
			}
			byShare := make(map[string][]string)
			for _, row := range rows {
				sharePath, ok := sharePaths[row.Share]
				if !ok {
					if sharePath, err = ix.EntryPath(ctx, row.Share); err != nil {
						return err
					}
					sharePaths[row.Share] = sharePath
				}
				byShare[sharePath] = append(byShare[sharePath], relTo(sharePath, row.Path))
			}
			// The list in the share is written first: it is the record, and
			// the index only a cache of it.
			for sharePath, rels := range byShare {
				if err := a.store(sharePath).AddSkips(rels); err != nil {
					return err
				}
			}
			added, err := ix.MarkSkipped(ctx, spec, group, ids)
			if err != nil {
				return err
			}
			total += added
		}
		return nil
	})
	return total, writeFailure(err)
}

// DeleteAnnotation removes an item's annotation, returning it to review queues.
func (a *App) DeleteAnnotation(ctx context.Context, id int64) error {
	if a.cfg.ReadOnly {
		return ErrReadOnly
	}
	a.writeMu.RLock()
	defer a.writeMu.RUnlock()
	return writeFailure(a.viewAt(ctx, func(ix *index.Index) error {
		t, err := a.target(ctx, ix, id)
		if err != nil {
			return err
		}
		store := a.store(t.sharePath)
		if err := store.Delete(t.rel); err != nil {
			return err
		}
		// A skip of the same item goes too, including one left behind when
		// the item was annotated later, or the next scan would bring it back.
		if err := store.RemoveSkip(t.rel); err != nil {
			return err
		}
		return ix.ClearAnnot(ctx, id)
	}))
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
	err := a.viewAt(ctx, func(ix *index.Index) error {
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
	CoveredBytes int64 `json:"covered_bytes"`
	// Writable is false when nothing can be recorded in the share: the
	// program is in read-only mode, or the share is mapped read-only.
	Writable bool `json:"writable"`
	// Problem says why nothing can be recorded when the cause is something
	// in the share's .reflection folder that this program did not make: a
	// link, a pipe, a file of someone else's.
	Problem string `json:"problem,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (a *App) Shares(ctx context.Context) ([]ShareInfo, error) {
	out := []ShareInfo{}
	err := a.View(func(ix *index.Index) error {
		shares, err := ix.Shares(ctx)
		if err != nil {
			return err
		}
		if err := ix.FillTypes(ctx, shares); err != nil {
			return err
		}
		if err := ix.FillPaths(ctx, shares); err != nil {
			return err
		}
		skipped, err := ix.SkippedByShare(ctx)
		if err != nil {
			return err
		}
		for _, sh := range shares {
			info := ShareInfo{Row: sh, Skipped: int(skipped[sh.ID])}
			info.Writable = !a.cfg.ReadOnly && unix.Access(sh.Path, unix.W_OK) == nil
			if err := a.store(sh.Path).Check(); isSuspect(err) {
				info.Problem, info.Writable = unsafe(sh.Path, err).Error(), false
			}
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
	failed  error          // set when the index could not be asked about a skipped item
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
	// Skips were once kept among the annotations, as records with nothing in
	// them. They move to the skipped list of the share the item is in now.
	type oldSkip struct{ from, path, to, rel string }
	var oldSkips []oldSkip
	var cache []index.Annot
	for _, it := range items {
		next, dest := it.an, it.sharePath
		next.Skipped = false
		empty := stateOf(next) == 0 && next.Owner == "" && next.ReviewAfter == ""
		destPath, inShare := sharePaths[it.row.Share]
		if !it.found || !inShare {
			if empty {
				// A skip of something that is gone records nothing at all.
				editFor(it.sharePath).remove = append(editFor(it.sharePath).remove, it.an.Path)
				continue
			}
			next.Orphaned = true
		} else {
			rowPath, err := ix.EntryPath(ctx, it.row.ID)
			if err != nil {
				warn("%s: %v", it.an.Path, err)
				continue
			}
			dest = destPath
			if empty {
				delete(r.claimed, it.row.ID) // the skipped list claims it below
				oldSkips = append(oldSkips, oldSkip{it.sharePath, it.an.Path, dest, relTo(dest, rowPath)})
				continue
			}
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

	// The skipped lists. A skip is kept while its item is where it was and
	// carries no annotation; one whose item was renamed or deleted is
	// dropped, which returns a renamed item to the review queues.
	unsaved := make(map[string]bool)
	for _, sh := range shares {
		sharePath, ok := sharePaths[sh.ID]
		if !ok {
			continue
		}
		keep := func(listed []string) []string {
			asFound := slices.Clone(listed)
			for _, old := range oldSkips {
				if old.to == sharePath {
					listed = append(listed, old.rel)
				}
			}
			// A bulk skip lists most of what a folder holds, so the items
			// are looked up a folder at a time.
			found := map[string]int64{".": sh.ID}
			inDir := make(map[string][]string)
			for i, rel := range listed {
				rel = filepath.Clean(rel)
				listed[i] = rel
				if rel != "." {
					dir, name := filepath.Split(rel)
					inDir[filepath.Clean(dir)] = append(inDir[filepath.Clean(dir)], name)
				}
			}
			dirs := map[string]int64{".": sh.ID}
			for dir, names := range inDir {
				parent := r.inShare(dirs, dir)
				if parent == 0 {
					continue
				}
				ids, err := ix.ChildIDs(ctx, parent, names)
				if err != nil {
					r.failed = err
					break
				}
				for name, id := range ids {
					found[filepath.Join(dir, name)] = id
				}
			}
			if r.failed != nil {
				// "Not found" must mean the item is gone, never that the
				// index could not be asked: leave the list as it was.
				warn("skipped items in %s could not be matched to the index: %v", sharePath, r.failed)
				r.failed, unsaved[sharePath] = nil, true
				return asFound
			}
			kept := make([]string, 0, len(listed))
			for _, rel := range listed {
				id := found[rel]
				if id == 0 || r.claimed[id] {
					continue
				}
				r.claimed[id] = true
				kept = append(kept, rel)
				cache = append(cache, index.Annot{Entry: id, State: index.StateSkipped})
			}
			return kept
		}
		store := a.store(sharePath)
		if a.cfg.ReadOnly {
			listed, err := store.Skips()
			if err != nil {
				warn("skipped items in %s could not be read: %v", sharePath, err)
				continue
			}
			keep(listed)
		} else if err := store.EditSkips(keep); err != nil {
			warn("skipped items in %s could not be updated: %v", sharePath, err)
			unsaved[sharePath] = true
		}
	}
	for _, old := range oldSkips {
		// The old record goes only once the skip is safely in its new place.
		if !unsaved[old.to] {
			editFor(old.from).remove = append(editFor(old.from).remove, old.path)
		}
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

// inShare returns the ID of the entry at a path inside a share, or 0 if there
// is none. dirs remembers the folders found so far, so that a long list of
// items costs one lookup each and not one for every folder on the way down.
func (r *reconciler) inShare(dirs map[string]int64, rel string) int64 {
	if id, ok := dirs[rel]; ok {
		return id
	}
	// The list can be edited by hand: a path that climbs out of the share
	// points nowhere.
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return 0
	}
	dir, name := filepath.Split(rel)
	parent, ok := dirs[filepath.Clean(dir)]
	if !ok {
		parent = r.inShare(dirs, filepath.Clean(dir))
		dirs[filepath.Clean(dir)] = parent
	}
	if parent == 0 {
		return 0
	}
	id, err := r.ix.ChildID(r.ctx, parent, name)
	if err != nil {
		if !errors.Is(err, index.ErrNotFound) {
			r.failed = err
		}
		return 0
	}
	return id
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

// DeleteMissingAnnotation removes a note whose item was not found at the
// last scan. Such a note has no entry to remove it through. A note whose
// item exists is left alone: that one is removed from the item itself.
func (a *App) DeleteMissingAnnotation(ctx context.Context, shareID int64, path string) error {
	if a.cfg.ReadOnly {
		return ErrReadOnly
	}
	a.writeMu.RLock()
	defer a.writeMu.RUnlock()
	return writeFailure(a.viewAt(ctx, func(ix *index.Index) error {
		share, err := ix.Entry(ctx, shareID)
		if err != nil {
			return err
		}
		if share.ID != share.Share {
			return index.ErrNotFound
		}
		sharePath, err := ix.EntryPath(ctx, shareID)
		if err != nil {
			return err
		}
		store := a.store(sharePath)
		an, found, err := store.Get(path)
		if err != nil {
			return err
		}
		if !found {
			return index.ErrNotFound
		}
		if !an.Orphaned {
			return InputError("this note belongs to an item that still exists; remove it from the item")
		}
		return store.Delete(path)
	}))
}

// ShareReadOnlyError reports that nothing can be written into a share,
// usually because the pool was mapped into the container read-only.
type ShareReadOnlyError struct {
	Path string // the share, or "" when it could not be told from the failure
}

func (e *ShareReadOnlyError) Error() string {
	where := "this share"
	if e.Path != "" {
		where = e.Path
	}
	return where + " cannot be written to from inside the container, so nothing can be recorded there. " +
		"Give the path read-write access (in Unraid: Read/Write - Slave), " +
		"or set RP_READ_ONLY=1 to run the whole program read-only."
}

// writeFailure gives a failed write to a share's metadata folder a message a
// person can act on. Without it, a pool mapped read-only shows as an internal
// error whose cause is only in the log.
func writeFailure(err error) error {
	if isSuspect(err) {
		var sus *meta.Suspect
		errors.As(err, &sus)
		share, _, _ := strings.Cut(sus.Path, "/"+meta.Dir)
		return unsafe(share, err)
	}
	var pe *fs.PathError
	if err == nil || !errors.As(err, &pe) {
		return err
	}
	share, _, inMeta := strings.Cut(pe.Path, "/"+meta.Dir)
	if !inMeta || !(errors.Is(err, unix.EROFS) || errors.Is(err, fs.ErrPermission)) {
		return err
	}
	return &ShareReadOnlyError{Path: share}
}

// previewCache is how much room generated previews may take in the data
// directory before the ones used longest ago are cleared out.
const previewCache = 1 << 30

// Preview returns the path of a picture of a file a browser cannot show by
// itself: stills for a film, a JPEG for a HEIC or RAW photo.
func (a *App) Preview(ctx context.Context, id int64) (string, preview.Info, error) {
	var path string
	var info preview.Info
	err := a.viewAt(ctx, func(ix *index.Index) error {
		row, err := ix.Entry(ctx, id)
		if err != nil {
			return err
		}
		if row.Kind != "file" {
			return ErrNotFile
		}
		kind := preview.KindOf(row.Type, row.Ext)
		if kind == preview.None {
			return preview.ErrUnsupported
		}
		file, err := ix.EntryPath(ctx, id)
		if err != nil {
			return err
		}
		f, err := a.openFile(file)
		if errors.Is(err, os.ErrNotExist) {
			return ErrGone
		}
		if err != nil {
			return err
		}
		defer f.Close()
		// Named by what the file is now, so a file that changes gets a new
		// preview and one that only moves keeps the one it has.
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			return err
		}
		key := fmt.Sprintf("%x-%x-%x-%x", st.Dev, st.Ino, st.Mtim.Nano(), st.Size)
		path, info, err = a.previews.Get(ctx, key, kind, f)
		return err
	})
	return path, info, err
}

// ShareUnsafeError reports that something in a share's metadata folder is not
// what the program made, so nothing is recorded there until it is removed. A
// share writer who could make the program follow a link out of the share
// would borrow its access to the whole pool; refusing is the fence.
type ShareUnsafeError struct {
	Share   string // the share's path
	Problem string // what was found, with its path
}

func (e *ShareUnsafeError) Error() string {
	return e.Problem + ". Nothing is recorded in " + e.Share + " until that is removed."
}

func isSuspect(err error) bool {
	var sus *meta.Suspect
	return errors.As(err, &sus)
}

// unsafe wraps a Suspect from a share's store with the share it concerns.
func unsafe(share string, err error) *ShareUnsafeError {
	var sus *meta.Suspect
	errors.As(err, &sus)
	return &ShareUnsafeError{Share: share, Problem: sus.Error()}
}
