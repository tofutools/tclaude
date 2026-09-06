package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/platform/conversation"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

// NewLogicalConversationID mints an opaque platform-owned history identity.
func NewLogicalConversationID() conversation.ID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("db: crypto/rand failed generating conversation id: " + err.Error())
	}
	return conversation.ID("cvn_" + hex.EncodeToString(b[:]))
}

// CurrentConversationAdmission returns the immutable admission that produced
// an execution's current selection. Replaying this value through
// AdmitConversationBinding is the only way a recurring managed observation
// (notably hook and statusline ticks) can obtain a current Duplicate verdict
// without manufacturing a new revision.
func CurrentConversationAdmission(executionID execution.ID) (conversation.Admission, bool, error) {
	d, err := Open()
	if err != nil {
		return conversation.Admission{}, false, err
	}
	var a conversation.Admission
	var mainProcess int
	err = d.QueryRow(`SELECT h.expected_revision, h.evidence_id, h.session_id,
		h.harness, h.namespace, h.external_ref, h.transition,
		h.process_instance, h.main_process, h.evidence_strength, h.evidence_source,
		h.observed_pid, h.observed_tmux_session, h.observed_pane_id
		FROM conversation_attempt_bindings b
		JOIN conversation_reference_bindings h
		  ON h.execution_id=b.execution_id AND h.revision=b.revision
		WHERE b.execution_id=?`, executionID).
		Scan(&a.ExpectedRevision, &a.Evidence.ID, &a.Attempt.LegacySessionID,
			&a.Reference.Harness, &a.Reference.Namespace, &a.Reference.Value, &a.Transition,
			&a.Evidence.ProcessInstance, &mainProcess, &a.Evidence.Strength, &a.Evidence.Source,
			&a.Evidence.PID, &a.Evidence.TmuxSession, &a.Evidence.PaneID)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Admission{}, false, nil
	}
	if err != nil {
		return conversation.Admission{}, false, err
	}
	a.Origin = conversation.Managed
	a.Attempt.ExecutionID = executionID
	a.Evidence.MainProcess = mainProcess != 0
	return a, true, nil
}

// CurrentConversationSelection reads the platform-owned current selection for
// an execution without consulting the legacy sessions.conv_id projection.
func CurrentConversationSelection(executionID execution.ID) (conversation.Selection, bool, error) {
	d, err := Open()
	if err != nil {
		return conversation.Selection{}, false, err
	}
	var s conversation.Selection
	err = d.QueryRow(`SELECT conversation_id, harness, namespace, external_ref, revision
		FROM conversation_attempt_bindings WHERE execution_id=?`, executionID).
		Scan(&s.Conversation, &s.Reference.Harness, &s.Reference.Namespace,
			&s.Reference.Value, &s.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Selection{}, false, nil
	}
	return s, err == nil, err
}

