package db

import (
	"database/sql"
	"fmt"
	"time"
)

// SessionRow represents a session row in the database.
type SessionRow struct {
	ID             string
	TmuxSession    string
	PID            int
	Cwd            string
	ConvID         string
	Status         string
	StatusDetail   string
	SubagentCount  int
	AutoRegistered bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastHook       time.Time
	// Harness is the coding tool this session belongs to ("claude",
	// "codex", …). Empty is coalesced to DefaultHarness ("claude") on
	// write (schema v56).
	Harness string
	// SandboxMode is the launch-time OS-sandbox mode the session was
	// spawned under (Codex's --sandbox: read-only / workspace-write /
	// danger-full-access), or "" for a harness with no launch sandbox flag
	// (Claude Code). Set once at spawn by `session new`; the dashboard
	// renders it as a per-agent badge (schema v58, JOH-162). Unlike
	// Harness, "" is a genuine value (no sandbox), so it is stored verbatim
	// — never coalesced.
	SandboxMode string
}

// SaveSession inserts or updates a session, setting updated_at to now.
//
// On an existing row this is an UPSERT that writes ONLY the columns
// SaveSession owns (the state-tracking set: id … harness, sandbox_mode).
// It deliberately does NOT touch the
// context-window columns (context_pct, tokens_input, tokens_output,
// context_window_size) or the compact bookkeeping (compact_pending,
// nudged_pct). Those are out-of-band: owned by the statusline hook
// (UpdateContextSnapshot) and the compact path, written on a different
// cadence from the state-tracking hooks that call SaveSession on every
// tick.
//
// This used to be INSERT OR REPLACE — which deletes and re-inserts the
// whole row, silently resetting every out-of-band column to its
// DEFAULT 0 on every hook tick. That was the dashboard context-meter
// dropout: a state-tracking hook (Stop -> idle, UserPromptSubmit)
// fired SaveSession between statusline renders and wiped context_pct
// back to 0 until the next render restored it. context-window data is
// only ever reliably present in the statusline hook, so only that hook
// may write it; SaveSession must leave those columns alone.
// (migrateV25toV26 already documents this exact hazard — agent_workdir
// was made its own table specifically to dodge INSERT OR REPLACE.)
func SaveSession(s *SessionRow) error {
	db, err := Open()
	if err != nil {
		return err
	}
	s.UpdatedAt = time.Now()

	// An empty Harness defaults to "claude" so a caller that hasn't set
	// it writes the same value the column DEFAULT would, not "".
	harness := s.Harness
	if harness == "" {
		harness = DefaultHarness
	}

	_, err = db.Exec(`INSERT INTO sessions
		(id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tmux_session = excluded.tmux_session,
			pid = excluded.pid,
			cwd = excluded.cwd,
			conv_id = excluded.conv_id,
			status = excluded.status,
			status_detail = excluded.status_detail,
			subagent_count = excluded.subagent_count,
			auto_registered = excluded.auto_registered,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			last_hook = excluded.last_hook,
			harness = excluded.harness,
			sandbox_mode = excluded.sandbox_mode`,
		s.ID, s.TmuxSession, s.PID, s.Cwd, s.ConvID,
		s.Status, s.StatusDetail, s.SubagentCount, boolToInt(s.AutoRegistered),
		s.CreatedAt.Format(time.RFC3339Nano), s.UpdatedAt.Format(time.RFC3339Nano), s.LastHook.Format(time.RFC3339Nano), harness, s.SandboxMode)
	return err
}

