package testharness

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// Codex splits a session's storage across two stores: the per-turn rollout
// `.jsonl` (which CodexSim owns by default) AND a sidecar state DB
// (~/.codex/state_5.sqlite, table `threads`) that holds the durable
// metadata — title, cwd, branch, model, first user message. Real Codex
// creates the threads row at session start; the title a user rename writes
// lands in threads.title, not the rollout.
//
// CodexSim does NOT write the state DB by default (the rollout is its
// contract, and the read-path unit tests assert the rollout-only,
// no-threads-row case). WriteThreadRow is the opt-in that models "Codex
// created/updated this session's threads row", so a test can exercise the
// state-DB read path (enrichment, rename detection) and the write path
// (ConvStore.SetTitle UPDATEs an existing row — which needs a row to hit).

// CodexThreadSeed is the subset of a real `threads` row a test seeds. It
// mirrors the columns the production read path SELECTs (codex_state.go) and
// the rename writer UPDATEs (codex_convstore_settitle.go).
type CodexThreadSeed struct {
	Title            string
	Cwd              string
	GitBranch        string
	Model            string
	FirstUserMessage string
	Preview          string
	CreatedAt        int64 // unix seconds; 0 ⇒ left at the column default
	UpdatedAt        int64
	Archived         bool
}

// WriteThreadRow lays down (or replaces) this session's row in the Codex
// threads state DB under the sim's HOME, modelling Codex's own
// session-start row creation. Keyed by the sim's ConvID. The schema is the
// verified-real column subset the production read + SetTitle paths use.
func (c *CodexSim) WriteThreadRow(seed CodexThreadSeed) error {
	cwd := seed.Cwd
	if cwd == "" {
		cwd = c.Cwd
	}
	path := filepath.Join(c.home, ".codex", "state_5.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	// busy_timeout mirrors the production read/write opens (codex_state.go,
	// codex_convstore_settitle.go): the state DB is rollback-journal, so a
	// concurrent writer's lock would otherwise fail this seed immediately.
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
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
		return err
	}

	archived := 0
	if seed.Archived {
		archived = 1
	}
	_, err = d.Exec(`INSERT OR REPLACE INTO threads
		(id, rollout_path, cwd, title, git_branch, model, first_user_message,
		 preview, tokens_used, created_at, updated_at, archived, archived_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ConvID, c.RolloutPath, cwd, seed.Title, seed.GitBranch, seed.Model,
		seed.FirstUserMessage, seed.Preview, 0, seed.CreatedAt, seed.UpdatedAt,
		archived, nil)
	return err
}

// ThreadTitle reads back this session's threads.title — a convenience for
// tests asserting a rename (ConvStore.SetTitle) landed. Returns ("", nil)
// when there is no state DB or no row.
//
// busy_timeout mirrors the production read open (loadCodexThreads): without
// it, this read races the daemon's concurrent SetTitle writes against the
// same rollback-journal state DB (e.g. a reincarnate's background `<prev>-r-N`
// rename) and returns SQLITE_BUSY immediately — the original flow-test flake.
func (c *CodexSim) ThreadTitle() (string, error) {
	path := filepath.Join(c.home, ".codex", "state_5.sqlite")
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { _ = d.Close() }()
	var title string
	err = d.QueryRow(`SELECT title FROM threads WHERE id = ?`, c.ConvID).Scan(&title)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return title, nil
}