// BindManagedAttemptMainPID promotes the launch-time pane-shell PID to the
// main harness PID proved by the host adapter. The compare-and-swap includes
// the entire durable launch attachment, so it cannot repair a successor or an
// exited attempt. Conversation admission subsequently rechecks this promoted
// association in its own transaction.
func BindManagedAttemptMainPID(
	attempt execution.AttemptRef,
	tmuxSession, paneID string,
	observedPID, mainPID int,
) (bool, error) {
	if strings.TrimSpace(attempt.LegacySessionID) == "" || observedPID <= 0 || mainPID <= 0 ||
		strings.TrimSpace(tmuxSession) == "" || strings.TrimSpace(paneID) == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE sessions SET pid=?
		WHERE id=? AND exit_callback_generation=? AND tmux_session=?
		  AND exit_callback_pane_id=? AND status<>'exited' AND pid IN (?, ?)`,
		mainPID, attempt.LegacySessionID, attempt.ExecutionID, tmuxSession,
		paneID, observedPID, mainPID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

type bindingRecord struct {
	revision         conversation.Revision
	expectedRevision conversation.Revision
	evidenceID       string
	sessionID        string
	conversationID   conversation.ID
	reference        conversation.Reference
	transition       conversation.Transition
	processInstance  string
	mainProcess      bool
	strength         conversation.EvidenceStrength
	source           conversation.EvidenceSource
	pid              int
	tmuxSession      string
	paneID           string
}

// AdmitConversationBinding is the single transactional writer for a managed
// attempt's selected logical conversation and immutable reference history.
// The host adapter validates the OS process before calling this function; the
// transaction independently rechecks the durable session, generation and
// recorded attachment. The two checks deliberately do not pretend to be one
// atomic OS/database operation.
func AdmitConversationBinding(a conversation.Admission) (conversation.Decision, error) {
	if err := validateConversationAdmission(a); err != nil {
		return conversation.Decision{}, err
	}
	if a.Origin == conversation.Discovered {
		return conversation.Decision{
			Outcome: conversation.Ambiguous,
			Reason:  "discovered execution requires a separately persisted workload association",
		}, nil
	}
	if !a.Evidence.MainProcess || a.Evidence.Strength != conversation.VerifiedMainProcess || a.Evidence.Source != conversation.HostAdapter {
		return conversation.Decision{Outcome: conversation.Rejected, Reason: "host adapter did not verify the attempt main process"}, nil
	}
	if a.Transition == conversation.Unspecified {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "transition was not classified"}, nil
	}

	d, err := Open()
	if err != nil {
		return conversation.Decision{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return conversation.Decision{}, err
	}
	defer func() { _ = tx.Rollback() }()

	ended, proofDecision, err := recheckManagedAttempt(tx, a)
	if err != nil || proofDecision.Outcome != "" {
		return proofDecision, err
	}

	current, found, err := loadCurrentBinding(tx, a.Attempt.ExecutionID)
	if err != nil {
		return conversation.Decision{}, err
	}
	replay, replayFound, err := loadBindingReplay(tx, a.Attempt.ExecutionID, a.Evidence.ID)
	if err != nil {
		return conversation.Decision{}, err
	}
	if replayFound {
		selection := selectionFromRecord(replay)
		if !sameAdmission(replay, a) {
			return conversation.Decision{Outcome: conversation.Conflict, Reason: "evidence replay key has different immutable admission data", Selection: selection}, nil
		}
		if ended {
			return conversation.Decision{Outcome: conversation.Historical, Reason: "exact replay belongs to a durably exited attempt", Selection: selection}, nil
		}
		if found && replay.revision == current.revision {
			return conversation.Decision{Outcome: conversation.Duplicate, Reason: "exact admission replay", Selection: selection}, nil
		}
		return conversation.Decision{Outcome: conversation.Historical, Reason: "exact replay belongs to a superseded binding revision", Selection: selection}, nil
	}
	if found && current.reference != a.Reference {
		prior, priorFound, err := loadReferenceHistory(tx, a.Attempt.ExecutionID, a.Reference)
		if err != nil {
			return conversation.Decision{}, err
		}
		if priorFound {
			// The managed hook transport does not carry a harness-issued event
			// identity. Once a reference has been superseded, a later observation
			// naming it cannot prove that it is a new transition rather than a
			// delayed retry. Preserve the append-only history and fail closed.
			return conversation.Decision{
				Outcome:   conversation.Historical,
				Reason:    "reference belongs to a superseded binding revision",
				Selection: selectionFromRecord(prior),
			}, nil
		}
	}
	if ended {
		if found {
			return conversation.Decision{Outcome: conversation.Historical, Reason: "managed attempt has durably exited", Selection: selectionFromRecord(current)}, nil
		}
		return conversation.Decision{Outcome: conversation.Rejected, Reason: "managed attempt has durably exited"}, nil
	}

	currentRevision := conversation.Revision(0)
	if found {
		currentRevision = current.revision
	}
	if a.ExpectedRevision != currentRevision {
		decision := conversation.Decision{Outcome: conversation.Conflict, Reason: fmt.Sprintf("expected binding revision %d, current revision is %d", a.ExpectedRevision, currentRevision)}
		if found {
			decision.Selection = selectionFromRecord(current)
		}
		return decision, nil
	}

	resumeConversation := conversation.ID("")
	if a.Transition == conversation.Resume {
		var authorized string
		err := tx.QueryRow(`SELECT logical_conversation_id FROM execution_operations
			WHERE intended_execution_id=? AND intended_session_id=? AND conv_id=?
			AND tmux_session=? AND pane_id=? AND state IN ('accepted','started','unknown')
			AND launch_phase IN ('release_granted','released')`, a.Attempt.ExecutionID, a.Attempt.LegacySessionID,
			a.Reference.Value, a.Evidence.TmuxSession, a.Evidence.PaneID).Scan(&authorized)
		if errors.Is(err, sql.ErrNoRows) || strings.TrimSpace(authorized) == "" {
			decision := conversation.Decision{Outcome: conversation.Ambiguous, Reason: "resume requires an authorized logical Conversation target"}
			if found {
				decision.Selection = selectionFromRecord(current)
			}
			return decision, nil
		}
		if err != nil {
			return conversation.Decision{}, err
		}
		resumeConversation = conversation.ID(authorized)
	}
	if !found && a.Transition != conversation.Resume {
		var pendingResume int
		err := tx.QueryRow(`SELECT COUNT(*) FROM execution_operations
			WHERE intended_execution_id=? AND intended_session_id=?
			AND state IN ('requested','accepted','started','unknown','cancelling')`,
			a.Attempt.ExecutionID, a.Attempt.LegacySessionID).Scan(&pendingResume)
		if err != nil {
			return conversation.Decision{}, err
		}
		if pendingResume != 0 {
			return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "pending resume requires exact SessionStart(resume) continuity evidence"}, nil
		}
	}

	owner, err := loadLiveReferenceOwner(tx, a)
	if err != nil {
		return conversation.Decision{}, err
	}
	if owner != "" {
		return conversation.Decision{Outcome: conversation.Conflict, Reason: "external reference is selected by another current managed attempt"}, nil
	}

	conversationID := conversation.ID("")
	if a.Transition == conversation.Resume {
		conversationID = resumeConversation
	} else if found && a.Transition == conversation.Continue {
		conversationID = current.conversationID
	} else {
		conversationID = NewLogicalConversationID()
	}
	nextRevision := currentRevision + 1
	stamp := dbTime(time.Now().UTC())
	if _, err := tx.Exec(`INSERT INTO logical_conversations(id, created_at) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`, conversationID, stamp); err != nil {
		return conversation.Decision{}, err
	}

	if found {
		result, err := tx.Exec(`UPDATE conversation_attempt_bindings SET
			session_id=?, process_instance=?, main_process=?, evidence_strength=?, evidence_source=?,
			observed_pid=?, observed_tmux_session=?, observed_pane_id=?, harness=?, namespace=?,
			conversation_id=?, external_ref=?, revision=?
			WHERE execution_id=? AND revision=?`,
			a.Attempt.LegacySessionID, a.Evidence.ProcessInstance, boolToInt(a.Evidence.MainProcess), a.Evidence.Strength, a.Evidence.Source,
			a.Evidence.PID, a.Evidence.TmuxSession, a.Evidence.PaneID, a.Reference.Harness, a.Reference.Namespace,
			conversationID, a.Reference.Value, nextRevision, a.Attempt.ExecutionID, a.ExpectedRevision)
		if err != nil {
			return conversation.Decision{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return conversation.Decision{}, err
		}
		if rows != 1 {
			return conversation.Decision{Outcome: conversation.Conflict, Reason: "binding revision changed during admission"}, nil
		}
	} else {
		result, err := tx.Exec(`INSERT INTO conversation_attempt_bindings(
			execution_id, session_id, process_instance, main_process, evidence_strength, evidence_source,
			observed_pid, observed_tmux_session, observed_pane_id, harness, namespace,
			conversation_id, external_ref, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(execution_id) DO NOTHING`,
			a.Attempt.ExecutionID, a.Attempt.LegacySessionID, a.Evidence.ProcessInstance, boolToInt(a.Evidence.MainProcess), a.Evidence.Strength, a.Evidence.Source,
			a.Evidence.PID, a.Evidence.TmuxSession, a.Evidence.PaneID, a.Reference.Harness, a.Reference.Namespace,
			conversationID, a.Reference.Value, nextRevision)
		if err != nil {
			return conversation.Decision{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return conversation.Decision{}, err
		}
		if rows != 1 {
			return conversation.Decision{Outcome: conversation.Conflict, Reason: "attempt acquired a binding concurrently"}, nil
		}
	}

	_, err = tx.Exec(`INSERT INTO conversation_reference_bindings(
		execution_id, revision, expected_revision, evidence_id, session_id, conversation_id,
		harness, namespace, external_ref, transition, process_instance, main_process,
		evidence_strength, evidence_source, observed_pid, observed_tmux_session, observed_pane_id, admitted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Attempt.ExecutionID, nextRevision, a.ExpectedRevision, a.Evidence.ID, a.Attempt.LegacySessionID, conversationID,
		a.Reference.Harness, a.Reference.Namespace, a.Reference.Value, a.Transition, a.Evidence.ProcessInstance, boolToInt(a.Evidence.MainProcess),
		a.Evidence.Strength, a.Evidence.Source, a.Evidence.PID, a.Evidence.TmuxSession, a.Evidence.PaneID, stamp)
	if err != nil {
		return conversation.Decision{}, err
	}
	if a.Transition == conversation.Resume {
		result, updateErr := tx.Exec(`UPDATE execution_operations SET state='ready', launch_phase='ready',
			ready_at=?, revision=revision+1 WHERE intended_execution_id=? AND intended_session_id=?
			AND logical_conversation_id=? AND tmux_session=? AND pane_id=?
			AND state IN ('accepted','started','unknown')
			AND launch_phase IN ('release_granted','released')`,
			stamp, a.Attempt.ExecutionID, a.Attempt.LegacySessionID, conversationID,
			a.Evidence.TmuxSession, a.Evidence.PaneID)
		if updateErr != nil {
			return conversation.Decision{}, updateErr
		}
		if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
			if rowsErr != nil {
				return conversation.Decision{}, rowsErr
			}
			return conversation.Decision{Outcome: conversation.Conflict, Reason: "resume readiness operation changed during admission"}, nil
		}
		// Recovery is optional. When the operation references one, readiness and
		// lease confirmation commit in this transaction.
		_, updateErr = tx.Exec(`UPDATE agent_recovery SET status='recovered', reason_code='',
			next_attempt_at=NULL, lease_token='', lease_expires_at=NULL,
			successor_session_id=?, successor_generation=?, recovered_at=?, healthy_since=?, updated_at=?
			WHERE agent_id=(SELECT recovery_agent_id FROM execution_operations WHERE intended_execution_id=?)
			AND predecessor_generation=(SELECT recovery_generation FROM execution_operations WHERE intended_execution_id=?)
			AND status='restarting'`, a.Attempt.LegacySessionID, a.Attempt.ExecutionID,
			stamp, stamp, stamp, a.Attempt.ExecutionID, a.Attempt.ExecutionID)
		if updateErr != nil {
			return conversation.Decision{}, updateErr
		}
	}
	if err := tx.Commit(); err != nil {
		return conversation.Decision{}, err
	}
	return conversation.Decision{
		Outcome: conversation.Accepted,
		Reason:  "managed binding admitted",
		Selection: conversation.Selection{
			Conversation: conversationID,
			Reference:    a.Reference,
			Revision:     nextRevision,
		},
		Changed: true,
	}, nil
}

