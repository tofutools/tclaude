package harness

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Codex's state DB (~/.codex/state_5.sqlite) is a rollback-journal (non-WAL)
// SQLite file. In rollback mode a writer takes an exclusive lock that blocks
// readers, so a read landing during a concurrent write returns SQLITE_BUSY
// *immediately* unless the read connection has a busy_timeout to retry under.
//
// This pins the fix for the intermittent "database is locked (5) (SQLITE_BUSY)"
// flake: the daemon reads this DB (loadCodexThreads — conv enrichment) while
// other goroutines write it (setCodexTitle — e.g. a reincarnate's background
// `<prev>-r-N` rename, or a live Codex instance). Before the fix, the read
// opened with `mode=ro` and no busy_timeout and lost the race ~99% of the
// time under a concurrent writer; with busy_timeout(5000) on both paths it
// retries and succeeds.
//
// Guard against regressing the read DSN back to a busy_timeout-less open.
func TestLoadCodexThreads_NoBusyUnderConcurrentWrites(t *testing.T) {
	home := t.TempDir()
	const convID = "019ec004-0000-0000-0000-0000000000aa"
	seedCodexThreadRow(t, home, convID, "seed-title")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: hammer setCodexTitle (the production write path) with realistic
	// spacing — single-statement UPDATEs, not a pathological tight loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := setCodexTitle(home, convID, fmt.Sprintf("title-%d", i)); err != nil {
				// The writer has its own busy_timeout(5000) and contends only
				// with read-only readers (which take a brief SHARED lock), so
				// it should never legitimately error. The reader is the subject
				// under test, but any writer error invalidates the run — surface
				// it rather than letting a broken writer mask the read result.
				t.Errorf("setCodexTitle: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Reader: the production read path must never surface SQLITE_BUSY while a
	// writer is active. An un-timed read loses this race ~99% of the time, so
	// even a modest number of iterations near-certainly catches a regression
	// while keeping the test quick (each read opens/closes its own connection).
	const reads = 60
	for i := 0; i < reads; i++ {
		threads, err := loadCodexThreads(home)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("loadCodexThreads read %d/%d failed under concurrent writes: %v", i+1, reads, err)
		}
		if _, ok := threads[convID]; !ok {
			close(stop)
			wg.Wait()
			t.Fatalf("loadCodexThreads read %d: seeded thread row missing", i+1)
		}
	}

	close(stop)
	wg.Wait()
}

// seedCodexThreadRow lays down a single threads row at the codex state DB
// under home, with the column subset loadCodexThreads SELECTs and
// setCodexTitle UPDATEs. Left in the default rollback-journal mode on
// purpose — that is the real Codex DB's mode and the condition the flake
// needs.
func seedCodexThreadRow(t *testing.T, home, convID, title string) {
	t.Helper()
	path := codexStateDBPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS threads (
		id TEXT PRIMARY KEY,
		rollout_path TEXT NOT NULL DEFAULT '',
		cwd TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		git_branch TEXT,
		model TEXT,
		first_user_message TEXT,
		preview TEXT,
		tokens_used INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0,
		archived INTEGER NOT NULL DEFAULT 0,
		archived_at INTEGER
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR REPLACE INTO threads (id, title) VALUES (?, ?)`, convID, title); err != nil {
		t.Fatal(err)
	}
}