// LoadSession loads a session by primary key.
func LoadSession(id string) (*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// DeleteSession removes a session by ID.
func DeleteSession(id string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ListSessions returns all sessions.
func ListSessions() ([]*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSessions(rows)
}

// FindSessionByConvID finds a session by conversation ID using the index.
// When multiple rows exist for the same conv_id (e.g. auto-register
// created a new row alongside an old one with a different short id), we
// return the most recently updated one.
func FindSessionByConvID(convID string) (*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode FROM sessions WHERE conv_id = ?
		ORDER BY updated_at DESC LIMIT 1`, convID)
	s, err := scanSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// FindSessionByPID finds a session by its (host) process PID — the PID
// of the Claude Code process, as recorded by the hook callback. Used by
// agentd's identity resolution as a fallback conv-id source when a
// caller's per-pid ~/.claude/sessions/<pid>.json is missing or
// transiently unreadable. Returns the most recently updated row for that
// PID; nil (no error) when none match. pid 0 — the column default — never
// matches.
func FindSessionByPID(pid int) (*SessionRow, error) {
	if pid <= 0 {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode FROM sessions WHERE pid = ?
		ORDER BY updated_at DESC LIMIT 1`, pid)
	s, err := scanSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// FindSessionsByConvID returns every row for the given conv_id, most
// recently updated first. Used by the agent daemon to find a row whose
// tmux session is actually alive when several stale rows coexist.
func FindSessionsByConvID(convID string) ([]*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode FROM sessions WHERE conv_id = ?
		ORDER BY updated_at DESC`, convID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSessions(rows)
}

// SessionExists checks whether a session with the given ID exists.
func SessionExists(id string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&count)
	return count > 0, err
}

// CleanupOldExited deletes exited sessions older than maxAge and returns the count deleted.
func CleanupOldExited(maxAge time.Duration) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339Nano)
	result, err := db.Exec(`DELETE FROM sessions WHERE status = 'exited' AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MaxUpdatedAt returns the most recent updated_at across all sessions.
// Returns zero time if no sessions exist.
func MaxUpdatedAt() (time.Time, error) {
	db, err := Open()
	if err != nil {
		return time.Time{}, err
	}
	var s sql.NullString
	err = db.QueryRow(`SELECT MAX(updated_at) FROM sessions`).Scan(&s)
	if err != nil || !s.Valid {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, s.String)
}

// scanSession scans a single session row.
func scanSession(row *sql.Row) (*SessionRow, error) {
	var s SessionRow
	var autoReg int
	var createdStr, updatedStr, lastHookStr string
	err := row.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
		&s.Status, &s.StatusDetail, &s.SubagentCount, &autoReg, &createdStr, &updatedStr, &lastHookStr, &s.Harness, &s.SandboxMode)
	if err != nil {
		return nil, err
	}
	s.AutoRegistered = autoReg != 0
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	if lastHookStr != "" {
		s.LastHook, _ = time.Parse(time.RFC3339Nano, lastHookStr)
	}
	return &s, nil
}

// scanSessions scans multiple session rows.
func scanSessions(rows *sql.Rows) ([]*SessionRow, error) {
	var result []*SessionRow
	for rows.Next() {
		var s SessionRow
		var autoReg int
		var createdStr, updatedStr, lastHookStr string
		err := rows.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
			&s.Status, &s.StatusDetail, &s.SubagentCount, &autoReg, &createdStr, &updatedStr, &lastHookStr, &s.Harness, &s.SandboxMode)
		if err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}
		s.AutoRegistered = autoReg != 0
		s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
		if lastHookStr != "" {
			s.LastHook, _ = time.Parse(time.RFC3339Nano, lastHookStr)
		}
		result = append(result, &s)
	}
	return result, rows.Err()
}

