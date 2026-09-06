package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// advanceGraphWork advances at most one effectful node per pass. Pure graph
// transitions may cascade inside graphOutcomeTransition, while every external
// effect first receives an exact durable issuance and Operation.
func (s *Service) advanceGraphWork(ctx context.Context, record WorkRunRecord) (WorkRunRecord, error) {
	if record.Run.Graph == nil {
		return record, nil
	}
	now := s.now().UTC()
	if record.Run.State != model.WorkRunFailed && record.Run.State != model.WorkRunCancelled && record.Run.State != model.WorkRunUncertain && !now.Before(record.Run.Deadline) {
		return s.expireGraphWork(ctx, record, now)
	}
	for _, attempt := range record.Run.NodeAttempts {
		if (attempt.State != model.NodeAttemptWaiting && attempt.State != model.NodeAttemptBlocked) || attempt.DecisionID == "" {
			continue
		}
		decision, err := s.store.Decision(ctx, attempt.DecisionID)
		if err != nil {
			return record, err
		}
		if decision.Submission != nil {
			return s.applyAnsweredDecision(ctx, record, attempt, *decision.Submission)
		}
	}
	for _, attempt := range record.Run.NodeAttempts {
		if attempt.State == model.NodeAttemptRetryWait && attempt.RetryAt != nil && !now.Before(*attempt.RetryAt) {
			transition := GraphTransition{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision,
				Updates:  []GraphAttemptUpdate{{Ref: attempt.Ref, State: model.NodeAttemptReady}},
				RunState: model.WorkRunRunning, ControlState: model.WorkControlActive, RunOutcome: record.Run.Outcome, At: now}
			return s.store.ApplyGraphTransition(ctx, transition)
		}
	}
	for _, attempt := range record.Run.NodeAttempts {
		if attempt.State != model.NodeAttemptReady {
			continue
		}
		node := graphNode(*record.Run.Graph, attempt.Ref.NodeID)
		if node.Kind == model.WorkNodeWait {
			if now.Before(attempt.ReadyAt.Add(node.Wait.Duration)) {
				continue
			}
			transition := s.graphOutcomeTransition(record, attempt, model.WorkOutcomeVerified, "wait elapsed")
			return s.store.ApplyGraphTransition(ctx, transition)
		}
		if attempt.Performer == nil {
			continue
		}
		switch attempt.Performer.Kind {
		case model.PerformerProgram:
			return s.admitAndRunProgram(ctx, record, attempt)
		case model.PerformerAgent:
			return s.admitAndRunAgent(ctx, record, attempt)
		case model.PerformerHuman:
			return record, fail(ErrInvalid, "human node %s has no decision audience", node.ID)
		}
	}
	for _, attempt := range record.Run.NodeAttempts {
		if attempt.State != model.NodeAttemptAdmitted && attempt.State != model.NodeAttemptRunning {
			continue
		}
		if attempt.Performer != nil && attempt.Performer.Kind == model.PerformerProgram {
			return s.reconcileProgramAttempt(ctx, record, attempt)
		}
		if attempt.State == model.NodeAttemptAdmitted && attempt.Performer != nil && attempt.Performer.Kind == model.PerformerAgent {
			return s.reconcileAgentAttempt(ctx, record, attempt)
		}
	}
	if record.Run.State == model.WorkRunFailed && record.Run.ControlState == model.WorkControlDraining && !hasOwnedGraphEffects(record.Run.NodeAttempts) {
		transition := GraphTransition{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, RunState: record.Run.State, ControlState: model.WorkControlSettled, RunOutcome: record.Run.Outcome, At: now}
		return s.store.ApplyGraphTransition(ctx, transition)
	}
	return record, nil
}

