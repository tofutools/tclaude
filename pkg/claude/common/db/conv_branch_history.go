package db

import (
	"path/filepath"
	"time"
)

// conv_branch_history records, per conversation, the distinct set of
// git branches an agent has worked on — every branch that ever appeared
// in the conversation's .jsonl turns, plus (via the hook append) any
// branch the agent edited files on in a worktree the launch-dir .jsonl
// never names. Each row optionally carries a PR snapshot.
//
// Identity is (conv_id, repo_dir, branch). A single conversation can
// hop across several repos, and a bare branch name collides between
// them — two repos each have a `main`, and same-named feature branches
// happen — so the repo directory is part of the key. repo_dir is run
// through CanonicalizeRepoDir before every read and write so the two
// writers below agree on one spelling.
//
// Two writers, distinguished by the `source` column:
//
//   - 'scan' — the idempotent .jsonl re-scan, the source of truth.
//     RebuildConvBranchHistoryScan fully rebuilds the 'scan' rows for a
//     conv on every pass: a (repo_dir, branch) pair the .jsonl no
//     longer names is dropped, so the row set is a true mirror of the
//     conversation data rather than a monotonic pile.
//   - 'hook' — AppendConvBranchHistoryHook, called by the PostToolUse
//     hook as agents edit files. Cheap and additive; the re-scan never
//     deletes a 'hook' row (it can't see worktree branches to confirm
//     them). A pair first seen by the hook and later named by the
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

