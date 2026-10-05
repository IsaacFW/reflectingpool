// Package index stores one scan in a SQLite file and answers queries over it.
package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var ErrNotFound = errors.New("not found")

// QueryError reports a listing request that names an unknown sort key,
// filter value or group key.
type QueryError string

func (e QueryError) Error() string { return string(e) }

// Annotation state bits cached per entry.
const (
	StateNote = 1 << iota
	StatePrefix
	StateSkipped
	StateDisplayName
	StateOrphaned
)

var kindNames = []string{"file", "dir", "symlink", "other"}

// Row is one entry as the API returns it.
type Row struct {
	ID      int64  `json:"id"`
	Parent  int64  `json:"parent"`
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Kind    string `json:"kind"`
	Flags   int    `json:"flags"`
	Size    int64  `json:"size"`
	Disk    int64  `json:"disk"`
	Mtime   int64  `json:"mtime"`
	Btime   int64  `json:"btime"`
	Atime   int64  `json:"atime"`
	Nlink   int64  `json:"nlink"`
	Share   int64  `json:"share"`
	Counted bool   `json:"counted"`
	Ext     string `json:"ext"`
	Type    string `json:"type"`
	State   int    `json:"state"`

	// Rollups, set for directories.
	Files    int64 `json:"files"`
	Dirs     int64 `json:"dirs"`
	MaxMtime int64 `json:"max_mtime"`
	MaxBtime int64 `json:"max_btime"`
	MaxAtime int64 `json:"max_atime"`

	Ino       uint64 `json:"-"`
	Dev       uint64 `json:"-"`
	BtimeNsec int64  `json:"-"`
}

func (r *Row) IsDir() bool { return r.Kind == "dir" }

const (
	rowCols = `e.id, e.parent, e.name, e.kind, e.flags, e.size, e.disk, e.mtime, e.btime, e.btime_ns, e.atime,
		e.ino, e.dev, e.nlink, e.share, e.counted, e.ext, e.cat, e.files, e.dirs,
		e.max_mtime, e.max_btime, e.max_atime, COALESCE(a.state, 0)`
	rowFrom = ` FROM entries e LEFT JOIN annot a ON a.entry = e.id`
	// A directory's modified time is the newest file beneath it; an empty
	// directory falls back to its own.
	modifiedExpr = `(CASE WHEN e.kind = 1 AND e.max_mtime > 0 THEN e.max_mtime ELSE e.mtime END)`
)

type scanner interface{ Scan(dest ...any) error }

func scanRow(s scanner) (Row, error) {
	var r Row
	var kind, cat int
	var ino, dev int64
	err := s.Scan(&r.ID, &r.Parent, &r.Name, &kind, &r.Flags, &r.Size, &r.Disk, &r.Mtime, &r.Btime, &r.BtimeNsec, &r.Atime,
		&ino, &dev, &r.Nlink, &r.Share, &r.Counted, &r.Ext, &cat, &r.Files, &r.Dirs,
		&r.MaxMtime, &r.MaxBtime, &r.MaxAtime, &r.State)
	if kind >= 0 && kind < len(kindNames) {
		r.Kind = kindNames[kind]
	}
	r.Type = CatName(cat)
	r.Ino, r.Dev = uint64(ino), uint64(dev)
	return r, err
}

// Index is an open scan index.
type Index struct {
	db   *sql.DB
	Path string
	Info Info

	queueMu sync.Mutex
	queues  map[string][]QueueGroup // ranked groups per queue definition
}

func Open(path string) (*Index, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(path, "journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)", "cache_size(-65536)"))
	if err != nil {
		return nil, err
	}
	ix := &Index{db: db, Path: path}
	var blob string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'info'`).Scan(&blob); err != nil {
		db.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	if err := json.Unmarshal([]byte(blob), &ix.Info); err != nil {
		db.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	return ix, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

// Latest returns the newest finished index in dir, or "" if there is none.
func Latest(dir string) string {
	all := list(dir)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

// Prune deletes all but the newest keep indexes in dir, along with anything
// left behind by an interrupted scan.
func Prune(dir string, keep int) {
	all := list(dir)
	for i := 0; i < len(all)-keep; i++ {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(all[i] + suffix)
		}
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "scan-*.db.tmp"))
	for _, p := range stale {
		os.Remove(p)
	}
}

func list(dir string) []string {
	all, _ := filepath.Glob(filepath.Join(dir, "scan-*.db"))
	sort.Strings(all) // the timestamp in the name sorts chronologically
	return all
}

func (ix *Index) Entry(ctx context.Context, id int64) (Row, error) {
	r, err := scanRow(ix.db.QueryRowContext(ctx, `SELECT `+rowCols+rowFrom+` WHERE e.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// EntryPath returns the absolute path of an entry.
func (ix *Index) EntryPath(ctx context.Context, id int64) (string, error) {
	return ix.pathOf(ctx, id, nil)
}

