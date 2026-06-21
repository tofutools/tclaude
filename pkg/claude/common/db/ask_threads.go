package db

import (
	"database/sql"
	"errors"
	"time"
)

// ask_threads maps a (terminal, cwd) pair to the harness conversation that
// `tclaude ask` resumes for it — the persistence behind "ask repeatedly from
// the same terminal+project and keep one thread" (project tclaude-ask,
// JOH-250).
//
// Why keyed on (term_key, cwd) and not term_key alone: a Claude Code
// conversation is stored under the cwd it was created in
// (~/.claude/projects/<encoded-cwd>/<id>.jsonl) and `--resume <id>` only finds
// it from that cwd. So a thread is pinned to the cwd it was born in; the
// terminal id (WT_SESSION / TERM_SESSION_ID / ITERM_SESSION_ID, else the
// shell's pid, salted by boot id) scopes it to one terminal so two terminals
// in the same directory get independent threads.

// AskThread is one persisted ask conversation for a (terminal, cwd) pair.
type AskThread struct {
	TermKey   string
	Cwd       string
	ConvID    string
	Harness   string
	CreatedAt string
	UpdatedAt string
}

// GetAskThread returns the ask thread for (termKey, cwd), or (nil, nil) when
// none is recorded — the signal to mint a fresh conversation.
func GetAskThread(termKey, cwd string) (*AskThread, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	var t AskThread
	err = db.QueryRow(
		`SELECT term_key, cwd, conv_id, harness, created_at, updated_at
		   FROM ask_threads WHERE term_key = ? AND cwd = ?`,
		termKey, cwd,
	).Scan(&t.TermKey, &t.Cwd, &t.ConvID, &t.Harness, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SetAskThread upserts the ask thread for (termKey, cwd) to point at convID on
// harness. created_at is set once (on first insert) and preserved on later
// upserts; updated_at advances every call. Idempotent: re-recording the same
// conv just bumps updated_at.
func SetAskThread(termKey, cwd, convID, harness string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	now := time.Now().Format(time.RFC3339Nano)
	_, err = db.Exec(
		`INSERT INTO ask_threads (term_key, cwd, conv_id, harness, created_at, updated_at)
		      VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(term_key, cwd) DO UPDATE SET
		      conv_id    = excluded.conv_id,
		      harness    = excluded.harness,
		      updated_at = excluded.updated_at`,
		termKey, cwd, convID, harness, now, now)
	return err
}

// DeleteAskThread drops the ask thread for (termKey, cwd) — the `--new` /
// `tclaude ask new` reset that starts the next question on a fresh
// conversation. Deleting a missing row is a no-op.
func DeleteAskThread(termKey, cwd string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM ask_threads WHERE term_key = ? AND cwd = ?`, termKey, cwd)
	return err
}