func validateConversationAdmission(a conversation.Admission) error {
	if a.Origin != conversation.Managed && a.Origin != conversation.Discovered {
		return fmt.Errorf("conversation admission has invalid origin %q", a.Origin)
	}
	if a.ExpectedRevision < 0 {
		return errors.New("conversation admission expected revision cannot be negative")
	}
	if strings.TrimSpace(a.Reference.Harness) == "" || strings.TrimSpace(a.Reference.Namespace) == "" || strings.TrimSpace(a.Reference.Value) == "" {
		return errors.New("conversation admission requires harness, namespace, and external reference")
	}
	switch a.Transition {
	case conversation.Unspecified, conversation.Continue, conversation.Clear, conversation.Resume, conversation.Reincarnate, conversation.Fork:
	default:
		return fmt.Errorf("conversation admission has invalid transition %q", a.Transition)
	}
	if a.Origin == conversation.Discovered {
		return nil
	}
	parsed, err := execution.ParseID(a.Attempt.ExecutionID.String())
	if err != nil {
		return fmt.Errorf("conversation admission execution: %w", err)
	}
	if parsed != a.Attempt.ExecutionID || strings.TrimSpace(a.Attempt.LegacySessionID) == "" {
		return errors.New("conversation admission requires a canonical execution and durable session locator")
	}
	if strings.TrimSpace(a.Evidence.ID) == "" || strings.TrimSpace(a.Evidence.ProcessInstance) == "" {
		return errors.New("conversation admission requires evidence and process-instance identifiers")
	}
	return nil
}