// UpdateSessionLastHook writes only the last_hook column for a session,
// leaving updated_at unchanged so watch-mode polling is not perturbed.
func UpdateSessionLastHook(id string, t time.Time) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET last_hook = ? WHERE id = ?`, t.Format(time.RFC3339Nano), id)
	return err
}

// MarkSessionExitedIfUnchanged sets a session's status to "exited" —
// but only if the row still carries the status and updated_at the
// caller observed. It is a compare-and-swap: when the row changed
// underneath the caller (most often a resume's SessionStart hook
// flipping status back and bumping updated_at) the WHERE clause fails,
// nothing is written, and `false` is returned.
//
// The session reaper uses this so a session that resumed in the gap
// between "observed dead" and "write exited" is never clobbered. A
// false return is benign — the reaper re-evaluates the row next sweep.
//
// Reaching this path means no graceful SessionEnd hook fired for the
// session: the reaper only ever marks a row whose status was still
// live — a cleanly-exited row is already status='exited' and the
// reaper skips it. So when no exit_reason was recorded the death was
// unexpected (a crash, an OOM kill, `tclaude session kill`, a reboot),
// and the COALESCE stamps 'unexpected'. An exit_reason already present
// — a narrow race where a real SessionEnd landed first — is preserved.
func MarkSessionExitedIfUnchanged(id, observedStatus string, observedUpdatedAt time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE sessions
		SET status = 'exited', status_detail = '', updated_at = ?,
			exit_reason = COALESCE(exit_reason, 'unexpected')
		WHERE id = ? AND status = ? AND updated_at = ?`,
		time.Now().Format(time.RFC3339Nano),
		id, observedStatus, observedUpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkSessionsIdleAfterInterrupt flips every 'working' session row of a
// conversation back to 'idle', clearing status_detail. It is the
// recovery path for a user-interrupt: when the user cancels an
// in-flight turn with Escape, Claude Code writes a
// "[Request interrupted by user]" marker into the .jsonl but fires NO
// Stop — or any — hook (anthropics/claude-code#11189, closed as
// not-planned), so the session row stays stuck at e.g. status='working',
// status_detail='UserPromptSubmit'.
//
// convops.ScanAndUpsertFile calls this when a .jsonl rescan finds the
// last conversation turn is that marker. The rescan already runs on
// every dashboard poll (RefreshConvIndexEntry), so no extra poller is
// introduced. conv-scoped because a conv can own several session rows
// (resume, auto-registration). Only 'working' rows are touched: an
// 'exited' / 'awaiting_*' / already-'idle' row is left alone, and a
// repeated rescan that finds no 'working' row is a zero-row no-op.
//
// Not a compare-and-swap (unlike MarkSessionExitedIfUnchanged): in the
// narrow window between the .jsonl scan and this UPDATE the user could
// submit a new prompt, whose UserPromptSubmit hook sets status back to
// 'working' — this UPDATE would then flip that genuinely-working row
// to 'idle' for one dashboard poll. That is benign and self-healing
// (the next hook, or the next rescan that now sees the new turn as the
// last, corrects it) and far too tight a race to be worth a CAS guard.
//
// Returns the number of rows flipped.
func MarkSessionsIdleAfterInterrupt(convID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE sessions
		SET status = 'idle', status_detail = '', updated_at = ?
		WHERE conv_id = ? AND status = 'working'`,
		time.Now().Format(time.RFC3339Nano), convID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetSessionExitReason records why a session ended — the `reason` from
// a graceful SessionEnd hook (logout / prompt_input_exit /
// bypass_permissions_disabled / other; clear and resume are non-exits
// and never recorded). It is row-scoped: the SessionEnd
// hook resolves the exact row whose process exited, and SaveSession
// bumps that row's updated_at so stateForConv picks it. It is also
// authoritative — a real SessionEnd overrides any 'unexpected' a reaper
// sweep stamped in a narrow race. Cleared by ClearSessionExitReasonByConv
// when the conversation comes back alive.
func SetSessionExitReason(id, reason string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE sessions SET exit_reason = ? WHERE id = ?`, reason, id)
	return err
}