func (ix *Index) pathOf(ctx context.Context, id int64, cache map[int64]string) (string, error) {
	var parts []string
	base := ""
	for id != 0 {
		if p, ok := cache[id]; ok {
			base = p
			break
		}
		var parent int64
		var name string
		err := ix.db.QueryRowContext(ctx, `SELECT parent, name FROM entries WHERE id = ?`, id).Scan(&parent, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, name)
		id = parent
	}
	for i := len(parts) - 1; i >= 0; i-- {
		switch {
		case base == "":
			base = parts[i] // a root's name is its absolute path
		case base == "/":
			base += parts[i]
		default:
			base += "/" + parts[i]
		}
	}
	return base, nil
}

// FillPaths sets Path on each row. Rows in a listing mostly share parents, so
// directory paths are resolved once.
func (ix *Index) FillPaths(ctx context.Context, rows []Row) error {
	cache := make(map[int64]string)
	for i := range rows {
		dir, err := ix.pathOf(ctx, rows[i].Parent, cache)
		if err != nil {
			return err
		}
		cache[rows[i].Parent] = dir
		switch {
		case rows[i].Parent == 0:
			rows[i].Path = rows[i].Name
		case dir == "/":
			rows[i].Path = "/" + rows[i].Name
		default:
			rows[i].Path = dir + "/" + rows[i].Name
		}
	}
	return nil
}

// Lookup returns the entry at an absolute path.
func (ix *Index) Lookup(ctx context.Context, path string) (Row, error) {
	path = filepath.Clean(path)
	roots, _, err := ix.Children(ctx, 0, Sort{Key: "name"}, 0, 0)
	if err != nil {
		return Row{}, err
	}
	for _, root := range roots {
		rest, ok := strings.CutPrefix(path, root.Name)
		if !ok || (rest != "" && rest[0] != '/' && root.Name != "/") {
			continue
		}
		id := root.ID
		for _, part := range strings.Split(strings.Trim(rest, "/"), "/") {
			if part == "" {
				continue
			}
			err := ix.db.QueryRowContext(ctx, `SELECT id FROM entries WHERE parent = ? AND name = ?`, id, part).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return Row{}, ErrNotFound
			}
			if err != nil {
				return Row{}, err
			}
		}
		return ix.Entry(ctx, id)
	}
	return Row{}, ErrNotFound
}

// Sort names a sort key and direction for listings.
type Sort struct {
	Key  string
	Desc bool
}

var sortExprs = map[string]string{
	"size":     "e.size",
	"disk":     "e.disk",
	"name":     "e.name COLLATE NOCASE",
	"modified": modifiedExpr,
	"files":    "e.files",
}

func (s Sort) sql() (string, error) {
	if s.Key == "" {
		s = Sort{Key: "size", Desc: true}
	}
	expr, ok := sortExprs[s.Key]
	if !ok {
		return "", QueryError(fmt.Sprintf("unknown sort key %q", s.Key))
	}
	if s.Desc {
		expr += " DESC"
	}
	return " ORDER BY " + expr + ", e.id", nil
}

func (ix *Index) query(ctx context.Context, q string, args ...any) ([]Row, error) {
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func limitSQL(limit, offset int) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf(" LIMIT %d OFFSET %d", limit, max(offset, 0))
}

