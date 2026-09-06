package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ConversationBindingAttempt is the verified runtime identity supplied by an
// execution adapter. External references are never sufficient to admit a
// binding.
type ConversationBindingAttempt struct {
	ExecutionID      string
	SessionID        string
	LaunchGeneration string
	ProcessInstance  string
	Harness          string
	Namespace        string
}

type ConversationBindingAdmission struct {
	Attempt             ConversationBindingAttempt
	ExternalRef         string
	LogicalConversation string
	Transition          string // continue, clear, resume, reincarnate, fork
	Revision            int64
}

// NewLogicalConversationID mints an opaque platform-owned history identity.
func NewLogicalConversationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("db: crypto/rand failed generating conversation id: " + err.Error())
	}
	return "cvn_" + hex.EncodeToString(b[:])
}

// AdmitConversationBinding records a verified current binding and its
// historical reference interval atomically. The caller must supply an
// execution/process proof and an expected revision; stale attempts are
// rejected instead of moving the selected history backwards.
func AdmitConversationBinding(a ConversationBindingAdmission) error {
	if strings.TrimSpace(a.Attempt.ExecutionID) == "" || strings.TrimSpace(a.Attempt.SessionID) == "" ||
		strings.TrimSpace(a.Attempt.LaunchGeneration) == "" || strings.TrimSpace(a.Attempt.ProcessInstance) == "" ||
		strings.TrimSpace(a.Attempt.Harness) == "" || strings.TrimSpace(a.Attempt.Namespace) == "" ||
		strings.TrimSpace(a.ExternalRef) == "" || strings.TrimSpace(a.LogicalConversation) == "" {
		return errors.New("conversation binding requires verified attempt, namespace, external ref, and logical conversation")
	}
	if a.Transition == "" {
		a.Transition = "continue"
	}
	if a.Revision < 1 {
		return errors.New("conversation binding revision must be positive")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	var currentRev int64
	err = tx.QueryRow(`SELECT conversation_id, revision FROM conversation_attempt_bindings WHERE execution_id = ?`, a.Attempt.ExecutionID).Scan(&current, &currentRev)
	if err == nil {
		if a.Revision < currentRev {
			return fmt.Errorf("stale conversation binding revision %d (current %d)", a.Revision, currentRev)
		}
		if a.Revision == currentRev && current != a.LogicalConversation {
			return fmt.Errorf("conversation binding revision %d already selects %s", a.Revision, current)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO logical_conversations(id, created_at) VALUES (?, ?)`, a.LogicalConversation, dbTime(time.Now().UTC())); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO conversation_attempt_bindings(execution_id, session_id, launch_generation, process_instance, harness, namespace, conversation_id, external_ref, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(execution_id) DO UPDATE SET session_id=excluded.session_id, launch_generation=excluded.launch_generation, process_instance=excluded.process_instance, harness=excluded.harness, namespace=excluded.namespace, conversation_id=excluded.conversation_id, external_ref=excluded.external_ref, revision=excluded.revision`, a.Attempt.ExecutionID, a.Attempt.SessionID, a.Attempt.LaunchGeneration, a.Attempt.ProcessInstance, a.Attempt.Harness, a.Attempt.Namespace, a.LogicalConversation, a.ExternalRef, a.Revision); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO conversation_reference_bindings(execution_id, revision, conversation_id, harness, namespace, external_ref, transition, admitted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(execution_id, revision) DO UPDATE SET conversation_id=excluded.conversation_id, harness=excluded.harness, namespace=excluded.namespace, external_ref=excluded.external_ref, transition=excluded.transition, admitted_at=excluded.admitted_at`, a.Attempt.ExecutionID, a.Revision, a.LogicalConversation, a.Attempt.Harness, a.Attempt.Namespace, a.ExternalRef, a.Transition, dbTime(time.Now().UTC())); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO conversation_binding_observations(execution_id, external_ref, outcome, reason, process_instance, received_at) VALUES (?, ?, 'accepted', ?, ?, ?) ON CONFLICT(execution_id, external_ref, outcome, reason) DO UPDATE SET count=count+1, received_at=excluded.received_at`, a.Attempt.ExecutionID, a.ExternalRef, a.Transition, a.Attempt.ProcessInstance, dbTime(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}
