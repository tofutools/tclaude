package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Service) RefreshHistory(ctx context.Context, req RefreshHistoryRequest) (HistorySearchResult, error) {
	if strings.TrimSpace(req.Harness) == "" {
		return HistorySearchResult{}, fail(ErrInvalid, "harness is required")
	}
	if s.historySources == nil {
		return HistorySearchResult{}, fail(ErrUnavailable, "history source registry is unavailable")
	}
	scope, ok := s.historySources.HistorySource(req.Harness, req.SourceName)
	if !ok {
		return HistorySearchResult{}, fail(ErrNotFound, "history source %q is not configured", req.SourceName)
	}
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionRefreshHistory, Resource: model.ResourceSelector{Kind: model.ResourceSelf}}, s.now().UTC()); err != nil {
		return HistorySearchResult{}, err
	}
	provider, ok := s.providers.Provider(req.Harness)
	if !ok {
		return HistorySearchResult{}, fail(ErrUnavailable, "harness %q has no provider", req.Harness)
	}
	historyProvider, ok := provider.(ports.HistoryProvider)
	if !ok || historyProvider.History() == nil || !historyProvider.History().Capabilities().MetadataDiscovery {
		return HistorySearchResult{}, fail(ErrUnsupported, "%v", ports.ErrHistoryUnsupported)
	}
	result, err := historyProvider.History().Discover(ctx, ports.HistoryDiscoveryRequest{Scope: scope})
	if err != nil {
		if errors.Is(err, ports.ErrHistoryUnsupported) {
			return HistorySearchResult{}, fail(ErrUnsupported, "%v", err)
		}
		return HistorySearchResult{}, err
	}
	writes := make([]HistoryCatalogWrite, 0, len(result.Histories))
	for _, found := range result.Histories {
		if found.Native.Namespace == "" || found.Native.Reference == "" || found.SourceToken == "" || found.SourceFingerprint == "" {
			return HistorySearchResult{}, fail(ErrInvalid, "provider returned incomplete history source")
		}
		if found.Evidence.Provider != "" && found.Evidence.Provider != provider.Name() {
			return HistorySearchResult{}, fail(ErrInvalid, "provider returned foreign history evidence")
		}
		entry := model.HistoryCatalogEntry{ConversationID: model.ConversationID(s.newID("con_")), Harness: provider.Name(), Title: found.Title, WorkspaceHint: found.WorkspaceHint, Availability: found.Availability, Coverage: found.Coverage, ModifiedAt: found.ModifiedAt, Revision: 1}
		points := make([]HistoryPointWrite, 0, len(found.Points))
		for _, point := range found.Points {
			if point.Token == "" {
				return HistorySearchResult{}, fail(ErrInvalid, "provider returned empty history point")
			}
			points = append(points, HistoryPointWrite{Point: model.HistoryPoint{ID: model.HistoryPointID(s.newID("point_")), ConversationID: entry.ConversationID, Kind: point.Kind, OccurredAt: point.OccurredAt, Revision: 1}, Token: point.Token})
		}
		writes = append(writes, HistoryCatalogWrite{Entry: entry, Native: found.Native, SourceToken: found.SourceToken, SourceFingerprint: found.SourceFingerprint, Evidence: found.Evidence, Points: points})
	}
	entries, err := s.store.CatalogHistory(ctx, provider.Name(), req.SourceName, writes, result.Coverage, s.now().UTC())
	if err != nil {
		return HistorySearchResult{}, err
	}
	return HistorySearchResult{Entries: entries, Coverage: result.Coverage}, nil
}

func (s *Service) SearchHistory(ctx context.Context, req SearchHistoryRequest) (HistorySearchResult, error) {
	result, err := s.store.SearchHistory(ctx, HistorySearchFilter{Harness: req.Harness, WorkspaceID: req.WorkspaceID, Query: req.Query, Archived: req.Archived})
	if err != nil {
		return result, err
	}
	if req.Principal.Kind == model.PrincipalOperator {
		return result, nil
	}
	allowed := result.Entries[:0]
	for _, entry := range result.Entries {
		decision, decisionErr := s.store.Authorize(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadHistory, Resource: model.ResourceSelector{Kind: model.ResourceConversation, ConversationID: entry.ConversationID}}, s.now().UTC())
		if decisionErr == nil && decision.Allowed {
			allowed = append(allowed, entry)
		}
	}
	result.Entries = allowed
	return result, nil
}

func (s *Service) ReadHistory(ctx context.Context, req ReadHistoryRequest) (HistoryReadResult, error) {
	resolved, err := s.store.ResolveHistory(ctx, req.Selection)
	if err != nil {
		return HistoryReadResult{}, err
	}
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadHistory, Resource: model.ResourceSelector{Kind: model.ResourceConversation, ConversationID: req.Selection.ConversationID}}, s.now().UTC()); err != nil {
		return HistoryReadResult{}, err
	}
	provider, ok := s.providers.Provider(resolved.Source.Provider)
	if !ok {
		return HistoryReadResult{}, fail(ErrUnavailable, "history provider unavailable")
	}
	historyProvider, ok := provider.(ports.HistoryProvider)
	if !ok || historyProvider.History() == nil || !historyProvider.History().Capabilities().ContentRead {
		return HistoryReadResult{}, fail(ErrUnsupported, "%v", ports.ErrHistoryUnsupported)
	}
	read, err := historyProvider.History().Read(ctx, resolved.Source)
	if err != nil {
		if errors.Is(err, ports.ErrHistoryUnsupported) {
			return HistoryReadResult{}, fail(ErrUnsupported, "%v", err)
		}
		return HistoryReadResult{}, err
	}
	var textParts []string
	turns := make([]HistoryTurn, 0, len(read.Turns))
	for _, turn := range read.Turns {
		view := HistoryTurn{Role: turn.Role, Parts: append([]ports.HistoryPart(nil), turn.Parts...)}
		if resolved.Point != nil && resolved.Source.Point != nil && turn.Point.Token == resolved.Source.Point.Token {
			view.PointID = resolved.Point.ID
		}
		for _, part := range turn.Parts {
			if part.Kind == ports.HistoryPartText && !part.Omitted {
				textParts = append(textParts, part.Text)
			}
		}
		turns = append(turns, view)
	}
	updatedEntry, err := s.store.IndexHistoryRead(ctx, resolved.Entry.ConversationID, strings.Join(textParts, "\n"), read.Coverage, s.now().UTC())
	if err != nil {
		return HistoryReadResult{}, err
	}
	points, err := s.store.HistoryPoints(ctx, resolved.Entry.ConversationID)
	if err != nil {
		return HistoryReadResult{}, err
	}
	return HistoryReadResult{Entry: updatedEntry, Point: resolved.Point, Points: points, Turns: turns, Coverage: read.Coverage}, nil
}

