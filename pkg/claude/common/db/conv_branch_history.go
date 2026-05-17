package db

import "time"

// conv_branch_history records, per conversation, the distinct set of
// git branches an agent has worked on — every branch that ever appeared
// in the conversation's .jsonl turns, plus (via the hook append) any
// branch the agent edited files on in a worktree the launch-dir .jsonl
// never names. Each row optionally carries a PR snapshot.
//
// Two writers, distinguished by the `source` column:
//
//   - 'scan' — the idempotent .jsonl re-scan, the source of truth.
//     RebuildConvBranchHistoryScan fully rebuilds the 'scan' rows for a
//     conv on every pass: a branch the .jsonl no longer names is
//     dropped, so the row set is a true mirror of the conversation
//     data rather than a monotonic pile.
//   - 'hook' — AppendConvBranchHistoryHook, called by the PostToolUse
//     hook as agents edit files. Cheap and additive; the re-scan never
//     deletes a 'hook' row (it can't see worktree branches to confirm
//     them). A branch first seen by the hook and later named by the
//     .jsonl is upgraded to 'scan' on the next rebuild.
//
// pr_* is a best-effort snapshot stamped by SetConvBranchHistoryPR off
// the dashboard's existing branch-link resolver — neither writer above
// touches it, so a branch with no PR (or one not yet resolved) simply
// keeps the zero values.

// Branch-history source discriminators (conv_branch_history.source).
const (
	// BranchSourceScan marks a row owned by the .jsonl re-scan.
	BranchSourceScan = "scan"
	// BranchSourceHook marks a row appended by the PostToolUse hook.
	BranchSourceHook = "hook"
)

// ConvBranchHistoryRow is one (conv_id, branch) entry — a git branch a
// conversation has worked on, with a best-effort PR snapshot.
type ConvBranchHistoryRow struct {
	ConvID    string
	Branch    string
	RepoDir   string    // worktree root / cwd the branch was last seen in
	PRNumber  int       // PR number; 0 = no PR or not yet resolved
	PRURL     string    // web link to the PR; "" likewise
	PRState   string    // "" (unresolved/none) | "open" | "merged" | "closed"
	Source    string    // BranchSourceScan | BranchSourceHook
	FirstSeen time.Time // earliest observation of this branch
	LastSeen  time.Time // latest observation of this branch
}

// BranchObservation is one branch sighting fed to
// RebuildConvBranchHistoryScan — a branch name, the dir it was seen in,
// and the timestamps bracketing where it appeared in the .jsonl.
type BranchObservation struct {
	Branch    string
	RepoDir   string
	FirstSeen time.Time
	LastSeen  time.Time
}