// Children lists the entries directly inside a directory, and how many there
// are in total. Parent 0 lists the scan roots.
func (ix *Index) Children(ctx context.Context, parent int64, s Sort, limit, offset int) ([]Row, int64, error) {
	order, err := s.sql()
	if err != nil {
		return nil, 0, err
	}
	rows, err := ix.query(ctx, `SELECT `+rowCols+rowFrom+` WHERE e.parent = ?`+order+limitSQL(limit, offset), parent)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	err = ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries WHERE parent = ?`, parent).Scan(&total)
	return rows, total, err
}

// Shares lists the top-level directories under every root.
func (ix *Index) Shares(ctx context.Context) ([]Row, error) {
	return ix.query(ctx, `SELECT `+rowCols+rowFrom+` WHERE e.id = e.share ORDER BY e.name COLLATE NOCASE`)
}

// Filter narrows a listing. Zero values mean "any".
type Filter struct {
	Kind           string   `json:"kind"`   // "file" or "dir"
	Shares         []int64  `json:"shares"` // share IDs
	Types          []string `json:"types"`  // category names
	Exts           []string `json:"exts"`
	MinSize        int64    `json:"min_size"`
	MaxSize        int64    `json:"max_size"`
	ModifiedBefore int64    `json:"modified_before"` // unix seconds
	Name           string   `json:"name"`            // case-insensitive substring
	State          string   `json:"state"`           // "annotated", "unannotated" or "skipped"
	Prefix         string   `json:"prefix"`
}

func (f Filter) where() (string, []any, error) {
	var conds []string
	var args []any
	in := func(col string, n int) string {
		return col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
	}
	switch f.Kind {
	case "":
	case "file":
		conds = append(conds, "e.kind = 0")
	case "dir":
		conds = append(conds, "e.kind = 1")
	default:
		return "", nil, QueryError(fmt.Sprintf("unknown kind %q", f.Kind))
	}
	if len(f.Shares) > 0 {
		conds = append(conds, in("e.share", len(f.Shares)))
		for _, s := range f.Shares {
			args = append(args, s)
		}
	}
	if len(f.Types) > 0 {
		conds = append(conds, in("e.cat", len(f.Types)))
		for _, t := range f.Types {
			cat, ok := CatByName(t)
			if !ok {
				return "", nil, QueryError(fmt.Sprintf("unknown type %q", t))
			}
			args = append(args, cat)
		}
	}
	if len(f.Exts) > 0 {
		conds = append(conds, in("e.ext", len(f.Exts)))
		for _, e := range f.Exts {
			args = append(args, strings.ToLower(strings.TrimPrefix(e, ".")))
		}
	}
	if f.MinSize > 0 {
		conds = append(conds, "e.size >= ?")
		args = append(args, f.MinSize)
	}
	if f.MaxSize > 0 {
		conds = append(conds, "e.size <= ?")
		args = append(args, f.MaxSize)
	}
	if f.ModifiedBefore > 0 {
		conds = append(conds, modifiedExpr+" < ?")
		args = append(args, f.ModifiedBefore)
	}
	if f.Name != "" {
		conds = append(conds, `e.name LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(f.Name)+"%")
	}
	switch f.State {
	case "":
	case "annotated":
		conds = append(conds, fmt.Sprintf("a.entry IS NOT NULL AND a.state & %d = 0", StateSkipped))
	case "unannotated":
		conds = append(conds, "a.entry IS NULL")
	case "skipped":
		conds = append(conds, fmt.Sprintf("a.state & %d != 0", StateSkipped))
	default:
		return "", nil, QueryError(fmt.Sprintf("unknown state %q", f.State))
	}
	if f.Prefix != "" {
		conds = append(conds, `a.prefixes LIKE ? ESCAPE '\'`)
		args = append(args, "%|"+likeEscape(f.Prefix)+"|%")
	}
	if len(conds) == 0 {
		return " WHERE 1", args, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Find lists entries anywhere in the index that match a filter.
func (ix *Index) Find(ctx context.Context, f Filter, s Sort, limit, offset int) ([]Row, error) {
	where, args, err := f.where()
	if err != nil {
		return nil, err
	}
	order, err := s.sql()
	if err != nil {
		return nil, err
	}
	return ix.query(ctx, `SELECT `+rowCols+rowFrom+where+order+limitSQL(limit, offset), args...)
}

// ByInode lists every entry for one inode: a file and its hardlinks.
func (ix *Index) ByInode(ctx context.Context, dev, ino uint64) ([]Row, error) {
	return ix.query(ctx, `SELECT `+rowCols+rowFrom+` WHERE e.dev = ? AND e.ino = ? ORDER BY e.id`, int64(dev), int64(ino))
}

// SameSizeAndTime lists files that could be a moved copy of a file with the
// given size and modified time.
func (ix *Index) SameSizeAndTime(ctx context.Context, size, mtime int64, limit int) ([]Row, error) {
	return ix.query(ctx, `SELECT `+rowCols+rowFrom+` WHERE e.kind = 0 AND e.size = ? AND e.mtime = ? ORDER BY e.id`+limitSQL(limit, 0), size, mtime)
}

// Datasets lists the filesystems seen by the scan.
func (ix *Index) Datasets(ctx context.Context) ([]Dataset, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT dev, name, mountpoint, fstype FROM datasets ORDER BY mountpoint`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dataset{}
	for rows.Next() {
		var d Dataset
		var dev int64
		if err := rows.Scan(&dev, &d.Name, &d.Mountpoint, &d.FSType); err != nil {
			return nil, err
		}
		d.Dev = uint64(dev)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Annot is one row of the annotation cache.
type Annot struct {
	Entry    int64
	State    int
	Prefixes []string
}

func prefixColumn(prefixes []string) string {
	if len(prefixes) == 0 {
		return ""
	}
	return "|" + strings.Join(prefixes, "|") + "|"
}

func (ix *Index) SetAnnot(ctx context.Context, a Annot) error {
	_, err := ix.db.ExecContext(ctx, `INSERT OR REPLACE INTO annot VALUES(?,?,?)`, a.Entry, a.State, prefixColumn(a.Prefixes))
	return err
}

func (ix *Index) ClearAnnot(ctx context.Context, entry int64) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM annot WHERE entry = ?`, entry)
	return err
}

// ReplaceAnnots swaps the whole annotation cache.
func (ix *Index) ReplaceAnnots(ctx context.Context, all []Annot) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM annot`); err != nil {
		return err
	}
	for _, a := range all {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO annot VALUES(?,?,?)`, a.Entry, a.State, prefixColumn(a.Prefixes)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