// ClearSessionExitReasonByConv drops exit_reason back to NULL for EVERY
// session row of a conversation. Called on SessionStart: the conv is
// alive again, so no row of it may keep a stale reason from a previous
// exit. It is conv-scoped, not row-scoped, on purpose — a conv can own
// several session rows (an auto-registered row alongside an older one,
// see FindSessionByConvID), and stateForConv reads exit_reason off
// whichever row is most recent. Clearing only the row the SessionStart
// hook resolved to would strand a stale 'unexpected' on a sibling row
// that a later dashboard read could pick up and misreport as a crash.
func ClearSessionExitReasonByConv(convID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE sessions SET exit_reason = NULL WHERE conv_id = ?`, convID)
	return err
}

// GetSessionExitReason returns the recorded exit_reason for a session,
// or "" when none is recorded (NULL) — a live session, or a row that
// exited before the exit_reason column existed. A "" result must be
// rendered as a plain exit, never as a crash.
func GetSessionExitReason(id string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var reason sql.NullString
	err = d.QueryRow(`SELECT exit_reason FROM sessions WHERE id = ?`, id).Scan(&reason)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return reason.String, nil
}

// SetSessionPendingConv records the conv-id a transition SessionStart
// (source clear / resume / compact) announced as the session's next
// conversation. The hook callback consults it to tell an announced
// conv rotation apart from a foreign process's hooks (a one-shot
// headless claude run inheriting the pane's TCLAUDE_SESSION_ID) — see
// migrateV48toV49. Overwritten by each new announcement; deliberately
// never cleared (a stale UUID can't collide with a future foreign
// conv-id).
func SetSessionPendingConv(id, convID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE sessions SET pending_conv = ? WHERE id = ?`, convID, id)
	return err
}

// GetSessionPendingConv returns the last announced next-conv for a
// session, or "" when no transition has been announced.
func GetSessionPendingConv(id string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var conv string
	err = d.QueryRow(`SELECT pending_conv FROM sessions WHERE id = ?`, id).Scan(&conv)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return conv, nil
}

// UpdateContextPct stores the latest context window usage percentage for a session.
func UpdateContextPct(sessionID string, pct float64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET context_pct = ? WHERE id = ?`, pct, sessionID)
	return err
}

// UpdateContextSnapshot stores the full last-API-response context-window
// snapshot from Claude Code's statusline. Tokens come from the most
// recent API response (input includes cache reads/writes), windowSize
// is the model's actual context limit (200000 or 1000000) — no
// reverse-engineering or per-model lookup needed once this is populated.
//
// All four fields are written together so a partial update can never
// leave the row in a state where pct disagrees with abs counts.
//
// An all-zero snapshot is SKIPPED, not written. Claude Code emits
// statusline renders whose context_window block is empty/absent (e.g.
// before a turn's first API response); those arrive here as
// (0, 0, 0, 0). Writing them would clobber a good snapshot back to
// zero — the bug behind the dashboard context meter flickering empty.
// context_pct is never legitimately 0 for a live session (the system
// prompt + conversation always occupy the window), so an all-zero
// input is unambiguously "no data", not "0% used". This guard lives
// at the DB chokepoint so no caller — present or future — can
// reintroduce the clobber.
func UpdateContextSnapshot(sessionID string, pct float64, tokensInput, tokensOutput, windowSize int64) error {
	if pct == 0 && tokensInput == 0 && tokensOutput == 0 && windowSize == 0 {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions
		SET context_pct = ?, tokens_input = ?, tokens_output = ?, context_window_size = ?
		WHERE id = ?`, pct, tokensInput, tokensOutput, windowSize, sessionID)
	return err
}