func (s *Service) expireGraphWork(ctx context.Context, record WorkRunRecord, now time.Time) (WorkRunRecord, error) {
	transition := GraphTransition{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, RunState: model.WorkRunFailed, ControlState: model.WorkControlSettled, RunOutcome: model.WorkOutcomeExpired, At: now}
	for _, attempt := range record.Run.NodeAttempts {
		switch attempt.State {
		case model.NodeAttemptReady, model.NodeAttemptRetryWait, model.NodeAttemptBlocked, model.NodeAttemptWaiting:
			transition.Updates = append(transition.Updates, GraphAttemptUpdate{Ref: attempt.Ref, State: model.NodeAttemptSuppressed, Outcome: model.WorkOutcomeExpired, Detail: "suppressed after work deadline"})
		case model.NodeAttemptAdmitted, model.NodeAttemptRunning, model.NodeAttemptUncertain:
			transition.ControlState = model.WorkControlDraining
		}
	}
	return s.store.ApplyGraphTransition(ctx, transition)
}

func hasOwnedGraphEffects(attempts []model.WorkNodeAttempt) bool {
	for _, attempt := range attempts {
		if attempt.State == model.NodeAttemptAdmitted || attempt.State == model.NodeAttemptRunning || attempt.State == model.NodeAttemptUncertain {
			return true
		}
	}
	return false
}

func (s *Service) recoverProgramExecution(ctx context.Context, execution model.Execution, report *RecoveryReport) error {
	unknown := func(evidence model.ProviderEvidence) error {
		report.Unknown = append(report.Unknown, execution.ID)
		_, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionUnknown, nil, evidence, s.now().UTC())
		return err
	}
	if s.programHost == nil {
		return unknown(execution.Evidence)
	}
	use, err := s.store.WorkspaceUseForExecution(ctx, execution.ID)
	if err != nil || use.ReleasedAt != nil {
		return unknown(execution.Evidence)
	}
	record, err := s.store.WorkRun(ctx, use.WorkRunID)
	if err != nil {
		return unknown(execution.Evidence)
	}
	var attempt model.WorkNodeAttempt
	found := false
	for _, candidate := range record.Run.NodeAttempts {
		if candidate.ExecutionID == execution.ID && candidate.Performer != nil && candidate.Performer.Program != nil {
			attempt, found = candidate, true
			break
		}
	}
	if !found {
		return unknown(execution.Evidence)
	}
	profile, err := s.store.ProgramProfileRevision(ctx, attempt.Performer.Program.Profile.RevisionID)
	if err != nil {
		return unknown(execution.Evidence)
	}
	workspace, err := s.store.Workspace(ctx, use.WorkspaceID)
	if err != nil {
		return unknown(execution.Evidence)
	}
	workingDirectory, err := programWorkingDirectory(workspace.Observation.ActualPath, profile.WorkingDirectory)
	if err != nil {
		return unknown(execution.Evidence)
	}
	result, recoverErr := s.programHost.RecoverProgram(ctx, ports.ProgramRecoveryRequest{Execution: execution, Profile: profile, Workspace: workspace, WorkspaceUse: use, WorkingDirectory: workingDirectory, Evidence: execution.Evidence})
	if recoverErr != nil || result.State == ports.RecoveryUnknown || result.Runtime == nil || result.Runtime.ExecutionID() != execution.ID {
		evidence := result.Evidence
		if !validProviderEvidence(evidence) {
			evidence = execution.Evidence
		}
		return unknown(evidence)
	}
	s.rememberProgramRuntime(result.Runtime)
	evidence := result.Observation.Evidence
	if !validProviderEvidence(evidence) {
		evidence = result.Evidence
	}
	if result.State == ports.RecoveryExited || result.Observation.Workload == ports.WorkloadExited {
		state, outcome, detail := model.ExecutionExited, model.WorkOutcomeVerified, "program exited during recovery"
		if result.Observation.ExitCode == nil || *result.Observation.ExitCode != 0 {
			state, outcome, detail = model.ExecutionFailed, model.WorkOutcomeRejected, fmt.Sprintf("program exited with code %v", result.Observation.ExitCode)
		}
		if _, err = s.store.RecordRecovery(context.WithoutCancel(ctx), execution.ID, state, nil, evidence, s.now().UTC()); err != nil {
			return err
		}
		updated, settleErr := s.settleProgramAttempt(ctx, record, attempt, outcome, detail)
		if settleErr != nil {
			return settleErr
		}
		_ = updated
		if err = result.Runtime.ReleaseProgramResources(context.WithoutCancel(ctx), evidence); err != nil {
			return err
		}
		if err = s.store.ReleaseWorkspaceUse(ctx, use.ID, execution.ID, s.now().UTC()); err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
		report.Exited = append(report.Exited, execution.ID)
		return nil
	}
	if _, err = s.store.RecordRecovery(ctx, execution.ID, model.ExecutionRunning, nil, evidence, s.now().UTC()); err != nil {
		return err
	}
	report.Controlled = append(report.Controlled, execution.ID)
	return nil
}

