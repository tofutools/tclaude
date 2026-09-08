package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Service) admitAndRunAgent(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt) (WorkRunRecord, error) {
	performer := attempt.Performer.Agent
	if performer == nil || performer.AgentID == "" {
		return record, fail(ErrUnsupported, "agent graph node requires a deployed agent binding")
	}
	agent, err := s.store.Agent(ctx, performer.AgentID)
	if err != nil {
		return record, err
	}
	if performer.ContextPolicy == model.AgentContextReuse && agent.PrimaryExecutionID != "" {
		return s.admitGraphInteraction(ctx, record, attempt, agent)
	}
	if grouped, ok := taskGroup(*record.Run.Graph, attempt.Ref.NodeID); ok && agent.PrimaryExecutionID != "" {
		ready, cleanupErr := s.prepareFreshTaskAgent(ctx, record, attempt, agent, grouped)
		if cleanupErr != nil || !ready {
			return record, cleanupErr
		}
		agent, err = s.store.Agent(ctx, agent.ID)
		if err != nil {
			return record, err
		}
	}
	desired := agent.Desired
	var workspaceUse *model.WorkspaceUse
	var additionalAuthority []model.AuthorityRequest
	if performer.WorkspaceID != "" {
		workspace, readErr := s.store.Workspace(ctx, performer.WorkspaceID)
		if readErr != nil {
			return record, readErr
		}
		if workspace.State != model.WorkspaceAvailable || strings.TrimSpace(workspace.Observation.ActualPath) == "" {
			return record, fail(ErrConflict, "agent workspace is not available")
		}
		desired.WorkingDirectory = workspace.Observation.ActualPath
		additionalAuthority = append(additionalAuthority, model.AuthorityRequest{Principal: record.Run.Requester, Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}})
	}
	if desired.HostSandbox != nil {
		return record, fail(ErrUnsupported, "provider host sandbox preparation is not configured")
	}
	provider, ok := s.providers.Provider(desired.Harness)
	if !ok {
		return s.recordGraphAttemptUnavailable(ctx, record, attempt, fail(ErrUnavailable, "harness %q has no provider", desired.Harness))
	}
	if !provider.Capabilities().PreparedInitialInput {
		return record, fail(ErrUnsupported, "provider %q cannot prepare required first work", provider.Name())
	}
	now := s.now().UTC()
	issuanceID := model.WorkIssuanceID(s.newID("issuance_"))
	operationID := model.OperationID(s.newID("op_"))
	executionID := model.ExecutionID(s.newID("exe_"))
	conversationID := model.ConversationID(s.newID("con_"))
	spec := resolvedSpec(executionID, agent.ID, desired, conversationID)
	spec.ConfigurationProfile = agent.ConfigurationProfile
	execution := model.Execution{ID: executionID, Workload: model.ExecutionWorkloadHarness, AgentID: agent.ID, ConversationID: conversationID, Spec: spec, State: model.ExecutionReserved, Attempt: 1, ContextReadiness: model.ContextReadinessPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = s.requireNativeGuidanceComposition(ctx, execution); errors.Is(err, ErrUnavailable) {
		return s.recordGraphAttemptUnavailable(ctx, record, attempt, err)
	} else if err != nil {
		return record, err
	}
	operation := model.Operation{ID: operationID, RequestID: model.RequestID(issuanceID), Kind: model.OperationAssignWork, Principal: record.Run.Requester, ExecutionID: executionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	authority := model.AuthorityRequest{Principal: record.Run.Requester, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agent.ID}, RequestedConfiguration: &desired}
	if performer.WorkspaceID != "" {
		workspaceUse = &model.WorkspaceUse{ID: model.WorkspaceUseID(s.newID("workspace_use_")), WorkspaceID: performer.WorkspaceID, ExecutionID: executionID, WorkRunID: record.Run.ID, CreatedAt: now}
	}
	var access *model.ExecutionAccess
	var credential *ports.ActionCredentialMaterial
	if _, capable := provider.(ports.ActionCredentialProvider); capable {
		if strings.TrimSpace(s.agentAPIEndpoint) == "" {
			return s.recordGraphAttemptUnavailable(ctx, record, attempt, fail(ErrUnavailable, "agent API endpoint is required for credential-capable provider"))
		}
		secret, generateErr := generateActionCredential()
		if generateErr != nil {
			return record, generateErr
		}
		defer clear(secret)
		digest := sha256.Sum256(secret)
		deliveryID := s.newID("delivery_")
		value := model.ExecutionAccess{ExecutionID: executionID, AgentID: agent.ID, Generation: 1, CredentialDigest: digest[:], DeliveryID: deliveryID, State: model.ExecutionAccessInactive, IssuedAt: now, ExpiresAt: now.Add(s.accessLease), Revision: 1}
		access = &value
		credential = &ports.ActionCredentialMaterial{ExecutionID: executionID, Generation: 1, DeliveryID: deliveryID, Secret: secret, ExpiresAt: value.ExpiresAt}
	}
	transition := GraphTransition{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, Authority: authority, AdditionalAuthority: additionalAuthority, Operation: &operation, Execution: &execution, WorkspaceUse: workspaceUse, AgentExpected: agent.Revision, Access: access, Updates: []GraphAttemptUpdate{{Ref: attempt.Ref, NewIssuanceID: issuanceID, OperationID: operationID, ExecutionID: executionID, State: model.NodeAttemptAdmitted}}, RunState: model.WorkRunRunning, ControlState: model.WorkControlActive, At: now}
	admitted, err := s.store.ApplyGraphTransition(ctx, transition)
	if err != nil {
		return record, err
	}
	admittedAttempt, ok := graphAttempt(admitted.Run, model.WorkAttemptRef{RunID: attempt.Ref.RunID, NodeID: attempt.Ref.NodeID, ActivationID: attempt.Ref.ActivationID, Attempt: attempt.Ref.Attempt, IssuanceID: issuanceID})
	if !ok {
		return admitted, ErrConflict
	}
	input := &ports.PreparedInitialInput{Body: performer.Brief, Correlation: string(issuanceID), RequiredBeforeFirstWork: true}
	workflowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	prepared, err := provider.Prepare(workflowCtx, ports.PreparationRequest{Spec: spec, Intent: ports.StartFresh, ActionCredential: credential, Observations: s.primaryObservationSink(executionID, 1, provider.Name()), NativeGuidance: s.boundNativeGuidance(execution), AgentAPIEndpoint: s.agentAPIEndpoint, InitialInput: input, CallbackIngress: s.callbackIngress})
	if err != nil {
		return s.failAgentOperation(ctx, admitted, admittedAttempt, "prepare_failed", err)
	}
	description := prepared.Describe()
	if err = validatePrepared(provider.Name(), spec, description); err == nil {
		if description.InitialInput == nil || !description.InitialInput.Supported || description.InitialInput.Correlation != input.Correlation {
			err = fail(ErrUnsupported, "provider did not confirm exact prepared first work")
		}
	}
	if err != nil {
		_ = prepared.Abort(workflowCtx)
		return s.failAgentOperation(ctx, admitted, admittedAttempt, "invalid_preparation", err)
	}
	if credential != nil {
		if description.AccessDelivery == nil || description.AccessDelivery.ExecutionID != executionID || description.AccessDelivery.Generation != credential.Generation || description.AccessDelivery.DeliveryID != credential.DeliveryID {
			_ = prepared.Abort(workflowCtx)
			return s.failAgentOperation(ctx, admitted, admittedAttempt, "invalid_credential_delivery", fail(ErrInvalid, "provider omitted exact credential delivery proof"))
		}
		if _, err = s.store.RecordAccessDelivery(workflowCtx, executionID, credential.Generation, *description.AccessDelivery, s.now().UTC()); err != nil {
			_ = prepared.Abort(workflowCtx)
			return admitted, err
		}
	}
	if _, err = s.store.RecordPrepared(context.WithoutCancel(ctx), executionID, operationID, description.Evidence, s.now().UTC()); err != nil {
		_ = prepared.Abort(workflowCtx)
		return admitted, err
	}
	permit := &releasePermit{store: s.store, executionID: executionID, operationID: operationID, now: s.now}
	released, releaseErr := prepared.Release(workflowCtx, permit)
	if !permit.consumed.Load() && releaseErr == nil {
		releaseErr = fail(ErrInvalid, "provider released without consuming permit")
	}
	evidence := released.Evidence
	if evidence.Provider == "" {
		evidence = description.Evidence
	}
	if releaseErr != nil || released.State == ports.ReleaseUncertain || released.Runtime == nil || released.Runtime.ExecutionID() != executionID {
		detail := "agent release uncertain"
		if releaseErr != nil {
			detail = releaseErr.Error()
		}
		_, persistErr := s.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: operationID, OperationState: model.OperationUncertain, ResultCode: "release_uncertain", Detail: detail, ExecutionID: executionID, ExecutionState: model.ExecutionUnknown, UpdateExecutionState: true, Evidence: evidence, At: s.now().UTC()})
		if persistErr != nil {
			return admitted, persistErr
		}
		return s.settleProgramAttempt(ctx, admitted, admittedAttempt, model.WorkOutcomeUnknown, detail)
	}
	s.rememberRuntime(released.Runtime)
	if _, err = s.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: operationID, OperationState: model.OperationSucceeded, ResultCode: "released_with_prepared_work", ExecutionID: executionID, ExecutionState: model.ExecutionRunning, UpdateExecutionState: true, Evidence: evidence, At: s.now().UTC()}); err != nil {
		return admitted, err
	}
	return s.reconcileAgentAttempt(ctx, admitted, admittedAttempt)
}