func (s *Service) SetConversationMetadata(ctx context.Context, req SetConversationMetadataRequest) (HistorySearchResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return HistorySearchResult{}, err
	}
	if req.ExpectedRevision == 0 {
		return HistorySearchResult{}, fail(ErrInvalid, "expected revision is required")
	}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionSetHistoryMetadata, Resource: model.ResourceSelector{Kind: model.ResourceConversation, ConversationID: req.ConversationID}}
	entry, err := s.store.SetHistoryMetadata(ctx, req.ConversationID, req.ExpectedRevision, req.Title, req.Archived, req.Context.RequestID, authority, s.now().UTC())
	if err != nil {
		return HistorySearchResult{}, err
	}
	return HistorySearchResult{Entries: []model.HistoryCatalogEntry{entry}, Coverage: entry.Coverage}, nil
}

func (s *Service) RegisterWorkspace(ctx context.Context, req RegisterWorkspaceRequest) (WorkspaceResult, error) {
	req.Intent.Provenance = model.WorkspaceRegistered
	if req.Intent.Ownership == "" {
		req.Intent.Ownership = model.WorkspaceExternal
	}
	return s.createWorkspace(ctx, req.Context, req.ID, req.Intent, model.ActionRegisterWorkspace)
}

func (s *Service) CreateCheckout(ctx context.Context, req CreateCheckoutRequest) (WorkspaceResult, error) {
	req.Intent.Provenance, req.Intent.Ownership = model.WorkspacePlatformCreated, model.WorkspaceOwned
	return s.createWorkspace(ctx, req.Context, req.ID, req.Intent, model.ActionCreateWorkspace)
}

func (s *Service) createWorkspace(ctx context.Context, request RequestContext, id model.WorkspaceID, intent model.WorkspaceIntent, action model.Action) (WorkspaceResult, error) {
	if err := validateEffectContext(request); err != nil {
		return WorkspaceResult{}, err
	}
	if err := id.Validate(); err != nil {
		return WorkspaceResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(intent.IntendedPath) == "" {
		return WorkspaceResult{}, fail(ErrInvalid, "intended path is required")
	}
	if s.workspaceHost == nil {
		return WorkspaceResult{}, fail(ErrUnavailable, "workspace host is unavailable")
	}
	now := s.now().UTC()
	opKind := model.OperationCreateWorkspace
	workspace := model.Workspace{ID: id, Intent: intent, State: model.WorkspacePending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	op := model.Operation{ID: model.OperationID(s.newID("op_")), RequestID: request.RequestID, Kind: opKind, Principal: request.Principal, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	authority := model.AuthorityRequest{Principal: request.Principal, Action: action, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: id}}
	admitted, err := s.store.AdmitWorkspaceEffect(ctx, WorkspaceEffectAdmission{Operation: op, Workspace: workspace, Authority: authority})
	if err != nil {
		return WorkspaceResult{}, err
	}
	if admitted.Repeated {
		return workspaceResult(admitted.Workspace), nil
	}
	workflowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	permit := &resourceEffectPermit{store: s.store, operationID: op.ID, now: s.now}
	effect, effectErr := s.workspaceHost.CreateCheckout(workflowCtx, ports.CheckoutCreateRequest{WorkspaceID: id, Intent: intent}, permit)
	state := model.WorkspacePending
	switch effect.Disposition {
	case ports.EffectAccepted:
		state = model.WorkspaceAvailable
	case ports.EffectUnknown:
		state = model.WorkspaceUncertain
	}
	if effect.Disposition == ports.EffectAccepted && (effect.Resource.Owner == "" || effect.Resource.Version == 0 || len(effect.Resource.Payload) == 0) {
		effectErr = fail(ErrInvalid, "host omitted workspace ownership evidence")
		effect.Disposition = ports.EffectUnknown
		state = model.WorkspaceUncertain
	}
	detail := ""
	if effectErr != nil {
		detail = effectErr.Error()
	}
	settleCtx, settleCancel := settlementContext(ctx)
	defer settleCancel()
	stored, settleErr := s.store.CompleteWorkspaceEffect(settleCtx, WorkspaceEffectCompletion{OperationID: op.ID, WorkspaceID: id, State: state, Observation: effect.Observation, Resource: effect.Resource, Disposition: effect.Disposition, Detail: detail, At: s.now().UTC()})
	if settleErr != nil {
		return WorkspaceResult{}, settleErr
	}
	if effectErr != nil {
		return workspaceResult(stored), effectErr
	}
	if effect.Disposition == ports.EffectUnknown {
		return workspaceResult(stored), fail(ErrUncertain, "workspace effect is uncertain")
	}
	if effect.Disposition != ports.EffectAccepted {
		return workspaceResult(stored), fail(ErrUnavailable, "workspace creation refused")
	}
	return workspaceResult(stored), nil
}

func (s *Service) InspectWorkspace(ctx context.Context, req InspectWorkspaceRequest) (WorkspaceResult, error) {
	workspace, err := s.store.Workspace(ctx, req.WorkspaceID)
	if err != nil {
		return WorkspaceResult{}, err
	}
	if err = s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: req.WorkspaceID}}, s.now().UTC()); err != nil {
		return WorkspaceResult{}, err
	}
	if s.workspaceHost == nil {
		return WorkspaceResult{}, fail(ErrUnavailable, "workspace host is unavailable")
	}
	observed, err := s.workspaceHost.InspectWorkspace(ctx, workspace)
	if err != nil {
		return WorkspaceResult{}, err
	}
	state := model.WorkspaceAvailable
	if observed.Disposition == ports.EffectUnknown {
		state = model.WorkspaceUncertain
	} else if observed.Disposition != ports.EffectAccepted {
		return WorkspaceResult{}, fail(ErrUnavailable, "workspace inspection refused")
	}
	resource := observed.Resource
	if resource.Owner == "" {
		resource = workspace.Resource
	}
	updated, err := s.store.UpdateWorkspaceObservation(ctx, workspace.ID, workspace.Revision, state, observed.Observation, resource, s.now().UTC())
	if err != nil {
		return WorkspaceResult{}, err
	}
	return workspaceResult(updated), nil
}

