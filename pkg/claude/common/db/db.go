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

		// Test-only fast path: seed the db file from a cached, fully-migrated
		// snapshot so the migrate() below short-circuits (the v0->vN chain
		// costs ~290ms on pure-Go sqlite and the suite opens a fresh db per
		// test). Inert in production. See migration_template.go.
		seededFromTemplate := maybeSeedFromTemplate(dbPath)

		// PRAGMAs in the DSN are applied on every new pooled connection.
		// `foreign_keys` in particular is per-connection in SQLite, so a
		// one-shot db.Exec wouldn't survive when the pool opens new
		// connections under load.
		dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
		globalDB, initErr = sql.Open("sqlite", dsn)
		if initErr != nil {
			return
		}

		initErr = migrate(globalDB)
		if initErr != nil {
			_ = globalDB.Close()
			globalDB = nil
			return
		}

		// Test-only: capture the freshly-migrated empty schema so sibling
		// tests in this process reuse it via the fast path above. No-op when
		// we just seeded from the template or in production.
		if !seededFromTemplate {
			maybeCaptureTemplate(globalDB, dbPath)
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
		_ = globalDB.Close()
		globalDB = nil
	}
	once = sync.Once{}
	initErr = nil
	// Arm the migration-template fast path. The first Open in this process
	// pays the full migration cost and caches the result; every later
	// ResetForTest+Open reuses the snapshot. See migration_template.go.
	enableMigrationTemplate()
}
