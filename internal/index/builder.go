package index

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/scan"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE entries(
	id INTEGER PRIMARY KEY,
	parent INTEGER NOT NULL,
	name TEXT NOT NULL,
	kind INTEGER NOT NULL,
	flags INTEGER NOT NULL,
	size INTEGER NOT NULL,
	disk INTEGER NOT NULL,
	mtime INTEGER NOT NULL,
	btime INTEGER NOT NULL,
	btime_ns INTEGER NOT NULL,
	atime INTEGER NOT NULL,
	ino INTEGER NOT NULL,
	dev INTEGER NOT NULL,
	nlink INTEGER NOT NULL,
	share INTEGER NOT NULL,
	counted INTEGER NOT NULL,
	ext TEXT NOT NULL,
	cat INTEGER NOT NULL,
	files INTEGER NOT NULL,
	dirs INTEGER NOT NULL,
	max_mtime INTEGER NOT NULL,
	max_btime INTEGER NOT NULL,
	max_atime INTEGER NOT NULL
);
CREATE TABLE datasets(dev INTEGER PRIMARY KEY, name TEXT NOT NULL, mountpoint TEXT NOT NULL, fstype TEXT NOT NULL);
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- A cache of which entries carry an annotation, rebuilt from the .reflection
-- files after every scan. Those files are the source of truth.
CREATE TABLE annot(entry INTEGER PRIMARY KEY, state INTEGER NOT NULL, prefixes TEXT NOT NULL);
`

// Built after loading: maintaining them during the load roughly doubles its cost.
const indexes = `
CREATE INDEX entries_parent ON entries(parent, name);
CREATE INDEX entries_size ON entries(kind, size DESC);
CREATE INDEX entries_ino ON entries(dev, ino);
CREATE INDEX entries_group ON entries(share, kind, cat, size DESC);
`

// Parts of the format that came after the first indexes were built. Open
// creates them in an index that lacks them, so that an upgrade does not make
// the index on disk useless until the next scan.
const upgrades = `
-- Every folder whose contents count as reviewed because the folder, or one
-- above it, has been described. A cache like annot; see covered.go.
CREATE TABLE IF NOT EXISTS covered(dir INTEGER PRIMARY KEY);
-- Folders only, for walking a subtree without touching its files.
CREATE INDEX IF NOT EXISTS entries_dirs ON entries(parent) WHERE kind = 1;
-- What each folder is made of: bytes and files of each type beneath it. Worked
-- out from the entries once the index is built; see types.go.
CREATE TABLE IF NOT EXISTS dir_types(dir INTEGER NOT NULL, cat INTEGER NOT NULL, size INTEGER NOT NULL,
	disk INTEGER NOT NULL, files INTEGER NOT NULL, PRIMARY KEY(dir, cat)) WITHOUT ROWID;
`

const (
	entryCols   = 23
	rowsPerStmt = 64
	rowsPerTx   = 200_000
)

// Info describes the scan an index was built from.
type Info struct {
	// ID names this index. Entry IDs mean nothing outside the index that
	// issued them, so anything that holds one must also hold this.
	ID         string    `json:"id"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	Roots      []string  `json:"roots"`
	Files      int64     `json:"files"`
	Dirs       int64     `json:"dirs"`
	Size       int64     `json:"size"`
	Disk       int64     `json:"disk"`
	Hardlinked int64     `json:"hardlinked"`
	Errors     int64     `json:"errors"`
	// Intensity is the scan intensity the index was built at.
	Intensity string `json:"intensity,omitempty"`
}

// Dataset is a mounted filesystem seen during a scan.
type Dataset struct {
	Dev        uint64 `json:"-"`
	Name       string `json:"name"`
	Mountpoint string `json:"mountpoint"`
	FSType     string `json:"fstype"`
}

// Builder writes a new index file. It implements scan.Sink.
type Builder struct {
	db         *sql.DB
	tx         *sql.Tx
	multi, one *sql.Stmt
	args       []any
	inTx       int
	tmp, final string
}

// NewBuilder starts an index in dir. The file only takes its final name, and
// so only becomes visible to Latest, once Finish succeeds.
func NewBuilder(dir string, now time.Time) (*Builder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	final := filepath.Join(dir, "scan-"+now.UTC().Format("20060102T150405.000Z")+".db")
	b := &Builder{tmp: final + ".tmp", final: final, args: make([]any, 0, entryCols*rowsPerStmt)}
	os.Remove(b.tmp)
	// Building the lookup indexes sorts every row. Keep SQLite's spill files
	// next to the index, on the data volume, rather than in memory or in a
	// container's small /tmp.
	os.Setenv("SQLITE_TMPDIR", dir)
	// The file is disposable until Finish, so durability is traded for speed.
	db, err := sql.Open("sqlite", dsn(b.tmp, "journal_mode(OFF)", "synchronous(OFF)", "locking_mode(EXCLUSIVE)", "cache_size(-65536)"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	b.db = db
	if _, err := db.Exec(schema); err != nil {
		b.Abort()
		return nil, err
	}
	return b, nil
}

func insertSQL(rows int) string {
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", entryCols), ",") + ")"
	return "INSERT INTO entries VALUES " + strings.TrimSuffix(strings.Repeat(row+",", rows), ",")
}

func (b *Builder) begin() error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	if b.multi, err = tx.Prepare(insertSQL(rowsPerStmt)); err != nil {
		tx.Rollback()
		return err
	}
	if b.one, err = tx.Prepare(insertSQL(1)); err != nil {
		tx.Rollback()
		return err
	}
	b.tx, b.inTx = tx, 0
	return nil
}