// reconcileProgramResourceCleanup closes the post-exit crash/retry gap. A
// runtime may observe process exit before its independent bounded-output
// spoolers are complete, so cleanup is retried until WorkloadExited and the
// host acknowledges the exact final observation evidence.
func (s *Service) reconcileProgramResourceCleanup(ctx context.Context) error {
	executions, err := s.store.RecoverableExecutions(ctx)
	if err != nil {
		return err
	}
	for _, execution := range executions {
		if execution.Workload != model.ExecutionWorkloadProgram || (execution.State != model.ExecutionExited && execution.State != model.ExecutionFailed) {
			continue
		}
		use, useErr := s.store.WorkspaceUseForExecution(ctx, execution.ID)
		if useErr != nil || use.ReleasedAt != nil {
			continue
		}
		runtime := s.programRuntime(execution.ID)
		if runtime == nil {
			continue
		}
		observation, observeErr := runtime.ObserveProgram(ctx)
		if observeErr != nil || observation.Workload != ports.WorkloadExited || !validProviderEvidence(observation.Evidence) {
			continue
		}
		if _, err = s.store.RecordRecovery(context.WithoutCancel(ctx), execution.ID, execution.State, nil, observation.Evidence, s.now().UTC()); err != nil {
			return err
		}
		if err = runtime.ReleaseProgramResources(context.WithoutCancel(ctx), observation.Evidence); err != nil {
			continue
		}
		if err = s.store.ReleaseWorkspaceUse(ctx, use.ID, execution.ID, s.now().UTC()); err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return nil
}

func (s *Service) admitAndRunProgram(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt) (WorkRunRecord, error) {
	if s.programHost == nil {
		return record, fail(ErrUnavailable, "program host is unavailable")
	}
	if attempt.Performer == nil || attempt.Performer.Program == nil {
		return record, fail(ErrInvalid, "program performer is incomplete")
	}
	profile, err := s.store.ProgramProfileRevision(ctx, attempt.Performer.Program.Profile.RevisionID)
	if err != nil {
		return record, err
	}
	if profile.ProfileID != attempt.Performer.Program.Profile.ProfileID || profile.ContentHash != attempt.Performer.Program.Profile.ContentHash {
		return record, fail(ErrConflict, "program profile pin changed")
	}
	workspace, err := s.store.Workspace(ctx, record.Run.Scope.WorkspaceID)
	if err != nil {
		return record, err
	}
	if workspace.State != model.WorkspaceAvailable || strings.TrimSpace(workspace.Observation.ActualPath) == "" {
		return record, fail(ErrConflict, "program workspace is not available")
	}
	workingDirectory, err := programWorkingDirectory(workspace.Observation.ActualPath, profile.WorkingDirectory)
	if err != nil {
		return record, err
	}
	if len(profile.EffectAuthority) == 0 {
		return record, fail(ErrInvalid, "program profile has no effect authority")
	}
	var executeRequirement model.ProgramEffectRequirement
	var additionalAuthority []model.AuthorityRequest
	for _, requirement := range profile.EffectAuthority {
		if requirement.Action == model.ActionExecuteProgram && requirement.Resource.Kind == model.ResourceWorkspace {
			executeRequirement = requirement
			continue
		}
		bound := bindProgramAuthority(requirement, workspace.ID)
		bound.Principal = record.Run.Requester
		additionalAuthority = append(additionalAuthority, bound)
	}
	authority := bindProgramAuthority(executeRequirement, workspace.ID)
	authority.Principal = record.Run.Requester
	now := s.now().UTC()
	issuanceID := model.WorkIssuanceID(s.newID("issuance_"))
	operationID := model.OperationID(s.newID("op_"))
	executionID := model.ExecutionID(s.newID("exe_"))
	execution := model.Execution{
		ID: executionID, Workload: model.ExecutionWorkloadProgram, State: model.ExecutionReserved,
		Attempt: 1, ContextReadiness: model.ContextReadinessPending, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Spec: model.ResolvedExecutionSpec{ExecutionID: executionID, Workload: model.ExecutionWorkloadProgram, Attempt: 1, WorkingDirectory: workingDirectory, Sandbox: profile.Sandbox},
	}
	operation := model.Operation{ID: operationID, RequestID: model.RequestID(issuanceID), Kind: model.OperationRunProgram, Principal: record.Run.Requester, ExecutionID: executionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	use := model.WorkspaceUse{ID: model.WorkspaceUseID(s.newID("workspace_use_")), WorkspaceID: workspace.ID, ExecutionID: executionID, WorkRunID: record.Run.ID, CreatedAt: now}
	transition := GraphTransition{
		WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, Authority: authority, AdditionalAuthority: additionalAuthority,
		Operation: &operation, Execution: &execution, WorkspaceUse: &use,
		Updates:  []GraphAttemptUpdate{{Ref: attempt.Ref, NewIssuanceID: issuanceID, OperationID: operationID, ExecutionID: executionID, State: model.NodeAttemptAdmitted}},
		RunState: model.WorkRunRunning, ControlState: model.WorkControlActive, At: now,
	}
	admitted, err := s.store.ApplyGraphTransition(ctx, transition)
	if err != nil {
		return record, err
	}
	admittedAttempt, ok := graphAttempt(admitted.Run, model.WorkAttemptRef{RunID: attempt.Ref.RunID, NodeID: attempt.Ref.NodeID, ActivationID: attempt.Ref.ActivationID, Attempt: attempt.Ref.Attempt, IssuanceID: issuanceID})
	if !ok {
		return admitted, ErrConflict
	}
	return s.executeProgram(ctx, admitted, admittedAttempt, profile, workspace, use, workingDirectory)
}

func (s *Service) executeProgram(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, profile model.ProgramProfileRevision, workspace model.Workspace, use model.WorkspaceUse, workingDirectory string) (WorkRunRecord, error) {
	execution, err := s.store.Execution(ctx, attempt.ExecutionID)
	if err != nil {
		return record, err
	}
	workflowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	prepared, err := s.programHost.PrepareProgram(workflowCtx, ports.ProgramPreparationRequest{
		Execution: execution, Profile: profile,
		Arguments: append([]string(nil), attempt.Performer.Program.Arguments...), Input: append([]byte(nil), attempt.Performer.Program.Input...),
		Workspace: workspace, WorkspaceUse: use, WorkingDirectory: workingDirectory, Deadline: attempt.Deadline,
	})
	if err != nil {
		return s.failProgramOperation(ctx, record, attempt, model.OperationFailed, model.ExecutionFailed, "prepare_failed", err)
	}
	description := prepared.Describe()
	if description.ExecutionID != attempt.ExecutionID || description.Attempt != 1 || !validProviderEvidence(description.Evidence) || description.EffectivePolicy.Sandbox != profile.Sandbox || !description.EffectivePolicy.Enforced || (description.Requirements.WorkingDirectory != "" && description.Requirements.WorkingDirectory != workingDirectory) {
		_ = prepared.Abort(workflowCtx)
		return s.failProgramOperation(ctx, record, attempt, model.OperationFailed, model.ExecutionFailed, "invalid_preparation", fail(ErrInvalid, "program host returned mismatched preparation"))
	}
	if _, err = s.store.RecordPrepared(context.WithoutCancel(ctx), attempt.ExecutionID, attempt.OperationID, description.Evidence, s.now().UTC()); err != nil {
		_ = prepared.Abort(workflowCtx)
		return record, err
	}
	permit := &releasePermit{store: s.store, executionID: attempt.ExecutionID, operationID: attempt.OperationID, now: s.now}
	released, releaseErr := prepared.Release(workflowCtx, permit)
	if !permit.consumed.Load() && releaseErr == nil {
		releaseErr = fail(ErrInvalid, "program host released without consuming permit")
	}
	evidence := released.Evidence
	if !validProviderEvidence(evidence) {
		evidence = description.Evidence
	}
	if releaseErr != nil || released.State == ports.ReleaseUncertain || released.Runtime == nil || released.Runtime.ExecutionID() != attempt.ExecutionID {
		detail := "program release uncertain"
		if releaseErr != nil {
			detail = releaseErr.Error()
		}
		_, persistErr := s.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: attempt.OperationID, OperationState: model.OperationUncertain, ResultCode: "release_uncertain", Detail: detail, ExecutionID: attempt.ExecutionID, ExecutionState: model.ExecutionUnknown, UpdateExecutionState: true, Evidence: evidence, At: s.now().UTC()})
		if released.Runtime != nil && released.Runtime.ExecutionID() == attempt.ExecutionID {
			s.rememberProgramRuntime(released.Runtime)
		}
		if persistErr != nil {
			return record, persistErr
		}
		return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeUnknown, detail)
	}
	s.rememberProgramRuntime(released.Runtime)
	if _, err = s.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: attempt.OperationID, OperationState: model.OperationSucceeded, ResultCode: "released", ExecutionID: attempt.ExecutionID, ExecutionState: model.ExecutionRunning, UpdateExecutionState: true, Evidence: evidence, At: s.now().UTC()}); err != nil {
		return record, err
	}
	return s.reconcileProgramAttempt(ctx, record, attempt)
}