// UpdateSessionModel stores the LLM model display name ("Opus 4.8",
// "Sonnet 4.6", …) the session is currently running on. Claude Code's
// statusline carries model.display_name on every render, so the
// statusbar hook records it here keyed by the tclaude session id.
//
// Written separately from UpdateContextSnapshot — and crucially NOT
// gated on the all-zero context guard — because the model is present
// in every statusline render, including the empty-context ones that
// arrive before a turn's first API response. Folding it into the
// snapshot write would mean a freshly-spawned agent shows no model
// until its first response. An empty model is a no-op so a stray
// render without one can never blank a good value.
func UpdateSessionModel(sessionID, model string) error {
	if model == "" {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET model = ? WHERE id = ?`, model, sessionID)
	return err
}

// UpdateSessionModelID stores the full Claude model ID
// ("claude-fable-5", "claude-sonnet-4-6", …) the session is currently
// running on — the statusline's model.id, the machine-facing sibling of
// the display name UpdateSessionModel records. Unlike the display name
// it round-trips into `claude --model`, which is what lets a
// reincarnated / cloned / resumed agent come back on the same model as
// its predecessor (see agentd's inheritedLaunchFlags).
//
// Same write discipline as UpdateSessionModel: written on every
// statusline render (not gated on the all-zero context guard), and an
// empty ID is a no-op so a stray render without one — e.g. an older
// Claude Code that doesn't emit model.id — can never blank a good value.
func UpdateSessionModelID(sessionID, modelID string) error {
	if modelID == "" {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET model_id = ? WHERE id = ?`, modelID, sessionID)
	return err
}

// SessionModels returns the model display name of every session that
// has reported one, keyed by session id — the Costs tab's per-agent
// model lookup. Sessions whose row has since been deleted (kill,
// agent delete) simply aren't in the map; their cost history keeps an
// empty model.
func SessionModels() (map[string]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, model FROM sessions WHERE model <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, model string
		if err := rows.Scan(&id, &model); err != nil {
			return nil, err
		}
		out[id] = model
	}
	return out, rows.Err()
}

