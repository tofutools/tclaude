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
	// stateMu guards the singleton state below. A plain sync.Once is not
	// enough: ResetForTest must be able to tear the singleton down while a
	// background goroutine left over from a prior test (a daemon loop, a conv
	// monitor mid-startup-scan) may still be calling Open(). Reassigning a
	// sync.Once under such a concurrent caller is a data race that corrupts
	// the Once's internal mutex and parks the next Open() forever (the macOS
	// CI 10m timeout). The mutex makes init and reset mutually exclusive.
	stateMu  sync.Mutex
	globalDB *sql.DB
	dbReady  bool
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
// the database on first call. Concurrent callers block until the first
// initialization completes, then share its result (or its error).
func Open() (*sql.DB, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if dbReady {
		return globalDB, initErr
	}
	// Mark ready up front: even on an init error we cache the failed result
	// (globalDB stays nil, initErr set) rather than retrying the full chain
	// on every call — same memoization the previous sync.Once gave.
	dbReady = true

	dbPath := DBPath()
	if dbPath == "" {
		initErr = os.ErrNotExist
		return globalDB, initErr
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		initErr = err
		return globalDB, initErr
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
	//
	// `_txlock=immediate` makes every sql.Tx a write transaction from BEGIN.
	// Without it, Begin() starts DEFERRED: a tx that reads first (e.g.
	// DeleteAgentGroup's SELECT before its UPDATEs) pins a WAL read snapshot,
	// and if any other connection commits before the tx's first write, the
	// read->write upgrade fails instantly with SQLITE_BUSY — busy_timeout
	// deliberately does not retry that case (the snapshot is stale; waiting
	// can't fix it). With IMMEDIATE the write lock is taken at BEGIN, where
	// busy_timeout(5000) applies, so concurrent writers queue (up to the
	// timeout) instead of erroring (JOH-348). Every Begin() call site here is
	// a write tx, so this costs reads nothing (plain Query/Exec don't use
	// transactions); should a read-only multi-statement tx ever be needed,
	// BeginTx with TxOptions{ReadOnly: true} bypasses the immediate mode.
	dsn := dbPath + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	globalDB, initErr = sql.Open("sqlite", dsn)
	if initErr != nil {
		return globalDB, initErr
	}

	initErr = migrate(globalDB)
	if initErr != nil {
		_ = globalDB.Close()
		globalDB = nil
		return globalDB, initErr
	}

	// Self-healing role-library seed (JOH-240): re-add any missing canonical
	// seed role without overwriting a user's edits. Runs once per process (Open
	// memoizes), so a deleted seed reappears on the next open while edits stay
	// sacred. A seed failure must not brick the DB — the roles are a
	// convenience, so log-and-continue rather than fail Open.
	if err := ensureSeededRoles(globalDB); err != nil {
		slog.Warn("db: seeding roles failed", "error", err)
	}

	// Test-only: capture the freshly-migrated empty schema so sibling
	// tests in this process reuse it via the fast path above. No-op when
	// we just seeded from the template or in production.
	if !seededFromTemplate {
		maybeCaptureTemplate(globalDB, dbPath)
	}
	return globalDB, initErr
}

// Close closes the singleton database connection if it is open.
// It is safe to call multiple times.
func Close() {
	stateMu.Lock()
	defer stateMu.Unlock()
	if globalDB != nil {
		err := globalDB.Close()
		if err != nil {
			slog.Warn("Unable to close DB", "error", err)
		}
		globalDB = nil
	}
	dbReady = false
	initErr = nil
}

// ResetForTest allows tests to reset the singleton so Open() re-initializes.
// Must only be called from tests. Safe to call while a leftover goroutine
// from a prior test is concurrently in Open(): stateMu serializes the two so
// the reset never races the init (see the stateMu comment above).
func ResetForTest() {
	stateMu.Lock()
	defer stateMu.Unlock()
	if globalDB != nil {
		_ = globalDB.Close()
		globalDB = nil
	}
	dbReady = false
	initErr = nil
	// Arm the migration-template fast path. The first Open in this process
	// pays the full migration cost and caches the result; every later
	// ResetForTest+Open reuses the snapshot. See migration_template.go.
	enableMigrationTemplate()
}