// ConvBranchHistoryRow is one (conv_id, repo_dir, branch) entry — a git
// branch a conversation has worked on in a given repo, with a
// best-effort PR snapshot.
type ConvBranchHistoryRow struct {
	ConvID    string
	RepoDir   string    // canonicalised repo/worktree dir the branch was seen in
	Branch    string    // git branch name
	PRNumber  int       // PR number; 0 = no PR or not yet resolved
	PRURL     string    // web link to the PR; "" likewise
	PRState   string    // "" (unresolved/none) | "open" | "merged" | "closed"
	Source    string    // BranchSourceScan | BranchSourceHook
	FirstSeen time.Time // earliest observation of this (repo_dir, branch)
	LastSeen  time.Time // latest observation
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

// CanonicalizeRepoDir reduces a repo/worktree directory path to one
// canonical spelling so the table's (conv_id, repo_dir, branch) key is
// stable no matter which writer recorded the row. It resolves symlinks
// (so two spellings of the same repo collapse) and cleans the path.
//
// It deliberately does NOT resolve the git worktree root: the .jsonl
// re-scan that supplies most rows must stay subprocess-free (it runs in
// `conv ls` for every conversation), and `git rev-parse` is a
// subprocess. The consequence is a known imperfection — an agent
// launched in a *subdirectory* of a repo records that subdir here,
// while the PostToolUse hook records the worktree root, so the same
// logical branch can land as two rows. That is cosmetic (the FE may
// show a branch twice) and never loses data; the common case — an
// agent launched at a repo or worktree root — is exact.
//
// An empty input returns empty. A path that no longer exists (a
// deleted worktree) can't be symlink-resolved — it falls back to a
// lexical clean, which is still stable.
func CanonicalizeRepoDir(dir string) string {
	if dir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Clean(dir)
}

// cbhKey is the in-memory composite-key string for a (repo_dir, branch)
// pair — used to index the maps inside RebuildConvBranchHistoryScan.
// The NUL separator can't occur in either component.
func cbhKey(repoDir, branch string) string {
	return repoDir + "\x00" + branch
}

// RebuildConvBranchHistoryScan replaces the 'scan'-sourced branch
// history of one conversation with exactly the supplied observations.
// It is the idempotent core of the feature: re-running it with the
// same observations converges to the same rows, so a full .jsonl
// re-scan can rebuild the history from scratch without depending on
// any incremental state.
//
// Within a single transaction it: (1) upserts each observed
// (repo_dir, branch) as a 'scan' row — first_seen/last_seen merge as a
// true min/max with any existing row so a hook row's earlier sighting
// is never lost, and a pre-existing PR snapshot is preserved;
// (2) deletes 'scan' rows for pairs no longer observed. 'hook' rows for
// pairs absent from the observations are left untouched — the re-scan
// cannot see the worktree branches the hook recorded, so it must not
// delete them.
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
	// and the stale-row delete knows which 'scan' pairs to drop.
	type existingRow struct {
		repoDir   string
		branch    string
		source    string
		firstSeen time.Time
		lastSeen  time.Time
	}
	existing := map[string]existingRow{}
	rows, err := tx.Query(`SELECT repo_dir, branch, source, first_seen, last_seen
		FROM conv_branch_history WHERE conv_id = ?`, convID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var e existingRow
		var fs, ls string
		if err := rows.Scan(&e.repoDir, &e.branch, &e.source, &fs, &ls); err != nil {
			_ = rows.Close()
			return err
		}
		e.firstSeen = parseTimeOrZero(fs)
		e.lastSeen = parseTimeOrZero(ls)
		existing[cbhKey(e.repoDir, e.branch)] = e
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	// Pre-merge the observations by canonical key. Two sightings of the
	// same (repo_dir, branch) in one batch must fold into a single row
	// with min/max timestamps *here*: the INSERT...ON CONFLICT below
	// overwrites first_seen with the excluded value and cannot see a
	// row inserted earlier in the same transaction, so it would not
	// merge same-batch duplicates on its own.
	type mergedObs struct {
		repoDir   string
		branch    string
		firstSeen time.Time
		lastSeen  time.Time
	}
	merged := map[string]mergedObs{}
	for _, o := range obs {
		if o.Branch == "" {
			continue
		}
		repoDir := CanonicalizeRepoDir(o.RepoDir)
		key := cbhKey(repoDir, o.Branch)
		m, ok := merged[key]
		if !ok {
			merged[key] = mergedObs{
				repoDir: repoDir, branch: o.Branch,
				firstSeen: o.FirstSeen, lastSeen: o.LastSeen,
			}
			continue
		}
		if !o.FirstSeen.IsZero() && (m.firstSeen.IsZero() || o.FirstSeen.Before(m.firstSeen)) {
			m.firstSeen = o.FirstSeen
		}
		if o.LastSeen.After(m.lastSeen) {
			m.lastSeen = o.LastSeen
		}
		merged[key] = m
	}

	for key, m := range merged {
		first, last := m.firstSeen, m.lastSeen
		if e, ok := existing[key]; ok {
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
		// upgraded once the .jsonl names the pair.
		if _, err := tx.Exec(`INSERT INTO conv_branch_history
			(conv_id, repo_dir, branch, source, first_seen, last_seen)
			VALUES (?, ?, ?, 'scan', ?, ?)
			ON CONFLICT(conv_id, repo_dir, branch) DO UPDATE SET
				source = 'scan',
				first_seen = excluded.first_seen, last_seen = excluded.last_seen`,
			convID, m.repoDir, m.branch, fmtBranchTime(first), fmtBranchTime(last)); err != nil {
			return err
		}
	}

	for key, e := range existing {
		if _, stillObserved := merged[key]; e.source == BranchSourceScan && !stillObserved {
			if _, err := tx.Exec(`DELETE FROM conv_branch_history
				WHERE conv_id = ? AND repo_dir = ? AND branch = ? AND source = 'scan'`,
				convID, e.repoDir, e.branch); err != nil {
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
// now. On conflict it bumps last_seen but leaves source, first_seen and
// pr_* alone — so it never downgrades a 'scan' row and never loses the
// earliest sighting. An empty convID or branch (the hook fires for
// plenty of tool calls outside a git repo) is a no-op, not an error.
func AppendConvBranchHistoryHook(convID, branch, repoDir string) error {
	return appendConvBranchHistoryHookAt(convID, branch, repoDir, time.Now())
}

func appendConvBranchHistoryHookAt(convID, branch, repoDir string, observedAt time.Time) error {
	if convID == "" || branch == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	now := fmtBranchTime(observedAt)
	_, err = conn.Exec(`INSERT INTO conv_branch_history
		(conv_id, repo_dir, branch, source, first_seen, last_seen)
		VALUES (?, ?, ?, 'hook', ?, ?)
		ON CONFLICT(conv_id, repo_dir, branch) DO UPDATE SET
			last_seen = excluded.last_seen`,
		convID, CanonicalizeRepoDir(repoDir), branch, now, now)
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
// repoDir is canonicalised so it matches the stored key.
//
// This writes whatever it is handed — including zero values, which
// would blank a row. The resolver therefore only calls it with a real
// PR (PRNumber > 0); see refreshBranchLink for why a zero must not
// overwrite a good snapshot.
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
		prNumber, prURL, prState, CanonicalizeRepoDir(repoDir), branch)
	return err
}

// ListConvBranchHistory returns the branch history of one conversation,
// ordered by first sighting (oldest first), with repo_dir then branch
// breaking ties. An unknown convID yields an empty slice and a nil
// error.
func ListConvBranchHistory(convID string) ([]ConvBranchHistoryRow, error) {
	conn, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(`SELECT conv_id, repo_dir, branch, pr_number,
		pr_url, pr_state, source, first_seen, last_seen
		FROM conv_branch_history WHERE conv_id = ?
		ORDER BY first_seen, repo_dir, branch`, convID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ConvBranchHistoryRow
	for rows.Next() {
		var r ConvBranchHistoryRow
		var firstSeen, lastSeen string
		if err := rows.Scan(&r.ConvID, &r.RepoDir, &r.Branch, &r.PRNumber,
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