func (s *Service) reconcileProgramAttempt(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt) (WorkRunRecord, error) {
	operation, err := s.store.OperationResult(ctx, attempt.OperationID)
	if err != nil {
		return record, err
	}
	switch operation.Operation.State {
	case model.OperationUncertain:
		return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeUnknown, operation.Operation.Detail)
	case model.OperationFailed, model.OperationRefused:
		return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeRejected, operation.Operation.Detail)
	case model.OperationAdmitted, model.OperationRunning:
		return record, nil
	}
	execution, err := s.store.Execution(ctx, attempt.ExecutionID)
	if err != nil {
		return record, err
	}
	if execution.State == model.ExecutionUnknown {
		return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeUnknown, "program state is unknown")
	}
	runtime := s.programRuntime(attempt.ExecutionID)
	if runtime == nil {
		if execution.State == model.ExecutionExited {
			return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeVerified, "program exited")
		}
		return record, nil
	}
	observation, observeErr := runtime.ObserveProgram(ctx)
	if observeErr != nil || observation.Workload == ports.WorkloadUnknown {
		detail := "program observation unknown"
		if observeErr != nil {
			detail = observeErr.Error()
		}
		if _, err = s.store.RecordRecovery(context.WithoutCancel(ctx), attempt.ExecutionID, model.ExecutionUnknown, nil, observation.Evidence, s.now().UTC()); err != nil {
			return record, err
		}
		return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeUnknown, detail)
	}
	if observation.Workload != ports.WorkloadExited {
		return record, nil
	}
	state, outcome := model.ExecutionExited, model.WorkOutcomeVerified
	detail := "program exited"
	if observation.ExitCode == nil || *observation.ExitCode != 0 {
		state, outcome, detail = model.ExecutionFailed, model.WorkOutcomeRejected, fmt.Sprintf("program exited with code %v", observation.ExitCode)
	}
	if _, err = s.store.RecordRecovery(context.WithoutCancel(ctx), attempt.ExecutionID, state, nil, observation.Evidence, s.now().UTC()); err != nil {
		return record, err
	}
	updated, err := s.settleProgramAttempt(ctx, record, attempt, outcome, detail)
	if err != nil {
		return updated, err
	}
	if err = runtime.ReleaseProgramResources(context.WithoutCancel(ctx), observation.Evidence); err != nil {
		return updated, err
	}
	if use, useErr := s.store.WorkspaceUseForExecution(ctx, attempt.ExecutionID); useErr == nil && use.ReleasedAt == nil {
		if err = s.store.ReleaseWorkspaceUse(ctx, use.ID, attempt.ExecutionID, s.now().UTC()); err != nil && !errors.Is(err, ErrConflict) {
			return updated, err
		}
	}
	return updated, nil
}

