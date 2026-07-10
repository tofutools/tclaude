package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SessionRow represents a session row in the database.
type SessionRow struct {
	ID           string
	TmuxSession  string
	PID          int
	Cwd          string
	ConvID       string
	Status       string
	StatusDetail string
	// SubagentCount is the number of sub-agents believed to be running
	// under this session right now. It is a derived cache of SubagentsJSON
	// (recomputed on every hook write); read surfaces that can tolerate a
	// TTL-filtered view should prefer ParseSubagentSet(SubagentsJSON).
	// LiveCount over this raw figure — see SubagentSet in subagents.go.
	SubagentCount int
	// SubagentsJSON is the persisted sub-agent ledger (SubagentSet JSON,
	// "" = empty/never written). Owned by the hook callback; cleared at
	// known-zero boundaries (session exit, the .jsonl interrupt marker).
	SubagentsJSON  string
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
	// AskUserQuestionTimeout is the resolved Claude Code AskUserQuestion
	// idle-timeout (inherit|never|60s|5m|10m) the session was spawned under,
	// recorded once at spawn by `session new` so a relaunch (resume / clone /
	// reincarnate) can PRESERVE it — unlike sandbox/approval, which re-default
	// on relaunch, the operator wants a per-agent timeout carried across the
	// handoff (schema v97). "" for a pre-column row or a harness with no
	// AskUserQuestion dialog; stored verbatim like SandboxMode.
	AskUserQuestionTimeout string
	// RemoteControl is tclaude's best-known state of whether the harness's
	// built-in remote access (Claude Code's /remote-control) is ON for this
	// live session. CC exposes no programmatic readback, so tclaude tracks
	// it: the recorded flag decides whether the next toggle injection
	// enables or disables. Written out-of-band (SetSessionRemoteControl),
	// NOT by SaveSession's UPSERT, so a hook tick that builds a SessionRow
	// without setting this can't clobber it back to false — the same
	// discipline the context-window columns use (schema v65, JOH-256).
	RemoteControl bool
}

// SaveSession inserts or updates a session, setting updated_at to now.
//
// On an existing row this is an UPSERT that writes ONLY the columns
// SaveSession owns (the state-tracking set: id … harness, sandbox_mode,
// ask_user_question_timeout).
// It deliberately does NOT touch the
// context-window columns (context_pct, tokens_input, tokens_output,
// context_window_size) or the nudge bookkeeping (nudged_pct). Those are
// out-of-band: owned by the statusline hook (UpdateContextSnapshot) and
// the context-nudge path, written on a different cadence from the
// state-tracking hooks that call SaveSession on every tick.
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

	// agent_id is dual-written from conv_id. A session row is often created
	// before its conv enrolls (the hook registers the agent slightly later), so
	// this derivation yields '' at first insert; enrollment then fills it via
	// propagateAgentCompanions. The conflict guard re-derives only when the new
	// value is non-empty, so a later status-update upsert (whose conv may not
	// resolve) never wipes an agent already known for this session.
	_, err = db.Exec(`INSERT INTO sessions
		(id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, subagents_json, auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, agent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+agentForConvExpr+`)
		ON CONFLICT(id) DO UPDATE SET
			tmux_session = excluded.tmux_session,
			pid = excluded.pid,
			cwd = excluded.cwd,
			conv_id = excluded.conv_id,
			status = excluded.status,
			status_detail = excluded.status_detail,
			subagent_count = excluded.subagent_count,
			subagents_json = excluded.subagents_json,
			auto_registered = excluded.auto_registered,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			last_hook = excluded.last_hook,
			harness = excluded.harness,
			sandbox_mode = excluded.sandbox_mode,
			ask_user_question_timeout = excluded.ask_user_question_timeout,
			agent_id = CASE WHEN excluded.agent_id <> '' THEN excluded.agent_id ELSE sessions.agent_id END`,
		s.ID, s.TmuxSession, s.PID, s.Cwd, s.ConvID,
		s.Status, s.StatusDetail, s.SubagentCount, s.SubagentsJSON, boolToInt(s.AutoRegistered),
		s.CreatedAt.Format(time.RFC3339Nano), s.UpdatedAt.Format(time.RFC3339Nano), s.LastHook.Format(time.RFC3339Nano), harness, s.SandboxMode, s.AskUserQuestionTimeout, s.ConvID)
	return err
}

// LoadSession loads a session by primary key.
func LoadSession(id string) (*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, subagents_json,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, remote_control FROM sessions WHERE id = ?`, id)
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
	rows, err := db.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, subagents_json,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, remote_control FROM sessions`)
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
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, subagents_json,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, remote_control FROM sessions WHERE conv_id = ?
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
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, subagents_json,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, remote_control FROM sessions WHERE pid = ?
		ORDER BY updated_at DESC LIMIT 1`, pid)
	s, err := scanSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// SessionLaunchProfile is the observable launch shape of a conv's