func (s *Service) RemoveCheckout(ctx context.Context, req RemoveCheckoutRequest) (WorkspaceResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkspaceResult{}, err
	}
	workspace, err := s.store.Workspace(ctx, req.WorkspaceID)
	if err != nil {
		return WorkspaceResult{}, err
	}
	if req.ExpectedRevision == 0 || workspace.Revision != req.ExpectedRevision {
		return WorkspaceResult{}, ErrConflict
	}
	if workspace.Intent.Ownership != model.WorkspaceOwned || workspace.Resource.Owner == "" || workspace.Resource.Version == 0 || len(workspace.Resource.Payload) == 0 {
		return WorkspaceResult{}, fail(ErrUnauthorized, "workspace has no platform ownership receipt")
	}
	uses, err := s.store.ActiveWorkspaceUses(ctx, workspace.ID)
	if err != nil {
		return WorkspaceResult{}, err
	}
	if len(uses) > 0 {
		return WorkspaceResult{}, fail(ErrConflict, "workspace has active use claims")
	}
	if s.workspaceHost == nil {
		return WorkspaceResult{}, fail(ErrUnavailable, "workspace host is unavailable")
	}
	now := s.now().UTC()
	op := model.Operation{ID: model.OperationID(s.newID("op_")), RequestID: req.Context.RequestID, Kind: model.OperationRemoveWorkspace, Principal: req.Context.Principal, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionRemoveWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}}
	admitted, err := s.store.AdmitWorkspaceEffect(ctx, WorkspaceEffectAdmission{Operation: op, Workspace: workspace, Authority: authority})
	if err != nil {
		return WorkspaceResult{}, err
	}
	if admitted.Repeated {
		return workspaceResult(admitted.Workspace), nil
	}
	workflowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	effect, effectErr := s.workspaceHost.RemoveCheckout(workflowCtx, ports.CheckoutRemoveRequest{WorkspaceID: workspace.ID, Observation: workspace.Observation, Resource: workspace.Resource, Destructive: req.Destructive}, &resourceEffectPermit{store: s.store, operationID: op.ID, now: s.now})
	state := model.WorkspaceAvailable
	switch effect.Disposition {
	case ports.EffectAccepted:
		state = model.WorkspaceRemoved
	case ports.EffectUnknown:
		state = model.WorkspaceUncertain
	}
	detail := ""
	if effectErr != nil {
		detail = effectErr.Error()
	}
	settleCtx, settleCancel := settlementContext(ctx)
	defer settleCancel()
	observation := effect.Observation
	if observation.ActualPath == "" {
		observation = workspace.Observation
	}
	stored, settleErr := s.store.CompleteWorkspaceEffect(settleCtx, WorkspaceEffectCompletion{OperationID: op.ID, WorkspaceID: workspace.ID, State: state, Observation: observation, Resource: workspace.Resource, Disposition: effect.Disposition, Detail: detail, At: s.now().UTC()})
	if settleErr != nil {
		return WorkspaceResult{}, settleErr
	}
	if effectErr != nil {
		return workspaceResult(stored), effectErr
	}
	if effect.Disposition == ports.EffectUnknown {
		return workspaceResult(stored), fail(ErrUncertain, "workspace removal is uncertain")
	}
	return workspaceResult(stored), nil
}

