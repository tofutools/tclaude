// Package db provides a SQLite-backed store for tclaude session state
// and notification cooldown, replacing the previous file-based approach.
package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/tofutools/tclaude/pkg/common"
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
	stateMu      sync.Mutex
	globalDB     *sql.DB
	globalDBPath string
	dbReady      bool
	initErr      error
)

// DBPath returns the path to the SQLite database file
// (~/.tclaude/data/db.sqlite — private daemon state).
func DBPath() string {
	dataDir := common.TclaudeDataDir()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "db.sqlite")
}

// relocateLegacyDBFiles moves a pre-split database — ~/.tclaude/db.sqlite plus
// its -wal/-shm sidecars and any db.sqlite*.bak backups — into ~/.tclaude/data
// the first time the DB is opened after the api/data split. It self-heals in
// the load path (not a one-shot migration): whichever process opens the DB
// first relocates it, so a fresh empty database is never created at the new
// path while the real one still sits at the old one.
//
// It moves the whole group BEFORE the DB is opened, so we never open a
// db.sqlite whose -wal/-shm were left behind at the old path. The main file is
// moved LAST because its presence at the new path is the idempotency gate: once
// ~/.tclaude/data/db.sqlite exists this is a no-op. os.Rename within one
// filesystem is atomic per file; a rare cross-process race resolves to ENOENT
// on the source, which is treated as already-moved.
func relocateLegacyDBFiles() error {
	if common.PreSplitAgentdReachable() {
		return nil
	}
	root := common.TclaudeDir()
	dataDir := common.TclaudeDataDir()
	if root == "" || dataDir == "" {
		return nil // no home dir; nothing to relocate
	}
	newMain := filepath.Join(dataDir, "db.sqlite")
	if _, err := os.Stat(newMain); err == nil {
		if _, oldErr := os.Stat(filepath.Join(root, "db.sqlite")); oldErr == nil {
			return fmt.Errorf("both legacy and new databases exist (%s and %s); refusing to choose one", filepath.Join(root, "db.sqlite"), newMain)
		} else if !os.IsNotExist(oldErr) {
			return fmt.Errorf("stat legacy database: %w", oldErr)
		}
		return nil // already relocated
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", newMain, err)
	}
	oldMain := filepath.Join(root, "db.sqlite")
	if _, err := os.Stat(oldMain); os.IsNotExist(err) {
		return nil // no legacy DB (fresh install)
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", oldMain, err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	// Sidecars and backups first; the main file last (idempotency gate).
	names := []string{"db.sqlite-wal", "db.sqlite-shm"}
	if baks, err := filepath.Glob(filepath.Join(root, "db.sqlite*.bak")); err == nil {
		for _, bak := range baks {
			names = append(names, filepath.Base(bak))
		}
	}
	names = append(names, "db.sqlite")
	for _, name := range names {
		if err := renameIfAbsentAtDest(filepath.Join(root, name), filepath.Join(dataDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// renameIfAbsentAtDest moves oldPath to newPath unless newPath already exists
// (idempotent) or oldPath is gone (nothing to move, or a racing mover took it).
func renameIfAbsentAtDest(oldPath, newPath string) error {
	if _, err := os.Stat(newPath); err == nil {
		if _, oldErr := os.Stat(oldPath); oldErr == nil {
			return fmt.Errorf("both migration source and destination exist (%s and %s)", oldPath, newPath)
		} else if !os.IsNotExist(oldErr) {
			return fmt.Errorf("stat %s: %w", oldPath, oldErr)
		}
		return nil // already at destination
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", newPath, err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		if os.IsNotExist(err) {
			return nil // source absent — never existed, or a racing mover won
		}
		return fmt.Errorf("move %s -> %s: %w", oldPath, newPath, err)
	}
	slog.Info("relocated legacy database file into data dir", "from", oldPath, "to", newPath)
	return nil
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

	// Resolve the layout once. Re-checking daemon reachability between choosing
	// a path and relocating creates a TOCTOU hole: if an old daemon exits in
	// that gap, we could move the legacy DB to data/ and then open the now-empty
	// legacy path. When a pre-split daemon is observed, keep using its layout for
	// this whole Open; otherwise relocate first and use the canonical path.
	preSplitDaemon := common.PreSplitAgentdReachable()
	dbPath := DBPath()
	if preSplitDaemon {
		dbPath = filepath.Join(common.TclaudeDir(), "db.sqlite")
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				initErr = fmt.Errorf("pre-split agentd is live but its legacy database is missing at %s; refusing to create an empty replacement", dbPath)
			} else {
				initErr = fmt.Errorf("stat live legacy database %s: %w", dbPath, err)
			}
			return globalDB, initErr
		}
	}
	if dbPath == "" {
		initErr = os.ErrNotExist
		return globalDB, initErr
	}
	// Self-heal the api/data split in the load path: relocate a pre-split
	// database (and its sidecars/backups) from ~/.tclaude into ~/.tclaude/data
	// BEFORE creating or opening anything at the new path. Done here — rather
	// than only at daemon startup — so whichever process opens the DB first
	// (the daemon, or a CLI command run after an upgrade but before the daemon
	// restarts) moves the real database instead of silently creating a fresh
	// empty one beside it and stranding the old one.
	if !preSplitDaemon {
		if err := relocateLegacyDBFiles(); err != nil {
			initErr = err
			return globalDB, initErr
		}
		dbPath = filepath.Join(common.TclaudeDataDir(), "db.sqlite")
	}
	globalDBPath = dbPath
	// filepath.Dir(dbPath) is ~/.tclaude/data — private daemon state, so create
	// it 0700 (the daemon's startup migration does the same). Access is really
	// gated by the sandbox config, but 0700 keeps the state private by default.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
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
		maybeCaptureTemplate(globalDB)
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
	globalDBPath = ""
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
	globalDBPath = ""
	dbReady = false
	initErr = nil
	// Arm the migration-template fast path. The first Open in this process
	// pays the full migration cost and caches the result; every later
	// ResetForTest+Open reuses the snapshot. See migration_template.go.
	enableMigrationTemplate()
}
