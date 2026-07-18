package db

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const AuditVerbAgentExit = "managed_pane.exit"

const agentExitIntentMaxAge = 10 * time.Minute

const (
	AgentExitCauseNormal      = "normal_exit"
	AgentExitCauseSignal      = "signal"
	AgentExitCauseLaunch      = "launch_failure"
	AgentExitCauseDisappeared = "disappeared"
	AgentExitCauseUnknown     = "unknown"
)

const (
	AgentExitObservedProcessPaneBootstrap = "pane-bootstrap"
	AgentExitLaunchPhaseRuntime           = "runtime"
	AgentExitLaunchPhasePreHarness        = "pre-harness"
	AgentExitLaunchPhaseUnverified        = "unverified"
	SessionExitGatePending                = "pending"
	SessionExitGateReleasing              = "releasing"
	SessionExitGateReleased               = "released"
	SessionExitGateUngated                = "ungated"
)

const (
	AgentExitObserverTmux      = "tmux"
	AgentExitObserverHook      = "hook"
	AgentExitObserverReaper    = "reaper"
	AgentExitObserverReconcile = "reconcile"
)

const (
	AgentExitActionStop        = "stop"
	AgentExitActionForceStop   = "force_stop"
	AgentExitActionRetire      = "retire"
	AgentExitActionReincarnate = "reincarnate"
)

var ErrExitCallbackRejected = errors.New("exit callback rejected")

// NewAuditEventID returns an opaque bounded correlation id suitable for both
// command audit rows and lifecycle intent linkage. Exit observations derive
// their own deterministic id from the launch identity instead.
func NewAuditEventID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("evt_%x", raw[:]), nil
}

// AgentExitObservation is the bounded evidence tclaude has for one managed
// session launch ending. Empty cause fields mean unavailable, never inferred.
// Session/agent attribution is resolved from the durable session row rather
// than accepted from the callback caller.
type AgentExitObservation struct {
	At              time.Time
	SessionID       string
	TmuxSession     string
	PaneID          string
	Observer        string
	CauseKind       string
	ObservedProcess string
	LaunchPhase     string
	ExitCode        *int
	Signal          string
	LifecycleAction string
	Reason          string
	ObservedState   string
	RelatedEventID  string
	// ExpectedGeneration binds non-callback evidence to the launch that was
	// actually observed. A current generated launch rejects an empty or stale
	// value; legacy pre-v136 rows with no generation remain reconcilable.
	ExpectedGeneration string
}

// ExitCallbackAuth is the launch-scoped proof carried only by the pane-local
// tmux hook. TokenHash is SHA-256(token), never the plaintext token stored by
// the daemon. The callback command must independently verify the exact tmux
// pane is dead and its formats match the observation before calling the DB.
type ExitCallbackAuth struct {
	Generation string
	TokenHash  string
	PaneID     string
}

type AgentExitRecordResult struct {
	EventID  string
	Inserted bool
	Enriched bool
}

type exitSessionMeta struct {
	TmuxSession        string
	ConvID             string
	AgentID            string
	Status             string
	CreatedAt          string
	ExitReason         string
	Intent             string
	IntentEventID      string
	IntentGeneration   string
	IntentAt           sql.NullString
	CallbackGeneration string
	CallbackTokenHash  string
	CallbackPaneID     string
	CallbackUsedAt     sql.NullString
	LaunchGateState    string
}

type SessionExitLaunchIdentity struct {
	Generation  string
	TmuxSession string
	GateState   string
}

type SessionExitIntentRef struct {
	SessionID      string
	Generation     string
	Action         string
	RelatedEventID string
}

// SetSessionExitLaunchBinding replaces the callback authority for exactly one
// launch. A relaunch writes a fresh generation/token/pane tuple and clears the
// consumed marker, so a delayed callback from the predecessor fails closed.
func SetSessionExitLaunchGeneration(sessionID, generation string) error {
	if err := validateExitIdentifier("session_id", sessionID, 128); err != nil {
		return err
	}
	if !isLowerHex(generation, 32) {
		return fmt.Errorf("invalid exit callback generation")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE sessions SET exit_callback_generation = ?,
		exit_callback_token_hash = '', exit_callback_pane_id = '',
		exit_callback_used_at = NULL, exit_intent = '',
		exit_intent_event_id = '', exit_intent_generation = '',
		exit_intent_at = NULL, exit_launch_gate_state = ? WHERE id = ?`,
		generation, SessionExitGateUngated, sessionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("bind exit launch generation: session not found")
	}
	return nil
}

func SetSessionExitLaunchBinding(sessionID, generation, tokenHash, paneID string) error {
	if err := validateExitIdentifier("session_id", sessionID, 128); err != nil {
		return err
	}
	if !isLowerHex(generation, 32) || !isLowerHex(tokenHash, 64) || !validPaneID(paneID) {
		return fmt.Errorf("invalid exit callback binding")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE sessions SET
		exit_callback_token_hash = ?,
		exit_callback_pane_id = ?, exit_callback_used_at = NULL,
		exit_launch_gate_state = ?
		WHERE id = ? AND exit_callback_generation = ?`,
		tokenHash, paneID, SessionExitGatePending, sessionID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("bind exit callback: session not found")
	}
	return nil
}