func (s *Service) RestoreCheckout(ctx context.Context, req RestoreCheckoutRequest) (WorkspaceResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkspaceResult{}, err
	}
	if s.workspaceHost == nil {
		return WorkspaceResult{}, fail(ErrUnavailable, "workspace host is unavailable")
	}
	workspace, err := s.store.Workspace(ctx, req.WorkspaceID)
	if err != nil {
		return WorkspaceResult{}, err
	}
	if req.ExpectedRevision == 0 || workspace.Revision != req.ExpectedRevision || workspace.State != model.WorkspaceRemoved || workspace.Intent.Provenance != model.WorkspacePlatformCreated || workspace.Intent.Ownership != model.WorkspaceOwned {
		return WorkspaceResult{}, fail(ErrConflict, "only the exact removed owned checkout can be restored")
	}
	now := s.now().UTC()
	op := model.Operation{ID: model.OperationID(s.newID("op_")), RequestID: req.Context.RequestID, Kind: model.OperationRestoreWorkspace, Principal: req.Context.Principal, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionRestoreWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}}
	admitted, err := s.store.AdmitWorkspaceEffect(ctx, WorkspaceEffectAdmission{Operation: op, Workspace: workspace, Authority: authority})
	if err != nil {
		return WorkspaceResult{}, err
	}
	if admitted.Repeated {
		return workspaceResult(admitted.Workspace), nil
	}
	workflowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	effect, effectErr := s.workspaceHost.RestoreCheckout(workflowCtx, ports.CheckoutRestoreRequest{WorkspaceID: workspace.ID, Intent: workspace.Intent, Observation: workspace.Observation, Resource: workspace.Resource}, &resourceEffectPermit{store: s.store, operationID: op.ID, now: s.now})
	state := model.WorkspaceRemoved
	switch effect.Disposition {
	case ports.EffectAccepted:
		state = model.WorkspaceAvailable
	case ports.EffectUnknown:
		state = model.WorkspaceUncertain
	}
	if effect.Disposition == ports.EffectAccepted && (effect.Resource.Owner == "" || effect.Resource.Version == 0 || len(effect.Resource.Payload) == 0) {
		effectErr = fail(ErrInvalid, "host omitted workspace ownership evidence")
		effect.Disposition = ports.EffectUnknown
		state = model.WorkspaceUncertain
	}
	detail := ""
	if effectErr != nil {
		detail = effectErr.Error()
	}
	stored, settleErr := s.store.CompleteWorkspaceEffect(context.WithoutCancel(ctx), WorkspaceEffectCompletion{OperationID: op.ID, WorkspaceID: workspace.ID, State: state, Observation: effect.Observation, Resource: effect.Resource, Disposition: effect.Disposition, Detail: detail, At: s.now().UTC()})
	if settleErr != nil {
		return WorkspaceResult{}, settleErr
	}
	if effectErr != nil {
		return workspaceResult(stored), effectErr
	}
	if effect.Disposition == ports.EffectUnknown {
		return workspaceResult(stored), fail(ErrUncertain, "workspace restore is uncertain")
	}
	if effect.Disposition != ports.EffectAccepted {
		return workspaceResult(stored), fail(ErrUnavailable, "workspace restore refused")
	}
	return workspaceResult(stored), nil
}

func (s *Service) StartShell(ctx context.Context, req StartShellRequest) (OperationResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return OperationResult{}, err
	}
	if s.shellHost == nil {
		return OperationResult{}, fail(ErrUnavailable, "shell host is unavailable")
	}
	if req.Sandbox != model.SandboxUnconfined && req.Sandbox != model.SandboxReadOnly && req.Sandbox != model.SandboxWorkspaceWrite {
		return OperationResult{}, fail(ErrInvalid, "shell sandbox is required")
	}
	workspace, err := s.store.Workspace(ctx, req.WorkspaceID)
	if err != nil {
		return OperationResult{}, err
	}
	if req.ExpectedRevision == 0 || workspace.Revision != req.ExpectedRevision || workspace.State != model.WorkspaceAvailable || workspace.Observation.ActualPath == "" {
		return OperationResult{}, fail(ErrConflict, "workspace revision or availability changed")
	}
	now := s.now().UTC()
	executionID := model.ExecutionID(s.newID("exec_"))
	operationID := model.OperationID(s.newID("op_"))
	execution := model.Execution{ID: executionID, Workload: model.ExecutionWorkloadShell, Spec: model.ResolvedExecutionSpec{ExecutionID: executionID, Workload: model.ExecutionWorkloadShell, Attempt: 1, WorkingDirectory: workspace.Observation.ActualPath, Sandbox: req.Sandbox}, State: model.ExecutionReserved, Attempt: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	operation := model.Operation{ID: operationID, RequestID: req.Context.RequestID, Kind: model.OperationStartShell, Principal: req.Context.Principal, ExecutionID: executionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionStartShell, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}}
	use := model.WorkspaceUse{ID: model.WorkspaceUseID(s.newID("use_")), WorkspaceID: workspace.ID, ExecutionID: executionID, CreatedAt: now}
	admitted, err := s.store.AdmitShell(ctx, ShellAdmission{Operation: operation, Execution: execution, WorkspaceUse: use, WorkspaceRevision: req.ExpectedRevision, Authority: authority})
	if err != nil {
		return OperationResult{}, err
	}
	if admitted.Repeated {
		return operationResult(admitted), nil
	}
	workflowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	prepared, err := s.shellHost.PrepareShell(workflowCtx, ports.ShellPreparationRequest{ExecutionID: executionID, Attempt: 1, WorkspaceID: workspace.ID, WorkingDirectory: workspace.Observation.ActualPath, Sandbox: req.Sandbox})
	if err != nil {
		settled, persistErr := s.store.CompleteShell(context.WithoutCancel(ctx), OperationCompletion{OperationID: operationID, OperationState: model.OperationFailed, ResultCode: "prepare_failed", Detail: err.Error(), ExecutionID: executionID, ExecutionState: model.ExecutionFailed, UpdateExecutionState: true, At: s.now().UTC()}, ports.ShellResourceEvidence{})
		if persistErr != nil {
			return OperationResult{}, persistErr
		}
		return operationResult(settled), err
	}
	description := prepared.Describe()
	if description.ExecutionID != executionID || description.Attempt != 1 || description.Evidence.Owner == "" || description.Evidence.Version == 0 || len(description.Evidence.Payload) == 0 {
		_ = prepared.Abort(workflowCtx)
		err = fail(ErrInvalid, "shell host returned invalid preparation evidence")
		settled, persistErr := s.store.CompleteShell(context.WithoutCancel(ctx), OperationCompletion{OperationID: operationID, OperationState: model.OperationFailed, ResultCode: "invalid_preparation", Detail: err.Error(), ExecutionID: executionID, ExecutionState: model.ExecutionFailed, UpdateExecutionState: true, At: s.now().UTC()}, description.Evidence)
		if persistErr != nil {
			return OperationResult{}, persistErr
		}
		return operationResult(settled), err
	}
	if _, err = s.store.RecordShellPrepared(workflowCtx, executionID, operationID, description.Evidence, s.now().UTC()); err != nil {
		_ = prepared.Abort(workflowCtx)
		return OperationResult{}, err
	}
	released, releaseErr := prepared.Release(workflowCtx, &releasePermit{store: s.store, executionID: executionID, operationID: operationID, now: s.now})
	if released.State == ports.ReleaseStarted && released.Runtime != nil {
		s.runtimeMu.Lock()
		s.hostRuntimes[executionID] = released.Runtime
		s.runtimeMu.Unlock()
		settled, persistErr := s.store.CompleteShell(context.WithoutCancel(ctx), OperationCompletion{OperationID: operationID, OperationState: model.OperationSucceeded, ResultCode: "released", ExecutionID: executionID, ExecutionState: model.ExecutionRunning, UpdateExecutionState: true, At: s.now().UTC()}, released.Evidence)
		if persistErr != nil {
			return OperationResult{}, persistErr
		}
		return operationResult(settled), releaseErr
	}
	detail := "shell release was uncertain"
	if releaseErr != nil {
		detail = releaseErr.Error()
	}
	settled, persistErr := s.store.CompleteShell(context.WithoutCancel(ctx), OperationCompletion{OperationID: operationID, OperationState: model.OperationUncertain, ResultCode: "release_uncertain", Detail: detail, ExecutionID: executionID, ExecutionState: model.ExecutionUnknown, UpdateExecutionState: true, At: s.now().UTC()}, released.Evidence)
	if persistErr != nil {
		return OperationResult{}, persistErr
	}
	return operationResult(settled), fail(ErrUncertain, "%s", detail)
}