// most-recent session — what a from-group template snapshot re-traces per
// member (JOH-239) so a round-trip preserves each role's launch fields.
//
// ModelID is the resume-safe full model ID (sessions.model_id, e.g.
// "claude-fable-5"), NOT the display alias (sessions.model, "Opus 4.8") —
// only the ID passes ValidateModel and is what reincarnate/clone forward.
// Harness / SandboxMode are spawn-recorded; Effort is statusline-reported.
// Any field can be "" ("not observed" — e.g. a session that hasn't ticked
// the statusline has no model/effort yet, or a harness with no sandbox flag).
type SessionLaunchProfile struct {
	Harness     string
	ModelID     string
	Effort      string
	SandboxMode string
}

// SessionLaunchProfileForConv reads the launch fields of a conv's most-recent
// session row. Returns a zero-value profile (no error) when the conv has no
// session row, so a snapshot of a member whose row was pruned degrades to "no
// overrides" rather than failing.
func SessionLaunchProfileForConv(convID string) (SessionLaunchProfile, error) {
	d, err := Open()
	if err != nil {
		return SessionLaunchProfile{}, err
	}
	var p SessionLaunchProfile
	err = d.QueryRow(
		`SELECT harness, model_id, effort_level, sandbox_mode FROM sessions
		 WHERE conv_id = ? ORDER BY updated_at DESC LIMIT 1`, convID).
		Scan(&p.Harness, &p.ModelID, &p.Effort, &p.SandboxMode)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionLaunchProfile{}, nil
	}
	if err != nil {
		return SessionLaunchProfile{}, err
	}
	return p, nil
}