// UpdateSessionEffort stores the reasoning-effort level ("low", "medium",
// "high", "xhigh", "max") the session is currently running on. Claude
// Code's statusline carries it as effort.level on every render (when the
// model supports the reasoning-effort parameter), so the statusbar hook
// records it here keyed by the tclaude session id — the sibling write to
// UpdateSessionModel.
//
// An empty level is a no-op, mirroring UpdateSessionModel: the field is
// absent both on renders before a turn's first API response and whenever
// the model lacks reasoning-effort support, and a stray empty render must
// never blank a good value. The rare model→non-effort-model switch
// therefore leaves the last level stale, which is benign for a
// display-only field.
func UpdateSessionEffort(sessionID, level string) error {
	if level == "" {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET effort_level = ? WHERE id = ?`, level, sessionID)
	return err
}

// UpdateSessionCost stores the session's cumulative API cost in USD —
// Claude Code's cost.total_cost_usd from the statusline input. The
// statusbar hook records it here keyed by the tclaude session id, the
// sibling write to UpdateSessionModel — but ONLY when the session runs
// on API/enterprise pricing (no subscription rate-limit buckets in the
// statusline input), mirroring the statusbar's own display gate. On a
// subscription plan this is never called, so the column stays 0 and
// every surface renders "no cost data".
//
// A zero/negative cost is a no-op, mirroring UpdateSessionModel: cost
// is cumulative within a conversation so a real value never decreases,
// and the empty renders before a turn's first API response carry 0 —
// writing that would blank a good value for one poll. After a /clear
// the last pre-clear cost therefore lingers until the new conversation
// accrues its first nonzero cost, which is benign for a display-only
// field (and arguably right: the money was still spent).
func UpdateSessionCost(sessionID string, costUSD float64) error {
	if costUSD <= 0 {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	// One transaction for both writes: the sessions column and the
	// daily snapshot must never diverge on a mid-write failure (the
	// figure would self-heal on the next statusline tick, but there is
	// no reason to allow the window at all).
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE sessions SET cost_usd = ? WHERE id = ?`, costUSD, sessionID); err != nil {
		return err
	}
	// Sibling write: snapshot the cumulative figure onto today's
	// session_cost_daily row, so the Costs tab can recover per-day
	// spend as deltas between consecutive days. INSERT…SELECT keys the
	// write to an existing sessions row — an unknown session id (the
	// UPDATE above no-ops too) must not mint an orphan, attributionless
	// daily row. MAX keeps the row monotonic within the day (cumulative
	// cost never decreases inside a session, but a stale render must
	// never lower a recorded value), and the CASE keeps a previously
	// recorded conv_id when the sessions row has since lost its own.
	// conv_id is denormalised in at write time — the daily history must
	// survive the sessions row being deleted later (session kill, agent
	// delete).
	// updated_at stamps the wall-clock moment of the most recent spend
	// on this (session, day) row: set on insert, and refreshed on
	// conflict only when the new cumulative figure actually exceeds the
	// stored one — a stale render at an equal/lower cost is real
	// activity for cost purposes only if it raised the total, so an idle
	// session whose statusline keeps ticking never bumps its
	// last-activity time. Powers the Costs tab's last-activity column.
	now := time.Now()
	_, err = tx.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, updated_at)
		SELECT id, ?, conv_id, ?, ? FROM sessions WHERE id = ?
		ON CONFLICT(session_id, day) DO UPDATE SET
			updated_at = CASE WHEN excluded.cost_usd > session_cost_daily.cost_usd
			                  THEN excluded.updated_at ELSE session_cost_daily.updated_at END,
			cost_usd = MAX(session_cost_daily.cost_usd, excluded.cost_usd),
			conv_id  = CASE WHEN excluded.conv_id <> '' THEN excluded.conv_id
			                ELSE session_cost_daily.conv_id END`,
		now.Format(costDayFormat), costUSD, now.Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// costDayFormat is the session_cost_daily.day key — a local-time
// calendar date. Local because the human reads the Costs chart in
// their own day boundaries, matching the migration backfill's
// date('now','localtime').
const costDayFormat = "2006-01-02"

// CostDailyRow is one (session, day) snapshot from session_cost_daily:
// the highest cumulative cost the session had reported as of that
// local day. ConvID groups sessions into agents; it's denormalised at
// write time so it survives the sessions row's deletion.
type CostDailyRow struct {
	SessionID string
	Day       string // local "2006-01-02"
	ConvID    string
	CostUSD   float64 // cumulative within the session as of that day
	UpdatedAt string // RFC3339Nano of the day's last spend; "" if unknown
}

// SumCostSinceDay totals the actual spend recorded on or after fromDay
// (a "2006-01-02" key) — the top bar's month-to-date figure, computed
// DB-side so the 2s snapshot tick never scans cost history into Go.
//
// Per session, spend within the window is the peak cumulative snapshot
// in the window minus the high-water mark before it, clamped at zero —
// the closed form of the day-by-day delta walk agentd's Costs tab
// aggregation performs (rises above a running maximum telescope to
// final-peak − initial-peak), so the two surfaces always agree. The
// agentd unit tests pin both to the same fixture.
func SumCostSinceDay(fromDay string) (float64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	var total float64
	err = db.QueryRow(`
		SELECT COALESCE(SUM(MAX(0, w.peak - COALESCE(b.base, 0))), 0)
		FROM (SELECT session_id, MAX(cost_usd) AS peak
		      FROM session_cost_daily WHERE day >= ? GROUP BY session_id) w
		LEFT JOIN (SELECT session_id, MAX(cost_usd) AS base
		           FROM session_cost_daily WHERE day < ? GROUP BY session_id) b
		  ON b.session_id = w.session_id`, fromDay, fromDay).Scan(&total)
	return total, err
}

// AllCostDailyRows returns every session_cost_daily row ordered by
// (session_id, day) — the order the cost aggregation walks to turn
// cumulative snapshots into per-day deltas. The table stays small
// (sessions × active days, API-priced sessions only), so callers read
// it whole and aggregate in Go rather than encoding the windowed
// delta logic in SQL.
func AllCostDailyRows() ([]CostDailyRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT session_id, day, conv_id, cost_usd, updated_at
		FROM session_cost_daily ORDER BY session_id, day`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CostDailyRow
	for rows.Next() {
		var r CostDailyRow
		if err := rows.Scan(&r.SessionID, &r.Day, &r.ConvID, &r.CostUSD, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ContextSnapshot is the full context-window state for a session.
// Zero values mean "not populated yet" — caller should fall back to
// the percentage-only display.
type ContextSnapshot struct {
	ContextPct        float64
	TokensInput       int64
	TokensOutput      int64
	ContextWindowSize int64
	CompactPending    float64
	// Model is the LLM model display name the session last reported
	// running on (from the statusline hook). "" until the statusbar
	// has ticked at least once. Rides on the same row read so the
	// dashboard gets it with no extra query.
	Model string
	// ModelID is the full Claude model ID ("claude-fable-5", …) behind
	// Model — the statusline's model.id, recorded by the same hook. ""
	// until the statusbar has ticked, or when Claude Code predates the
	// field. Unlike Model it can be passed back to `claude --model`;
	// reincarnate / clone / resume read it to keep the successor on the
	// predecessor's model.
	ModelID string
	// EffortLevel is the reasoning-effort level the session last
	// reported ("low"…"max"), from the same statusline hook. "" until
	// the statusbar has ticked, or when the model lacks reasoning-effort
	// support. Rides on the same row read as Model.
	EffortLevel string
	// CostUSD is the session's cumulative API cost in USD, recorded by
	// the statusline hook only on API/enterprise pricing (no
	// subscription rate-limit data). 0 means "no cost data" — a
	// subscription-plan session, or a statusbar that hasn't ticked —
	// and surfaces render nothing for it. Rides on the same row read.
	CostUSD float64
}

// GetContextSnapshot reads the full context-window state for a
// session. Returns zero values when the row isn't found.
func GetContextSnapshot(sessionID string) (ContextSnapshot, error) {
	db, err := Open()
	if err != nil {
		return ContextSnapshot{}, err
	}
	var s ContextSnapshot
	err = db.QueryRow(
		`SELECT context_pct, tokens_input, tokens_output, context_window_size, compact_pending, model, model_id, effort_level, cost_usd
		 FROM sessions WHERE id = ?`, sessionID).
		Scan(&s.ContextPct, &s.TokensInput, &s.TokensOutput, &s.ContextWindowSize, &s.CompactPending, &s.Model, &s.ModelID, &s.EffortLevel, &s.CostUSD)
	return s, err
}

// TryClaimCompact atomically sets compact_pending to the current unix timestamp
// if it is currently 0. Returns true if the claim was made (caller should send /compact).
func TryClaimCompact(sessionID string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	now := float64(time.Now().Unix())
	result, err := db.Exec(
		`UPDATE sessions SET compact_pending = ? WHERE id = ? AND compact_pending = 0`,
		now, sessionID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// ResetCompact clears compact_pending and zeroes context_pct for a session.
// Also zeroes nudged_pct so a compacted session can be re-nudged from
// scratch as its context climbs again.
func ResetCompact(sessionID string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions
		SET compact_pending = 0, context_pct = 0, nudged_pct = 0
		WHERE id = ?`, sessionID)
	return err
}

// GetCompactState returns the context_pct and compact_pending values for a session.
func GetCompactState(sessionID string) (contextPct float64, compactPending float64, err error) {
	db, err := Open()
	if err != nil {
		return 0, 0, err
	}
	err = db.QueryRow(`SELECT context_pct, compact_pending FROM sessions WHERE id = ?`, sessionID).
		Scan(&contextPct, &compactPending)
	return
}

// GetNudgedPct returns the highest threshold the context-nudge path
// has already fired for this session. 0 when the session has never
// been nudged or has been freshly compacted.
func GetNudgedPct(sessionID string) (float64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	var pct float64
	err = db.QueryRow(`SELECT nudged_pct FROM sessions WHERE id = ?`, sessionID).Scan(&pct)
	return pct, err
}

// SetNudgedPct stamps the highest-threshold-already-fired value
// after a successful nudge. Subsequent ticks at the same threshold
// no-op; the next climb beyond this value re-arms the nudge.
func SetNudgedPct(sessionID string, pct float64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET nudged_pct = ? WHERE id = ?`, pct, sessionID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