func (s *Service) settleProgramAttempt(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, outcome model.WorkOutcome, detail string) (WorkRunRecord, error) {
	latest, err := s.store.WorkRun(ctx, record.Run.ID)
	if err != nil {
		return record, err
	}
	current, ok := graphAttempt(latest.Run, attempt.Ref)
	if !ok {
		return latest, ErrConflict
	}
	if current.State == model.NodeAttemptSucceeded || current.State == model.NodeAttemptFailed || current.State == model.NodeAttemptUncertain {
		return latest, nil
	}
	transition := s.graphOutcomeTransition(latest, current, outcome, detail)
	return s.store.ApplyGraphTransition(ctx, transition)
}

func (s *Service) failProgramOperation(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, operationState model.OperationState, executionState model.ExecutionState, code string, cause error) (WorkRunRecord, error) {
	_, persistErr := s.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: attempt.OperationID, OperationState: operationState, ResultCode: code, Detail: cause.Error(), ExecutionID: attempt.ExecutionID, ExecutionState: executionState, UpdateExecutionState: true, At: s.now().UTC()})
	if persistErr != nil {
		return record, persistErr
	}
	updated, settleErr := s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeRejected, cause.Error())
	if settleErr != nil {
		return updated, settleErr
	}
	return updated, cause
}