// FindSessionsByConvID returns every row for the given conv_id, most
// recently updated first. Used by the agent daemon to find a row whose
// tmux session is actually alive when several stale rows coexist.
func FindSessionsByConvID(convID string) ([]*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, subagents_json,
		auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, remote_control FROM sessions WHERE conv_id = ?
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
	var autoReg, remoteCtl int
	var createdStr, updatedStr, lastHookStr string
	err := row.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
		&s.Status, &s.StatusDetail, &s.SubagentCount, &s.SubagentsJSON, &autoReg, &createdStr, &updatedStr, &lastHookStr, &s.Harness, &s.SandboxMode, &s.AskUserQuestionTimeout, &remoteCtl)
	if err != nil {
		return nil, err
	}
	s.AutoRegistered = autoReg != 0
	s.RemoteControl = remoteCtl != 0
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
		var autoReg, remoteCtl int
		var createdStr, updatedStr, lastHookStr string
		err := rows.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
			&s.Status, &s.StatusDetail, &s.SubagentCount, &s.SubagentsJSON, &autoReg, &createdStr, &updatedStr, &lastHookStr, &s.Harness, &s.SandboxMode, &s.AskUserQuestionTimeout, &remoteCtl)
		if err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}
		s.AutoRegistered = autoReg != 0
		s.RemoteControl = remoteCtl != 0
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
// The caller supplies fallbackExitReason, which is used only when no
// reason was already recorded. Passing "" leaves exit_reason NULL:
// useful for harnesses such as Codex where a normal close can have no
// SessionEnd-style hook. An exit_reason already present — a narrow race
// where a real SessionEnd or daemon soft-stop landed first — is
// preserved.
func MarkSessionExitedIfUnchanged(id, observedStatus string, observedUpdatedAt time.Time, fallbackExitReason string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE sessions
		SET status = 'exited', status_detail = '', updated_at = ?,
			subagent_count = 0, subagents_json = '',
			exit_reason = COALESCE(exit_reason, NULLIF(?, ''))
		WHERE id = ? AND status = ? AND updated_at = ?`,
		time.Now().Format(time.RFC3339Nano),
		fallbackExitReason, id, observedStatus, observedUpdatedAt.Format(time.RFC3339Nano))
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
	// The interrupt also aborted any in-flight foreground Task calls, and
	// Claude Code fires no SubagentStop for them either — so the sub-agent
	// ledger is cleared here too, or every Esc mid-Task would leave a
	// phantom "🤖+N" badge until the TTL sweeps it. A background sub-agent
	// that genuinely survives the interrupt re-adds itself via Sight() on
	// its next hook (see SubagentSet in subagents.go).
	res, err := d.Exec(`UPDATE sessions
		SET status = 'idle', status_detail = '', updated_at = ?,
			subagent_count = 0, subagents_json = ''
		WHERE conv_id = ? AND status = 'working'`,
		time.Now().Format(time.RFC3339Nano), convID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetSessionExitReason records why a session ended — usually the
// `reason` from a graceful SessionEnd hook (logout / prompt_input_exit /
// bypass_permissions_disabled / other), or a daemon-owned clean reason
// when a harness has no SessionEnd-style shutdown hook. It is row-scoped:
// the caller resolves the exact row whose process exited, and SaveSession
// or the reaper bumps that row's updated_at so stateForConv picks it. It
// is also authoritative — a real SessionEnd overrides any 'unexpected' a
// reaper sweep stamped in a narrow race. Cleared by
// ClearSessionExitReasonByConv when the conversation comes back alive.
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

// SetSessionConvID sets a session row's conv_id directly. The daemon's spawn
// path uses it to record a conv-id discovered from the harness's conv store
// for a harness (Codex) that does not report its conv-id through an immediate
// launch hook — so the row is linked at launch instead of only when the first
// user turn finally fires a hook. The hook callback later writes the same
// conv-id (keyed by the session's TCLAUDE_SESSION_ID), so this is idempotent
// with the hook path. Mirrors SetSessionPendingConv: conv_id only, no other
// columns touched.
func SetSessionConvID(id, convID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	// Keep agent_id in step with conv_id, but never wipe a known agent with ''
	// when the freshly-set conv has not enrolled yet (enrollment fills it).
	_, err = d.Exec(`UPDATE sessions SET conv_id = ?,
		agent_id = CASE WHEN `+agentForConvExpr+` <> '' THEN `+agentForConvExpr+` ELSE agent_id END
		WHERE id = ?`, convID, convID, convID, id)
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

// UpdateStatuslineSnapshot stores the verbatim raw JSON of the most recent
// statusline callback for a session onto sessions.last_statusline_json,
// keyed by the tclaude session id — overwritten every render (latest-wins,
// never appended). It captures the FULL harness payload, including fields
// StatusLineInput doesn't name (Go's decoder drops unknown keys), so a newly
// shipped usage bucket — e.g. Fable 5's separate limit — is preserved for
// inspection off the DB even though nothing in code deserialises it yet.
//
// A plain UPDATE — a no-op when the session row is absent, mirroring the other
// statusbar writers (UpdateSessionModel/Cost/…). An empty payload is skipped so
// a stray render with nothing to store can't blank a good snapshot. The column
// is write-only by design: it is read by hand off the DB, so there is no getter.
func UpdateStatuslineSnapshot(sessionID, rawJSON string) error {
	if rawJSON == "" {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET last_statusline_json = ? WHERE id = ?`, rawJSON, sessionID)
	return err
}