func (s *Service) StartWork(ctx context.Context, req StartWorkRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	if err := req.ID.Validate(); err != nil {
		return WorkRunResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.Spec.Brief) == "" {
		return WorkRunResult{}, fail(ErrInvalid, "work brief is required")
	}
	if req.Spec.WorkspaceID == "" || req.Spec.WorkerAgentID == "" {
		return WorkRunResult{}, fail(ErrInvalid, "workspace and worker are required")
	}
	if err := validateOutcomePolicy(req.Spec.Outcome); err != nil {
		return WorkRunResult{}, err
	}
	if existing, lookupErr := s.store.WorkRunByRequest(ctx, req.Context.Principal, req.Context.RequestID); lookupErr == nil {
		if existing.Run.ID != req.ID || !reflect.DeepEqual(existing.Run.Spec, req.Spec) || !reflect.DeepEqual(existing.Run.Requester, req.Context.Principal) {
			return WorkRunResult{}, ErrConflict
		}
		return WorkRunResult(existing), nil
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return WorkRunResult{}, lookupErr
	}
	workspace, err := s.store.Workspace(ctx, req.Spec.WorkspaceID)
	if err != nil {
		return WorkRunResult{}, err
	}
	if req.Spec.WorkspaceRevision == 0 || workspace.Revision != req.Spec.WorkspaceRevision {
		return WorkRunResult{}, fail(ErrConflict, "workspace revision changed")
	}
	if workspace.State != model.WorkspaceAvailable {
		return WorkRunResult{}, fail(ErrConflict, "workspace is not available")
	}
	agent, err := s.store.Agent(ctx, req.Spec.WorkerAgentID)
	if err != nil {
		return WorkRunResult{}, err
	}
	if req.Spec.WorkerAgentRevision == 0 || agent.Revision != req.Spec.WorkerAgentRevision {
		return WorkRunResult{}, fail(ErrConflict, "worker agent revision changed")
	}
	if !reflect.DeepEqual(agent.Desired, req.Spec.WorkerDesired) {
		return WorkRunResult{}, fail(ErrConflict, "worker desired configuration changed")
	}
	if agent.Desired.WorkingDirectory != workspace.Observation.ActualPath {
		return WorkRunResult{}, fail(ErrConflict, "worker configuration does not select the exact workspace")
	}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionStartWork, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agent.ID}, RequestedConfiguration: &req.Spec.WorkerDesired}
	if err = s.requireAuthority(ctx, authority, s.now().UTC()); err != nil {
		return WorkRunResult{}, err
	}
	now := s.now().UTC()
	authoritySubject := req.Context.Principal.Authority
	if req.Context.Principal.Kind == model.PrincipalOperator {
		authoritySubject = model.AuthoritySubject{Kind: model.AuthorityOperator}
	}
	var delegation *model.AutomationDelegation
	if req.Context.Principal.Delegation != nil {
		copied := *req.Context.Principal.Delegation
		delegation = &copied
	}
	run := model.WorkRun{ID: req.ID, RequestID: req.Context.RequestID, Requester: req.Context.Principal, Authority: authoritySubject, Delegation: delegation, Spec: req.Spec, WorkspaceUseID: model.WorkspaceUseID(s.newID("use_")), State: model.WorkRunRunning, Revision: 1, CreatedAt: now, UpdatedAt: now}
	settled := now
	run.Attempts = []model.WorkStepAttempt{{Step: model.WorkStepPrepareWorkspace, Attempt: 1, State: model.WorkAttemptSucceeded, StartedAt: now, SettledAt: &settled}, {Step: model.WorkStepPrepareHistory, Attempt: 1, State: model.WorkAttemptSucceeded, StartedAt: now, SettledAt: &settled}, {Step: model.WorkStepLaunchWorker, Attempt: 1, OperationID: model.OperationID(s.newID("op_")), State: model.WorkAttemptPending, StartedAt: now}, {Step: model.WorkStepDeliverRequest, Attempt: 1, OperationID: model.OperationID(s.newID("op_")), State: model.WorkAttemptPending, StartedAt: now}, {Step: model.WorkStepAwaitEvidence, Attempt: 1, State: model.WorkAttemptPending, StartedAt: now}, {Step: model.WorkStepEvaluate, Attempt: 1, State: model.WorkAttemptPending, StartedAt: now}}
	var claim *model.HistoryUseClaim
	switch req.Spec.SourceMode {
	case model.WorkSourceFork:
		resolved, resolveErr := s.store.ResolveHistory(ctx, req.Spec.History)
		if resolveErr != nil {
			return WorkRunResult{}, resolveErr
		}
		provider, ok := s.providers.Provider(resolved.Source.Provider)
		if !ok {
			return WorkRunResult{}, fail(ErrUnavailable, "history provider unavailable")
		}
		hp, ok := provider.(ports.HistoryProvider)
		if !ok || hp.History() == nil {
			return WorkRunResult{}, fail(ErrUnsupported, "fork history unsupported")
		}
		precision := hp.History().Capabilities().ForkPrecision
		if !supportsSelectionPrecision(precision, resolved.Point) {
			return WorkRunResult{}, fail(ErrUnsupported, "selected fork precision is unsupported")
		}
		if hp.History().Capabilities().ForkRequiresExclusive {
			claim = &model.HistoryUseClaim{ID: model.HistoryUseID(s.newID("history_use_")), ConversationID: resolved.Entry.ConversationID, OperationID: run.Attempts[2].OperationID, WorkRunID: run.ID, SourceRevision: resolved.Source.SourceRevision, SourceFingerprint: resolved.Source.SourceFingerprint, State: model.HistoryUseHeld, Revision: 1, CreatedAt: now}
			if resolved.Point != nil {
				claim.PointID = resolved.Point.ID
			}
			run.HistoryUseID = claim.ID
		}
	case model.WorkSourceFreshHandoff:
		if strings.TrimSpace(req.Spec.FreshHandoff) == "" {
			return WorkRunResult{}, fail(ErrInvalid, "fresh handoff is required")
		}
	default:
		return WorkRunResult{}, fail(ErrInvalid, "work source mode is required")
	}
	created, _, err := s.store.CreateWorkRun(ctx, run, claim)
	if err != nil {
		return WorkRunResult{}, err
	}
	record, err := s.store.WorkRun(ctx, created.ID)
	if err != nil {
		return WorkRunResult{}, err
	}
	return WorkRunResult(record), nil
}

