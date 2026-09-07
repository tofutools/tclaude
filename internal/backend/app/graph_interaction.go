package app

import (
	"context"
	"errors"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type GraphInteractionAdmission struct {
	Attempt           model.WorkAttemptRef
	ExecutionID       model.ExecutionID
	ExecutionRevision model.Revision
	AgentID           model.AgentID
	AgentRevision     model.Revision
}

type GraphInteractionStore interface {
	ClaimGraphInteraction(context.Context, model.OperationID, string, time.Time) (bool, error)
}

func (s *Service) admitGraphInteraction(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, agent model.Agent) (WorkRunRecord, error) {
	execution, err := s.store.Execution(ctx, agent.PrimaryExecutionID)
	if err != nil {
		return record, err
	}
	if execution.State == model.ExecutionExited || execution.State == model.ExecutionFailed {
		return record, fail(ErrUnsupported, "reuse requires a live conversation; choose fresh context to start another execution")
	}
	if _, err = s.runtimeFor(ctx, execution); err != nil {
		return s.recordGraphAttemptUnavailable(ctx, record, attempt, err)
	}
	if attempt.Performer.Agent.WorkspaceID != "" {
		workspace, readErr := s.store.Workspace(ctx, attempt.Performer.Agent.WorkspaceID)
		if readErr != nil {
			return record, readErr
		}
		if workspace.State != model.WorkspaceAvailable || workspace.Observation.ActualPath != execution.Spec.WorkingDirectory {
			return record, fail(ErrConflict, "reused execution does not match the requested workspace")
		}
	}
	now := s.now().UTC()
	issuance := model.WorkIssuanceID(s.newID("issuance_"))
	operation := model.Operation{ID: model.OperationID(s.newID("op_")), RequestID: model.RequestID(issuance), Kind: model.OperationInteract, Principal: record.Run.Requester, ExecutionID: execution.ID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	ref := attempt.Ref
	ref.IssuanceID = issuance
	transition := GraphTransition{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision,
		Authority: model.AuthorityRequest{Principal: record.Run.Requester, Action: model.ActionInteract, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: execution.ID}},
		Operation: &operation, Interaction: &GraphInteractionAdmission{Attempt: ref, ExecutionID: execution.ID, ExecutionRevision: execution.Revision, AgentID: agent.ID, AgentRevision: agent.Revision},
		Updates:  []GraphAttemptUpdate{{Ref: attempt.Ref, NewIssuanceID: issuance, OperationID: operation.ID, ExecutionID: execution.ID, State: model.NodeAttemptAdmitted}},
		RunState: model.WorkRunRunning, ControlState: model.WorkControlActive, At: now}
	admitted, err := s.store.ApplyGraphTransition(ctx, transition)
	if err != nil {
		return record, err
	}
	current, ok := graphAttempt(admitted.Run, ref)
	if !ok {
		return admitted, ErrConflict
	}
	return s.dispatchGraphInteraction(ctx, admitted, current, operation)
}

func (s *Service) dispatchGraphInteraction(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, operation model.Operation) (WorkRunRecord, error) {
	// Only one dispatch may be in flight for this exact operation in a service.
	// After this call returns, a later claim has a different owner and can settle
	// a consumed input whose completion write failed, without sending it again.
	s.graphInteractionMu.Lock()
	if s.graphInteractions[operation.ID] {
		s.graphInteractionMu.Unlock()
		return record, nil
	}
	if s.graphInteractions == nil {
		s.graphInteractions = make(map[model.OperationID]bool)
	}
	s.graphInteractions[operation.ID] = true
	s.graphInteractionMu.Unlock()
	defer func() {
		s.graphInteractionMu.Lock()
		delete(s.graphInteractions, operation.ID)
		s.graphInteractionMu.Unlock()
	}()
	owner := randomID("dispatch_")
	store, ok := s.store.(GraphInteractionStore)
	if !ok {
		return record, ErrUnsupported
	}
	execution, err := s.store.Execution(ctx, operation.ExecutionID)
	if err != nil {
		return record, err
	}
	runtime, err := s.runtimeFor(ctx, execution)
	if err != nil {
		return record, err
	}
	claimed, err := store.ClaimGraphInteraction(ctx, operation.ID, owner, s.now().UTC())
	if err != nil {
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrConflict) {
			settlement, finish := settlementContext(ctx)
			defer finish()
			if _, writeErr := s.store.CompleteOperation(settlement, OperationCompletion{OperationID: operation.ID, OperationState: model.OperationRefused, ResultCode: "work_input_refused", Detail: err.Error(), ExecutionID: execution.ID, At: s.now().UTC()}); writeErr != nil {
				return record, writeErr
			}
			return s.reconcileAgentAttempt(settlement, record, attempt)
		}
		return record, err
	}
	if !claimed {
		latest, readErr := s.store.OperationResult(ctx, operation.ID)
		if readErr != nil {
			return record, readErr
		}
		if latest.Operation.State == model.OperationAdmitted {
			return record, nil
		}
		return s.reconcileAgentAttempt(ctx, record, attempt)
	}
	effectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	response, effectErr := runtime.Interact(effectCtx, ports.Interaction{Text: attempt.Performer.Agent.Brief})
	if effectErr != nil || response.Disposition == "" {
		response.Disposition = ports.EffectUnknown
	}
	completion := completionFromDisposition(operation, execution, response.Disposition, response.Evidence, "work_input_accepted", effectErr, s.now().UTC())
	settlement, finish := settlementContext(ctx)
	defer finish()
	if _, err = s.store.CompleteOperation(settlement, completion); err != nil {
		return record, err
	}
	return s.reconcileAgentAttempt(settlement, record, attempt)
}

