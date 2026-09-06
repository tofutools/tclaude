package sqlite

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Store) AdmitPrimaryContext(ctx context.Context, in app.PrimaryContextAdmission) (model.Execution, error) {
	evidence := in.Evidence
	if evidence.ExecutionID == "" || evidence.Attempt == 0 || evidence.Provider == "" || evidence.PrimaryCorrelation == "" || evidence.ProviderOrder == "" || evidence.ObservedAt.IsZero() {
		return model.Execution{}, app.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Execution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	execution, err := executionTx(ctx, tx, evidence.ExecutionID)
	if err != nil {
		return model.Execution{}, err
	}
	if execution.Attempt != evidence.Attempt || execution.Spec.Harness != evidence.Provider || execution.State == model.ExecutionExited || execution.State == model.ExecutionFailed {
		return model.Execution{}, app.ErrUnauthorized
	}
	if execution.AgentID != "" {
		var primary model.ExecutionID
		if err := tx.QueryRowContext(ctx, `SELECT primary_execution_id FROM agents WHERE id=?`, execution.AgentID).Scan(&primary); err != nil || primary != execution.ID {
			return model.Execution{}, app.ErrUnauthorized
		}
	}
	var seen int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM native_binding_history WHERE execution_id=? AND attempt_generation=? AND provider=? AND provider_order=?`, execution.ID, execution.Attempt, evidence.Provider, evidence.ProviderOrder).Scan(&seen); err != nil {
		return model.Execution{}, err
	}
	if seen != 0 {
		return execution, tx.Commit()
	}
	if evidence.PriorProviderOrder != execution.ContextOrder {
		return model.Execution{}, app.ErrConflict
	}
	switch evidence.Disposition {
	case ports.PrimaryContextInitial:
		if execution.NativeConversation != nil || evidence.PriorBinding != nil || execution.ContextOrder != "" {
			return model.Execution{}, app.ErrConflict
		}
	case ports.PrimaryContextContinuity:
	case ports.PrimaryContextReset, ports.PrimaryContextUnresolved:
	default:
		return model.Execution{}, app.ErrInvalid
	}
	if (evidence.Disposition == ports.PrimaryContextContinuity || evidence.Disposition == ports.PrimaryContextReset) && !bindingEqual(execution.NativeConversation, evidence.PriorBinding) {
		return model.Execution{}, app.ErrConflict
	}
	if evidence.Disposition != ports.PrimaryContextUnresolved && evidence.NextBinding == nil {
		return model.Execution{}, app.ErrInvalid
	}
	conversationID := execution.ConversationID
	var pendingOperation model.OperationID
	if evidence.Disposition == ports.PrimaryContextReset {
		if in.ResetConversationID == "" || evidence.TransitionCorrelation == "" || evidence.ExpectedConversation == "" || evidence.ExpectedAssociationRevision == 0 {
			return model.Execution{}, app.ErrInvalid
		}
		if err := tx.QueryRowContext(ctx, `SELECT operation_id FROM pending_context_transitions WHERE execution_id=? AND expected_conversation_id=? AND expected_association_revision=? AND correlation=?`, execution.ID, evidence.ExpectedConversation, evidence.ExpectedAssociationRevision, evidence.TransitionCorrelation).Scan(&pendingOperation); err != nil {
			return model.Execution{}, classify(err)
		}
		if execution.ConversationID != evidence.ExpectedConversation {
			return model.Execution{}, app.ErrConflict
		}
		conversationID = in.ResetConversationID
		if execution.AgentID != "" {
			var expected model.Revision
			if err := tx.QueryRowContext(ctx, `SELECT revision FROM agent_conversations WHERE agent_id=? AND current=1`, execution.AgentID).Scan(&expected); err != nil {
				return model.Execution{}, classify(err)
			}
			native := nativeEvidence(evidence.NextBinding, evidence.ObservedAt)
			if err := associateConversationTx(ctx, tx, app.ContextAssociation{ExecutionID: execution.ID, AgentID: execution.AgentID, ConversationID: conversationID, ExpectedRevision: expected, Native: native, At: in.At}); err != nil {
				return model.Execution{}, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,revision,created_at,updated_at) VALUES(?,1,?,?)`, conversationID, nanos(in.At), nanos(in.At)); err != nil {
				return model.Execution{}, classify(err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE executions SET conversation_id=? WHERE id=?`, conversationID, execution.ID); err != nil {
				return model.Execution{}, err
			}
		}
	}
	priorNamespace, priorReference := bindingParts(evidence.PriorBinding)
	nextNamespace, nextReference := bindingParts(evidence.NextBinding)
	_, err = tx.ExecContext(ctx, `INSERT INTO native_binding_history(execution_id,conversation_id,attempt_generation,provider,disposition,prior_namespace,prior_reference,next_namespace,next_reference,primary_correlation,transition_correlation,prior_provider_order,provider_order,observed_at,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, execution.ID, conversationID, execution.Attempt, evidence.Provider, evidence.Disposition, priorNamespace, priorReference, nextNamespace, nextReference, evidence.PrimaryCorrelation, evidence.TransitionCorrelation, evidence.PriorProviderOrder, evidence.ProviderOrder, nanos(evidence.ObservedAt), nanos(in.At))
	if err != nil {
		return model.Execution{}, classify(err)
	}
	if evidence.Disposition == ports.PrimaryContextUnresolved {
		_, err = tx.ExecContext(ctx, `UPDATE executions SET context_readiness=?,context_provider_order=?,revision=revision+1,updated_at=? WHERE id=?`, model.ContextReadinessUnresolved, evidence.ProviderOrder, nanos(in.At), execution.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE executions SET context_readiness=?,context_provider_order=?,native_namespace=?,native_reference=?,native_observed_at=?,revision=revision+1,updated_at=? WHERE id=?`, model.ContextReadinessReady, evidence.ProviderOrder, nextNamespace, nextReference, nanos(evidence.ObservedAt), nanos(in.At), execution.ID)
	}
	if err != nil {
		return model.Execution{}, err
	}
	if pendingOperation != "" {
		result, err := tx.ExecContext(ctx, `UPDATE operations SET state=?,result_code=?,revision=revision+1,updated_at=? WHERE id=? AND state IN (?,?)`, model.OperationSucceeded, "context_evidence_confirmed", nanos(in.At), pendingOperation, model.OperationAdmitted, model.OperationRunning)
		if err != nil {
			return model.Execution{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return model.Execution{}, app.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_context_transitions WHERE operation_id=?`, pendingOperation); err != nil {
			return model.Execution{}, err
		}
	}
	if err := bumpTx(ctx, tx); err != nil {
		return model.Execution{}, err
	}
	updated, err := executionTx(ctx, tx, execution.ID)
	if err != nil {
		return model.Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Execution{}, err
	}
	return updated, nil
}

func (s *Store) BeginContextTransition(ctx context.Context, pending app.PendingContextTransition) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_context_transitions(operation_id,execution_id,expected_conversation_id,expected_association_revision,correlation,created_at) VALUES(?,?,?,?,?,?)`, pending.OperationID, pending.ExecutionID, pending.ExpectedConversationID, pending.ExpectedAssociationRevision, pending.Correlation, nanos(pending.CreatedAt))
	return classify(err)
}

func (s *Store) CancelContextTransition(ctx context.Context, operationID model.OperationID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_context_transitions WHERE operation_id=?`, operationID)
	return err
}

func bindingEqual(current *model.NativeConversationEvidence, reported *model.NativeBinding) bool {
	if current == nil || reported == nil {
		return current == nil && reported == nil
	}
	return current.Namespace == reported.Namespace && current.Reference == reported.Reference
}

func bindingParts(binding *model.NativeBinding) (string, string) {
	if binding == nil {
		return "", ""
	}
	return binding.Namespace, binding.Reference
}

func nativeEvidence(binding *model.NativeBinding, observedAt time.Time) *model.NativeConversationEvidence {
	if binding == nil {
		return nil
	}
	return &model.NativeConversationEvidence{Namespace: binding.Namespace, Reference: binding.Reference, ObservedAt: observedAt}
}
