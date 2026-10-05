package index

import (
	"context"
	"database/sql"
	"strings"
)

// TypeTotal is how much of one file type a folder holds, at any depth.
type TypeTotal struct {
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Disk  int64  `json:"disk"`
	Files int64  `json:"files"`
}

type typeSum struct{ size, disk, files int64 }

// fillDirTypes works out, for every folder, the bytes and files of each type
// beneath it, and stores them in dir_types. Everything needed is already in
// the index: each file's type, size and folder. So this costs no extra pass
// over the pool, and an index built before the table existed gains it when
// opened.
//
// It reads everything before it writes anything, because the builder's
// database has a single connection.
func fillDirTypes(db *sql.DB) error {
	// What each folder holds directly. A hardlinked file is counted at one of
	// its names only, the same one its folder's size counts it at.
	totals := make(map[int64]*[numCats]typeSum)
	rows, err := db.Query(`SELECT parent, cat, SUM(size), SUM(disk), COUNT(*) FROM entries WHERE kind != 1 AND counted GROUP BY parent, cat`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var dir int64
		var cat int
		var s typeSum
		if err := rows.Scan(&dir, &cat, &s.size, &s.disk, &s.files); err != nil {
			rows.Close()
			return err
		}
		if cat < 0 || cat >= len(catNames) {
			cat = CatOther
		}
		t := totals[dir]
		if t == nil {
			t = new([numCats]typeSum)
			totals[dir] = t
		}
		t[cat].size += s.size
		t[cat].disk += s.disk
		t[cat].files += s.files
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// Folders from the newest ID down. The scan numbers a folder before
	// anything inside it, so by the time a folder comes up here everything
	// beneath it has been folded into it and its totals are final.
	type dir struct{ id, parent int64 }
	var dirs []dir
	rows, err = db.Query(`SELECT id, parent FROM entries WHERE kind = 1 ORDER BY id DESC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d dir
		if err := rows.Scan(&d.id, &d.parent); err != nil {
			rows.Close()
			return err
		}
		dirs = append(dirs, d)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM dir_types`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO dir_types VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		t := totals[d.id]
		if t == nil {
			continue
		}
		for cat, s := range t {
			if s.files == 0 {
				continue
			}
			if _, err := stmt.Exec(d.id, cat, s.size, s.disk, s.files); err != nil {
				return err
			}
		}
		if d.parent != 0 {
			p := totals[d.parent]
			if p == nil {
				p = new([numCats]typeSum)
				totals[d.parent] = p
			}
			for cat, s := range t {
				p[cat].size += s.size
				p[cat].disk += s.disk
				p[cat].files += s.files
			}
		}
		delete(totals, d.id) // written and passed up: no longer needed
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta VALUES('dir_types', '1')`); err != nil {
		return err
	}
	return tx.Commit()
}

// FillTypes sets Types on the folders among rows: what each is made of.
func (ix *Index) FillTypes(ctx context.Context, rows []Row) error {
	at := make(map[int64]int)
	var args []any
	for i := range rows {
		if rows[i].IsDir() {
			at[rows[i].ID] = i
			args = append(args, rows[i].ID)
		}
	}
	if len(args) == 0 {
		return nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	found, err := ix.db.QueryContext(ctx, `SELECT dir, cat, size, disk, files FROM dir_types WHERE dir IN (`+marks+`) ORDER BY size DESC`, args...)
	if err != nil {
		return err
	}
	defer found.Close()
	for found.Next() {
		var dir int64
		var cat int
		var t TypeTotal
		if err := found.Scan(&dir, &cat, &t.Size, &t.Disk, &t.Files); err != nil {
			return err
		}
		t.Type = CatName(cat)
		rows[at[dir]].Types = append(rows[at[dir]].Types, t)
	}
	return found.Err()
}