func recheckManagedAttempt(tx *sql.Tx, a conversation.Admission) (bool, conversation.Decision, error) {
	var tmuxSession, harness, generation, paneID, status string
	var pid int
	err := tx.QueryRow(`SELECT tmux_session, pid, harness, exit_callback_generation, exit_callback_pane_id, status FROM sessions WHERE id=?`, a.Attempt.LegacySessionID).
		Scan(&tmuxSession, &pid, &harness, &generation, &paneID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, conversation.Decision{Outcome: conversation.Rejected, Reason: "durable session does not exist"}, nil
	}
	if err != nil {
		return false, conversation.Decision{}, err
	}
	parsedGeneration, err := execution.ParseID(generation)
	if err != nil || parsedGeneration != a.Attempt.ExecutionID {
		return false, conversation.Decision{Outcome: conversation.Rejected, Reason: "durable launch generation does not identify the managed attempt"}, nil
	}
	if harness != a.Reference.Harness {
		return false, conversation.Decision{Outcome: conversation.Rejected, Reason: "durable session harness does not match the reference"}, nil
	}
	if pid <= 0 || tmuxSession == "" || paneID == "" {
		return false, conversation.Decision{Outcome: conversation.Ambiguous, Reason: "durable process or pane association is unavailable"}, nil
	}
	if pid != a.Evidence.PID || tmuxSession != a.Evidence.TmuxSession || paneID != a.Evidence.PaneID {
		return false, conversation.Decision{Outcome: conversation.Rejected, Reason: "host evidence does not match the durable process and pane association"}, nil
	}
	return status == "exited", conversation.Decision{}, nil
}

