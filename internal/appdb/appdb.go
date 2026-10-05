// Package appdb opens the database that holds state which must outlive any
// one scan: the admin account, sessions and the prefix list.
package appdb

import (
	"database/sql"
	"os"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	pwhash TEXT NOT NULL,
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions(
	token TEXT PRIMARY KEY, -- SHA-256 of the cookie value, never the value itself
	user TEXT NOT NULL,
	csrf TEXT NOT NULL,
	created INTEGER NOT NULL,
	seen INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS prefixes(
	name TEXT PRIMARY KEY,
	meaning TEXT NOT NULL,
	position INTEGER NOT NULL
);
`

func Open(path string) (*sql.DB, error) {
	// The file holds password and session hashes: create it private before
	// SQLite can create it with the default mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One connection keeps writes serialised and is plenty for this workload.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