// Fresh context may stop only an execution already owned by a concluded stage
// of this exact task activation. It never stops an unrelated primary execution.
func (s *Service) prepareFreshTaskAgent(ctx context.Context, record WorkRunRecord, current model.WorkNodeAttempt, agent model.Agent, group model.CompiledTaskGroup) (bool, error) {
	execution, err := s.store.Execution(ctx, agent.PrimaryExecutionID)
	if err != nil {
		return false, err
	}
	if execution.State == model.ExecutionExited || execution.State == model.ExecutionFailed {
		return true, nil
	}
	owned := false
	for _, prior := range record.Run.NodeAttempts {
		priorGroup, ok := taskGroup(*record.Run.Graph, prior.Ref.NodeID)
		if ok && priorGroup.ID == group.ID && prior.Ref.ActivationID == current.Ref.ActivationID && prior.ExecutionID == execution.ID && graphAttemptTerminal(prior.State) && prior.State != model.NodeAttemptUncertain {
			owned = true
		}
	}
	if !owned {
		return false, fail(ErrConflict, "agent primary is not a concluded stage owned by this task")
	}
	requestID := model.RequestID(deterministicOrchestrationID("request_", string(record.Run.ID)+":stage-stop:"+string(execution.ID)))
	stopped, err := s.Stop(ctx, StopRequest{RequestContext: RequestContext{Principal: record.Run.Requester, RequestID: requestID}, ExecutionID: execution.ID})
	if err != nil {
		return false, err
	}
	switch stopped.Operation.State {
	case model.OperationSucceeded:
	case model.OperationUncertain:
		return false, fail(ErrUncertain, "task stage stop outcome is uncertain")
	case model.OperationFailed, model.OperationRefused:
		return false, fail(ErrConflict, "task stage stop was refused or failed: %s", stopped.Operation.Detail)
	default:
		return false, nil
	}
	_, err = s.Observe(ctx, ObserveRequest{Principal: record.Run.Requester, ExecutionID: execution.ID})
	if err != nil && !errors.Is(err, ErrConflict) {
		return false, err
	}
	execution, err = s.store.Execution(ctx, execution.ID)
	return err == nil && (execution.State == model.ExecutionExited || execution.State == model.ExecutionFailed), err
}