func (s *Service) InspectWork(ctx context.Context, req InspectWorkRequest) (WorkRunResult, error) {
	record, err := s.store.WorkRun(ctx, req.WorkRunID)
	if err != nil {
		return WorkRunResult{}, err
	}
	if err = s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: req.WorkRunID}}, s.now().UTC()); err != nil {
		return WorkRunResult{}, err
	}
	return WorkRunResult(record), nil
}

func (s *Service) RecordWorkEvidence(ctx context.Context, req RecordWorkEvidenceRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	evidence := model.WorkEvidence{ID: model.WorkEvidenceID(s.newID("evidence_")), RequestID: req.Context.RequestID, WorkRunID: req.WorkRunID, Step: req.Step, Attempt: req.Attempt, Kind: req.Kind, Reporter: req.Context.Principal, ArtifactRevision: req.ArtifactRevision, Passed: req.Passed, Detail: req.Detail, RecordedAt: s.now().UTC(), Revision: 1}
	if err := evidence.ID.Validate(); err != nil {
		return WorkRunResult{}, fail(ErrInvalid, "%v", err)
	}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionRecordWorkEvidence, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: evidence.WorkRunID}}
	record, err := s.store.RecordWorkEvidence(ctx, evidence, req.ExpectedRunRevision, authority, evidence.RecordedAt)
	if err != nil {
		return WorkRunResult{}, err
	}
	return WorkRunResult(record), nil
}

func (s *Service) DecideWork(ctx context.Context, req DecideWorkRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	record, err := s.store.WorkRun(ctx, req.WorkRunID)
	if err != nil {
		return WorkRunResult{}, err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return WorkRunResult{}, fail(ErrInvalid, "decision reason is required")
	}
	if req.Decision == model.WorkDecisionAccept && record.Run.Spec.Outcome.Mode == model.WorkOutcomeVerification {
		matched := false
		for _, e := range record.Evidence {
			if e.Step == req.Step && e.Attempt == req.Attempt && e.Kind == model.WorkEvidenceVerification && e.Passed != nil && *e.Passed && e.ArtifactRevision != "" {
				matched = true
				break
			}
		}
		if !matched {
			return WorkRunResult{}, fail(ErrConflict, "matching successful verification evidence is required")
		}
	}
	decision := model.WorkDecision{WorkRunID: req.WorkRunID, RequestID: req.Context.RequestID, Step: req.Step, Attempt: req.Attempt, Decision: req.Decision, Decider: req.Context.Principal, Reason: req.Reason, DecidedAt: s.now().UTC(), Revision: 1}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionDecideWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: req.WorkRunID}}
	updated, err := s.store.DecideWork(ctx, decision, req.ExpectedRunRevision, authority, decision.DecidedAt)
	if err != nil {
		return WorkRunResult{}, err
	}
	return WorkRunResult(updated), nil
}

func (s *Service) CancelWork(ctx context.Context, req CancelWorkRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionCancelWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: req.WorkRunID}}
	record, err := s.store.CancelWork(ctx, req.WorkRunID, req.ExpectedRunRevision, req.Reason, req.Context.RequestID, authority, s.now().UTC())
	if err != nil {
		return WorkRunResult{}, err
	}
	return WorkRunResult(record), nil
}