// SetSessionRemoteControl records tclaude's best-known remote-control state
// for a session, keyed by session id — the same out-of-band discipline as
// UpdateContextPct: a targeted UPDATE that SaveSession's UPSERT never writes,
// so a state-tracking hook tick can't reset it to its column default. The
// agentd toggle path sets this only AFTER a successful /remote-control
// injection, so the recorded flag stays in step with what was actually typed
// into the pane. See JOH-256 / JOH-257.
func SetSessionRemoteControl(sessionID string, on bool) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET remote_control = ? WHERE id = ?`, boolToInt(on), sessionID)
	return err
}

// RemoteControlForConv reports tclaude's best-known remote-control state for
// a conversation: the flag on its most-recently-updated session row (the row
// FindSessionByConvID resolves). false (no error) when the conv has no
// session row. The toggle direction logic reads SessionRow.RemoteControl off
// the resolved ALIVE row directly; this is the convenience reader for the CLI
// `status` verb and the dashboard payload. See JOH-256.
func RemoteControlForConv(convID string) (bool, error) {
	s, err := FindSessionByConvID(convID)
	if err != nil || s == nil {
		return false, err
	}
	return s.RemoteControl, nil
}

// AskTimeoutForConv returns the resolved AskUserQuestion idle-timeout recorded
// on a conv's most-recent session row (schema v97), or "" when none is stored
// (a pre-column row, a non-Claude harness, or an un-chosen timeout). A relaunch
// (resume / clone / reincarnate) reads it to PRESERVE the per-agent timeout —
// the sibling of RemoteControlForConv (which preserves remote-access state), and
// deliberately unlike sandboxForHarness / approvalForHarness, which re-default.
func AskTimeoutForConv(convID string) (string, error) {
	s, err := FindSessionByConvID(convID)
	if err != nil || s == nil {
		return "", err
	}
	return s.AskUserQuestionTimeout, nil
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

// UpdateSessionModelSlug stores a model token that is both the human-facing
// model name and the machine-facing resume ID. Codex hook payloads have this
// shape: their `model` field is the active model slug, so the dashboard's
// display value and the lifecycle inheritance value must advance together.
//
// Keeping this as one UPDATE avoids a partially-refreshed row if a process is
// interrupted between the two writes. Claude Code continues using the two
// independent setters above because its statusline reports distinct display
// and ID fields. An empty slug is a no-op, matching those setters.
func UpdateSessionModelSlug(sessionID, model string) error {
	if model == "" {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET model = ?, model_id = ? WHERE id = ?`, model, model, sessionID)
	return err
}

// SessionModels returns the model display name of every session that
// has reported one, keyed by session id — the Costs tab's per-agent
// model lookup. Since v71 the model is denormalised onto each
// session_cost_daily row, so this is now only the FALLBACK for pre-v71
// history of a still-alive session; a deleted session's history carries
// its own denormalised model. Sessions whose row has since been deleted
// simply aren't in the map.
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