func ClearSessionExitLaunchBinding(sessionID, generation string) error {
	if err := validateExitIdentifier("session_id", sessionID, 128); err != nil {
		return err
	}
	if !isLowerHex(generation, 32) {
		return fmt.Errorf("invalid exit callback generation")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE sessions SET exit_callback_token_hash = '', exit_callback_pane_id = '',
		exit_callback_used_at = NULL WHERE id = ? AND exit_callback_generation = ?`,
		sessionID, generation)
	return err
}

func MarkSessionExitLaunchReleasing(sessionID, generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE sessions SET exit_launch_gate_state = ?
		WHERE id = ? AND exit_callback_generation = ? AND exit_launch_gate_state = ?`,
		SessionExitGateReleasing, sessionID, generation, SessionExitGatePending)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("mark exit launch releasing: stale generation or state")
	}
	return nil
}

// MarkSessionExitLaunchReleased may only follow the pane's atomic gate
// acknowledgement. Keeping this transition releasing-only prevents any
// production caller from claiming runtime before the pane crossed the gate.
func MarkSessionExitLaunchReleased(sessionID, generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE sessions SET exit_launch_gate_state = ?
		WHERE id = ? AND exit_callback_generation = ? AND exit_launch_gate_state = ?`,
		SessionExitGateReleased, sessionID, generation, SessionExitGateReleasing)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("mark exit launch released: stale generation or state")
	}
	return nil
}

func MarkSessionExitLaunchUngated(sessionID, generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE sessions SET exit_launch_gate_state = ?
		WHERE id = ? AND exit_callback_generation = ?`,
		SessionExitGateUngated, sessionID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("mark exit launch ungated: stale generation")
	}
	return nil
}

