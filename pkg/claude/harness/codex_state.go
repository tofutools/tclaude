package harness

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// Codex keeps each conversation's durable metadata — title, cwd, git
// branch, model, first user message, archived flag — in a sidecar SQLite
// database ~/.codex/state_5.sqlite (table `threads`), separate from the
// per-turn rollout `.jsonl`. This file reads that DB, read-only, as
// *enrichment* over the rollout scan: a present row supplies the title and
// rename signal a rollout can't, while an absent DB (or row) leaves the
// read path to assemble from the rollout head alone.

// codexThread is the subset of a `threads` row the read path consumes. The
// column set is the one verified against a real Codex v0.139 DB (see
// docs/plans/codex-convstore.md); nullable columns are read through
// sql.Null* so a sparse row (or a slightly older schema) still scans.
type codexThread struct {
	ID               string
	RolloutPath      string
	Cwd              string
	Title            string
	GitBranch        string
	Model            string
	FirstUserMessage string
	Preview          string
	TokensUsed       int64
	CreatedAt        int64 // unix seconds
	UpdatedAt        int64 // unix seconds
	Archived         bool
	ArchivedAt       sql.NullInt64 // unix seconds; null when never archived
}

// loadCodexThreads reads every `threads` row into a map keyed by id (the
// rollout uuid). An absent state DB is the documented "no enrichment"
// case: it returns an empty map and no error. A present-but-unreadable DB
// (open/query/scan failure) returns the error — callers degrade to
// rollout-only assembly rather than failing the whole listing.
//
// The DB is opened read-only (`mode=ro`) so a concurrently-running Codex
// instance is never disturbed; this read path is, by contract, read-only.
func loadCodexThreads(home string) (map[string]codexThread, error) {
	path := codexStateDBPath(home)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]codexThread{}, nil
		}
		return nil, err
	}

	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()

	rows, err := d.Query(`SELECT id, rollout_path, cwd, title, git_branch, model,
		first_user_message, preview, tokens_used, created_at, updated_at,
		archived, archived_at FROM threads`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]codexThread{}
	for rows.Next() {
		var t codexThread
		// git_branch / model are nullable in the schema; first_user_message
		// / preview are NOT NULL DEFAULT '' in real Codex but a test or an
		// older schema may leave them NULL — read all four defensively.
		var gitBranch, model, firstMsg, preview sql.NullString
		var archived int64
		if err := rows.Scan(
			&t.ID, &t.RolloutPath, &t.Cwd, &t.Title,
			&gitBranch, &model, &firstMsg, &preview,
			&t.TokensUsed, &t.CreatedAt, &t.UpdatedAt,
			&archived, &t.ArchivedAt,
		); err != nil {
			return nil, err
		}
		t.GitBranch = gitBranch.String
		t.Model = model.String
		t.FirstUserMessage = firstMsg.String
		t.Preview = preview.String
		t.Archived = archived != 0
		out[t.ID] = t
	}
	return out, rows.Err()
}

// codexStateDBPath is the sidecar threads database.
func codexStateDBPath(home string) string {
	return filepath.Join(home, ".codex", "state_5.sqlite")
}