// SessionHarnesses returns the harness of every live session, keyed by session
// id. Since v103 the harness is denormalised onto each session_cost_daily row,
// so this is only the fallback for pre-v103 cost history that still has a live
// sessions row.
func SessionHarnesses() (map[string]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, COALESCE(NULLIF(harness, ''), 'claude') FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, harness string
		if err := rows.Scan(&id, &harness); err != nil {
			return nil, err
		}
		out[id] = harness
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
	// conv_id and model are denormalised in at write time — the daily
	// history must survive the sessions row being deleted later (session
	// kill, agent delete), and model is what the Costs tab's per-agent
	// breakdown names; resolving it live against the sessions row blanked
	// the column the instant that row was gone.
	// updated_at stamps the wall-clock moment of the most recent spend
	// on this (session, day) row: set on insert, and refreshed on
	// conflict only when the new cumulative figure actually exceeds the
	// stored one — a stale render at an equal/lower cost is real
	// activity for cost purposes only if it raised the total, so an idle
	// session whose statusline keeps ticking never bumps its
	// last-activity time. Powers the Costs tab's last-activity column.
	// The model CASE mirrors conv_id: a render that carries no model
	// (the empty-context ones before a turn's first response) keeps the
	// last good value rather than blanking it.
	now := time.Now()
	// agent_id is denormalised in alongside conv_id, with the same keep-last-good
	// CASE guard. Prefer the session's persisted agent_id (the v77 companion,
	// propagated by enrollment) and fall back to deriving it from the session's
	// conv via agent_conversations — the persisted column survives a /clear or
	// clone that moves the conv's actor mapping, which the live conv lookup alone
	// would miss.
	_, err = tx.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, updated_at, model, agent_id, harness)
		SELECT id, ?, conv_id, ?, ?, model,
		       COALESCE(NULLIF(sessions.agent_id, ''),
		                (SELECT agent_id FROM agent_conversations WHERE conv_id = sessions.conv_id), ''),
		       COALESCE(NULLIF(harness, ''), 'claude')
		FROM sessions WHERE id = ?
		ON CONFLICT(session_id, day) DO UPDATE SET
			updated_at = CASE WHEN excluded.cost_usd > session_cost_daily.cost_usd
			                  THEN excluded.updated_at ELSE session_cost_daily.updated_at END,
			cost_usd = MAX(session_cost_daily.cost_usd, excluded.cost_usd),
			conv_id  = CASE WHEN excluded.conv_id <> '' THEN excluded.conv_id
			                ELSE session_cost_daily.conv_id END,
			model    = CASE WHEN excluded.model <> '' THEN excluded.model
			                ELSE session_cost_daily.model END,
			agent_id = CASE WHEN excluded.agent_id <> '' THEN excluded.agent_id
			                ELSE session_cost_daily.agent_id END,
			harness  = CASE WHEN excluded.harness <> '' THEN excluded.harness
			                ELSE session_cost_daily.harness END`,
		now.Format(costDayFormat), costUSD, now.Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateSessionVirtualCost stores the session's cumulative pay-per-token-
// EQUIVALENT cost in USD — the WHAT-IF sibling of UpdateSessionCost. Claude
// Code emits cost.total_cost_usd on every statusline render regardless of
// billing mode; the statusbar hook records it HERE (not via UpdateSessionCost)
// when the session runs on a SUBSCRIPTION — i.e. the statusline carries
// rate-limit buckets, the inverse of UpdateSessionCost's gate. So a given
// session normally populates virtual_cost_usd or cost_usd, not both (billing
// mode is stable per account); the rare exception is an account whose
// rate-limit state flips mid-session, which could leave both columns
// non-zero — harmless, since the two delta walks read one column each and
// HasAnyRealCost lets real spend win for tab visibility. The Costs tab's
// WHAT-IF view runs the same per-day delta walk over the virtual column
// that the real view runs over cost_usd. The recorded figure is hypothetical
// ("what this would have cost on pay-per-token"), never a real charge — the
// dashboard only surfaces it behind the cost.show_on_subscription opt-in.
//
// Byte-for-byte the same transactional shape as UpdateSessionCost (zero/
// negative no-op; sessions column + monotonic session_cost_daily upsert in one
// tx; INSERT…SELECT keyed to an existing row so an unknown id mints no orphan);
// only the target column differs.
func UpdateSessionVirtualCost(sessionID string, costUSD float64) error {
	if costUSD <= 0 {
		return nil
	}
	db, err := Open()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE sessions SET virtual_cost_usd = ? WHERE id = ?`, costUSD, sessionID); err != nil {
		return err
	}
	now := time.Now()
	_, err = tx.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, virtual_cost_usd, updated_at, model, agent_id, harness)
		SELECT id, ?, conv_id, ?, ?, model,
		       COALESCE(NULLIF(sessions.agent_id, ''),
		                (SELECT agent_id FROM agent_conversations WHERE conv_id = sessions.conv_id), ''),
		       COALESCE(NULLIF(harness, ''), 'claude')
		FROM sessions WHERE id = ?
		ON CONFLICT(session_id, day) DO UPDATE SET
			updated_at = CASE WHEN excluded.virtual_cost_usd > session_cost_daily.virtual_cost_usd
			                  THEN excluded.updated_at ELSE session_cost_daily.updated_at END,
			virtual_cost_usd = MAX(session_cost_daily.virtual_cost_usd, excluded.virtual_cost_usd),
			conv_id  = CASE WHEN excluded.conv_id <> '' THEN excluded.conv_id
			                ELSE session_cost_daily.conv_id END,
			model    = CASE WHEN excluded.model <> '' THEN excluded.model
			                ELSE session_cost_daily.model END,
			agent_id = CASE WHEN excluded.agent_id <> '' THEN excluded.agent_id
			                ELSE session_cost_daily.agent_id END,
			harness  = CASE WHEN excluded.harness <> '' THEN excluded.harness
			                ELSE session_cost_daily.harness END`,
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
	// VirtualCostUSD is the WHAT-IF sibling of CostUSD: the cumulative
	// pay-per-token-equivalent cost captured on a subscription session
	// (see UpdateSessionVirtualCost). Normally exclusive with CostUSD per
	// session — one is populated, the other 0 — so the Costs tab's WHAT-IF
	// view runs the same delta walk over this column. (See
	// UpdateSessionVirtualCost for the rare mid-session-billing-flip case.)
	VirtualCostUSD float64
	UpdatedAt      string // RFC3339Nano of the day's last spend; "" if unknown
	// Model is the LLM model display name the session reported, denormalised
	// onto the row at write time (the model sibling of ConvID) so the Costs
	// tab's per-agent breakdown keeps naming a retired agent's model after
	// its sessions row is deleted. "" for pre-v71 history of an
	// already-deleted session, or a session that never reported a model.
	Model   string
	Harness string
}

// CostDelta is one recovered slice of actual spend: on this local day,
// the conversation spent this many dollars. Derived from consecutive
// cumulative snapshots, baselined per conversation (see CostDeltas). The
// fields are the CostDailyRow fields the cost surfaces attribute a slice
// on — day, actor keys, the timestamp and the model — minus the raw
// cumulative the walk consumes.
type CostDelta struct {
	Day       string
	ConvID    string
	SessionID string
	USD       float64
	UpdatedAt string // RFC3339Nano of the day's last spend; "" if unknown
	Model     string // model display name denormalised onto the row; "" if unknown
	Harness   string // harness denormalised onto the row; "" if unknown/pre-v103
}

// CostDeltas turns cumulative (conv, day) snapshots into per-day spend
// deltas — the ONE walk both cost surfaces build on (the Costs tab via
// agentd's costDeltasFromRows, the top bar via SumCostSinceDay), so the
// headline figure and the tab's breakdown can never drift. Rows must be
// ordered (conv-key, day, updated_at, session_id) — the order
// AllCostDailyRows returns.
//
// The tie-break WITHIN a (conv, day) is chronological (updated_at, the
// last-spend time), NOT lexical session_id, and this matters: the
// session-boundary reset below assumes a carry-forward resume is monotonic
// — a statement about TIME, so the walk must see a conv's sessions in the
// order they actually spent. A reinstated agent resumes under a session
// whose id equals the conv id, which sorts BEFORE the original spwn- session
// lexically but AFTER it in time; ordering by session_id would process the
// resume (higher cumulative) first, then read the earlier original as a
// below-peak drop and reset — double-counting the whole overlap (the
// reinstate double-count: a conv opened at $43 showing $85). Ordering by
// updated_at puts the original first, so the carry-forward telescopes to its
// rise as intended.
//
// The high-water baseline is carried per CONVERSATION, not per session:
// Claude Code's total_cost_usd is cumulative within a session and, on a
// carry-forward resume, the resuming session's first snapshot already
// includes the prior spend. Baselining per conv recovers only the genuine
// rise, so a resume's first day no longer re-counts the whole cumulative
// (the multi-day / same-day double-count). The conv-key falls back to
// session_id for the rare row with no denormalised conv_id, so unrelated
// sessions never merge. A day's spend is its snapshot minus the running
// high-water mark; a conversation's first day carries its whole cumulative
// (for rows born in the v51 backfill, pre-existing history lands on the
// migration day). The high-water baseline clamps a dip-and-recover: only
// the rise above the previous maximum counts, never a negative day.
//
// BUT total_cost_usd is a PER-SESSION counter, and a resume after the
// prior session has EXITED starts a fresh one — the new session's
// cumulative begins near zero, BELOW the conversation's prior peak,
// rather than carrying it forward. Clamping that to the old high-water
// mark would swallow the new session's entire spend, so the conversation
// vanishes from every span after the one holding its first (higher-cost)
// session — the cross-month "agent only shows in the previous month" bug.
// So at a SESSION boundary (session_id changed within the same conv) where
// the cumulative DROPS below the baseline, restart the baseline: that drop
// can only be a fresh independent counter (a carry-forward resume is
// monotonic and stays at or above the peak), so its spend is counted from
// scratch. The reset is gated on the session change — a dip WITHIN a
// session is still a stale render and stays clamped, never a reset.
//
// whatif selects which cumulative column the walk reads: false → cost_usd
// (real pay-per-token spend), true → virtual_cost_usd (the subscription
// WHAT-IF estimate). The delta logic is identical — virtual cost is the
// same cumulative total_cost_usd, just captured on the subscription path.
func CostDeltas(rows []CostDailyRow, whatif bool) []CostDelta {
	var out []CostDelta
	prevKey := ""
	prevSession := ""
	baseline := 0.0
	for _, r := range rows {
		val := r.CostUSD
		if whatif {
			val = r.VirtualCostUSD
		}
		key := r.ConvID
		if key == "" {
			key = r.SessionID
		}
		switch {
		case key != prevKey:
			// New conversation — restart the baseline.
			baseline = 0
		case r.SessionID != prevSession && val < baseline:
			// Same conv, new session, cumulative below the running peak: a
			// fresh per-session counter (resume-after-exit), not a
			// carry-forward. Count it from scratch rather than clamping its
			// whole spend away.
			baseline = 0
		}
		prevKey = key
		prevSession = r.SessionID
		if d := val - baseline; d > 0 {
			out = append(out, CostDelta{Day: r.Day, ConvID: r.ConvID, SessionID: r.SessionID, USD: d, UpdatedAt: r.UpdatedAt, Model: r.Model, Harness: r.Harness})
			baseline = val
		}
	}
	return out
}

// SumCostSinceDay totals the actual spend recorded on or after fromDay
// (a "2006-01-02" key) — the top bar's month-to-date figure.
//
// It runs the SAME per-conversation delta walk (CostDeltas) the Costs tab
// aggregates, summing the slices whose day is in the window, so the
// headline number and the tab's breakdown always agree — the agentd unit
// tests pin both surfaces to one fixture. It reads the whole
// session_cost_daily table (small — sessions × active days) and walks it
// in Go rather than a closed-form aggregate query: the walk's
// session-boundary baseline reset (a resume-after-exit's fresh per-session
// counter, see CostDeltas) is path-dependent and has no clean SQL closed
// form, and the scan is negligible even on the 2s snapshot tick.
func SumCostSinceDay(fromDay string) (float64, error) {
	rows, err := AllCostDailyRows()
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, d := range CostDeltas(rows, false) {
		if d.Day >= fromDay {
			total += d.USD
		}
	}
	return total, nil
}

// AllCostDailyRows returns every session_cost_daily row ordered by
// (conv-key, day, updated_at, session_id) — the order the cost aggregation
// walks to turn cumulative snapshots into per-day deltas. The conv-key groups
// all of a conversation's sessions together (falling back to session_id for
// the rare row with no denormalised conv_id) so the per-day delta walk can
// carry one high-water baseline across a resume; day then updated_at orders
// within a conversation CHRONOLOGICALLY by last-spend time. That chronological
// tie-break — not lexical session_id — is what CostDeltas's carry-forward /
// session-reset logic depends on: a reinstated agent's resumed session (id ==
// conv id) sorts before the original spwn- session lexically but must be
// walked after it, or its carry-forward looks like a below-peak drop and gets
// double-counted (see CostDeltas). session_id remains the final tiebreaker for
// a deterministic order when two rows share a timestamp. The table stays small
// (sessions × active days — pay-per-token sessions via cost_usd, subscription
// sessions via virtual_cost_usd), so callers read it whole and aggregate in Go
// rather than encoding the windowed delta logic in SQL.
func AllCostDailyRows() ([]CostDailyRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT session_id, day, conv_id, cost_usd, virtual_cost_usd, updated_at, model, harness
		FROM session_cost_daily
		ORDER BY COALESCE(NULLIF(conv_id, ''), session_id), day, updated_at, session_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CostDailyRow
	for rows.Next() {
		var r CostDailyRow
		if err := rows.Scan(&r.SessionID, &r.Day, &r.ConvID, &r.CostUSD, &r.VirtualCostUSD, &r.UpdatedAt, &r.Model, &r.Harness); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasAnyRealCost reports whether any REAL pay-per-token spend has ever been
// recorded (a session_cost_daily row with cost_usd > 0). It is the dashboard's
// "is this account on pay-per-token" signal: pay-per-token sessions populate
// cost_usd (via UpdateSessionCost), subscription sessions populate only
// virtual_cost_usd, so a true result means the Costs tab has real money to
// show. Drives the Costs-tab auto-hide (a subscription-only account has no real
// cost and hides the tab unless cost.show_on_subscription opts into the WHAT-IF
// view). Cheap EXISTS probe — safe on the 2s snapshot tick.
func HasAnyRealCost() (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	var exists int
	err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_cost_daily WHERE cost_usd > 0)`).Scan(&exists)
	return exists == 1, err
}

