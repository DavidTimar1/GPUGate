// Package store owns the SQLite file and every read and write of it. One writer, one place
// transactions are opened. schema.sql says what each table is for.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// TimeLayout is the shape of every timestamp the store keeps: UTC to the second, fixed width and
// zero padded, so a stored value sorts as a string in the order it sorts in time.
const TimeLayout = "2006-01-02T15:04:05Z"

// DefaultIdleMinutes is the idle timeout a launch gets when it names none (per machine, default
// 30). Zero means never, and the user can type it; it is not the default, because a machine
// nobody is using bills all night.
const DefaultIdleMinutes = 30

// busyTimeoutMillis is how long a statement waits for a lock another process holds.
const busyTimeoutMillis = 30000

// Store wraps the one *sql.DB this process opens.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite file at path, creating it if needed, and applies schema.sql. The file is
// untracked working state, never a source of truth about what is running.
//
// GOTCHA: schema.sql uses CREATE TABLE IF NOT EXISTS, which leaves a table that already exists
// untouched. A column added to a table in schema.sql reaches a database file that already has
// the table only through a migration written for it.
func Open(path string) (*Store, error) {
	// busyTimeoutMillis: a second process on this file (the list and destroy commands beside a
	// running server) waits for the other's write to finish instead of failing at once with
	// "database is locked". modernc applies a busy timeout only from this DSN parameter.
	db, err := sql.Open("sqlite", path+"?_busy_timeout="+strconv.Itoa(busyTimeoutMillis))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One writer: SQLite allows a single writer, and capping the pool to one connection makes
	// that constraint explicit rather than something a busy moment discovers.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := seedCodingProject(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed the coding project: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database file.
func (s *Store) Close() error {
	return s.db.Close()
}