func (s *Service) ResolveWorkUncertainty(ctx context.Context, req ResolveWorkUncertaintyRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	if err := requireOperator(req.Context.Principal); err != nil {
		return WorkRunResult{}, err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return WorkRunResult{}, fail(ErrInvalid, "uncertainty resolution reason is required")
	}
	record, err := s.store.WorkRun(ctx, req.WorkRunID)
	if err != nil {
		return WorkRunResult{}, err
	}
	if record.Run.State != model.WorkRunUncertain {
		return WorkRunResult{}, fail(ErrConflict, "work run is not uncertain")
	}
	var uncertain model.WorkStepAttempt
	found := false
	for _, attempt := range record.Run.Attempts {
		if attempt.State == model.WorkAttemptUncertain {
			uncertain = attempt
			found = true
			break
		}
	}
	if !found {
		return WorkRunResult{}, fail(ErrConflict, "work run has no uncertain attempt")
	}
	decision := model.WorkDecision{WorkRunID: req.WorkRunID, RequestID: req.Context.RequestID, Step: uncertain.Step, Attempt: uncertain.Attempt, Decision: model.WorkDecisionReject, Decider: req.Context.Principal, Reason: "confirmed no external effect: " + req.Reason, DecidedAt: s.now().UTC(), Revision: 1}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionResolveWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: req.WorkRunID}}
	updated, err := s.store.ResolveWorkUncertainty(ctx, decision, req.ExpectedRunRevision, authority, decision.DecidedAt)
	if err != nil {
		return WorkRunResult{}, err
	}
	return WorkRunResult(updated), nil
}

// ReconcilePendingWork is the bounded server-owned worker sweep. Each native
// effect has its own durable OperationID before dispatch. A restart first reads
// that operation and settles the Work attempt; uncertain operations are never
// replayed.
func (s *Service) ReconcilePendingWork(ctx context.Context) (WorkReconcileReport, error) {
	if err := s.reconcileProgramResourceCleanup(ctx); err != nil {
		return WorkReconcileReport{}, err
	}
	if err := s.collectAutomationFacts(ctx); err != nil {
		return WorkReconcileReport{}, err
	}
	if err := s.reconcileTeamDeployments(ctx); err != nil {
		return WorkReconcileReport{}, err
	}
	occurrences, err := s.reconcileAutomation(ctx)
	if err != nil {
		return WorkReconcileReport{}, err
	}
	records, err := s.store.PendingWorkRuns(ctx)
	if err != nil {
		return WorkReconcileReport{}, err
	}
	report := WorkReconcileReport{Occurrences: occurrences}
	var firstAdvanceErr error
	for _, record := range records {
		if record.Run.State == model.WorkRunUncertain {
			report.Uncertain = append(report.Uncertain, record.Run.ID)
			continue
		}
		var updated WorkRunRecord
		var advanceErr error
		if record.Run.Graph != nil {
			updated, advanceErr = s.advanceGraphWork(ctx, record)
		} else {
			updated, advanceErr = s.advanceWork(ctx, record)
		}
		if advanceErr != nil && updated.Run.State != model.WorkRunFailed && updated.Run.State != model.WorkRunUncertain {
			// A temporarily unavailable provider is local to this run. Keep the
			// honest pending state, but finish the bounded sweep so an unrelated
			// run cannot be starved behind it. Return the first error after all
			// independent records have had their turn.
			if !errors.Is(advanceErr, ErrUnavailable) {
				return report, advanceErr
			}
			if firstAdvanceErr == nil {
				firstAdvanceErr = advanceErr
			}
		}
		switch updated.Run.State {
		case model.WorkRunUncertain:
			report.Uncertain = append(report.Uncertain, updated.Run.ID)
		case model.WorkRunRunning, model.WorkRunPending:
			report.Pending = append(report.Pending, updated.Run.ID)
		}
	}
	if err := s.reconcileTeamDeployments(ctx); err != nil {
		return report, err
	}
	return report, firstAdvanceErr
}

func (s *Service) advanceWork(ctx context.Context, record WorkRunRecord) (WorkRunRecord, error) {
	launchAttempt, ok := workAttempt(record.Run, model.WorkStepLaunchWorker)
	if !ok {
		return record, fail(ErrInvalid, "work run has no launch attempt")
	}
	if launchAttempt.State == model.WorkAttemptPending || launchAttempt.State == model.WorkAttemptRunning {
		result, err := s.existingOrLaunchWork(ctx, record, launchAttempt)
		attemptState, runState := workStates(result.Operation.State)
		detail := result.Operation.Detail
		if err != nil && detail == "" {
			detail = err.Error()
		}
		workerExecution := model.ExecutionID("")
		if result.Execution != nil {
			workerExecution = result.Execution.ID
		}
		var progressErr error
		record, progressErr = s.store.RecordWorkProgress(ctx, WorkProgress{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, Step: model.WorkStepLaunchWorker, Attempt: launchAttempt.Attempt, OperationID: launchAttempt.OperationID, AttemptState: attemptState, RunState: runState, WorkerExecutionID: workerExecution, Detail: detail, At: s.now().UTC()})
		if progressErr != nil {
			return record, progressErr
		}
		if err != nil || attemptState != model.WorkAttemptSucceeded {
			return record, err
		}
	}
	deliverAttempt, ok := workAttempt(record.Run, model.WorkStepDeliverRequest)
	if !ok {
		return record, fail(ErrInvalid, "work run has no delivery attempt")
	}
	if deliverAttempt.State != model.WorkAttemptPending && deliverAttempt.State != model.WorkAttemptRunning {
		return record, nil
	}
	result, err := s.existingOrDeliverWork(ctx, record, deliverAttempt)
	attemptState, runState := workStates(result.Operation.State)
	if attemptState == model.WorkAttemptSucceeded {
		runState = model.WorkRunWaiting
	}
	detail := result.Operation.Detail
	if err != nil && detail == "" {
		detail = err.Error()
	}
	updated, progressErr := s.store.RecordWorkProgress(ctx, WorkProgress{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, Step: model.WorkStepDeliverRequest, Attempt: deliverAttempt.Attempt, OperationID: deliverAttempt.OperationID, AttemptState: attemptState, RunState: runState, WorkerExecutionID: record.Run.WorkerExecutionID, Detail: detail, At: s.now().UTC()})
	if progressErr != nil {
		return updated, progressErr
	}
	return updated, err
}

