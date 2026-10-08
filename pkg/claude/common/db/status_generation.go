package db

import (
	"database/sql"
	"net/url"
	"sync"
	"sync/atomic"
)

var statusGeneration atomic.Uint64

// SQLite data_version is connection-local: retain a separate read-only
// connection so writes on EVERY application connection, including hook CLI
// processes and transactional imports, change its counter. Reads do not.
var statusVersionReader struct {
	sync.Mutex
	db      *sql.DB
	path    string
	version int64
}

// StatusSnapshotGeneration detects persisted changes across processes as well
// as explicit daemon-owned pane actions. The PRAGMA is a cheap read, not a
// status gather; it does not scan tables, transcripts, or processes.
func StatusSnapshotGeneration() uint64 {
	if _, err := Open(); err != nil {
		return statusGeneration.Add(1)
	}
	stateMu.Lock()
	path := globalDBPath
	stateMu.Unlock()
	statusVersionReader.Lock()
	defer statusVersionReader.Unlock()
	if statusVersionReader.db == nil || statusVersionReader.path != path {
		closeStatusVersionReaderLocked()
		dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=busy_timeout(100)"
		reader, err := sql.Open("sqlite", dsn)
		if err != nil {
			return statusGeneration.Add(1)
		}
		// data_version values are comparable only on the same connection.
		reader.SetMaxOpenConns(1)
		reader.SetMaxIdleConns(1)
		statusVersionReader.db = reader
		statusVersionReader.path = path
	}
	var version int64
	if err := statusVersionReader.db.QueryRow("PRAGMA data_version").Scan(&version); err != nil {
		closeStatusVersionReaderLocked()
		return statusGeneration.Add(1) // Failed observation must not preserve a warm view.
	}
	if statusVersionReader.version != 0 && statusVersionReader.version != version {
		statusGeneration.Add(1)
	}
	statusVersionReader.version = version
	return statusGeneration.Load()
}

// NotifyStatusChanged covers pane-only lifecycle actions and explicit reader
// invalidation, which need not write SQLite at all.
func NotifyStatusChanged() { statusGeneration.Add(1) }

func closeStatusVersionReader() {
	statusVersionReader.Lock()
	defer statusVersionReader.Unlock()
	closeStatusVersionReaderLocked()
	statusGeneration.Add(1)
}
func closeStatusVersionReaderLocked() {
	if statusVersionReader.db != nil {
		_ = statusVersionReader.db.Close()
	}
	statusVersionReader.db = nil
	statusVersionReader.path = ""
	statusVersionReader.version = 0
}