func (s *Service) reconcileAgentAttempt(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt) (WorkRunRecord, error) {
	result, err := s.store.OperationResult(ctx, attempt.OperationID)
	if err != nil {
		return record, err
	}
	if result.Operation.Kind == model.OperationInteract && result.Operation.State == model.OperationAdmitted {
		return s.dispatchGraphInteraction(ctx, record, attempt, result.Operation)
	}
	switch result.Operation.State {
	case model.OperationUncertain:
		return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeUnknown, result.Operation.Detail)
	case model.OperationFailed, model.OperationRefused:
		return s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeRejected, result.Operation.Detail)
	case model.OperationSucceeded:
		if attempt.State == model.NodeAttemptAdmitted {
			latest, readErr := s.store.WorkRun(ctx, record.Run.ID)
			if readErr != nil {
				return record, readErr
			}
			current, found := graphAttempt(latest.Run, attempt.Ref)
			if !found {
				return latest, ErrConflict
			}
			deploymentLaunch, deploymentErr := s.isDeploymentLaunch(ctx, latest, current)
			if deploymentErr != nil {
				return latest, deploymentErr
			}
			if deploymentLaunch {
				deployment, deploymentErr := s.store.TeamDeployment(ctx, latest.Run.Scope.DeploymentID)
				if deploymentErr != nil {
					return latest, deploymentErr
				}
				return s.reconcileDeploymentMember(ctx, latest, current, deployment)
			}
			runState, controlState, runOutcome := model.WorkRunRunning, model.WorkControlActive, model.WorkOutcomeNone
			if latest.Run.State == model.WorkRunFailed && latest.Run.ControlState == model.WorkControlDraining {
				runState, controlState, runOutcome = latest.Run.State, latest.Run.ControlState, latest.Run.Outcome
			}
			return s.store.ApplyGraphTransition(ctx, GraphTransition{WorkRunID: latest.Run.ID, ExpectedRevision: latest.Run.Revision, Updates: []GraphAttemptUpdate{{Ref: current.Ref, State: model.NodeAttemptRunning, OperationID: current.OperationID, ExecutionID: current.ExecutionID}}, RunState: runState, ControlState: controlState, RunOutcome: runOutcome, At: s.now().UTC()})
		}
	}
	return record, nil
}

func (s *Service) isDeploymentLaunch(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt) (bool, error) {
	if record.Run.Scope.DeploymentID == "" || attempt.Performer == nil || attempt.Performer.Agent == nil {
		return false, nil
	}
	deployment, err := s.store.TeamDeployment(ctx, record.Run.Scope.DeploymentID)
	if err != nil {
		return false, err
	}
	if deployment.WorkRunID != record.Run.ID || deployment.GroupID != record.Run.Scope.GroupID {
		return false, nil
	}
	for _, id := range deployment.Members {
		if id == attempt.Performer.Agent.AgentID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) failAgentOperation(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, code string, cause error) (WorkRunRecord, error) {
	_, persistErr := s.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: attempt.OperationID, OperationState: model.OperationFailed, ResultCode: code, Detail: cause.Error(), ExecutionID: attempt.ExecutionID, ExecutionState: model.ExecutionFailed, UpdateExecutionState: true, At: s.now().UTC()})
	if persistErr != nil {
		return record, persistErr
	}
	updated, settleErr := s.settleProgramAttempt(ctx, record, attempt, model.WorkOutcomeRejected, cause.Error())
	if settleErr != nil {
		return updated, settleErr
	}
	return updated, cause
}
