package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	AgentRecoveryStatusCrashed    = "crashed"
	AgentRecoveryStatusRestarting = "restarting"
	AgentRecoveryStatusBackoff    = "backoff"
	AgentRecoveryStatusRecovered  = "recovered"
	AgentRecoveryStatusSuppressed = "suppressed"
	AgentRecoveryStatusCancelled  = "cancelled"

	AuditVerbAgentRecoveryEligible   = "managed_agent.recovery_eligible"
	AuditVerbAgentRecoverySuppressed = "managed_agent.recovery_suppressed"
	AuditVerbAgentRecoveryStarted    = "managed_agent.recovery_started"
	AuditVerbAgentRecoveryConfirmed  = "managed_agent.recovery_live_confirmed"
	AuditVerbAgentRecoveryFailed     = "managed_agent.recovery_launch_failed"
	AuditVerbAgentRecoveryBackoff    = "managed_agent.recovery_backoff_entered"
	AuditVerbAgentRecoveryScheduled  = "managed_agent.recovery_retry_scheduled"
	AuditVerbAgentRecoveryRaced      = "managed_agent.recovery_raced_lost"
	AuditVerbAgentRecoveryCancelled  = "managed_agent.recovery_retry_cancelled"
	AuditVerbAgentRecoveryManual     = "managed_agent.recovery_manual_retry"
	AuditVerbAgentRecoveryReset      = "managed_agent.recovery_healthy_reset"
)

var agentRecoveryBackoff = [...]time.Duration{
	5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
	80 * time.Second, 160 * time.Second, 5 * time.Minute, 10 * time.Minute,
}

const AgentRecoveryHealthyReset = 30 * time.Minute
const AgentRecoveryLaunchLease = 2 * time.Minute

// AgentRecoveryBackoff returns the configured delay for a one-based
// consecutive-crash count. It deliberately has no terminal attempt limit.
func AgentRecoveryBackoff(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	i := consecutive - 1
	if i >= len(agentRecoveryBackoff) {
		i = len(agentRecoveryBackoff) - 1
	}
	return agentRecoveryBackoff[i]
}

type AgentRecovery struct {
	AgentID, ConvID                             string
	PredecessorSessionID, PredecessorGeneration string
	ExitEventID, Status, ReasonCode             string
	ConsecutiveCrashes, BackoffStep             int
	NextAttemptAt                               time.Time
	BackoffSeconds                              int
	LeaseToken                                  string
	LeaseExpiresAt, AttemptStartedAt            time.Time
	SuccessorSessionID, SuccessorGeneration     string
	LastExitCode                                *int
	LastExitSignal                              string
	LastExitAt, RecoveredAt, HealthySince       time.Time
	NotifiedCrash, NotifiedBackoff              bool
	UpdatedAt                                   time.Time
}

func recoveryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseRecoveryTime(raw string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, raw)
	return t
}

func scanAgentRecovery(s interface{ Scan(...any) error }) (*AgentRecovery, error) {
	var r AgentRecovery
	var next, lease, started, exited, recovered, healthy, updated string
	var code sql.NullInt64
	var crashNotified, backoffNotified int
	err := s.Scan(&r.AgentID, &r.ConvID, &r.PredecessorSessionID, &r.PredecessorGeneration,
		&r.ExitEventID, &r.Status, &r.ReasonCode, &r.ConsecutiveCrashes, &r.BackoffStep,
		&next, &r.BackoffSeconds, &r.LeaseToken, &lease, &started,
		&r.SuccessorSessionID, &r.SuccessorGeneration, &code, &r.LastExitSignal,
		&exited, &recovered, &healthy, &crashNotified, &backoffNotified, &updated)
	if err != nil {
		return nil, err
	}
	r.NextAttemptAt, r.LeaseExpiresAt, r.AttemptStartedAt = parseRecoveryTime(next), parseRecoveryTime(lease), parseRecoveryTime(started)
	r.LastExitAt, r.RecoveredAt, r.HealthySince, r.UpdatedAt = parseRecoveryTime(exited), parseRecoveryTime(recovered), parseRecoveryTime(healthy), parseRecoveryTime(updated)
	if code.Valid {
		v := int(code.Int64)
		r.LastExitCode = &v
	}
	r.NotifiedCrash, r.NotifiedBackoff = crashNotified != 0, backoffNotified != 0
	return &r, nil
}