func MarkSessionExitLaunchPending(sessionID, generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE sessions SET exit_launch_gate_state = ?
		WHERE id = ? AND exit_callback_generation = ?`,
		SessionExitGatePending, sessionID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("mark exit launch pending: stale generation")
	}
	return nil
}

func GetSessionExitLaunchIdentity(sessionID string) (SessionExitLaunchIdentity, error) {
	d, err := Open()
	if err != nil {
		return SessionExitLaunchIdentity{}, err
	}
	var out SessionExitLaunchIdentity
	err = d.QueryRow(`SELECT exit_callback_generation, tmux_session,
		exit_launch_gate_state FROM sessions WHERE id = ?`, sessionID).
		Scan(&out.Generation, &out.TmuxSession, &out.GateState)
	return out, err
}

// SetSessionExitIntent records an authorized lifecycle request immediately
// before its termination attempt. Callers must clear it if that attempt fails.
func SetSessionExitIntent(sessionID, action, relatedEventID string, at time.Time) (SessionExitIntentRef, error) {
	if !validExitAction(action) {
		return SessionExitIntentRef{}, fmt.Errorf("invalid exit lifecycle action %q", action)
	}
	if relatedEventID != "" && !validEventID(relatedEventID) {
		return SessionExitIntentRef{}, fmt.Errorf("invalid related event id %q", relatedEventID)
	}
	if at.IsZero() {
		return SessionExitIntentRef{}, fmt.Errorf("missing exit intent timestamp")
	}
	if relatedEventID != "" && !validEventID(relatedEventID) {
		return SessionExitIntentRef{}, fmt.Errorf("invalid related audit event id")
	}
	if at.IsZero() {
		at = time.Now()
	}
	d, err := Open()
	if err != nil {
		return SessionExitIntentRef{}, err
	}
	var generation string
	err = d.QueryRow(`UPDATE sessions SET exit_intent = ?,
		exit_intent_event_id = ?, exit_intent_generation = exit_callback_generation,
		exit_intent_at = ? WHERE id = ? RETURNING exit_callback_generation`,
		action, relatedEventID, at.UTC().Format(time.RFC3339Nano), sessionID).Scan(&generation)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionExitIntentRef{}, fmt.Errorf("set exit intent: session not found")
		}
		return SessionExitIntentRef{}, err
	}
	return SessionExitIntentRef{SessionID: sessionID, Generation: generation,
		Action: action, RelatedEventID: relatedEventID}, nil
}

// SetSessionExitIntentIfTarget is an additive selected-launch CAS used by
// lifecycle stop. It refuses row/session/generation reuse before arming intent.
func SetSessionExitIntentIfTarget(sessionID, tmuxSession, generation, action, relatedEventID string, at time.Time) (SessionExitIntentRef, error) {
	if !validExitAction(action) {
		return SessionExitIntentRef{}, fmt.Errorf("invalid exit lifecycle action %q", action)
	}
	d, err := Open()
	if err != nil {
		return SessionExitIntentRef{}, err
	}
	var got string
	err = d.QueryRow(`UPDATE sessions SET exit_intent = ?, exit_intent_event_id = ?, exit_intent_generation = exit_callback_generation, exit_intent_at = ?
		WHERE id = ? AND tmux_session = ? AND exit_callback_generation = ? RETURNING exit_callback_generation`,
		action, relatedEventID, at.UTC().Format(time.RFC3339Nano), sessionID, tmuxSession, generation).Scan(&got)
	if err != nil {
		return SessionExitIntentRef{}, err
	}
	return SessionExitIntentRef{SessionID: sessionID, Generation: got, Action: action, RelatedEventID: relatedEventID}, nil
}

func ClearSessionExitIntentIfCurrent(ref SessionExitIntentRef) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE sessions SET exit_intent = '',
		exit_intent_event_id = '', exit_intent_generation = '', exit_intent_at = NULL
		WHERE id = ? AND exit_callback_generation = ? AND exit_intent_generation = ?
		AND exit_intent = ? AND exit_intent_event_id = ?`,
		ref.SessionID, ref.Generation, ref.Generation, ref.Action, ref.RelatedEventID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClearSessionExitIntentIfTarget clears intent only when both the selected
// launch generation and tmux session still match. It is additive so existing
// lifecycle CAS predicates remain unchanged.
func ClearSessionExitIntentIfTarget(ref SessionExitIntentRef, tmuxSession string) (bool, error) {
	if tmuxSession == "" {
		return false, fmt.Errorf("missing tmux session")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE sessions SET exit_intent = '', exit_intent_event_id = '', exit_intent_generation = '', exit_intent_at = NULL
		WHERE id = ? AND tmux_session = ? AND exit_callback_generation = ? AND exit_intent_generation = ?
		AND exit_intent = ? AND exit_intent_event_id = ?`,
		ref.SessionID, tmuxSession, ref.Generation, ref.Generation, ref.Action, ref.RelatedEventID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func ClearSessionExitIntent(sessionID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE sessions SET exit_intent = '',
		exit_intent_event_id = '', exit_intent_generation = '',
		exit_intent_at = NULL WHERE id = ?`, sessionID)
	return err
}

func ClearSessionExitIntentByConv(convID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE sessions SET exit_intent = '',
		exit_intent_event_id = '', exit_intent_generation = '',
		exit_intent_at = NULL WHERE conv_id = ?`, convID)
	return err
}

func RecordAgentExitObservation(o AgentExitObservation) (AgentExitRecordResult, error) {
	if o.Observer == AgentExitObserverTmux {
		return AgentExitRecordResult{}, fmt.Errorf("%w: tmux observation requires callback authentication", ErrExitCallbackRejected)
	}
	return recordAgentExitObservation(o, nil)
}

// RecordAuthenticatedAgentExitObservation atomically consumes a valid
// launch-scoped callback credential and records the observation. Replay,
// forged attribution, or a stale predecessor credential changes neither the
// session nor the audit row.
func RecordAuthenticatedAgentExitObservation(o AgentExitObservation, auth ExitCallbackAuth) (AgentExitRecordResult, error) {
	if o.Observer != AgentExitObserverTmux || o.PaneID != auth.PaneID {
		return AgentExitRecordResult{}, fmt.Errorf("%w: callback observer or pane mismatch", ErrExitCallbackRejected)
	}
	return recordAgentExitObservation(o, &auth)
}

func recordAgentExitObservation(o AgentExitObservation, auth *ExitCallbackAuth) (AgentExitRecordResult, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		result, err := recordAgentExitObservationOnce(o, auth)
		if err == nil || !retryableExitAuditConflict(err) {
			return result, err
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return AgentExitRecordResult{}, lastErr
}

func recordAgentExitObservationOnce(o AgentExitObservation, auth *ExitCallbackAuth) (AgentExitRecordResult, error) {
	if err := validateExitObservation(&o); err != nil {
		return AgentExitRecordResult{}, err
	}
	d, err := Open()
	if err != nil {
		return AgentExitRecordResult{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return AgentExitRecordResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	result, entry, changed, err := recordAgentExitObservationTx(tx, o, auth)
	if err != nil {
		return AgentExitRecordResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentExitRecordResult{}, err
	}
	if changed {
		logAgentExitObservation(entry, result)
	}
	return result, nil
}

// recordAgentExitObservationTx performs generation validation, optional
// callback consumption, intent correlation, and audit deduplication inside the
// caller's transaction. Keeping the transaction boundary with the caller lets
// reaper and SessionEnd state changes commit atomically with their observation.
func recordAgentExitObservationTx(tx *sql.Tx, o AgentExitObservation, auth *ExitCallbackAuth) (AgentExitRecordResult, AuditLogEntry, bool, error) {
	meta, err := loadExitSessionMeta(tx, o.SessionID)
	if err != nil {
		return AgentExitRecordResult{}, AuditLogEntry{}, false, err
	}
	if o.TmuxSession != "" && o.TmuxSession != meta.TmuxSession {
		return AgentExitRecordResult{}, AuditLogEntry{}, false, fmt.Errorf("%w: tmux session mismatch", ErrExitCallbackRejected)
	}
	if o.TmuxSession == "" {
		o.TmuxSession = meta.TmuxSession
	}
	expectedGeneration := o.ExpectedGeneration
	if auth != nil {
		expectedGeneration = auth.Generation
	}
	if meta.CallbackGeneration != "" && expectedGeneration != meta.CallbackGeneration {
		return AgentExitRecordResult{}, AuditLogEntry{}, false, fmt.Errorf("%w: stale or missing launch generation", ErrExitCallbackRejected)
	}
	if meta.CallbackGeneration == "" && expectedGeneration != "" {
		return AgentExitRecordResult{}, AuditLogEntry{}, false, fmt.Errorf("%w: launch generation mismatch", ErrExitCallbackRejected)
	}
	if auth != nil {
		if err := consumeExitCallback(tx, o.SessionID, meta, *auth); err != nil {
			return AgentExitRecordResult{}, AuditLogEntry{}, false, err
		}
	}
	if o.PaneID == "" {
		o.PaneID = meta.CallbackPaneID
	}
	intentMatchesLaunch := meta.IntentGeneration == meta.CallbackGeneration &&
		recentExitIntent(meta.IntentAt, time.Now())
	if o.LifecycleAction == "" && intentMatchesLaunch {
		o.LifecycleAction = meta.Intent
	}
	if o.RelatedEventID == "" && intentMatchesLaunch {
		o.RelatedEventID = meta.IntentEventID
	}
	if o.Reason == "" {
		o.Reason = meta.ExitReason
	}
	if o.ObservedState == "" {
		o.ObservedState = meta.Status
	}
	o.ObservedProcess = AgentExitObservedProcessPaneBootstrap
	switch meta.LaunchGateState {
	case SessionExitGateReleased:
		o.LaunchPhase = AgentExitLaunchPhaseRuntime
	case SessionExitGatePending:
		o.LaunchPhase = AgentExitLaunchPhasePreHarness
		o.CauseKind = AgentExitCauseLaunch
	case SessionExitGateReleasing:
		// The parent has begun release but has not durably observed the
		// pane's gate acknowledgement. A hard parent death may mean either
		// side of the crossing, so neither runtime nor launch failure is safe.
		o.LaunchPhase = AgentExitLaunchPhaseUnverified
		o.CauseKind = AgentExitCauseUnknown
	default:
		o.LaunchPhase = AgentExitLaunchPhaseUnverified
	}
	if err := validateExitObservation(&o); err != nil {
		return AgentExitRecordResult{}, AuditLogEntry{}, false, err
	}

	launchIdentity := meta.CallbackGeneration
	if launchIdentity == "" {
		launchIdentity = o.SessionID + "\x00" + meta.CreatedAt + "\x00" + meta.TmuxSession
	}
	dedupKey, eventID := exitEventIdentity(launchIdentity)
	existing, err := loadExitAuditByDedup(tx, dedupKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AgentExitRecordResult{}, AuditLogEntry{}, false, err
	}

	entry := AuditLogEntry{
		At: o.At, ActorKind: AuditActorSystem, ActorLabel: "tclaude",
		Verb: AuditVerbAgentExit, TargetConv: meta.ConvID, TargetAgent: meta.AgentID,
		TargetLabel: meta.ConvID, Status: 200, Source: exitAuditSource(o.Observer),
		EventID: eventID, RelatedEventID: o.RelatedEventID,
		SessionID: o.SessionID, TmuxSession: o.TmuxSession, PaneID: o.PaneID,
		Observer: o.Observer, CauseKind: o.CauseKind,
		ObservedProcess: o.ObservedProcess, LaunchPhase: o.LaunchPhase,
		ExitCode: cloneInt(o.ExitCode),
		Signal:   o.Signal, LifecycleAction: o.LifecycleAction, Reason: o.Reason,
		ObservedState: o.ObservedState, DedupKey: dedupKey,
	}
	entry.Detail = exitAuditDetail(entry)
	result := AgentExitRecordResult{EventID: eventID}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := insertAuditLog(tx, entry); err != nil {
			return AgentExitRecordResult{}, AuditLogEntry{}, false, err
		}
		result.Inserted = true
	} else {
		merged := mergeExitAudit(*existing, entry)
		if exitAuditEqual(*existing, merged) {
			return result, *existing, false, nil
		}
		merged.Detail = exitAuditDetail(merged)
		if err := updateExitAudit(tx, merged); err != nil {
			return AgentExitRecordResult{}, AuditLogEntry{}, false, err
		}
		entry = merged
		result.Enriched = true
	}
	return result, entry, true, nil
}

func logAgentExitObservation(entry AuditLogEntry, result AgentExitRecordResult) {
	slog.Info("managed pane exit observed",
		"event_id", entry.EventID,
		"related_event_id", entry.RelatedEventID,
		"agent_id", entry.TargetAgent,
		"conv_id", entry.TargetConv,
		"session_id", entry.SessionID,
		"tmux_session", entry.TmuxSession,
		"pane_id", entry.PaneID,
		"observer", entry.Observer,
		"cause_kind", entry.CauseKind,
		"observed_process", entry.ObservedProcess,
		"launch_phase", entry.LaunchPhase,
		"exit_code", nullableLogInt(entry.ExitCode),
		"signal", unavailable(entry.Signal),
		"lifecycle_action", unavailable(entry.LifecycleAction),
		"observed_state", entry.ObservedState,
		"enriched", result.Enriched)
}

// MarkSessionExitedAndRecordObservationIfUnchanged atomically applies the
// reaper's state CAS and records the evidence for the same launch generation.
// A relaunch, hook update, or generation rotation makes the CAS a benign no-op.
func MarkSessionExitedAndRecordObservationIfUnchanged(
	id, observedStatus string,
	observedUpdatedAt time.Time,
	fallbackExitReason string,
	o AgentExitObservation,
) (bool, AgentExitRecordResult, error) {
	if o.SessionID != id {
		return false, AgentExitRecordResult{}, fmt.Errorf("exit observation session mismatch")
	}
	if err := validateExitObservation(&o); err != nil {
		return false, AgentExitRecordResult{}, err
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		ok, result, err := markSessionExitedAndRecordObservationOnce(
			id, observedStatus, observedUpdatedAt, fallbackExitReason, o,
		)
		if err == nil || !retryableExitAuditConflict(err) {
			return ok, result, err
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return false, AgentExitRecordResult{}, lastErr
}

func markSessionExitedAndRecordObservationOnce(
	id, observedStatus string,
	observedUpdatedAt time.Time,
	fallbackExitReason string,
	o AgentExitObservation,
) (bool, AgentExitRecordResult, error) {
	d, err := Open()
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE sessions
		SET status = 'exited', status_detail = '', updated_at = ?,
			subagent_count = 0, subagents_json = '',
			exit_reason = COALESCE(exit_reason, NULLIF(?, ''))
		WHERE id = ? AND status = ? AND updated_at = ?
			AND exit_callback_generation = ?`,
		time.Now().Format(time.RFC3339Nano), fallbackExitReason,
		id, observedStatus, observedUpdatedAt.Format(time.RFC3339Nano), o.ExpectedGeneration)
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	if n != 1 {
		return false, AgentExitRecordResult{}, nil
	}
	o.ObservedState = "exited"
	result, entry, changed, err := recordAgentExitObservationTx(tx, o, nil)
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, AgentExitRecordResult{}, err
	}
	if changed {
		logAgentExitObservation(entry, result)
	}
	return true, result, nil
}

