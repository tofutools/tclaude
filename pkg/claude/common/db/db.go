// Package db provides a SQLite-backed store for tclaude session state
// and notification cooldown, replacing the previous file-based approach.
package db

import (
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

var (
	once     sync.Once
	globalDB *sql.DB
	initErr  error
)

// DBPath returns the path to the SQLite database file.
func DBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "db.sqlite")
}

// Open returns the singleton database connection, creating and migrating
// the database on first call.
func Open() (*sql.DB, error) {
	once.Do(func() {
		dbPath := DBPath()
		if dbPath == "" {
			initErr = os.ErrNotExist
			return
		}
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			initErr = err
			return
		}

		globalDB, initErr = sql.Open("sqlite", dbPath)
		if initErr != nil {
			return
		}

		// Enable WAL mode and set busy timeout for concurrent access
		for _, pragma := range []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA busy_timeout=5000",
		} {
			if _, err := globalDB.Exec(pragma); err != nil {
				initErr = err
				globalDB.Close()
				globalDB = nil
				return
			}
		}

		initErr = migrate(globalDB)
		if initErr != nil {
			globalDB.Close()
			globalDB = nil
		}
	})
	return globalDB, initErr
}

// Close closes the singleton database connection if it is open.
// It is safe to call multiple times.
func Close() {
	if globalDB != nil {
		err := globalDB.Close()
		if err != nil {
			slog.Warn("Unable to close DB", "error", err)
		}
		globalDB = nil
	}
}

// ResetForTest allows tests to reset the singleton so Open() re-initializes.
// Must only be called from tests.
func ResetForTest() {
	if globalDB != nil {
		globalDB.Close()
		globalDB = nil
	}
	once = sync.Once{}
	initErr = nil
}