func loadCurrentBinding(tx *sql.Tx, executionID execution.ID) (bindingRecord, bool, error) {
	var r bindingRecord
	var mainProcess int
	err := tx.QueryRow(`SELECT revision, session_id, conversation_id, harness, namespace, external_ref,
		process_instance, main_process, evidence_strength, evidence_source,
		observed_pid, observed_tmux_session, observed_pane_id
		FROM conversation_attempt_bindings WHERE execution_id=?`, executionID).
		Scan(&r.revision, &r.sessionID, &r.conversationID, &r.reference.Harness, &r.reference.Namespace, &r.reference.Value,
			&r.processInstance, &mainProcess, &r.strength, &r.source, &r.pid, &r.tmuxSession, &r.paneID)
	if errors.Is(err, sql.ErrNoRows) {
		return bindingRecord{}, false, nil
	}
	r.mainProcess = mainProcess != 0
	return r, err == nil, err
}

func loadBindingReplay(tx *sql.Tx, executionID execution.ID, evidenceID string) (bindingRecord, bool, error) {
	var r bindingRecord
	var mainProcess int
	err := tx.QueryRow(`SELECT revision, expected_revision, evidence_id, session_id, conversation_id,
		harness, namespace, external_ref, transition, process_instance, main_process,
		evidence_strength, evidence_source, observed_pid, observed_tmux_session, observed_pane_id
		FROM conversation_reference_bindings WHERE execution_id=? AND evidence_id=?`, executionID, evidenceID).
		Scan(&r.revision, &r.expectedRevision, &r.evidenceID, &r.sessionID, &r.conversationID,
			&r.reference.Harness, &r.reference.Namespace, &r.reference.Value, &r.transition, &r.processInstance, &mainProcess,
			&r.strength, &r.source, &r.pid, &r.tmuxSession, &r.paneID)
	if errors.Is(err, sql.ErrNoRows) {
		return bindingRecord{}, false, nil
	}
	r.mainProcess = mainProcess != 0
	return r, err == nil, err
}

