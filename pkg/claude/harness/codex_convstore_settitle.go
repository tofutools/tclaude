package harness

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// SetTitle is the write counterpart to codexConvStore's reads. Codex's
// title lives in the threads state DB (~/.codex/state_5.sqlite, threads.title),
// so a rename here is a direct row write — NOT an in-pane slash injection
// (Codex has no TUI rename command). agentd's rename dispatch routes a
// Codex conversation here because the Codex harness exposes no
// Lifecycle.RenameCommand.
//
// Contract (JOH-161): an UPDATE of the existing row's title, never an
// insert. A real Codex session always has a threads row (Codex creates it
// at session start), so a missing row means "this conversation isn't a
// renameable Codex session" and is an error — we never fabricate a partial
// row (the real schema has many NOT NULL columns Codex owns). We write
// ONLY the title column: bumping ordering/timestamp columns is Codex's own
// bookkeeping, and the read path derives Modified from the rollout mtime,
// not threads.updated_at, so a title write needs nothing else.
//
// Lives in its own file (not codex.go / codex_convstore.go) so this write
// stays separable from the Codex reader the parser slice (JOH-152) owns.
func (codexConvStore) SetTitle(convID, title string) error {
	if convID == "" {
		return fmt.Errorf("codex SetTitle: empty conversation id")
	}
	if title == "" {
		// A rename to "" would blank the title; the caller's rename gate
		// already rejects empty titles, so this is defence in depth.
		return fmt.Errorf("codex SetTitle: refusing to write an empty title for %s", convID)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return setCodexTitle(home, convID, title)
}

// setCodexTitle performs the threads.title UPDATE against the state DB
// under home. Split out from the method so it is testable against a temp
// HOME (the methods resolve home via os.UserHomeDir; the helpers take it
// explicitly — same split as the read side in codex_convstore.go).
func setCodexTitle(home, convID, title string) error {
	path := codexStateDBPath(home)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("codex SetTitle: no state DB at %s (conversation not renameable)", path)
		}
		return err
	}

	// Read-WRITE open (the reads use mode=ro) with the same busy_timeout
	// the rest of tclaude uses, so a concurrently-running Codex instance's
	// transient write lock retries rather than failing the rename outright.
	// journal_mode is deliberately left untouched: we never reconfigure the
	// user's live Codex DB, only update one row in it.
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	res, err := d.Exec(`UPDATE threads SET title = ? WHERE id = ?`, title, convID)
	if err != nil {
		return fmt.Errorf("codex SetTitle: update threads.title for %s: %w", convID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("codex SetTitle: rows affected for %s: %w", convID, err)
	}
	if n == 0 {
		// No row for this id: not a renameable Codex conversation (a real
		// session always has its threads row). Surfacing this lets agentd's
		// deliverRename log a precise failure rather than silently no-op.
		return fmt.Errorf("codex SetTitle: no threads row for conversation %s", convID)
	}
	return nil
}