// RecordSessionEndExitObservation atomically binds a SessionEnd status/reason
// transition and its exit audit row to the generation supplied by the hook.
// A delayed predecessor hook therefore cannot mutate or attribute a relaunch.
func RecordSessionEndExitObservation(o AgentExitObservation) (bool, AgentExitRecordResult, error) {
	if o.Observer != AgentExitObserverHook {
		return false, AgentExitRecordResult{}, fmt.Errorf("SessionEnd observation requires hook observer")
	}
	if err := validateExitObservation(&o); err != nil {
		return false, AgentExitRecordResult{}, err
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		ok, result, err := recordSessionEndExitObservationOnce(o)
		if err == nil || !retryableExitAuditConflict(err) {
			return ok, result, err
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return false, AgentExitRecordResult{}, lastErr
}

func recordSessionEndExitObservationOnce(o AgentExitObservation) (bool, AgentExitRecordResult, error) {
	d, err := Open()
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE sessions SET status = 'exited', status_detail = '',
		updated_at = ?, subagent_count = 0, subagents_json = '',
		exit_reason = CASE WHEN ? <> '' THEN ? ELSE exit_reason END
		WHERE id = ? AND exit_callback_generation = ?`,
		time.Now().Format(time.RFC3339Nano), o.Reason, o.Reason,
		o.SessionID, o.ExpectedGeneration)
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	if n != 1 {
		return false, AgentExitRecordResult{}, nil
	}
	o.ObservedState = "exited"
	result, entry, changed, err := recordAgentExitObservationTx(tx, o, nil)
	if err != nil {
		return false, AgentExitRecordResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, AgentExitRecordResult{}, err
	}
	if changed {
		logAgentExitObservation(entry, result)
	}
	return true, result, nil
}

func retryableExitAuditConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "unique constraint failed: audit_log.dedup_key")
}

func loadExitSessionMeta(tx *sql.Tx, sessionID string) (exitSessionMeta, error) {
	var m exitSessionMeta
	var exitReason sql.NullString
	err := tx.QueryRow(`SELECT tmux_session, conv_id, agent_id, status, created_at,
		exit_reason, exit_intent, exit_intent_event_id, exit_intent_generation, exit_intent_at,
		exit_callback_generation, exit_callback_token_hash,
		exit_callback_pane_id, exit_callback_used_at, exit_launch_gate_state
		FROM sessions WHERE id = ?`, sessionID).Scan(
		&m.TmuxSession, &m.ConvID, &m.AgentID, &m.Status, &m.CreatedAt,
		&exitReason, &m.Intent, &m.IntentEventID, &m.IntentGeneration, &m.IntentAt,
		&m.CallbackGeneration, &m.CallbackTokenHash,
		&m.CallbackPaneID, &m.CallbackUsedAt, &m.LaunchGateState)
	if err != nil {
		return m, err
	}
	m.ExitReason = exitReason.String
	return m, nil
}

func recentExitIntent(at sql.NullString, now time.Time) bool {
	if !at.Valid || at.String == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, at.String)
	if err != nil {
		return false
	}
	age := now.Sub(parsed)
	return age >= 0 && age <= agentExitIntentMaxAge
}

func consumeExitCallback(tx *sql.Tx, sessionID string, m exitSessionMeta, auth ExitCallbackAuth) error {
	if !isLowerHex(auth.Generation, 32) || !isLowerHex(auth.TokenHash, 64) || !validPaneID(auth.PaneID) {
		return fmt.Errorf("%w: invalid proof", ErrExitCallbackRejected)
	}
	if m.CallbackUsedAt.Valid || m.CallbackGeneration == "" || m.CallbackTokenHash == "" || m.CallbackPaneID == "" ||
		subtle.ConstantTimeCompare([]byte(auth.Generation), []byte(m.CallbackGeneration)) != 1 ||
		subtle.ConstantTimeCompare([]byte(auth.TokenHash), []byte(m.CallbackTokenHash)) != 1 ||
		subtle.ConstantTimeCompare([]byte(auth.PaneID), []byte(m.CallbackPaneID)) != 1 {
		return fmt.Errorf("%w: stale, replayed, or mismatched proof", ErrExitCallbackRejected)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.Exec(`UPDATE sessions SET exit_callback_used_at = ?
		WHERE id = ? AND exit_callback_generation = ?
		AND exit_callback_token_hash = ? AND exit_callback_pane_id = ?
		AND exit_callback_used_at IS NULL`, now, sessionID,
		auth.Generation, auth.TokenHash, auth.PaneID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: callback already consumed", ErrExitCallbackRejected)
	}
	return nil
}

func validateExitObservation(o *AgentExitObservation) error {
	if err := validateExitIdentifier("session_id", o.SessionID, 128); err != nil {
		return err
	}
	if o.TmuxSession != "" {
		if err := validateExitIdentifier("tmux_session", o.TmuxSession, 64); err != nil {
			return err
		}
	}
	if o.PaneID != "" && !validPaneID(o.PaneID) {
		return fmt.Errorf("invalid pane id")
	}
	if !validExitObserver(o.Observer) || !validExitCause(o.CauseKind) {
		return fmt.Errorf("invalid exit observer or cause")
	}
	if o.ObservedProcess != "" && o.ObservedProcess != AgentExitObservedProcessPaneBootstrap {
		return fmt.Errorf("invalid observed process")
	}
	if o.LaunchPhase != "" && o.LaunchPhase != AgentExitLaunchPhaseRuntime &&
		o.LaunchPhase != AgentExitLaunchPhasePreHarness && o.LaunchPhase != AgentExitLaunchPhaseUnverified {
		return fmt.Errorf("invalid launch phase")
	}
	if o.ExpectedGeneration != "" && !isLowerHex(o.ExpectedGeneration, 32) {
		return fmt.Errorf("invalid expected launch generation")
	}
	if o.ExitCode != nil && (*o.ExitCode < 0 || *o.ExitCode > 255) {
		return fmt.Errorf("invalid exit code")
	}
	if o.Signal != "" {
		o.Signal = strings.ToUpper(o.Signal)
		if len(o.Signal) > 16 || !asciiToken(o.Signal, "_") {
			return fmt.Errorf("invalid signal")
		}
	}
	if o.CauseKind == AgentExitCauseSignal && o.Signal == "" {
		return fmt.Errorf("signal cause requires signal")
	}
	if o.Signal != "" && o.ExitCode != nil {
		return fmt.Errorf("exit code and signal are mutually exclusive")
	}
	if o.LifecycleAction != "" && !validExitAction(o.LifecycleAction) {
		return fmt.Errorf("invalid lifecycle action")
	}
	if o.Reason != "" && !validExitReason(o.Reason) {
		return fmt.Errorf("invalid exit reason")
	}
	if o.ObservedState != "" && !validObservedState(o.ObservedState) {
		o.ObservedState = "unknown"
	}
	if o.RelatedEventID != "" && !validEventID(o.RelatedEventID) {
		return fmt.Errorf("invalid related event id")
	}
	return nil
}

func loadExitAuditByDedup(tx *sql.Tx, dedupKey string) (*AuditLogEntry, error) {
	var e AuditLogEntry
	err := tx.QueryRow(`SELECT id, at, actor_kind, actor_conv, actor_agent,
		actor_label, verb, target_conv, target_agent, target_label, group_name,
		detail, method, path, status, source, event_id, related_event_id,
		session_id, tmux_session, pane_id, observer, cause_kind,
		observed_process, launch_phase, exit_code, signal, lifecycle_action,
		reason, observed_state, dedup_key
		FROM audit_log WHERE dedup_key = ?`, dedupKey).Scan(
		&e.ID, new(string), &e.ActorKind, &e.ActorConv, &e.ActorAgent,
		&e.ActorLabel, &e.Verb, &e.TargetConv, &e.TargetAgent, &e.TargetLabel, &e.GroupName,
		&e.Detail, &e.Method, &e.Path, &e.Status, &e.Source, &e.EventID, &e.RelatedEventID,
		&e.SessionID, &e.TmuxSession, &e.PaneID, &e.Observer, &e.CauseKind,
		&e.ObservedProcess, &e.LaunchPhase, &e.ExitCode,
		&e.Signal, &e.LifecycleAction, &e.Reason, &e.ObservedState, &e.DedupKey)
	return &e, err
}

func updateExitAudit(tx *sql.Tx, e AuditLogEntry) error {
	_, err := tx.Exec(`UPDATE audit_log SET target_conv = ?, target_agent = ?,
		target_label = ?, detail = ?, source = ?, related_event_id = ?,
		tmux_session = ?, pane_id = ?, observer = ?, cause_kind = ?,
		observed_process = ?, launch_phase = ?, exit_code = ?,
		signal = ?, lifecycle_action = ?, reason = ?, observed_state = ? WHERE id = ?`,
		e.TargetConv, e.TargetAgent, e.TargetLabel, e.Detail, e.Source, e.RelatedEventID,
		e.TmuxSession, e.PaneID, e.Observer, e.CauseKind,
		e.ObservedProcess, e.LaunchPhase, e.ExitCode,
		e.Signal, e.LifecycleAction, e.Reason, e.ObservedState, e.ID)
	return err
}

func mergeExitAudit(old, next AuditLogEntry) AuditLogEntry {
	m := old
	if m.TargetConv == "" {
		m.TargetConv = next.TargetConv
	}
	if m.TargetAgent == "" {
		m.TargetAgent = next.TargetAgent
	}
	if m.TargetLabel == "" {
		m.TargetLabel = next.TargetLabel
	}
	if m.TmuxSession == "" {
		m.TmuxSession = next.TmuxSession
	}
	if m.PaneID == "" {
		m.PaneID = next.PaneID
	}
	if observerRank(next.Observer) > observerRank(m.Observer) {
		m.Observer, m.Source = next.Observer, next.Source
	}
	if causeRank(next.CauseKind) > causeRank(m.CauseKind) {
		m.CauseKind = next.CauseKind
	}
	if m.ObservedProcess == "" {
		m.ObservedProcess = next.ObservedProcess
	}
	if launchPhaseRank(next.LaunchPhase) > launchPhaseRank(m.LaunchPhase) {
		m.LaunchPhase = next.LaunchPhase
	}
	switch m.CauseKind {
	case AgentExitCauseSignal:
		m.ExitCode = nil
		if m.Signal == "" && next.Signal != "" {
			m.Signal = next.Signal
		}
	case AgentExitCauseNormal:
		m.Signal = ""
		if m.ExitCode == nil && next.ExitCode != nil && next.CauseKind == AgentExitCauseNormal {
			m.ExitCode = cloneInt(next.ExitCode)
		}
	}
	if m.LifecycleAction == "" {
		m.LifecycleAction = next.LifecycleAction
	}
	if m.RelatedEventID == "" {
		m.RelatedEventID = next.RelatedEventID
	}
	if reasonRank(next.Reason) > reasonRank(m.Reason) {
		m.Reason = next.Reason
	}
	if stateRank(next.ObservedState) > stateRank(m.ObservedState) {
		m.ObservedState = next.ObservedState
	}
	return m
}

func exitAuditEqual(a, b AuditLogEntry) bool {
	return a.TargetConv == b.TargetConv && a.TargetAgent == b.TargetAgent &&
		a.TargetLabel == b.TargetLabel && a.Source == b.Source &&
		a.RelatedEventID == b.RelatedEventID && a.TmuxSession == b.TmuxSession &&
		a.PaneID == b.PaneID && a.Observer == b.Observer && a.CauseKind == b.CauseKind &&
		a.ObservedProcess == b.ObservedProcess && a.LaunchPhase == b.LaunchPhase &&
		intEqual(a.ExitCode, b.ExitCode) && a.Signal == b.Signal &&
		a.LifecycleAction == b.LifecycleAction && a.Reason == b.Reason &&
		a.ObservedState == b.ObservedState
}

func exitAuditDetail(e AuditLogEntry) string {
	code := "unavailable"
	if e.ExitCode != nil {
		code = strconv.Itoa(*e.ExitCode)
	}
	return strings.Join([]string{
		"pane_cause=" + e.CauseKind,
		"observed_process=" + unavailable(e.ObservedProcess),
		"launch_phase=" + unavailable(e.LaunchPhase),
		"exit_code=" + code,
		"signal=" + unavailable(e.Signal),
		"lifecycle=" + unavailable(e.LifecycleAction),
		"observer=" + e.Observer,
		"state=" + unavailable(e.ObservedState),
		"reason=" + unavailable(e.Reason),
	}, " ")
}

func exitEventIdentity(launchIdentity string) (dedupKey, eventID string) {
	h := sha256.Sum256([]byte("agent-exit\x00" + launchIdentity))
	hex := fmt.Sprintf("%x", h[:])
	return "sha256:" + hex, "evt_" + hex[:24]
}

func exitAuditSource(observer string) string {
	switch observer {
	case AgentExitObserverTmux:
		return AuditSourceTmux
	case AgentExitObserverHook:
		return AuditSourceHook
	case AgentExitObserverReaper:
		return AuditSourceReaper
	default:
		return AuditSourceReconcile
	}
}

func validExitCause(v string) bool {
	return v == AgentExitCauseNormal || v == AgentExitCauseSignal ||
		v == AgentExitCauseLaunch || v == AgentExitCauseDisappeared || v == AgentExitCauseUnknown
}
func validExitObserver(v string) bool {
	return v == AgentExitObserverTmux || v == AgentExitObserverHook ||
		v == AgentExitObserverReaper || v == AgentExitObserverReconcile
}
func validExitAction(v string) bool {
	return v == AgentExitActionStop || v == AgentExitActionForceStop ||
		v == AgentExitActionRetire || v == AgentExitActionReincarnate
}
func validExitReason(v string) bool {
	switch v {
	case "logout", "prompt_input_exit", "bypass_permissions_disabled", "other",
		"soft_exit", "unexpected":
		return true
	default:
		return false
	}
}
func validObservedState(v string) bool {
	switch v {
	case "running", "working", "idle", "main_agent_idle", "awaiting_input",
		"awaiting_permission", "waiting_input", "waiting_permission", "error",
		"exited", "unknown":
		return true
	default:
		return false
	}
}
func validEventID(v string) bool {
	return strings.HasPrefix(v, "evt_") && len(v) == 28 && isLowerHex(strings.TrimPrefix(v, "evt_"), 24)
}
func validPaneID(v string) bool {
	if len(v) < 2 || len(v) > 12 || v[0] != '%' {
		return false
	}
	for _, r := range v[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func validateExitIdentifier(name, v string, max int) error {
	if v == "" || len(v) > max || !asciiToken(v, "_-.") {
		return fmt.Errorf("invalid %s", name)
	}
	return nil
}
func asciiToken(v, extra string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune(extra, r) {
			continue
		}
		return false
	}
	return true
}
func isLowerHex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
func causeRank(v string) int {
	return map[string]int{AgentExitCauseUnknown: 1, AgentExitCauseDisappeared: 2,
		AgentExitCauseNormal: 3, AgentExitCauseSignal: 4, AgentExitCauseLaunch: 5}[v]
}

func launchPhaseRank(v string) int {
	return map[string]int{AgentExitLaunchPhaseUnverified: 1,
		AgentExitLaunchPhaseRuntime: 2, AgentExitLaunchPhasePreHarness: 3}[v]
}
func observerRank(v string) int {
	return map[string]int{AgentExitObserverReconcile: 1, AgentExitObserverReaper: 2, AgentExitObserverHook: 3, AgentExitObserverTmux: 4}[v]
}
func reasonRank(v string) int {
	if v == "" {
		return 0
	}
	if v == "unexpected" {
		return 1
	}
	return 2
}
func stateRank(v string) int {
	if v == "" || v == "unknown" {
		return 0
	}
	if v == "exited" {
		return 2
	}
	return 1
}
func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
func intEqual(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func nullableLogInt(v *int) any {
	if v == nil {
		return "unavailable"
	}
	return *v
}
func unavailable(v string) string {
	if v == "" {
		return "unavailable"
	}
	return v
}