// RebuildConvBranchHistoryScan replaces the 'scan'-sourced branch
// history of one conversation with exactly the supplied observations.
// It is the idempotent core of the feature: re-running it with the
// same observations converges to the same rows, so a full .jsonl
// re-scan can rebuild the history from scratch without depending on
// any incremental state.
//
// Within a single transaction it: (1) upserts each observed branch as
// a 'scan' row — first_seen/last_seen merge as a true min/max with any
// existing row so a hook row's earlier sighting is never lost, and a
// pre-existing PR snapshot is preserved; (2) deletes 'scan' rows for
// branches no longer observed. 'hook' rows for branches absent from
// the observations are left untouched — the re-scan cannot see the
// worktree branches the hook recorded, so it must not delete them.
//
// An empty convID, or an observation with an empty branch, is skipped.
func RebuildConvBranchHistoryScan(convID string, obs []BranchObservation) error {
	if convID == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Snapshot existing rows so the timestamp merge is a real min/max
	// and the stale-row delete knows which 'scan' branches to drop.
	type existingRow struct {
		source    string
		firstSeen time.Time
		lastSeen  time.Time
	}
	existing := map[string]existingRow{}
	rows, err := tx.Query(`SELECT branch, source, first_seen, last_seen
		FROM conv_branch_history WHERE conv_id = ?`, convID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var branch, source, fs, ls string
		if err := rows.Scan(&branch, &source, &fs, &ls); err != nil {
			_ = rows.Close()
			return err
		}
		existing[branch] = existingRow{
			source:    source,
			firstSeen: parseTimeOrZero(fs),
			lastSeen:  parseTimeOrZero(ls),
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	observed := make(map[string]bool, len(obs))
	for _, o := range obs {
		if o.Branch == "" {
			continue
		}
		observed[o.Branch] = true
		first, last := o.FirstSeen, o.LastSeen
		if e, ok := existing[o.Branch]; ok {
			if !e.firstSeen.IsZero() && (first.IsZero() || e.firstSeen.Before(first)) {
				first = e.firstSeen
			}
			if e.lastSeen.After(last) {
				last = e.lastSeen
			}
		}
		// ON CONFLICT deliberately omits the pr_* columns: a PR
		// snapshot stamped by SetConvBranchHistoryPR survives the
		// rebuild. source is forced to 'scan' so a former hook row is
		// upgraded once the .jsonl names the branch.
		if _, err := tx.Exec(`INSERT INTO conv_branch_history
			(conv_id, branch, repo_dir, source, first_seen, last_seen)
			VALUES (?, ?, ?, 'scan', ?, ?)
			ON CONFLICT(conv_id, branch) DO UPDATE SET
				repo_dir = excluded.repo_dir, source = 'scan',
				first_seen = excluded.first_seen, last_seen = excluded.last_seen`,
			convID, o.Branch, o.RepoDir, fmtBranchTime(first), fmtBranchTime(last)); err != nil {
			return err
		}
	}

	for branch, e := range existing {
		if e.source == BranchSourceScan && !observed[branch] {
			if _, err := tx.Exec(`DELETE FROM conv_branch_history
				WHERE conv_id = ? AND branch = ? AND source = 'scan'`,
				convID, branch); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// AppendConvBranchHistoryHook records one branch sighting from the
// PostToolUse hook — the cheap, additive companion to the re-scan. It
// catches branches in worktrees that the launch-dir .jsonl never names
// (Claude Code stamps only the launch repo's branch onto each turn).
//
// On insert the row is 'hook'-sourced with first_seen == last_seen ==
// now. On conflict it bumps last_seen and repo_dir but leaves source,
// first_seen and pr_* alone — so it never downgrades a 'scan' row and
// never loses the earliest sighting. An empty convID or branch (the
// hook fires for plenty of tool calls outside a git repo) is a no-op,
// not an error.
func AppendConvBranchHistoryHook(convID, branch, repoDir string) error {
	if convID == "" || branch == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	now := fmtBranchTime(time.Now())
	_, err = conn.Exec(`INSERT INTO conv_branch_history
		(conv_id, branch, repo_dir, source, first_seen, last_seen)
		VALUES (?, ?, ?, 'hook', ?, ?)
		ON CONFLICT(conv_id, branch) DO UPDATE SET
			repo_dir = excluded.repo_dir, last_seen = excluded.last_seen`,
		convID, branch, repoDir, now, now)
	return err
}

// SetConvBranchHistoryPR stamps a PR snapshot onto every branch-history
// row matching (repoDir, branch). It is called from the dashboard's
// branch-link resolver, which already shells out to `gh` for the
// active and startup branches of live agents — so the history table
// rides that existing resolution rather than adding its own.
//
// Matching on (repo_dir, branch) rather than conv_id is deliberate:
// the resolver knows a directory and a branch, not a conversation, and
// two conversations sharing a worktree should both pick up the PR.
// prNumber 0 / empty strings clear a stale snapshot (a deleted PR).
//
// An empty repoDir or branch is a no-op.
func SetConvBranchHistoryPR(repoDir, branch string, prNumber int, prURL, prState string) error {
	if repoDir == "" || branch == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	_, err = conn.Exec(`UPDATE conv_branch_history
		SET pr_number = ?, pr_url = ?, pr_state = ?
		WHERE repo_dir = ? AND branch = ?`,
		prNumber, prURL, prState, repoDir, branch)
	return err
}

// ListConvBranchHistory returns the branch history of one conversation,
// oldest sighting first (branch name breaks ties). An unknown convID
// yields an empty slice and a nil error.
func ListConvBranchHistory(convID string) ([]ConvBranchHistoryRow, error) {
	conn, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(`SELECT conv_id, branch, repo_dir, pr_number,
		pr_url, pr_state, source, first_seen, last_seen
		FROM conv_branch_history WHERE conv_id = ?
		ORDER BY last_seen, branch`, convID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ConvBranchHistoryRow
	for rows.Next() {
		var r ConvBranchHistoryRow
		var firstSeen, lastSeen string
		if err := rows.Scan(&r.ConvID, &r.Branch, &r.RepoDir, &r.PRNumber,
			&r.PRURL, &r.PRState, &r.Source, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		r.FirstSeen = parseTimeOrZero(firstSeen)
		r.LastSeen = parseTimeOrZero(lastSeen)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteConvBranchHistory drops every branch-history row for convID.
// Called when a conversation is evicted (its .jsonl vanished, or it was
// pruned) so the table self-heals alongside conv_index.
func DeleteConvBranchHistory(convID string) error {
	conn, err := Open()
	if err != nil {
		return err
	}
	_, err = conn.Exec(`DELETE FROM conv_branch_history WHERE conv_id = ?`, convID)
	return err
}

// fmtBranchTime renders a timestamp for storage — RFC3339Nano in UTC so
// the stored strings sort lexically (the ORDER BY in ListConvBranchHistory
// relies on it). A zero time stores as "" rather than the year-1 string,
// matching parseTimeOrZero's empty-is-zero convention.
func fmtBranchTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