const agentRecoverySelect = `SELECT agent_id, conv_id, predecessor_session_id,
	predecessor_generation, exit_event_id, status, reason_code,
	consecutive_crashes, backoff_step, next_attempt_at, backoff_seconds,
	lease_token, lease_expires_at, attempt_started_at, successor_session_id,
	successor_generation, last_exit_code, last_exit_signal, last_exit_at,
	recovered_at, healthy_since, notified_crash, notified_backoff, updated_at
	FROM agent_recovery`

func AgentRecoveryForAgent(agentID string) (*AgentRecovery, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	r, err := scanAgentRecovery(d.QueryRow(agentRecoverySelect+` WHERE agent_id = ?`, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func AgentRecoveryForConv(convID string) (*AgentRecovery, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	r, err := scanAgentRecovery(d.QueryRow(agentRecoverySelect+` WHERE agent_id = (`+agentForConvExpr+`)`, convID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func DueAgentRecoveries(now time.Time) ([]AgentRecovery, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(agentRecoverySelect+` WHERE
		(status IN (?, ?) AND next_attempt_at <> '' AND next_attempt_at <= ?)
		OR (status = ? AND lease_expires_at <> '' AND lease_expires_at <= ?)
		ORDER BY next_attempt_at, agent_id`, AgentRecoveryStatusCrashed,
		AgentRecoveryStatusBackoff, recoveryTime(now), AgentRecoveryStatusRestarting, recoveryTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentRecovery
	for rows.Next() {
		r, err := scanAgentRecovery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func ActiveAgentRecoveries() ([]AgentRecovery, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(agentRecoverySelect+` WHERE status IN (?, ?, ?, ?, ?) ORDER BY updated_at, agent_id`,
		AgentRecoveryStatusCrashed, AgentRecoveryStatusRestarting, AgentRecoveryStatusBackoff,
		AgentRecoveryStatusRecovered, AgentRecoveryStatusSuppressed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentRecovery
	for rows.Next() {
		r, err := scanAgentRecovery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// MarkAgentRecoveryNotified is a durable one-shot used before dispatching the
// crash or crash-loop transition notification.
func MarkAgentRecoveryNotified(r AgentRecovery, backoff bool) (bool, error) {
	column := "notified_crash"
	if backoff {
		column = "notified_backoff"
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_recovery SET `+column+` = 1 WHERE agent_id = ?
		AND predecessor_generation = ? AND `+column+` = 0`, r.AgentID, r.PredecessorGeneration)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func newRecoveryLeaseToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("db: recovery lease randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ClaimAgentRecovery is the durable exactly-one launch CAS. The caller must
// retain the returned token for every outcome mutation.
func ClaimAgentRecovery(agentID, generation string, now time.Time) (*AgentRecovery, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	token := newRecoveryLeaseToken()
	res, err := d.Exec(`UPDATE agent_recovery SET status = ?, lease_token = ?,
		lease_expires_at = ?, attempt_started_at = ?, updated_at = ?
		WHERE agent_id = ? AND predecessor_generation = ?
		AND status IN (?, ?) AND next_attempt_at <> '' AND next_attempt_at <= ?`,
		AgentRecoveryStatusRestarting, token, recoveryTime(now.Add(AgentRecoveryLaunchLease)),
		recoveryTime(now), recoveryTime(now), agentID, generation,
		AgentRecoveryStatusCrashed, AgentRecoveryStatusBackoff, recoveryTime(now))
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	r, err := AgentRecoveryForAgent(agentID)
	if err != nil {
		return nil, err
	}
	if r == nil || r.LeaseToken != token {
		return nil, nil
	}
	return r, nil
}

func recoveryFailureUpdate(r AgentRecovery, now time.Time, reason string) (bool, error) {
	n := r.ConsecutiveCrashes + 1
	delay := AgentRecoveryBackoff(n)
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_recovery SET status = ?, reason_code = ?,
		consecutive_crashes = ?, backoff_step = ?, next_attempt_at = ?,
		backoff_seconds = ?, lease_token = '', lease_expires_at = '',
		attempt_started_at = '', updated_at = ? WHERE agent_id = ?
		AND predecessor_generation = ? AND status = ? AND lease_token = ?`,
		AgentRecoveryStatusBackoff, reason, n, n-1, recoveryTime(now.Add(delay)),
		int(delay/time.Second), recoveryTime(now), r.AgentID, r.PredecessorGeneration,
		AgentRecoveryStatusRestarting, r.LeaseToken)
	if err != nil {
		return false, err
	}
	changed, _ := res.RowsAffected()
	return changed == 1, nil
}

func FailAgentRecoveryLaunch(r AgentRecovery, now time.Time, reason string) (bool, error) {
	return recoveryFailureUpdate(r, now, boundedRecoveryReason(reason))
}

func SuppressAgentRecovery(r AgentRecovery, now time.Time, reason string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_recovery SET status = ?, reason_code = ?,
		next_attempt_at = '', lease_token = '', lease_expires_at = '', updated_at = ?
		WHERE agent_id = ? AND predecessor_generation = ? AND status = ?
		AND lease_token = ?`, AgentRecoveryStatusSuppressed, boundedRecoveryReason(reason),
		recoveryTime(now), r.AgentID, r.PredecessorGeneration,
		AgentRecoveryStatusRestarting, r.LeaseToken)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ExpireAgentRecoveryLease advances a launch whose daemon disappeared before
// it could prove a live successor. A live successor must be checked first.
func ExpireAgentRecoveryLease(r AgentRecovery, now time.Time) (bool, error) {
	if r.LeaseToken == "" || r.LeaseExpiresAt.After(now) {
		return false, nil
	}
	return recoveryFailureUpdate(r, now, "live_confirmation_timeout")
}

func ConfirmAgentRecovery(r AgentRecovery, successorSession, successorGeneration string, now time.Time) (bool, error) {
	if successorSession == "" || successorGeneration == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_recovery SET status = ?, reason_code = '',
		next_attempt_at = '', lease_token = '', lease_expires_at = '',
		successor_session_id = ?, successor_generation = ?, recovered_at = ?,
		healthy_since = ?, updated_at = ? WHERE agent_id = ?
		AND predecessor_generation = ? AND status = ?`, AgentRecoveryStatusRecovered,
		successorSession, successorGeneration, recoveryTime(now), recoveryTime(now),
		recoveryTime(now), r.AgentID, r.PredecessorGeneration, AgentRecoveryStatusRestarting)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CancelAgentRecoveryForConv makes a pending automatic attempt permanently
// ineligible for its predecessor generation. A later genuine crash writes a
// new generation and may create a fresh episode.
func CancelAgentRecoveryForConv(convID, reason string, now time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_recovery SET status = ?, reason_code = ?,
		next_attempt_at = '', lease_token = '', lease_expires_at = '', updated_at = ?
		WHERE agent_id = (`+agentForConvExpr+`) AND status IN (?, ?, ?)`,
		AgentRecoveryStatusCancelled, boundedRecoveryReason(reason), recoveryTime(now), convID,
		AgentRecoveryStatusCrashed, AgentRecoveryStatusBackoff, AgentRecoveryStatusRestarting)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CancelAgentRecoveryGeneration is the scheduler-facing cancellation CAS. It
// cannot let a stale sweep for one predecessor cancel a newer crash episode.
func CancelAgentRecoveryGeneration(r AgentRecovery, reason string, now time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_recovery SET status = ?, reason_code = ?,
		next_attempt_at = '', lease_token = '', lease_expires_at = '', updated_at = ?
		WHERE agent_id = ? AND predecessor_generation = ? AND status IN (?, ?, ?)`,
		AgentRecoveryStatusCancelled, boundedRecoveryReason(reason), recoveryTime(now),
		r.AgentID, r.PredecessorGeneration, AgentRecoveryStatusCrashed,
		AgentRecoveryStatusBackoff, AgentRecoveryStatusRestarting)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// BeginManualAgentRecovery transfers a pending/suppressed recovery episode to
// an operator-triggered attempt. The durable restarting row lets the normal
// successor confirmation establish healthy_since after the manual launch.
func BeginManualAgentRecovery(convID string, now time.Time) (*AgentRecovery, error) {
	r, err := AgentRecoveryForConv(convID)
	if err != nil || r == nil {
		return nil, err
	}
	token := newRecoveryLeaseToken()
	d, err := Open()
	if err != nil {
		return nil, err
	}
	res, err := d.Exec(`UPDATE agent_recovery SET status = ?, reason_code = 'manual_resume',
		next_attempt_at = '', lease_token = ?, lease_expires_at = ?,
		attempt_started_at = ?, successor_session_id = '', successor_generation = '',
		recovered_at = '', healthy_since = '', updated_at = ? WHERE agent_id = ?
		AND predecessor_generation = ? AND status IN (?, ?, ?, ?, ?, ?)`,
		AgentRecoveryStatusRestarting, token, recoveryTime(now.Add(AgentRecoveryLaunchLease)),
		recoveryTime(now), recoveryTime(now), r.AgentID, r.PredecessorGeneration,
		AgentRecoveryStatusCrashed, AgentRecoveryStatusBackoff, AgentRecoveryStatusRestarting,
		AgentRecoveryStatusSuppressed, AgentRecoveryStatusCancelled, AgentRecoveryStatusRecovered)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	claim, err := AgentRecoveryForAgent(r.AgentID)
	if err != nil || claim == nil || claim.LeaseToken != token {
		return nil, err
	}
	return claim, nil
}

// AgentRecoveryClaimCurrent revalidates both recovery ownership and stable
// actor authority immediately before a claimed launch crosses into Spawn.
func AgentRecoveryClaimCurrent(r AgentRecovery) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var ok int
	err = d.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM agent_recovery ar JOIN agents a ON a.agent_id = ar.agent_id
		WHERE ar.agent_id = ? AND ar.predecessor_generation = ?
		AND ar.status = ? AND ar.lease_token = ?
		AND a.retired_at = '' AND a.current_conv_id = ar.conv_id
	)`, r.AgentID, r.PredecessorGeneration, AgentRecoveryStatusRestarting, r.LeaseToken).Scan(&ok)
	return ok == 1, err
}

func ResetHealthyAgentRecovery(r AgentRecovery, now time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	cutoff := recoveryTime(now.Add(-AgentRecoveryHealthyReset))
	res, err := d.Exec(`DELETE FROM agent_recovery WHERE agent_id = ?
		AND predecessor_generation = ? AND successor_session_id = ?
		AND successor_generation = ? AND status = ?
		AND healthy_since <> '' AND healthy_since <= ?`, r.AgentID,
		r.PredecessorGeneration, r.SuccessorSessionID, r.SuccessorGeneration,
		AgentRecoveryStatusRecovered, cutoff)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func boundedRecoveryReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > 64 {
		reason = reason[:64]
	}
	for _, r := range reason {
		if r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "other"
		}
	}
	if reason == "" {
		return "unspecified"
	}
	return reason
}

func recoveryAuditDetail(r AgentRecovery, reason string, now time.Time) string {
	next := "unavailable"
	if !r.NextAttemptAt.IsZero() {
		next = recoveryTime(r.NextAttemptAt)
	}
	elapsedBackoff, liveConfirmation := 0, 0
	if !r.NextAttemptAt.IsZero() && r.BackoffSeconds > 0 {
		scheduled := r.NextAttemptAt.Add(-time.Duration(r.BackoffSeconds) * time.Second)
		if now.After(scheduled) {
			elapsedBackoff = int(now.Sub(scheduled) / time.Second)
		}
	}
	if !r.AttemptStartedAt.IsZero() && now.After(r.AttemptStartedAt) {
		liveConfirmation = int(now.Sub(r.AttemptStartedAt) / time.Second)
	}
	return strings.Join([]string{
		"reason=" + boundedRecoveryReason(reason),
		"restart_count=" + strconv.Itoa(r.ConsecutiveCrashes),
		"backoff_step=" + strconv.Itoa(r.BackoffStep),
		"backoff_seconds=" + strconv.Itoa(r.BackoffSeconds),
		"elapsed_backoff_seconds=" + strconv.Itoa(elapsedBackoff),
		"live_confirmation_seconds=" + strconv.Itoa(liveConfirmation),
		"next_attempt_at=" + next,
		"predecessor_session=" + r.PredecessorSessionID,
		"predecessor_generation=" + r.PredecessorGeneration,
		"successor_session=" + unavailable(r.SuccessorSessionID),
		"successor_generation=" + unavailable(r.SuccessorGeneration),
	}, " ")
}

func RecordAgentRecoveryAudit(r AgentRecovery, verb, reason string, now time.Time) error {
	_, err := InsertAuditLog(AuditLogEntry{At: now, ActorKind: AuditActorSystem,
		ActorLabel: "tclaude", Verb: verb, TargetConv: r.ConvID, TargetAgent: r.AgentID,
		TargetLabel: r.ConvID, Detail: recoveryAuditDetail(r, reason, now), Status: 200,
		Source: AuditSourceReconcile, RelatedEventID: r.ExitEventID,
		SessionID: r.PredecessorSessionID})
	return err
}

// reconcileAgentRecoveryCandidateTx is called only after the bounded exit row
// has been merged, so richer callback evidence can promote an earlier unknown
// reconciliation observation without the callback launching anything.
func reconcileAgentRecoveryCandidateTx(tx *sql.Tx, meta exitSessionMeta, e AuditLogEntry, now time.Time) error {
	if e.ExitCode != nil && *e.ExitCode == 0 {
		return nil
	}
	if e.ExitCode == nil && meta.Harness != "codex" {
		return nil
	}
	if meta.AgentID == "" {
		return nil
	}
	reason := ""
	if e.ExitCode == nil {
		reason = "unknown_exit_evidence"
	}
	if meta.Harness != "codex" {
		reason = "unsupported_harness"
	}
	if e.LaunchPhase != AgentExitLaunchPhaseRuntime {
		reason = "launch_not_runtime"
	}
	if e.LifecycleAction != "" {
		reason = "lifecycle_intent"
	}
	if meta.CallbackGeneration == "" {
		reason = "unknown_launch_generation"
	}
	if strings.TrimSpace(meta.ResumeProvenance) == "" {
		reason = "resume_provenance_missing"
	}
	var currentConv, retiredAt string
	if err := tx.QueryRow(`SELECT current_conv_id, retired_at FROM agents WHERE agent_id = ?`, meta.AgentID).Scan(&currentConv, &retiredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if retiredAt != "" || currentConv != meta.ConvID {
		return nil
	}
	var latestSession string
	if err := tx.QueryRow(`SELECT id FROM sessions WHERE conv_id = ? ORDER BY rowid DESC LIMIT 1`, meta.ConvID).Scan(&latestSession); err != nil {
		return err
	}
	if latestSession != e.SessionID {
		return nil
	}

	var oldCount int
	var oldStatus, oldGeneration, healthyRaw string
	err := tx.QueryRow(`SELECT consecutive_crashes, status, predecessor_generation, healthy_since
		FROM agent_recovery WHERE agent_id = ?`, meta.AgentID).Scan(&oldCount, &oldStatus, &oldGeneration, &healthyRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	sameGenerationPromotion := false
	if err == nil && oldGeneration == meta.CallbackGeneration {
		// An enriched observation may turn an earlier fail-closed row eligible.
		if oldStatus != AgentRecoveryStatusSuppressed || reason != "" {
			return nil
		}
		sameGenerationPromotion = true
	}
	count := oldCount + 1
	if errors.Is(err, sql.ErrNoRows) {
		count = 1
	}
	if sameGenerationPromotion {
		count = oldCount
	}
	if healthy := parseRecoveryTime(healthyRaw); !healthy.IsZero() && now.Sub(healthy) >= AgentRecoveryHealthyReset {
		count = 1
	}
	delay := AgentRecoveryBackoff(count)
	status := AgentRecoveryStatusCrashed
	if count > 1 {
		status = AgentRecoveryStatusBackoff
	}
	next := now.Add(delay)
	if reason != "" {
		status = AgentRecoveryStatusSuppressed
		next = time.Time{}
		delay = 0
	}
	_, err = tx.Exec(`INSERT INTO agent_recovery (agent_id, conv_id,
		predecessor_session_id, predecessor_generation, exit_event_id, status,
		reason_code, consecutive_crashes, backoff_step, next_attempt_at,
		backoff_seconds, last_exit_code, last_exit_signal, last_exit_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET conv_id=excluded.conv_id,
		predecessor_session_id=excluded.predecessor_session_id,
		predecessor_generation=excluded.predecessor_generation,
		exit_event_id=excluded.exit_event_id, status=excluded.status,
		reason_code=excluded.reason_code, consecutive_crashes=excluded.consecutive_crashes,
		backoff_step=excluded.backoff_step, next_attempt_at=excluded.next_attempt_at,
		backoff_seconds=excluded.backoff_seconds, lease_token='', lease_expires_at='',
		attempt_started_at='', successor_session_id='', successor_generation='',
		last_exit_code=excluded.last_exit_code, last_exit_signal=excluded.last_exit_signal,
		last_exit_at=excluded.last_exit_at, recovered_at='', healthy_since='',
		updated_at=excluded.updated_at`,
		meta.AgentID, meta.ConvID, e.SessionID, meta.CallbackGeneration, e.EventID,
		status, reason, count, count-1, recoveryTime(next), int(delay/time.Second),
		e.ExitCode, e.Signal, recoveryTime(now), recoveryTime(now))
	if err != nil {
		return fmt.Errorf("persist agent recovery candidate: %w", err)
	}
	candidate := AgentRecovery{AgentID: meta.AgentID, ConvID: meta.ConvID,
		PredecessorSessionID: e.SessionID, PredecessorGeneration: meta.CallbackGeneration,
		ExitEventID: e.EventID, Status: status, ReasonCode: reason,
		ConsecutiveCrashes: count, BackoffStep: count - 1, NextAttemptAt: next,
		BackoffSeconds: int(delay / time.Second), LastExitCode: cloneInt(e.ExitCode),
		LastExitSignal: e.Signal, LastExitAt: now, UpdatedAt: now}
	verb, auditReason, dedup := AuditVerbAgentRecoveryEligible, "eligible_nonzero_runtime_exit", "recovery-eligible:"
	if status == AgentRecoveryStatusSuppressed {
		verb, auditReason, dedup = AuditVerbAgentRecoverySuppressed, reason, "recovery-suppressed:"
	}
	auditIdentity := meta.CallbackGeneration
	if auditIdentity == "" {
		auditIdentity = e.EventID
	}
	if _, err := insertAuditLog(tx, AuditLogEntry{At: now, ActorKind: AuditActorSystem,
		ActorLabel: "tclaude", Verb: verb, TargetConv: meta.ConvID, TargetAgent: meta.AgentID,
		TargetLabel: meta.ConvID, Detail: recoveryAuditDetail(candidate, auditReason, now), Status: 200,
		Source: AuditSourceReconcile, RelatedEventID: e.EventID, SessionID: e.SessionID,
		DedupKey: dedup + auditIdentity}); err != nil {
		return err
	}
	if status != AgentRecoveryStatusSuppressed {
		if _, err := insertAuditLog(tx, AuditLogEntry{At: now, ActorKind: AuditActorSystem,
			ActorLabel: "tclaude", Verb: AuditVerbAgentRecoveryScheduled,
			TargetConv: meta.ConvID, TargetAgent: meta.AgentID, TargetLabel: meta.ConvID,
			Detail: recoveryAuditDetail(candidate, "retry_scheduled", now), Status: 200,
			Source: AuditSourceReconcile, RelatedEventID: e.EventID, SessionID: e.SessionID,
			DedupKey: "recovery-scheduled:" + auditIdentity}); err != nil {
			return err
		}
	}
	return nil
}