func (s *Service) rememberProgramRuntime(runtime ports.ProgramRuntime) {
	s.runtimeMu.Lock()
	s.programRuntimes[runtime.ExecutionID()] = runtime
	s.runtimeMu.Unlock()
}

func (s *Service) programRuntime(id model.ExecutionID) ports.ProgramRuntime {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.programRuntimes[id]
}

func programWorkingDirectory(workspacePath, relative string) (string, error) {
	root := filepath.Clean(workspacePath)
	working := root
	if strings.TrimSpace(relative) != "" {
		if filepath.IsAbs(relative) {
			return "", fail(ErrInvalid, "program working directory must be relative to the workspace")
		}
		working = filepath.Clean(filepath.Join(root, relative))
	}
	rel, err := filepath.Rel(root, working)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fail(ErrInvalid, "program working directory escapes the workspace")
	}
	return working, nil
}

func bindProgramAuthority(requirement model.ProgramEffectRequirement, workspaceID model.WorkspaceID) model.AuthorityRequest {
	resource := requirement.Resource
	if resource.Kind == model.ResourceWorkspace && resource.WorkspaceID == "" {
		resource.WorkspaceID = workspaceID
	}
	return model.AuthorityRequest{Action: requirement.Action, Resource: resource, RequestedConfiguration: requirement.RequestedConfiguration}
}

func validProviderEvidence(e model.ProviderEvidence) bool {
	return strings.TrimSpace(e.Provider) != "" && e.Version > 0 && len(e.Payload) > 0
}