func (s *Service) existingOrLaunchWork(ctx context.Context, record WorkRunRecord, attempt model.WorkStepAttempt) (OperationResult, error) {
	if existing, err := s.store.OperationResult(ctx, attempt.OperationID); err == nil {
		return operationResult(existing), nil
	} else if !errors.Is(err, ErrNotFound) {
		return OperationResult{}, err
	}
	var source *ports.HistorySourceSelection
	intent := ports.StartFresh
	if record.Run.Spec.SourceMode == model.WorkSourceFork {
		resolved, err := s.store.ResolveHistory(ctx, record.Run.Spec.History)
		if err != nil {
			return OperationResult{}, err
		}
		source = &resolved.Source
		intent = ports.StartFork
		if record.Run.HistoryUseID != "" {
			claim, claimErr := s.store.HistoryUse(ctx, record.Run.HistoryUseID)
			if claimErr != nil {
				return OperationResult{}, claimErr
			}
			source.UseClaim = &claim
		}
	}
	request := LaunchRequest{RequestContext: RequestContext{Principal: record.Run.Requester, RequestID: model.RequestID(attempt.OperationID)}, Target: LaunchTarget{Agent: &AgentLaunchTarget{AgentID: record.Run.Spec.WorkerAgentID, ExpectedRevision: record.Run.Spec.WorkerAgentRevision}}}
	return s.launch(ctx, request, model.OperationLaunch, nil, launchOptions{intent: intent, history: source, operationID: attempt.OperationID})
}

func (s *Service) existingOrDeliverWork(ctx context.Context, record WorkRunRecord, attempt model.WorkStepAttempt) (OperationResult, error) {
	if existing, err := s.store.OperationResult(ctx, attempt.OperationID); err == nil {
		return operationResult(existing), nil
	} else if !errors.Is(err, ErrNotFound) {
		return OperationResult{}, err
	}
	text := record.Run.Spec.Brief
	if record.Run.Spec.SourceMode == model.WorkSourceFreshHandoff {
		text = record.Run.Spec.FreshHandoff + "\n\n" + text
	}
	request := RequestContext{Principal: record.Run.Requester, RequestID: model.RequestID(attempt.OperationID)}
	return s.withRuntimeEffectID(ctx, request, record.Run.WorkerExecutionID, model.OperationInteract, attempt.OperationID, func(effectCtx context.Context, runtime ports.Runtime) (ports.EffectDisposition, model.ProviderEvidence, string, error) {
		result, err := runtime.Interact(effectCtx, ports.Interaction{Text: text})
		return result.Disposition, result.Evidence, "work_delivered", err
	})
}

func workAttempt(run model.WorkRun, step model.WorkStep) (model.WorkStepAttempt, bool) {
	for _, attempt := range run.Attempts {
		if attempt.Step == step {
			return attempt, true
		}
	}
	return model.WorkStepAttempt{}, false
}
func workStates(state model.OperationState) (model.WorkAttemptState, model.WorkRunState) {
	switch state {
	case model.OperationSucceeded:
		return model.WorkAttemptSucceeded, model.WorkRunRunning
	case model.OperationUncertain:
		return model.WorkAttemptUncertain, model.WorkRunUncertain
	case model.OperationAdmitted, model.OperationRunning:
		return model.WorkAttemptRunning, model.WorkRunRunning
	default:
		return model.WorkAttemptFailed, model.WorkRunFailed
	}
}

type resourceEffectPermit struct {
	store       Store
	operationID model.OperationID
	now         func() time.Time
}

func (p *resourceEffectPermit) OperationID() model.OperationID { return p.operationID }
func (p *resourceEffectPermit) Consume(ctx context.Context) error {
	return p.store.ConsumeExecutionEffect(ctx, p.operationID, p.now().UTC())
}

func workspaceResult(workspace model.Workspace) WorkspaceResult {
	return WorkspaceResult{Workspace: WorkspaceView{ID: workspace.ID, Intent: workspace.Intent, State: workspace.State, Observation: workspace.Observation, Revision: workspace.Revision}}
}
func validateOutcomePolicy(policy model.WorkOutcomePolicy) error {
	switch policy.Mode {
	case model.WorkOutcomeHumanDecision:
		return nil
	case model.WorkOutcomeVerification:
		if policy.Verification == nil || len(policy.Verification.Command) == 0 || strings.TrimSpace(policy.Verification.ArtifactRef) == "" {
			return fail(ErrInvalid, "verification command and artifact are required")
		}
		return fail(ErrUnsupported, "automatic verification requires an authenticated controlled verifier")
	default:
		return fail(ErrInvalid, "outcome policy is required")
	}
}
func supportsSelectionPrecision(precision ports.HistoryPrecision, point *model.HistoryPoint) bool {
	if point == nil {
		return precision == ports.HistoryPrecisionHead || precision == ports.HistoryPrecisionMessage || precision == ports.HistoryPrecisionTurn
	}
	switch point.Kind {
	case model.HistoryPointMessage:
		return precision == ports.HistoryPrecisionMessage
	case model.HistoryPointBeforeMessage:
		return precision == ports.HistoryPrecisionBeforeMessage
	case model.HistoryPointTurn:
		return precision == ports.HistoryPrecisionTurn
	case model.HistoryPointHead:
		return precision != ports.HistoryPrecisionNone
	default:
		return false
	}
}