func (b *Builder) commit() error {
	if b.tx == nil {
		return nil
	}
	err := b.tx.Commit()
	b.tx = nil
	return err
}

func appendEntry(args []any, e *scan.Entry) []any {
	ext, cat := "", CatFolder
	if e.Kind != scan.KindDir {
		ext, cat = classify(e.Name)
	}
	return append(args,
		e.ID, e.Parent, e.Name, int(e.Kind), int(e.Flags), e.Size, e.Disk,
		e.Mtime, e.Btime, int64(e.BtimeNsec), e.Atime, int64(e.Ino), int64(e.Dev), int64(e.Nlink),
		e.Share, e.Counted, ext, cat, e.Files, e.Dirs, e.MaxMtime, e.MaxBtime, e.MaxAtime)
}

func (b *Builder) Entries(batch []scan.Entry) error {
	if b.tx == nil {
		if err := b.begin(); err != nil {
			return err
		}
	}
	for len(batch) >= rowsPerStmt {
		b.args = b.args[:0]
		for i := range rowsPerStmt {
			b.args = appendEntry(b.args, &batch[i])
		}
		if _, err := b.multi.Exec(b.args...); err != nil {
			return err
		}
		batch = batch[rowsPerStmt:]
		b.inTx += rowsPerStmt
	}
	for i := range batch {
		b.args = appendEntry(b.args[:0], &batch[i])
		if _, err := b.one.Exec(b.args...); err != nil {
			return err
		}
		b.inTx++
	}
	if b.inTx >= rowsPerTx {
		return b.commit()
	}
	return nil
}

func (b *Builder) Counted(ids []int64) error {
	if err := b.commit(); err != nil {
		return err
	}
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE entries SET counted = 1 WHERE id = ?`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Finish records the scan's details, builds the lookup indexes and publishes
// the file. It returns the path of the finished index.
func (b *Builder) Finish(info Info, datasets []Dataset) (string, error) {
	if err := b.commit(); err != nil {
		b.Abort()
		return "", err
	}
	blob, err := json.Marshal(info)
	if err != nil {
		b.Abort()
		return "", err
	}
	if _, err := b.db.Exec(`INSERT INTO meta VALUES('info', ?)`, string(blob)); err != nil {
		b.Abort()
		return "", err
	}
	for _, d := range datasets {
		if _, err := b.db.Exec(`INSERT OR REPLACE INTO datasets VALUES(?,?,?,?)`, int64(d.Dev), d.Name, d.Mountpoint, d.FSType); err != nil {
			b.Abort()
			return "", err
		}
	}
	// ANALYZE costs about a tenth of the index build and keeps the grouped
	// queries off full-table plans.
	if _, err := b.db.Exec(indexes + upgrades + "ANALYZE;"); err != nil {
		b.Abort()
		return "", fmt.Errorf("building indexes: %w", err)
	}
	if err := fillDirTypes(b.db); err != nil {
		b.Abort()
		return "", fmt.Errorf("summing file types per folder: %w", err)
	}
	// Give a placeholder name to any filesystem the mount table did not
	// explain, so every entry's dev resolves to a dataset.
	const unnamed = `INSERT OR IGNORE INTO datasets
		SELECT DISTINCT dev, 'dev:' || dev, '', '' FROM entries WHERE kind = 1 AND flags & 1 != 0`
	if _, err := b.db.Exec(unnamed); err != nil {
		b.Abort()
		return "", err
	}
	if err := b.db.Close(); err != nil {
		os.Remove(b.tmp)
		return "", err
	}
	if err := os.Rename(b.tmp, b.final); err != nil {
		os.Remove(b.tmp)
		return "", err
	}
	return b.final, nil
}

// Abort discards the unfinished index.
func (b *Builder) Abort() {
	if b.tx != nil {
		b.tx.Rollback()
		b.tx = nil
	}
	b.db.Close()
	os.Remove(b.tmp)
}

func dsn(path string, pragmas ...string) string {
	var q strings.Builder
	for i, p := range pragmas {
		if i > 0 {
			q.WriteByte('&')
		}
		q.WriteString("_pragma=" + p)
	}
	return "file:" + path + "?" + q.String()
}