// ContextSnapshot is the full context-window state for a session.
// Zero values mean "not populated yet" — caller should fall back to
// the percentage-only display.
type ContextSnapshot struct {
	ContextPct        float64
	TokensInput       int64
	TokensOutput      int64
	ContextWindowSize int64
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
	// VirtualCostUSD is the WHAT-IF sibling of CostUSD: the cumulative
	// pay-per-token-equivalent cost recorded on a SUBSCRIPTION session
	// (UpdateSessionVirtualCost). 0 on a pay-per-token session or before
	// the statusbar has ticked. The dashboard's Groups tab shows it as the
	// per-agent badge when the WHAT-IF view is active, flagged hypothetical.
	VirtualCostUSD float64
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
		`SELECT context_pct, tokens_input, tokens_output, context_window_size, model, model_id, effort_level, cost_usd, virtual_cost_usd
		 FROM sessions WHERE id = ?`, sessionID).
		Scan(&s.ContextPct, &s.TokensInput, &s.TokensOutput, &s.ContextWindowSize, &s.Model, &s.ModelID, &s.EffortLevel, &s.CostUSD, &s.VirtualCostUSD)
	return s, err
}

// ResetCompact zeroes context_pct and nudged_pct for a session after a
// compaction. Zeroing nudged_pct lets a compacted session be re-nudged
// from scratch as its context climbs again; zeroing context_pct clears
// the pre-compaction figure until the statusline hook reports the new,
// smaller one.
func ResetCompact(sessionID string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions
		SET context_pct = 0, nudged_pct = 0
		WHERE id = ?`, sessionID)
	return err
}

// GetContextPct returns the stored context_pct for a session.
func GetContextPct(sessionID string) (float64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	var contextPct float64
	err = db.QueryRow(`SELECT context_pct FROM sessions WHERE id = ?`, sessionID).
		Scan(&contextPct)
	return contextPct, err
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