func loadReferenceHistory(tx *sql.Tx, executionID execution.ID, ref conversation.Reference) (bindingRecord, bool, error) {
	var r bindingRecord
	var mainProcess int
	err := tx.QueryRow(`SELECT revision, expected_revision, evidence_id, session_id, conversation_id,
		harness, namespace, external_ref, transition, process_instance, main_process,
		evidence_strength, evidence_source, observed_pid, observed_tmux_session, observed_pane_id
		FROM conversation_reference_bindings
		WHERE execution_id=? AND harness=? AND namespace=? AND external_ref=?
		ORDER BY revision DESC LIMIT 1`, executionID, ref.Harness, ref.Namespace, ref.Value).
		Scan(&r.revision, &r.expectedRevision, &r.evidenceID, &r.sessionID, &r.conversationID,
			&r.reference.Harness, &r.reference.Namespace, &r.reference.Value, &r.transition, &r.processInstance, &mainProcess,
			&r.strength, &r.source, &r.pid, &r.tmuxSession, &r.paneID)
	if errors.Is(err, sql.ErrNoRows) {
		return bindingRecord{}, false, nil
	}
	r.mainProcess = mainProcess != 0
	return r, err == nil, err
}

func loadLiveReferenceOwner(tx *sql.Tx, a conversation.Admission) (string, error) {
	var owner string
	err := tx.QueryRow(`SELECT b.execution_id FROM conversation_attempt_bindings b
		JOIN sessions s ON s.id=b.session_id AND s.exit_callback_generation=b.execution_id
		WHERE s.status<>'exited' AND b.harness=? AND b.namespace=? AND b.external_ref=? AND b.execution_id<>?
		LIMIT 1`, a.Reference.Harness, a.Reference.Namespace, a.Reference.Value, a.Attempt.ExecutionID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return owner, err
}

func sameAdmission(r bindingRecord, a conversation.Admission) bool {
	return r.expectedRevision == a.ExpectedRevision && r.evidenceID == a.Evidence.ID &&
		r.sessionID == a.Attempt.LegacySessionID && r.reference == a.Reference &&
		r.transition == a.Transition && r.processInstance == a.Evidence.ProcessInstance &&
		r.mainProcess == a.Evidence.MainProcess && r.strength == a.Evidence.Strength &&
		r.source == a.Evidence.Source && r.pid == a.Evidence.PID &&
		r.tmuxSession == a.Evidence.TmuxSession && r.paneID == a.Evidence.PaneID
}

func selectionFromRecord(r bindingRecord) conversation.Selection {
	return conversation.Selection{Conversation: r.conversationID, Reference: r.reference, Revision: r.revision}
}
