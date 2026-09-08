package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	cronv3 "github.com/robfig/cron/v3"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const (
	orchestrationCompilerVersion = "1"
	maxGraphNodes                = 512
	maxWorkAttempts              = 100
	maxProgramArguments          = 256
	maxProgramEnvironment        = 128
	maxProgramValueBytes         = 4096
	maxProgramInputBytes         = 1 << 20
	minimumScheduleCadence       = 30 * time.Second
)

func (s *Service) ValidateDefinition(ctx context.Context, req ValidateDefinitionRequest) (DefinitionResult, error) {
	if err := req.Draft.ID.Validate(); err != nil {
		return DefinitionResult{}, fail(ErrInvalid, "%v", err)
	}
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadDefinition, Resource: model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: req.Draft.ID}}, s.now().UTC()); err != nil {
			return DefinitionResult{}, err
		}
	}
	revision, err := s.compileDefinition(ctx, req.Draft, req.Principal)
	if err != nil {
		return DefinitionResult{}, err
	}
	return DefinitionResult{Definition: model.Definition{ID: req.Draft.ID, Name: strings.TrimSpace(req.Draft.Name), Kind: req.Draft.Kind}, Revision: revision}, nil
}

func (s *Service) SaveDefinition(ctx context.Context, req SaveDefinitionRequest) (DefinitionResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return DefinitionResult{}, err
	}
	resource := model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: req.Draft.ID}
	if req.Context.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionManageDefinition, Resource: resource}, s.now().UTC()); err != nil {
			return DefinitionResult{}, err
		}
	}
	revision, err := s.compileDefinition(ctx, req.Draft, req.Context.Principal)
	if err != nil {
		return DefinitionResult{}, err
	}
	now := s.now().UTC()
	revision.CreatedAt = now
	revision.RequestID = req.Context.RequestID
	definition := model.Definition{ID: req.Draft.ID, Name: strings.TrimSpace(req.Draft.Name), Kind: req.Draft.Kind, HeadRevisionID: revision.ID, Revision: req.ExpectedRevision + 1, CreatedAt: now, UpdatedAt: now}
	record, err := s.store.SaveDefinition(ctx, definition, revision, req.ExpectedRevision)
	return DefinitionResult{Definition: record.Definition, Revision: record.Head}, err
}

func (s *Service) GetDefinition(ctx context.Context, req GetDefinitionRequest) (DefinitionResult, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadDefinition, Resource: model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: req.DefinitionID}}, s.now().UTC()); err != nil {
			return DefinitionResult{}, err
		}
	}
	record, err := s.store.Definition(ctx, req.DefinitionID)
	if err != nil {
		return DefinitionResult{}, err
	}
	if req.RevisionID != "" {
		revision, readErr := s.store.DefinitionRevision(ctx, req.RevisionID)
		if readErr != nil {
			return DefinitionResult{}, readErr
		}
		if revision.DefinitionID != req.DefinitionID {
			return DefinitionResult{}, ErrNotFound
		}
		record.Head = revision
	}
	return DefinitionResult{Definition: record.Definition, Revision: record.Head}, err
}

func (s *Service) ListDefinitions(ctx context.Context, req ListDefinitionsRequest) ([]model.Definition, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadDefinition, Resource: model.ResourceSelector{Kind: model.ResourceDefinition}}, s.now().UTC()); err != nil {
			return nil, err
		}
	}
	return s.store.ListDefinitions(ctx, req.Kind, req.IncludeTombstoned)
}

func (s *Service) compileDefinition(ctx context.Context, draft DefinitionDraft, author model.Principal) (model.DefinitionRevision, error) {
	if err := draft.ID.Validate(); err != nil {
		return model.DefinitionRevision{}, fail(ErrInvalid, "%v", err)
	}
	if draft.RevisionID == "" {
		draft.RevisionID = model.DefinitionRevisionID(s.newID("defrev_"))
	}
	if err := draft.RevisionID.Validate(); err != nil {
		return model.DefinitionRevision{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(draft.Name) == "" || strings.TrimSpace(draft.Source) == "" {
		return model.DefinitionRevision{}, fail(ErrInvalid, "definition name and source are required")
	}
	if draft.SchemaVersion == 0 {
		return model.DefinitionRevision{}, fail(ErrInvalid, "definition schema version is required")
	}
	if err := validateParameters(draft.Parameters); err != nil {
		return model.DefinitionRevision{}, err
	}
	switch draft.Kind {
	case model.DefinitionProcess:
		if draft.Process == nil || draft.Team != nil {
			return model.DefinitionRevision{}, fail(ErrInvalid, "process definition requires exactly one process spec")
		}
		compiled, err := compileTaskStages(draft.Process.Graph)
		if err != nil {
			return model.DefinitionRevision{}, err
		}
		if err := validateProcessParameterSyntax(draft.Process.ParameterSyntax, compiled, draft.Parameters); err != nil {
			return model.DefinitionRevision{}, err
		}
	case model.DefinitionTeam:
		if draft.Team == nil || draft.Process != nil {
			return model.DefinitionRevision{}, fail(ErrInvalid, "team definition requires exactly one team spec")
		}
		if err := validateTeam(*draft.Team); err != nil {
			return model.DefinitionRevision{}, err
		}
		for _, ref := range draft.Team.Automation {
			ruleRevision, err := s.store.AutomationRuleRevision(ctx, ref.RevisionID)
			if err != nil {
				return model.DefinitionRevision{}, err
			}
			if ruleRevision.RuleID != ref.RuleID || ruleRevision.ContentHash != ref.ContentHash {
				return model.DefinitionRevision{}, fail(ErrConflict, "automation rule %s does not match its pinned revision", ref.RuleID)
			}
		}
	default:
		return model.DefinitionRevision{}, fail(ErrInvalid, "definition kind is required")
	}
	if err := validateEditorLayout(draft); err != nil {
		return model.DefinitionRevision{}, err
	}
	closure, err := s.definitionClosure(ctx, draft.ID, draft.Dependencies)
	if err != nil {
		return model.DefinitionRevision{}, err
	}
	hashInput := struct {
		Name          string
		Kind          model.DefinitionKind
		SchemaVersion uint32
		Source        string
		Parameters    []model.ParameterDeclaration
		Team          *model.TeamDefinition
		Process       *model.ProcessDefinition
		Dependencies  []model.DefinitionRef
		EditorLayout  *model.DefinitionEditorLayout `json:",omitempty"`
	}{strings.TrimSpace(draft.Name), draft.Kind, draft.SchemaVersion, draft.Source, draft.Parameters, draft.Team, draft.Process, closure, draft.EditorLayout}
	return model.DefinitionRevision{ID: draft.RevisionID, DefinitionID: draft.ID, ContentHash: contentHash(hashInput), SchemaVersion: draft.SchemaVersion, CompilerVersion: orchestrationCompilerVersion, Source: draft.Source, EditorLayout: draft.EditorLayout, Parameters: append([]model.ParameterDeclaration(nil), draft.Parameters...), Team: draft.Team, Process: draft.Process, Dependencies: closure, Author: author}, nil
}

func (s *Service) definitionClosure(ctx context.Context, owner model.DefinitionID, direct []model.DefinitionRef) ([]model.DefinitionRef, error) {
	seen := make(map[model.DefinitionRevisionID]bool)
	var closure []model.DefinitionRef
	var add func(model.DefinitionRef) error
	add = func(ref model.DefinitionRef) error {
		if ref.DefinitionID == owner {
			return fail(ErrInvalid, "definition dependency cycle through %s", owner)
		}
		if seen[ref.RevisionID] {
			return nil
		}
		revision, err := s.store.DefinitionRevision(ctx, ref.RevisionID)
		if err != nil {
			return err
		}
		if revision.DefinitionID != ref.DefinitionID || revision.ContentHash != ref.ContentHash || definitionKind(revision) != ref.Kind {
			return fail(ErrConflict, "definition dependency %s does not match its pinned revision", ref.DefinitionID)
		}
		for _, dependency := range revision.Dependencies {
			if err := add(dependency); err != nil {
				return err
			}
		}
		seen[ref.RevisionID] = true
		closure = append(closure, ref)
		return nil
	}
	for _, ref := range direct {
		if err := add(ref); err != nil {
			return nil, err
		}
	}
	return closure, nil
}

func definitionKind(revision model.DefinitionRevision) model.DefinitionKind {
	if revision.Team != nil {
		return model.DefinitionTeam
	}
	return model.DefinitionProcess
}

func (s *Service) SaveProgramProfile(ctx context.Context, req SaveProgramProfileRequest) (ProgramProfileResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return ProgramProfileResult{}, err
	}
	if err := req.ID.Validate(); err != nil {
		return ProgramProfileResult{}, fail(ErrInvalid, "%v", err)
	}
	if req.RevisionID == "" {
		req.RevisionID = model.ProgramProfileRevisionID(s.newID("profile_rev_"))
	}
	if err := req.RevisionID.Validate(); err != nil {
		return ProgramProfileResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Executable) == "" {
		return ProgramProfileResult{}, fail(ErrInvalid, "program profile name and executable are required")
	}
	if req.Timeout <= 0 || req.OutputLimitBytes <= 0 {
		return ProgramProfileResult{}, fail(ErrInvalid, "program timeout and output limit must be bounded")
	}
	if len(req.ArgumentPrefix) > maxProgramArguments || len(req.Environment) > maxProgramEnvironment {
		return ProgramProfileResult{}, fail(ErrInvalid, "program argv or environment exceeds bounded limits")
	}
	for _, argument := range req.ArgumentPrefix {
		if len(argument) > maxProgramValueBytes || strings.ContainsRune(argument, '\x00') {
			return ProgramProfileResult{}, fail(ErrInvalid, "program argument is invalid or too large")
		}
	}
	for key, value := range req.Environment {
		if strings.TrimSpace(key) == "" || len(key) > maxProgramValueBytes || len(value) > maxProgramValueBytes || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			return ProgramProfileResult{}, fail(ErrInvalid, "program environment is invalid or too large")
		}
	}
	if req.Sandbox == "" || len(req.EffectAuthority) == 0 {
		return ProgramProfileResult{}, fail(ErrInvalid, "program sandbox and effect authority are required")
	}
	executeRequirements := 0
	for _, requirement := range req.EffectAuthority {
		if requirement.Action == model.ActionExecuteProgram && requirement.Resource.Kind == model.ResourceWorkspace {
			executeRequirements++
		}
	}
	if executeRequirements != 1 {
		return ProgramProfileResult{}, fail(ErrInvalid, "program profile requires exactly one workspace execute authority")
	}
	if req.Context.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionManageProgramProfile, Resource: model.ResourceSelector{Kind: model.ResourceProgramProfile, ProgramProfileID: req.ID}}, s.now().UTC()); err != nil {
			return ProgramProfileResult{}, err
		}
	}
	now := s.now().UTC()
	hashInput := struct {
		Name             string
		Executable       string
		ArgumentPrefix   []string
		Environment      map[string]string
		WorkingDirectory string
		Sandbox          model.SandboxMode
		Timeout          time.Duration
		OutputLimitBytes int64
		EffectAuthority  []model.ProgramEffectRequirement
	}{strings.TrimSpace(req.Name), req.Executable, req.ArgumentPrefix, req.Environment, req.WorkingDirectory, req.Sandbox, req.Timeout, req.OutputLimitBytes, req.EffectAuthority}
	revision := model.ProgramProfileRevision{ID: req.RevisionID, ProfileID: req.ID, ContentHash: contentHash(hashInput), Executable: req.Executable, ArgumentPrefix: append([]string(nil), req.ArgumentPrefix...), Environment: cloneStringMap(req.Environment), WorkingDirectory: req.WorkingDirectory, Sandbox: req.Sandbox, Timeout: req.Timeout, OutputLimitBytes: req.OutputLimitBytes, EffectAuthority: append([]model.ProgramEffectRequirement(nil), req.EffectAuthority...), Author: req.Context.Principal, RequestID: req.Context.RequestID, CreatedAt: now}
	profile := model.ProgramProfile{ID: req.ID, Name: strings.TrimSpace(req.Name), HeadRevisionID: req.RevisionID, Revision: req.ExpectedRevision + 1, CreatedAt: now, UpdatedAt: now}
	record, err := s.store.SaveProgramProfile(ctx, profile, revision, req.ExpectedRevision)
	return ProgramProfileResult{Profile: record.Profile, Revision: record.Head}, err
}

func (s *Service) GetProgramProfile(ctx context.Context, req GetProgramProfileRequest) (ProgramProfileResult, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadProgramProfile, Resource: model.ResourceSelector{Kind: model.ResourceProgramProfile, ProgramProfileID: req.ID}}, s.now().UTC()); err != nil {
			return ProgramProfileResult{}, err
		}
	}
	record, err := s.store.ProgramProfile(ctx, req.ID)
	return ProgramProfileResult{Profile: record.Profile, Revision: record.Head}, err
}

func (s *Service) ListProgramProfiles(ctx context.Context, req ListProgramProfilesRequest) ([]model.ProgramProfile, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadProgramProfile, Resource: model.ResourceSelector{Kind: model.ResourceProgramProfile}}, s.now().UTC()); err != nil {
			return nil, err
		}
	}
	return s.store.ListProgramProfiles(ctx, req.IncludeTombstoned)
}

func (s *Service) StartProcess(ctx context.Context, req StartProcessRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	if err := req.ID.Validate(); err != nil {
		return WorkRunResult{}, fail(ErrInvalid, "%v", err)
	}
	if req.Start.RequestID != "" && req.Start.RequestID != req.Context.RequestID {
		return WorkRunResult{}, fail(ErrInvalid, "work start request identity does not match context")
	}
	if (req.Start.Definition == nil) == (req.Start.InlineGraph == nil) {
		return WorkRunResult{}, fail(ErrInvalid, "exactly one pinned definition or inline graph is required")
	}
	var graph model.WorkGraph
	var parameterSyntax string
	var declarations []model.ParameterDeclaration
	var closure []model.DefinitionRef
	parameters := cloneRawMap(req.Start.Parameters)
	if ref := req.Start.Definition; ref != nil {
		revision, err := s.store.DefinitionRevision(ctx, ref.RevisionID)
		if err != nil {
			return WorkRunResult{}, err
		}
		if revision.DefinitionID != ref.DefinitionID || revision.ContentHash != ref.ContentHash || ref.Kind != model.DefinitionProcess || revision.Process == nil {
			return WorkRunResult{}, fail(ErrConflict, "process definition reference is not the pinned revision")
		}
		graph = revision.Process.Graph
		parameterSyntax, declarations = revision.Process.ParameterSyntax, revision.Parameters
		closure = append(append([]model.DefinitionRef(nil), revision.Dependencies...), *ref)
		parameters = materializeParameterValues(revision.Parameters, parameters)
		if err := validateParameterValues(revision.Parameters, parameters); err != nil {
			return WorkRunResult{}, err
		}
	} else {
		if len(parameters) != 0 {
			return WorkRunResult{}, fail(ErrInvalid, "inline work graphs do not declare typed parameters")
		}
		graph = *req.Start.InlineGraph
	}
	for _, node := range graph.Nodes {
		if len(node.Captures) != 0 {
			return WorkRunResult{}, fail(ErrUnsupported, "task %s declares output captures; capture execution is not available", node.ID)
		}
	}
	graph, err := compileTaskStages(graph)
	if err != nil {
		return WorkRunResult{}, err
	}
	for _, node := range graph.Nodes {
		if node.Performer != nil && strings.TrimSpace(node.Performer.Timeout) != "" && node.Performer.Kind != model.PerformerProgram {
			return WorkRunResult{}, fail(ErrUnsupported, "task %s declares a performer timeout; only program timeout execution is available", node.ID)
		}
		if node.Performer != nil {
			duration, _ := performerTimeout(*node.Performer)
			if duration > time.Hour {
				return WorkRunResult{}, fail(ErrUnsupported, "program timeout execution is bounded to one hour")
			}
		}
		if node.Performer != nil && node.Performer.Contact != nil {
			return WorkRunResult{}, fail(ErrUnsupported, "task %s declares a contact schedule; scheduled performer contact is not available", node.ID)
		}
	}
	graph, err = materializePerformerBindings(graph, req.Start.PerformerBindings)
	if err != nil {
		return WorkRunResult{}, err
	}
	if err := validateProcessParameterSyntax(parameterSyntax, graph, declarations); err != nil {
		return WorkRunResult{}, err
	}
	if parameterSyntax != "" {
		graph, err = expandProcessParameters(graph, declarations, parameters)
		if err != nil {
			return WorkRunResult{}, err
		}
	}
	if err := validateWorkGraph(graph); err != nil {
		return WorkRunResult{}, err
	}
	for _, node := range graph.Nodes {
		if node.Kind == model.WorkNodeWait && (node.Wait.Until != "" || node.Wait.Signal != "") {
			return WorkRunResult{}, fail(ErrUnsupported, "wait %s declares an absolute time or signal; only duration waits are executable", node.ID)
		}
	}
	if req.Start.Deadline.IsZero() || !req.Start.Deadline.After(s.now().UTC()) {
		return WorkRunResult{}, fail(ErrInvalid, "a future work deadline is required")
	}
	if err := s.validateProgramBindings(ctx, graph, req.Start.AuthorizedProgramProfiles, req.Context.Principal, req.Start.Scope); err != nil {
		return WorkRunResult{}, err
	}
	authority := req.Context.Principal.Authority
	switch req.Context.Principal.Kind {
	case model.PrincipalOperator:
		authority = model.AuthoritySubject{Kind: model.AuthorityOperator}
	case model.PrincipalAgent:
		authority = model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: req.Context.Principal.AgentID}
	}
	if req.Context.Principal.Kind != model.PrincipalOperator {
		resource := model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: req.ID}
		if req.Context.Principal.Kind == model.PrincipalAutomation && req.Start.Scope.RuleID != "" {
			resource = model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: req.Start.Scope.RuleID}
		}
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionStartWork, Resource: resource}, s.now().UTC()); err != nil {
			return WorkRunResult{}, err
		}
	}
	if err := s.pinProgramActivationTimeouts(ctx, &graph); err != nil {
		return WorkRunResult{}, err
	}
	now := s.now().UTC()
	entry := graphNode(graph, graph.EntryNodeID)
	run := model.WorkRun{ID: req.ID, RequestID: req.Context.RequestID, Requester: req.Context.Principal, Authority: authority, Delegation: req.Context.Principal.Delegation, Graph: &graph, DefinitionClosure: closure, Parameters: parameters, Scope: req.Start.Scope, AuthorizedPrograms: append([]model.ProgramProfileRef(nil), req.Start.AuthorizedProgramProfiles...), ControlState: model.WorkControlActive, State: model.WorkRunRunning, Deadline: req.Start.Deadline.UTC(), Revision: 1, CreatedAt: now, UpdatedAt: now}
	attempt, windows := s.initialActivation(req.ID, req.Start.Scope, entry, now, run.Deadline, graph.ProgramActivationTimeouts[entry.ID])
	run.NodeAttempts = []model.WorkNodeAttempt{attempt}
	record, _, err := s.store.CreateGraphWorkRun(ctx, run, windows)
	if err == nil && (entry.Kind == model.WorkNodeFork || entry.Kind == model.WorkNodeJoin || entry.Kind == model.WorkNodeEnd) {
		transition := s.graphOutcomeTransition(record, attempt, model.WorkOutcomeVerified, "entry transition")
		record, err = s.store.ApplyGraphTransition(ctx, transition)
	}
	return WorkRunResult(record), err
}

func (s *Service) RecordNodeEvidence(ctx context.Context, req RecordNodeEvidenceRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	if req.ExpectedRunRevision == 0 || req.Attempt.RunID == "" || req.Attempt.NodeID == "" || req.Attempt.ActivationID == "" || req.Attempt.Attempt == 0 || req.Attempt.IssuanceID == "" {
		return WorkRunResult{}, fail(ErrInvalid, "exact issued attempt and expected run revision are required")
	}
	record, err := s.store.WorkRun(ctx, req.Attempt.RunID)
	if err != nil {
		return WorkRunResult{}, err
	}
	if record.Run.Revision != req.ExpectedRunRevision || record.Run.Graph == nil {
		return WorkRunResult{}, ErrConflict
	}
	attempt, ok := graphAttempt(record.Run, req.Attempt)
	if !ok || attempt.State == model.NodeAttemptSucceeded || attempt.State == model.NodeAttemptFailed || attempt.State == model.NodeAttemptWaived || attempt.State == model.NodeAttemptSuppressed {
		return WorkRunResult{}, ErrConflict
	}
	outcome := req.Disposition
	if outcome == model.WorkOutcomeNone {
		if req.Passed != nil && !*req.Passed {
			outcome = model.WorkOutcomeRejected
		} else {
			outcome = model.WorkOutcomeVerified
		}
	}
	node := graphNode(*record.Run.Graph, req.Attempt.NodeID)
	if outcome == model.WorkOutcomeWaived && !node.Waivable {
		return WorkRunResult{}, fail(ErrUnauthorized, "node %s does not permit waiver", node.ID)
	}
	switch outcome {
	case model.WorkOutcomeVerified, model.WorkOutcomeWaived, model.WorkOutcomeRejected, model.WorkOutcomeUnknown:
	default:
		return WorkRunResult{}, fail(ErrInvalid, "evidence disposition is unsupported")
	}
	evidence := model.WorkNodeEvidence{ID: model.WorkEvidenceID(s.newID("evidence_")), RequestID: req.Context.RequestID, Attempt: req.Attempt, Reporter: req.Context.Principal, Kind: req.Kind, ArtifactRevision: req.ArtifactRevision, Passed: req.Passed, Disposition: outcome, Detail: req.Detail, RecordedAt: s.now().UTC(), Revision: 1}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionRecordWorkEvidence, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: req.Attempt.RunID}}
	transition := s.graphOutcomeTransitionForVerdict(record, attempt, outcome, req.Detail, "")
	s.enforceOutcomePolicy(record, &transition, &evidence, false)
	transition.Authority, transition.Evidence = authority, &evidence
	updated, err := s.store.ApplyGraphTransition(ctx, transition)
	return WorkRunResult(updated), err
}

func (s *Service) graphOutcomeTransition(record WorkRunRecord, current model.WorkNodeAttempt, outcome model.WorkOutcome, detail string) GraphTransition {
	transition := s.graphOutcomeTransitionForVerdict(record, current, outcome, detail, "")
	s.enforceOutcomePolicy(record, &transition, nil, false)
	return transition
}

func (s *Service) graphOutcomeTransitionForVerdict(record WorkRunRecord, current model.WorkNodeAttempt, outcome model.WorkOutcome, detail, verdict string) GraphTransition {
	now := s.now().UTC()
	transition := GraphTransition{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, RunState: model.WorkRunRunning, ControlState: model.WorkControlActive, At: now}
	state := model.NodeAttemptSucceeded
	switch outcome {
	case model.WorkOutcomeWaived:
		state = model.NodeAttemptWaived
	case model.WorkOutcomeRejected:
		state = model.NodeAttemptFailed
	case model.WorkOutcomeUnknown:
		state = model.NodeAttemptUncertain
	}
	transition.Updates = append(transition.Updates, GraphAttemptUpdate{Ref: current.Ref, State: state, Outcome: outcome, Detail: detail})
	if record.Run.State == model.WorkRunFailed && record.Run.ControlState == model.WorkControlDraining {
		transition.RunState, transition.RunOutcome = model.WorkRunFailed, record.Run.Outcome
		transition.ControlState = model.WorkControlSettled
		if outcome == model.WorkOutcomeUnknown {
			transition.ControlState = model.WorkControlDraining
		}
		for _, attempt := range record.Run.NodeAttempts {
			if attempt.Ref != current.Ref && (attempt.State == model.NodeAttemptAdmitted || attempt.State == model.NodeAttemptRunning || attempt.State == model.NodeAttemptUncertain) {
				transition.ControlState = model.WorkControlDraining
			}
		}
		return transition
	}
	if record.Run.CancellationRequested {
		for _, attempt := range record.Run.NodeAttempts {
			if attempt.Ref == current.Ref {
				continue
			}
			if attempt.State == model.NodeAttemptReady || attempt.State == model.NodeAttemptRetryWait || attempt.State == model.NodeAttemptBlocked || attempt.State == model.NodeAttemptWaiting {
				transition.Updates = append(transition.Updates, GraphAttemptUpdate{Ref: attempt.Ref, State: model.NodeAttemptSuppressed, Outcome: model.WorkOutcomeCancelled, Detail: "suppressed by cancellation"})
			}
		}
		if outcome == model.WorkOutcomeUnknown {
			transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunUncertain, model.WorkControlDraining, model.WorkOutcomeUnknown
		} else {
			transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunCancelled, model.WorkControlSettled, model.WorkOutcomeCancelled
			for _, attempt := range record.Run.NodeAttempts {
				if attempt.Ref != current.Ref && (attempt.State == model.NodeAttemptAdmitted || attempt.State == model.NodeAttemptRunning || attempt.State == model.NodeAttemptUncertain) {
					transition.ControlState = model.WorkControlDraining
				}
			}
		}
		return transition
	}
	if outcome == model.WorkOutcomeUnknown {
		transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunUncertain, model.WorkControlDraining, model.WorkOutcomeUnknown
		return transition
	}
	if outcome == model.WorkOutcomeRejected {
		if retried, ok := s.taskGateFailure(record, current, detail, transition); ok {
			return retried
		}
		if retried, ok := s.retryFailureTransition(record, current, detail, transition); ok {
			return retried
		}
	}
	if outcome == model.WorkOutcomeRejected {
		transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunFailed, model.WorkControlSettled, model.WorkOutcomeRejected
		for _, attempt := range record.Run.NodeAttempts {
			if attempt.Ref == current.Ref {
				continue
			}
			if attempt.State == model.NodeAttemptReady || attempt.State == model.NodeAttemptRetryWait || attempt.State == model.NodeAttemptBlocked || attempt.State == model.NodeAttemptWaiting {
				transition.Updates = append(transition.Updates, GraphAttemptUpdate{Ref: attempt.Ref, State: model.NodeAttemptSuppressed, Outcome: model.WorkOutcomeCancelled, Detail: "suppressed after branch failure"})
			}
			if attempt.State == model.NodeAttemptAdmitted || attempt.State == model.NodeAttemptRunning || attempt.State == model.NodeAttemptUncertain {
				transition.ControlState = model.WorkControlDraining
			}
		}
		return transition
	}
	virtual := append([]model.WorkNodeAttempt(nil), record.Run.NodeAttempts...)
	for i := range virtual {
		if virtual[i].Ref == current.Ref {
			virtual[i].State, virtual[i].Outcome = state, outcome
		}
	}
	graph := *record.Run.Graph
	var activate func(model.WorkNodeID, model.WorkActivationID)
	activate = func(nodeID model.WorkNodeID, winner model.WorkActivationID) {
		node := graphNode(graph, nodeID)
		if node.ID == "" {
			return
		}
		if node.Kind == model.WorkNodeJoin {
			for _, attempt := range append(virtual, transition.Activations...) {
				if attempt.Ref.NodeID == nodeID {
					return
				}
			}
			if node.Join.Mode == model.JoinAll {
				for _, incoming := range incomingNodes(graph, nodeID) {
					if !nodeConcludedSuccessfully(append(virtual, transition.Activations...), incoming) {
						return
					}
				}
			}
		}
		attempt, windows := s.initialActivation(record.Run.ID, record.Run.Scope, node, now, record.Run.Deadline, graph.ProgramActivationTimeouts[node.ID])
		inheritTaskActivation(graph, current, &attempt, windows)
		taskStageFeedback(record, current, &attempt, detail)
		for i := range windows {
			if attempt.Performer != nil && attempt.Performer.Human != nil {
				windows[i].Question = attempt.Performer.Human.Prompt
			}
		}
		if node.Kind == model.WorkNodeFork || node.Kind == model.WorkNodeJoin || node.Kind == model.WorkNodeEnd || node.Kind == model.WorkNodeTaskComplete {
			attempt.State, attempt.Outcome = model.NodeAttemptSucceeded, model.WorkOutcomeVerified
			if node.Kind == model.WorkNodeTaskComplete && taskCompletionWaived(graph, virtual, current) {
				attempt.State, attempt.Outcome = model.NodeAttemptWaived, model.WorkOutcomeWaived
			}
			if node.Kind == model.WorkNodeEnd && node.End != nil {
				attempt.Outcome = node.End.Outcome
				if node.End.Outcome == model.WorkOutcomeRejected || node.End.Outcome == model.WorkOutcomeCancelled || node.End.Outcome == model.WorkOutcomeExpired {
					attempt.State = model.NodeAttemptFailed
				}
			}
			attempt.JoinWinner = winner
			settled := now
			attempt.SettledAt = &settled
		}
		transition.Activations = append(transition.Activations, attempt)
		transition.DecisionWindows = append(transition.DecisionWindows, windows...)
		if node.Kind == model.WorkNodeFork || node.Kind == model.WorkNodeJoin || node.Kind == model.WorkNodeTaskComplete {
			for _, next := range outgoingNodes(graph, nodeID) {
				activate(next, attempt.Ref.ActivationID)
			}
		}
	}
	for _, next := range outgoingNodesForVerdict(graph, current.Ref.NodeID, verdict) {
		activate(next, current.Ref.ActivationID)
	}
	combined := append(virtual, transition.Activations...)
	hasEnd, hasActive, hasUncertain := false, false, false
	for _, attempt := range combined {
		node := graphNode(graph, attempt.Ref.NodeID)
		if node.Kind == model.WorkNodeEnd && attempt.State == model.NodeAttemptSucceeded {
			hasEnd = true
		}
		switch attempt.State {
		case model.NodeAttemptReady, model.NodeAttemptAdmitted, model.NodeAttemptRunning, model.NodeAttemptWaiting, model.NodeAttemptRetryWait, model.NodeAttemptBlocked:
			hasActive = true
		case model.NodeAttemptUncertain:
			hasUncertain = true
		}
	}
	hasFailure := hasUnsupersededGraphFailure(graph, combined)
	if hasFailure {
		transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunFailed, model.WorkControlSettled, model.WorkOutcomeRejected
		for _, attempt := range combined {
			if attempt.State == model.NodeAttemptAdmitted || attempt.State == model.NodeAttemptRunning || attempt.State == model.NodeAttemptUncertain {
				transition.ControlState = model.WorkControlDraining
			}
		}
	} else if hasUncertain {
		transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunUncertain, model.WorkControlDraining, model.WorkOutcomeUnknown
	} else if hasEnd && !hasActive {
		transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunSucceeded, model.WorkControlSettled, model.WorkOutcomeVerified
	} else if hasEnd {
		transition.ControlState = model.WorkControlDraining
	} else if onlyWaiting(combined) {
		transition.RunState, transition.ControlState = model.WorkRunWaiting, model.WorkControlWaiting
	}
	return transition
}

func hasUnsupersededGraphFailure(graph model.WorkGraph, attempts []model.WorkNodeAttempt) bool {
	for _, failed := range attempts {
		if failed.State != model.NodeAttemptFailed {
			continue
		}
		superseded := false
		for _, candidate := range attempts {
			failedGroup, grouped := taskGroup(graph, failed.Ref.NodeID)
			candidateGroup, candidateGrouped := taskGroup(graph, candidate.Ref.NodeID)
			sameScope := candidate.Ref.NodeID == failed.Ref.NodeID || grouped && candidateGrouped && failedGroup.ID == candidateGroup.ID
			if sameScope && candidate.Ref.ActivationID == failed.Ref.ActivationID && candidate.Ref.Attempt > failed.Ref.Attempt {
				superseded = true
				break
			}
		}
		if !superseded {
			return true
		}
	}
	return false
}

func (s *Service) enforceOutcomePolicy(record WorkRunRecord, transition *GraphTransition, prospective *model.WorkNodeEvidence, humanJudgment bool) {
	if transition.RunState != model.WorkRunSucceeded || record.Run.Graph == nil {
		return
	}
	attempts := append([]model.WorkNodeAttempt(nil), record.Run.NodeAttempts...)
	for _, update := range transition.Updates {
		for i := range attempts {
			if attempts[i].Ref == update.Ref {
				attempts[i].State, attempts[i].Outcome = update.State, update.Outcome
			}
		}
	}
	attempts = append(attempts, transition.Activations...)
	policy := record.Run.Graph.Outcome
	accepted := true
	for _, required := range policy.RequiredNodes {
		accepted = accepted && nodeConcludedVerified(attempts, required)
	}
	if policy.ArtifactRevision != "" {
		artifactAccepted := false
		for _, evidence := range record.NodeEvidence {
			artifactAccepted = artifactAccepted || acceptedArtifactEvidence(evidence, policy.ArtifactRevision)
		}
		if prospective != nil {
			artifactAccepted = artifactAccepted || acceptedArtifactEvidence(*prospective, policy.ArtifactRevision)
		}
		accepted = accepted && artifactAccepted
	}
	if policy.HumanJudgment {
		for _, attempt := range attempts {
			node := graphNode(*record.Run.Graph, attempt.Ref.NodeID)
			isJudgment := node.Kind == model.WorkNodeDecision || node.Performer != nil && node.Performer.Kind == model.PerformerHuman
			humanJudgment = humanJudgment || isJudgment && graphAttemptTerminal(attempt.State)
		}
		accepted = accepted && humanJudgment
	}
	if accepted {
		return
	}
	transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunFailed, model.WorkControlSettled, model.WorkOutcomeRejected
	for i := range transition.Activations {
		node := graphNode(*record.Run.Graph, transition.Activations[i].Ref.NodeID)
		if node.Kind == model.WorkNodeEnd && transition.Activations[i].State == model.NodeAttemptSucceeded {
			transition.Activations[i].State = model.NodeAttemptFailed
			transition.Activations[i].Outcome = model.WorkOutcomeRejected
			transition.Activations[i].Detail = "pinned outcome policy was not satisfied"
		}
	}
}

func (s *Service) retryFailureTransition(record WorkRunRecord, current model.WorkNodeAttempt, detail string, transition GraphTransition) (GraphTransition, bool) {
	node := graphNode(*record.Run.Graph, current.Ref.NodeID)
	class := retryFailureClass(node)
	if node.Retry.MaxAttempts == 0 || !containsString(node.Retry.Retryable, class) {
		return GraphTransition{}, false
	}
	used := current.Ref.Attempt
	if _, grouped := taskGroup(*record.Run.Graph, current.Ref.NodeID); grouped {
		_, used = taskStageAttempts(record.Run, current.Ref.ActivationID, current.Ref.NodeID)
	}
	if used < current.RetryBudget {
		next, windows := s.retryActivation(record.Run, current, node, transition.At, current.RetryBudget)
		if _, grouped := taskGroup(*record.Run.Graph, current.Ref.NodeID); grouped {
			next.Performer = node.Performer
			taskStageFeedback(record, current, &next, detail)
			for i := range windows {
				if next.Performer.Human != nil {
					windows[i].Question = next.Performer.Human.Prompt
				}
			}
		}
		transition.Activations = append(transition.Activations, next)
		transition.DecisionWindows = append(transition.DecisionWindows, windows...)
		return transition, true
	}
	decisionID := model.DecisionID(s.newID("decision_"))
	answers := []string{string(model.BlockedRetry), string(model.BlockedRework), string(model.BlockedCancel)}
	if node.Waivable {
		answers = append(answers, string(model.BlockedWaive))
	}
	transition.Updates[0] = GraphAttemptUpdate{Ref: current.Ref, DecisionID: decisionID, State: model.NodeAttemptBlocked, Outcome: model.WorkOutcomeRejected, Detail: detail}
	transition.DecisionWindows = append(transition.DecisionWindows, model.DecisionWindow{
		ID: decisionID, Kind: model.DecisionBlocked, SourceRevision: record.Run.Revision,
		Attempt: current.Ref, Audience: []model.DecisionAudience{{Subject: record.Run.Authority}},
		Question: "Retry, rework, waive, or cancel the exhausted branch?", PermittedAnswers: answers,
		ExpiresAt: record.Run.Deadline, State: model.DecisionOpen, Revision: 1,
		CreatedAt: transition.At, UpdatedAt: transition.At,
	})
	virtual := append([]model.WorkNodeAttempt(nil), record.Run.NodeAttempts...)
	for i := range virtual {
		if virtual[i].Ref == current.Ref {
			virtual[i].State = model.NodeAttemptBlocked
		}
	}
	if onlyWaiting(virtual) {
		transition.RunState, transition.ControlState = model.WorkRunWaiting, model.WorkControlWaiting
	}
	return transition, true
}

func (s *Service) retryActivation(run model.WorkRun, current model.WorkNodeAttempt, node model.WorkNode, now time.Time, budget uint32) (model.WorkNodeAttempt, []model.DecisionWindow) {
	readyAt := now
	state := model.NodeAttemptReady
	var retryAt *time.Time
	if node.Retry.Backoff > 0 {
		value := now.Add(node.Retry.Backoff)
		readyAt, retryAt, state = value, &value, model.NodeAttemptRetryWait
	}
	deadline := current.Deadline
	if node.Performer != nil && strings.TrimSpace(node.Performer.Timeout) != "" {
		deadline = activationDeadline(node.Performer, run.Graph.ProgramActivationTimeouts[node.ID], readyAt, run.Deadline)
	}
	if node.Retry.AttemptBudget > 0 && now.Add(node.Retry.AttemptBudget).Before(deadline) {
		deadline = now.Add(node.Retry.AttemptBudget)
	}
	attempt := model.WorkNodeAttempt{
		Ref:   model.WorkAttemptRef{RunID: current.Ref.RunID, NodeID: current.Ref.NodeID, ActivationID: current.Ref.ActivationID, Attempt: current.Ref.Attempt + 1},
		State: state, Performer: current.Performer, ReadyAt: readyAt, RetryAt: retryAt,
		Deadline: deadline, RetryBudget: budget, CreatedAt: now, UpdatedAt: now,
	}
	if state == model.NodeAttemptRetryWait {
		return attempt, nil
	}
	return s.attachDecisionWindow(run, node, attempt, now)
}

func retryFailureClass(node model.WorkNode) string {
	if node.Kind == model.WorkNodeDecision || node.Performer != nil && node.Performer.Kind == model.PerformerHuman {
		return model.RetryableHumanRejection
	}
	if node.Performer != nil && node.Performer.Kind == model.PerformerAgent {
		return model.RetryableAgentRejection
	}
	return model.RetryableProgramFailure
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func acceptedArtifactEvidence(evidence model.WorkNodeEvidence, revision string) bool {
	if evidence.ArtifactRevision != revision || evidence.Disposition != model.WorkOutcomeVerified {
		return false
	}
	return evidence.Passed == nil || *evidence.Passed
}

func graphAttempt(run model.WorkRun, ref model.WorkAttemptRef) (model.WorkNodeAttempt, bool) {
	for _, attempt := range run.NodeAttempts {
		if attempt.Ref == ref {
			return attempt, true
		}
	}
	return model.WorkNodeAttempt{}, false
}

func (s *Service) recordGraphAttemptUnavailable(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, cause error) (WorkRunRecord, error) {
	detail := cause.Error()
	if attempt.Detail == detail {
		return record, cause
	}
	updated, err := s.store.ApplyGraphTransition(ctx, GraphTransition{
		WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision,
		Updates:      []GraphAttemptUpdate{{Ref: attempt.Ref, State: attempt.State, Outcome: attempt.Outcome, Detail: detail}},
		RunState:     record.Run.State,
		ControlState: record.Run.ControlState,
		RunOutcome:   record.Run.Outcome,
		At:           s.now().UTC(),
	})
	if err != nil {
		return record, err
	}
	return updated, cause
}

func graphAttemptTerminal(state model.WorkNodeAttemptState) bool {
	return state == model.NodeAttemptSucceeded || state == model.NodeAttemptFailed || state == model.NodeAttemptWaived || state == model.NodeAttemptSuppressed
}

func outgoingNodes(graph model.WorkGraph, id model.WorkNodeID) []model.WorkNodeID {
	var result []model.WorkNodeID
	for _, edge := range graph.Edges {
		if edge.From == id {
			result = append(result, edge.To)
		}
	}
	return result
}

func outgoingNodesForVerdict(graph model.WorkGraph, id model.WorkNodeID, verdict string) []model.WorkNodeID {
	if verdict == "" {
		return outgoingNodes(graph, id)
	}
	var matching []model.WorkNodeID
	for _, edge := range graph.Edges {
		if edge.From == id && edge.Verdict == verdict {
			matching = append(matching, edge.To)
		}
	}
	if len(matching) > 0 {
		return matching
	}
	// Unlabelled edges are defaults. A missing match never authorizes routes
	// labelled for another answer, including in already-persisted older graphs.
	var defaults []model.WorkNodeID
	for _, edge := range graph.Edges {
		if edge.From == id && edge.Verdict == "" {
			defaults = append(defaults, edge.To)
		}
	}
	return defaults
}

func hasVerdictEdge(graph model.WorkGraph, id model.WorkNodeID, verdict string) bool {
	for _, edge := range graph.Edges {
		if edge.From == id && edge.Verdict == verdict {
			return true
		}
	}
	return false
}

func incomingNodes(graph model.WorkGraph, id model.WorkNodeID) []model.WorkNodeID {
	var result []model.WorkNodeID
	for _, edge := range graph.Edges {
		if edge.To == id {
			result = append(result, edge.From)
		}
	}
	return result
}

func nodeConcludedSuccessfully(attempts []model.WorkNodeAttempt, nodeID model.WorkNodeID) bool {
	for _, attempt := range attempts {
		if attempt.Ref.NodeID == nodeID && (attempt.State == model.NodeAttemptSucceeded || attempt.State == model.NodeAttemptWaived) {
			return true
		}
	}
	return false
}

func nodeConcludedVerified(attempts []model.WorkNodeAttempt, nodeID model.WorkNodeID) bool {
	for _, attempt := range attempts {
		if attempt.Ref.NodeID == nodeID && attempt.State == model.NodeAttemptSucceeded && attempt.Outcome == model.WorkOutcomeVerified {
			return true
		}
	}
	return false
}

func onlyWaiting(attempts []model.WorkNodeAttempt) bool {
	found := false
	for _, attempt := range attempts {
		switch attempt.State {
		case model.NodeAttemptWaiting, model.NodeAttemptBlocked, model.NodeAttemptRetryWait:
			found = true
		case model.NodeAttemptReady, model.NodeAttemptAdmitted, model.NodeAttemptRunning:
			return false
		}
	}
	return found
}

func (s *Service) initialActivation(runID model.WorkRunID, scope model.WorkScope, node model.WorkNode, now, deadline time.Time, budget time.Duration) (model.WorkNodeAttempt, []model.DecisionWindow) {
	deadline = activationDeadline(node.Performer, budget, now, deadline)
	attempt := model.WorkNodeAttempt{Ref: model.WorkAttemptRef{RunID: runID, NodeID: node.ID, ActivationID: model.WorkActivationID(s.newID("activation_")), Attempt: 1}, State: model.NodeAttemptReady, Performer: node.Performer, ReadyAt: now, Deadline: deadline, RetryBudget: normalizedAttempts(node.Retry.MaxAttempts), CreatedAt: now, UpdatedAt: now}
	run := model.WorkRun{ID: runID, Scope: scope, Deadline: deadline, Revision: 1}
	return s.attachDecisionWindow(run, node, attempt, now)
}

func (s *Service) attachDecisionWindow(run model.WorkRun, node model.WorkNode, attempt model.WorkNodeAttempt, now time.Time) (model.WorkNodeAttempt, []model.DecisionWindow) {
	var audience []model.DecisionAudience
	var question string
	var answers []string
	var expires time.Time
	if node.Kind == model.WorkNodeDecision {
		audience, question, answers = append([]model.DecisionAudience(nil), node.Decision.Audience...), node.Name, append([]string(nil), node.Decision.PermittedAnswers...)
		if node.Decision.QuestionResolved || node.Decision.Question != "" {
			question = node.Decision.Question
		}
		expires = now.Add(node.Decision.ExpiresAfter)
	} else if node.Kind == model.WorkNodeTask && node.Performer != nil && node.Performer.Kind == model.PerformerHuman {
		human := attempt.Performer.Human
		if human.Operator {
			audience = append(audience, model.DecisionAudience{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}})
		}
		if human.AgentID != "" {
			audience = append(audience, model.DecisionAudience{Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: human.AgentID}})
		}
		if human.RoleID != "" {
			audience = append(audience, model.DecisionAudience{RoleID: human.RoleID, GroupID: run.Scope.GroupID})
		}
		question, answers, expires = human.Prompt, []string{"complete", "reject"}, attempt.Deadline
		if len(human.Choices) > 0 {
			answers = append([]string(nil), human.Choices...)
		}
	}
	if len(audience) == 0 {
		return attempt, nil
	}
	decisionID := model.DecisionID(s.newID("decision_"))
	attempt.State, attempt.DecisionID = model.NodeAttemptWaiting, decisionID
	if expires.After(attempt.Deadline) {
		expires = attempt.Deadline
	}
	window := model.DecisionWindow{ID: decisionID, Kind: model.DecisionWork, SourceRevision: run.Revision, Attempt: attempt.Ref, Audience: audience, Question: question, PermittedAnswers: answers, ExpiresAt: expires, State: model.DecisionOpen, Revision: 1, CreatedAt: now, UpdatedAt: now}
	return attempt, []model.DecisionWindow{window}
}

func (s *Service) GetDecision(ctx context.Context, req GetDecisionRequest) (DecisionResult, error) {
	record, err := s.store.Decision(ctx, req.DecisionID)
	if err != nil {
		return DecisionResult{}, err
	}
	if err = s.authorizeDecisionRead(ctx, req.Principal, record.Window); err != nil {
		return DecisionResult{}, err
	}
	return DecisionResult(record), nil
}

func (s *Service) ListPendingDecisions(ctx context.Context, req ListPendingDecisionsRequest) ([]DecisionResult, error) {
	records, err := s.store.PendingDecisions(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]DecisionResult, 0, len(records))
	for _, record := range records {
		if s.authorizeDecisionRead(ctx, req.Principal, record.Window) == nil {
			results = append(results, DecisionResult(record))
		}
	}
	return results, nil
}

func (s *Service) authorizeDecisionRead(ctx context.Context, principal model.Principal, window model.DecisionWindow) error {
	if principal.Kind == model.PrincipalOperator {
		return nil
	}
	resource := model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: window.Attempt.RunID}
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: principal, Action: model.ActionReadStatus, Resource: resource}, s.now().UTC()); err != nil {
		return err
	}
	decision, err := s.store.Authorize(ctx, model.AuthorityRequest{Principal: principal, Action: model.ActionDecideWork, Resource: resource}, s.now().UTC())
	if err != nil || !decision.Allowed {
		return ErrUnauthorized
	}
	subject := principal.Authority
	if principal.Kind == model.PrincipalAgent {
		subject = model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: principal.AgentID}
	}
	for _, audience := range window.Audience {
		if audience.Subject == subject || (audience.RoleID != "" && decision.SourceKind == model.AuthorityRole && decision.SourceID == string(audience.RoleID)) {
			return nil
		}
	}
	return ErrUnauthorized
}

func (s *Service) SubmitDecision(ctx context.Context, req SubmitDecisionRequest) (DecisionResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return DecisionResult{}, err
	}
	if strings.TrimSpace(req.Answer) == "" || strings.TrimSpace(req.Reason) == "" || req.ExpectedWindowRevision == 0 {
		return DecisionResult{}, fail(ErrInvalid, "answer, reason and expected decision revision are required")
	}
	window, err := s.store.Decision(ctx, req.DecisionID)
	if err != nil {
		return DecisionResult{}, err
	}
	submission := model.DecisionSubmission{RequestID: req.Context.RequestID, DecisionID: req.DecisionID, ExpectedWindowRevision: req.ExpectedWindowRevision, ExpectedRunRevision: req.ExpectedRunRevision, Answer: req.Answer, Reason: req.Reason, EvidenceRefs: append([]model.WorkEvidenceID(nil), req.EvidenceRefs...), Actor: req.Context.Principal, SubmittedAt: s.now().UTC()}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionDecideWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: window.Window.Attempt.RunID}}
	record, err := s.store.SubmitDecision(ctx, submission, authority, s.now().UTC())
	if err != nil {
		return DecisionResult{}, err
	}
	run, err := s.store.WorkRun(ctx, record.Window.Attempt.RunID)
	if err != nil {
		return DecisionResult{}, err
	}
	attempt, ok := graphAttempt(run.Run, record.Window.Attempt)
	if !ok {
		return DecisionResult{}, ErrConflict
	}
	if attempt.State == model.NodeAttemptSucceeded || attempt.State == model.NodeAttemptFailed || attempt.State == model.NodeAttemptWaived {
		return DecisionResult(record), nil
	}
	if _, err = s.applyAnsweredDecision(ctx, run, attempt, submission); err != nil && !errors.Is(err, ErrConflict) {
		return DecisionResult{}, err
	}
	return DecisionResult(record), nil
}

func (s *Service) applyAnsweredDecision(ctx context.Context, run WorkRunRecord, attempt model.WorkNodeAttempt, submission model.DecisionSubmission) (WorkRunRecord, error) {
	if attempt.State == model.NodeAttemptBlocked {
		return s.applyBlockedResolution(ctx, run, attempt, submission)
	}
	if group, grouped := taskGroup(*run.Run.Graph, attempt.Ref.NodeID); grouped && attempt.Ref.NodeID == group.Approval && submission.Answer == "rework" {
		return s.reworkTaskPlan(ctx, run, attempt, submission, group)
	}
	outcome := model.WorkOutcomeVerified
	verdict := submission.Answer
	if attempt.Performer != nil && attempt.Performer.Human != nil && len(attempt.Performer.Human.Choices) > 0 {
		switch attempt.Performer.Human.ChoiceOutcomes[submission.Answer] {
		case "pass":
			verdict = "complete"
		case "fail":
			verdict = "reject"
		default:
			return run, fail(ErrInvalid, "answer has no admitted human task outcome")
		}
	}
	switch verdict {
	case "reject":
		outcome = model.WorkOutcomeRejected
	case "cancel":
		outcome = model.WorkOutcomeCancelled
	case "waive":
		node := graphNode(*run.Run.Graph, attempt.Ref.NodeID)
		if !node.Waivable {
			return run, fail(ErrUnauthorized, "node %s does not permit waiver", node.ID)
		}
		outcome = model.WorkOutcomeWaived
	}
	if outcome == model.WorkOutcomeRejected && hasVerdictEdge(*run.Run.Graph, attempt.Ref.NodeID, verdict) {
		outcome = model.WorkOutcomeVerified
	}
	transition := s.graphOutcomeTransitionForVerdict(run, attempt, outcome, submission.Reason, verdict)
	if outcome == model.WorkOutcomeCancelled {
		transition.Updates[0].State = model.NodeAttemptFailed
		transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunCancelled, model.WorkControlDraining, model.WorkOutcomeCancelled
	}
	s.enforceOutcomePolicy(run, &transition, nil, true)
	return s.store.ApplyGraphTransition(ctx, transition)
}

func (s *Service) applyBlockedResolution(ctx context.Context, run WorkRunRecord, attempt model.WorkNodeAttempt, submission model.DecisionSubmission) (WorkRunRecord, error) {
	node := graphNode(*run.Run.Graph, attempt.Ref.NodeID)
	now := s.now().UTC()
	switch model.BlockedResolutionAction(submission.Answer) {
	case model.BlockedWaive:
		if !node.Waivable {
			return run, fail(ErrUnauthorized, "node %s does not permit waiver", node.ID)
		}
		transition := s.graphOutcomeTransitionForVerdict(run, attempt, model.WorkOutcomeWaived, submission.Reason, "")
		s.enforceOutcomePolicy(run, &transition, nil, true)
		return s.store.ApplyGraphTransition(ctx, transition)
	case model.BlockedCancel:
		transition := s.graphOutcomeTransitionForVerdict(run, attempt, model.WorkOutcomeCancelled, submission.Reason, "")
		transition.Updates[0].State = model.NodeAttemptFailed
		transition.RunState, transition.ControlState, transition.RunOutcome = model.WorkRunCancelled, model.WorkControlSettled, model.WorkOutcomeCancelled
		return s.store.ApplyGraphTransition(ctx, transition)
	case model.BlockedRetry, model.BlockedRework:
		if group, grouped := taskGroup(*run.Run.Graph, attempt.Ref.NodeID); grouped && (attempt.Ref.NodeID == group.Review || slices.Contains(group.Checks, attempt.Ref.NodeID)) {
			return s.resolveTaskGate(ctx, run, attempt, submission, group)
		}
		window := normalizedAttempts(node.Retry.MaxAttempts)
		if attempt.RetryBudget >= maxWorkAttempts || window > maxWorkAttempts-attempt.RetryBudget {
			return run, fail(ErrConflict, "node %s cannot extend retry budget beyond %d attempts", node.ID, maxWorkAttempts)
		}
		budget := attempt.RetryBudget + window
		next, windows := s.retryActivation(run.Run, attempt, node, now, budget)
		transition := GraphTransition{WorkRunID: run.Run.ID, ExpectedRevision: run.Run.Revision,
			Updates:     []GraphAttemptUpdate{{Ref: attempt.Ref, State: model.NodeAttemptFailed, Outcome: model.WorkOutcomeRejected, Detail: submission.Reason}},
			Activations: []model.WorkNodeAttempt{next}, RunState: model.WorkRunRunning,
			ControlState: model.WorkControlActive, RunOutcome: run.Run.Outcome, DecisionWindows: windows, At: now}
		return s.store.ApplyGraphTransition(ctx, transition)
	default:
		return run, fail(ErrInvalid, "blocked resolution action is unsupported")
	}
}

func (s *Service) ResolveBlocked(ctx context.Context, req ResolveBlockedRequest) (WorkRunResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return WorkRunResult{}, err
	}
	if req.DecisionID == "" || req.Attempt.RunID == "" || req.Attempt.NodeID == "" || req.Attempt.ActivationID == "" || req.Attempt.Attempt == 0 || req.ExpectedWindowRevision == 0 || req.ExpectedRunRevision == 0 || strings.TrimSpace(req.Reason) == "" {
		return WorkRunResult{}, fail(ErrInvalid, "exact blocked decision, attempt, revisions, action and reason are required")
	}
	window, err := s.store.Decision(ctx, req.DecisionID)
	if err != nil {
		return WorkRunResult{}, err
	}
	if window.Window.Kind != model.DecisionBlocked || window.Window.Attempt != req.Attempt {
		return WorkRunResult{}, ErrConflict
	}
	run, err := s.store.WorkRun(ctx, req.Attempt.RunID)
	if err != nil {
		return WorkRunResult{}, err
	}
	replay := blockedResolutionReplay(window.Submission, req)
	if !replay && (window.Window.Revision != req.ExpectedWindowRevision || run.Run.Revision != req.ExpectedRunRevision) {
		return WorkRunResult{}, ErrConflict
	}
	_, err = s.SubmitDecision(ctx, SubmitDecisionRequest{Context: req.Context, DecisionID: req.DecisionID,
		ExpectedWindowRevision: req.ExpectedWindowRevision, ExpectedRunRevision: req.ExpectedRunRevision, Answer: string(req.Action), Reason: req.Reason, EvidenceRefs: req.EvidenceRefs})
	if err != nil {
		return WorkRunResult{}, err
	}
	updated, err := s.store.WorkRun(ctx, req.Attempt.RunID)
	return WorkRunResult(updated), err
}

func blockedResolutionReplay(prior *model.DecisionSubmission, req ResolveBlockedRequest) bool {
	if prior == nil || prior.RequestID != req.Context.RequestID || prior.DecisionID != req.DecisionID || prior.ExpectedWindowRevision != req.ExpectedWindowRevision || prior.ExpectedRunRevision != req.ExpectedRunRevision || prior.Answer != string(req.Action) || prior.Reason != req.Reason || !reflect.DeepEqual(prior.EvidenceRefs, req.EvidenceRefs) {
		return false
	}
	return reflect.DeepEqual(prior.Actor, req.Context.Principal)
}

func (s *Service) validateProgramBindings(ctx context.Context, graph model.WorkGraph, authorized []model.ProgramProfileRef, principal model.Principal, scope model.WorkScope) error {
	allowed := make(map[model.ProgramProfileRevisionID]model.ProgramProfileRef, len(authorized))
	revisions := make(map[model.ProgramProfileRevisionID]model.ProgramProfileRevision, len(authorized))
	for _, ref := range authorized {
		revision, err := s.store.ProgramProfileRevision(ctx, ref.RevisionID)
		if err != nil {
			return err
		}
		if revision.ProfileID != ref.ProfileID || revision.ContentHash != ref.ContentHash {
			return fail(ErrConflict, "program profile %s does not match its pinned revision", ref.ProfileID)
		}
		allowed[ref.RevisionID] = ref
		revisions[ref.RevisionID] = revision
	}
	for _, node := range graph.Nodes {
		if node.Performer == nil || node.Performer.Kind != model.PerformerProgram {
			continue
		}
		if _, ok := allowed[node.Performer.Program.Profile.RevisionID]; !ok {
			return fail(ErrUnauthorized, "program profile %s is not explicitly authorized for the run", node.Performer.Program.Profile.ProfileID)
		}
		if principal.Kind != model.PrincipalOperator {
			for _, requirement := range revisions[node.Performer.Program.Profile.RevisionID].EffectAuthority {
				bound := bindProgramAuthority(requirement, scope.WorkspaceID)
				bound.Principal = principal
				if err := s.requireAuthority(ctx, bound, s.now().UTC()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) SaveAutomationRule(ctx context.Context, req SaveAutomationRuleRequest) (AutomationRuleResult, error) {
	return s.saveAutomationRule(ctx, req, "")
}

func (s *Service) saveAutomationRule(ctx context.Context, req SaveAutomationRuleRequest, deploymentID model.DeploymentID) (AutomationRuleResult, error) {
	if err := req.Delegation.Bounds.ValidateEnvironments(); err != nil {
		return AutomationRuleResult{}, fail(ErrInvalid, "%v", err)
	}
	if err := validateEffectContext(req.Context); err != nil {
		return AutomationRuleResult{}, err
	}
	if err := req.ID.Validate(); err != nil {
		return AutomationRuleResult{}, fail(ErrInvalid, "%v", err)
	}
	if req.RevisionID == "" {
		req.RevisionID = model.AutomationRuleRevisionID(s.newID("rule_rev_"))
	}
	if err := req.RevisionID.Validate(); err != nil {
		return AutomationRuleResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.Name) == "" {
		return AutomationRuleResult{}, fail(ErrInvalid, "automation rule name is required")
	}
	if err := validateAutomation(req.Condition, req.Action, req.Policy); err != nil {
		return AutomationRuleResult{}, err
	}
	closure, err := s.definitionClosure(ctx, "", req.Dependencies)
	if err != nil {
		return AutomationRuleResult{}, err
	}
	if req.Context.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionManageAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: req.ID}}, s.now().UTC()); err != nil {
			return AutomationRuleResult{}, err
		}
	}
	now := s.now().UTC()
	hashInput := struct {
		Name         string
		Enabled      bool
		Owner        model.AuthoritySubject
		Delegation   model.AutomationDelegation
		Condition    model.AutomationCondition
		Action       model.AutomationAction
		Policy       model.OccurrencePolicy
		Dependencies []model.DefinitionRef
	}{strings.TrimSpace(req.Name), req.Enabled, req.Owner, req.Delegation, req.Condition, req.Action, req.Policy, closure}
	revision := model.AutomationRuleRevision{ID: req.RevisionID, RuleID: req.ID, ContentHash: contentHash(hashInput), Owner: req.Owner, Delegation: req.Delegation, Condition: req.Condition, Action: req.Action, Policy: req.Policy, Dependencies: closure, Author: req.Context.Principal, RequestID: req.Context.RequestID, CreatedAt: now}
	rule := model.AutomationRule{ID: req.ID, Name: strings.TrimSpace(req.Name), HeadRevisionID: req.RevisionID, Enabled: req.Enabled, DeploymentID: deploymentID, Revision: req.ExpectedRevision + 1, CreatedAt: now, UpdatedAt: now}
	record, err := s.store.SaveAutomationRule(ctx, rule, revision, req.ExpectedRevision)
	return AutomationRuleResult{Rule: record.Rule, Revision: record.Head}, err
}

func (s *Service) GetAutomationRule(ctx context.Context, req GetAutomationRuleRequest) (AutomationRuleResult, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: req.ID}}, s.now().UTC()); err != nil {
			return AutomationRuleResult{}, err
		}
	}
	record, err := s.store.AutomationRule(ctx, req.ID)
	return AutomationRuleResult{Rule: record.Rule, Revision: record.Head}, err
}

func (s *Service) ListAutomationRules(ctx context.Context, req ListAutomationRulesRequest) ([]model.AutomationRule, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule}}, s.now().UTC()); err != nil {
			return nil, err
		}
	}
	return s.store.ListAutomationRules(ctx, req.IncludeTombstoned)
}

func (s *Service) RunRuleNow(ctx context.Context, req RunRuleNowRequest) (OccurrenceResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return OccurrenceResult{}, err
	}
	if err := req.OccurrenceID.Validate(); err != nil {
		return OccurrenceResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.SourceOccurrenceKey) == "" {
		return OccurrenceResult{}, fail(ErrInvalid, "manual occurrence key is required")
	}
	requestedRecipients := append([]model.AgentID(nil), req.Recipients...)
	sort.Slice(requestedRecipients, func(i, j int) bool { return requestedRecipients[i] < requestedRecipients[j] })
	unique := requestedRecipients[:0]
	for _, id := range requestedRecipients {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	fingerprint := contentHash(struct {
		Scope      string
		RuleID     model.AutomationRuleID
		Expected   model.Revision
		Key        string
		Recipients []model.AgentID
	}{requestScopeForDigest(req.Context.Principal), req.RuleID, req.ExpectedRuleRevision, req.SourceOccurrenceKey, unique})
	if stored, readErr := s.store.Occurrence(ctx, req.OccurrenceID); readErr == nil {
		if stored.Occurrence.RequestID != req.Context.RequestID || stored.Occurrence.RequestFingerprint != fingerprint {
			return OccurrenceResult{}, ErrConflict
		}
		return OccurrenceResult(stored), nil
	} else if !errors.Is(readErr, ErrNotFound) {
		return OccurrenceResult{}, readErr
	}
	record, err := s.store.AutomationRule(ctx, req.RuleID)
	if err != nil {
		return OccurrenceResult{}, err
	}
	if record.Rule.Revision != req.ExpectedRuleRevision || !record.Rule.Enabled {
		return OccurrenceResult{}, ErrConflict
	}
	if req.Context.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionRunAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: req.RuleID}}, s.now().UTC()); err != nil {
			return OccurrenceResult{}, err
		}
	}
	now := s.now().UTC()
	expires := now.Add(record.Head.Policy.ExpiresAfter)
	recipients := make([]model.OccurrenceRecipient, 0, len(req.Recipients))
	if len(req.Recipients) == 0 {
		recipients, err = s.automationRecipients(ctx, record.Head.Action)
		if err != nil {
			return OccurrenceResult{}, err
		}
	} else {
		seen := make(map[model.AgentID]bool, len(req.Recipients))
		for _, id := range req.Recipients {
			if seen[id] {
				continue
			}
			seen[id] = true
			recipients = append(recipients, model.OccurrenceRecipient{AgentID: id, Disposition: model.RecipientPending})
		}
	}
	requester := model.AutomationPrincipal(string(req.OccurrenceID), record.Head.Owner, record.Head.Delegation)
	occurrence := model.AutomationOccurrence{RequestFingerprint: fingerprint, RequestScope: requestScopeForDigest(req.Context.Principal), ID: req.OccurrenceID, RuleID: req.RuleID, RuleRevisionID: record.Head.ID, SourceOccurrenceKey: "manual:" + req.SourceOccurrenceKey, RequestID: req.Context.RequestID, Requester: requester, ScheduledAt: now, EligibleAt: now, ExpiresAt: expires, State: model.OccurrencePending, Recipients: recipients, Revision: 1, CreatedAt: now, UpdatedAt: now}
	created, _, err := s.store.MaterializeOccurrence(ctx, occurrence, req.ExpectedRuleRevision)
	return OccurrenceResult(created), err
}

func (s *Service) ListOccurrences(ctx context.Context, req ListOccurrencesRequest) ([]OccurrenceResult, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: req.RuleID}}, s.now().UTC()); err != nil {
			return nil, err
		}
	}
	records, err := s.store.OccurrencesForRule(ctx, req.RuleID)
	if err != nil {
		return nil, err
	}
	results := make([]OccurrenceResult, len(records))
	for i := range records {
		results[i] = OccurrenceResult{Occurrence: records[i].Occurrence}
	}
	return results, nil
}

func validateParameters(parameters []model.ParameterDeclaration) error {
	seen := make(map[string]bool, len(parameters))
	for _, parameter := range parameters {
		if len(parameter.DisplayName) > 200 || !utf8.ValidString(parameter.DisplayName) || strings.ContainsRune(parameter.DisplayName, 0) || parameter.DisplayName != strings.TrimSpace(parameter.DisplayName) {
			return fail(ErrInvalid, "parameter display name requires bounded valid text")
		}
		if err := validateProcessProse(parameter.Description, parameter.Doc); err != nil {
			return err
		}
		if strings.TrimSpace(parameter.Name) == "" || seen[parameter.Name] {
			return fail(ErrInvalid, "parameter names must be non-empty and unique")
		}
		seen[parameter.Name] = true
		switch parameter.Type {
		case model.ParameterString, model.ParameterNumber, model.ParameterBoolean, model.ParameterObject, model.ParameterArray:
		default:
			return fail(ErrInvalid, "parameter %s has unsupported type", parameter.Name)
		}
		if hasParameterDefault(parameter.Default) {
			if err := validateParameterValue(parameter, parameter.Default); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateParameterValues(declarations []model.ParameterDeclaration, values map[string]json.RawMessage) error {
	declared := make(map[string]model.ParameterDeclaration, len(declarations))
	for _, declaration := range declarations {
		declared[declaration.Name] = declaration
	}
	for _, declaration := range declarations {
		if _, ok := values[declaration.Name]; !ok && declaration.Required && !hasParameterDefault(declaration.Default) {
			return fail(ErrInvalid, "required parameter %s is missing", declaration.Name)
		}
	}
	for name, raw := range values {
		declaration, ok := declared[name]
		if !ok {
			return fail(ErrInvalid, "parameter %s is not declared", name)
		}
		if err := validateParameterValue(declaration, raw); err != nil {
			return err
		}
	}
	return nil
}

func validateParameterValue(declaration model.ParameterDeclaration, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fail(ErrInvalid, "parameter %s is invalid JSON", declaration.Name)
	}
	valid := false
	switch declaration.Type {
	case model.ParameterString:
		_, valid = value.(string)
	case model.ParameterNumber:
		_, valid = value.(float64)
	case model.ParameterBoolean:
		_, valid = value.(bool)
	case model.ParameterObject:
		_, valid = value.(map[string]any)
	case model.ParameterArray:
		_, valid = value.([]any)
	}
	if !valid {
		return fail(ErrInvalid, "parameter %s does not match declared type %s", declaration.Name, declaration.Type)
	}
	return nil
}

func materializeParameterValues(declarations []model.ParameterDeclaration, supplied map[string]json.RawMessage) map[string]json.RawMessage {
	values := cloneRawMap(supplied)
	if values == nil {
		values = make(map[string]json.RawMessage)
	}
	for _, declaration := range declarations {
		if _, exists := values[declaration.Name]; !exists && hasParameterDefault(declaration.Default) {
			values[declaration.Name] = append(json.RawMessage(nil), declaration.Default...)
		}
	}
	return values
}

func materializePerformerBindings(graph model.WorkGraph, bindings map[string]model.Performer) (model.WorkGraph, error) {
	resolved := graph
	resolved.Nodes = append([]model.WorkNode(nil), graph.Nodes...)
	used := make(map[string]bool, len(bindings))
	for i := range resolved.Nodes {
		performer := resolved.Nodes[i].Performer
		if performer == nil || performer.Kind != model.PerformerAgent || performer.Agent == nil {
			continue
		}
		if performer.Agent.CreateDesired != nil {
			return model.WorkGraph{}, fail(ErrUnsupported, "work node %s requests unsupported dynamic agent creation", resolved.Nodes[i].ID)
		}
		if performer.Agent.MemberKey == "" {
			continue
		}
		binding, ok := bindings[performer.Agent.MemberKey]
		if !ok || binding.Kind != model.PerformerAgent || binding.Agent == nil || binding.Agent.AgentID == "" || binding.Agent.MemberKey != "" || binding.Agent.CreateDesired != nil {
			return model.WorkGraph{}, fail(ErrInvalid, "work node %s requires exact agent binding %s", resolved.Nodes[i].ID, performer.Agent.MemberKey)
		}
		copy := *performer
		agent := *performer.Agent
		agent.AgentID, agent.MemberKey = binding.Agent.AgentID, ""
		copy.Agent = &agent
		resolved.Nodes[i].Performer = &copy
		used[performer.Agent.MemberKey] = true
	}
	for key := range bindings {
		if !used[key] {
			return model.WorkGraph{}, fail(ErrInvalid, "performer binding %s is not referenced by the pinned graph", key)
		}
	}
	return resolved, nil
}

func validateTeam(team model.TeamDefinition) error {
	if len(team.Members) == 0 || len(team.Waves) == 0 {
		return fail(ErrInvalid, "team members and waves are required")
	}
	if team.WorkspacePolicy != model.WorkspacePolicyShared && team.WorkspacePolicy != model.WorkspacePolicyPerMember {
		return fail(ErrInvalid, "team workspace policy is required")
	}
	members := make(map[string]bool, len(team.Members))
	for _, member := range team.Members {
		if err := member.Desired.Environment.Validate(); err != nil {
			return fail(ErrInvalid, "team member environment: %v", err)
		}
		if strings.TrimSpace(member.Key) == "" || members[member.Key] {
			return fail(ErrInvalid, "team member keys must be non-empty and unique")
		}
		if err := model.WorkNodeID(member.Key).Validate(); err != nil {
			return fail(ErrInvalid, "team member key: %v", err)
		}
		members[member.Key] = true
	}
	briefs := make(map[string]model.TeamBriefing, len(team.Briefings))
	for _, brief := range team.Briefings {
		if brief.Syntax != "" && (len(brief.Body) > maxMessageBodyBytes || !utf8.ValidString(brief.Body) || strings.ContainsRune(brief.Body, 0)) {
			return fail(ErrInvalid, "templated briefing requires bounded valid text")
		}
		if brief.Syntax != "" && brief.Syntax != "mission-v1" {
			return fail(ErrInvalid, "unsupported team briefing syntax")
		}
		if strings.TrimSpace(brief.ID) == "" || strings.TrimSpace(brief.Body) == "" || briefs[brief.ID].ID != "" {
			return fail(ErrInvalid, "briefing ids and bodies must be non-empty and unique")
		}
		if brief.Timing != model.BriefingBeforeFirstWork && brief.Timing != model.BriefingAfterReady {
			return fail(ErrInvalid, "briefing %s has unsupported timing", brief.ID)
		}
		for _, key := range brief.MemberKeys {
			if !members[key] {
				return fail(ErrInvalid, "briefing %s references unknown member %s", brief.ID, key)
			}
		}
		briefs[brief.ID] = brief
	}
	for _, member := range team.Members {
		for _, id := range member.BriefingIDs {
			if _, ok := briefs[id]; !ok {
				return fail(ErrInvalid, "member %s references unknown briefing %s", member.Key, id)
			}
		}
	}
	waves := make(map[string]model.TeamWave, len(team.Waves))
	assigned := make(map[string]bool, len(team.Members))
	for _, wave := range team.Waves {
		if strings.TrimSpace(wave.ID) == "" || waves[wave.ID].ID != "" || len(wave.MemberKeys) == 0 {
			return fail(ErrInvalid, "wave ids must be unique and contain members")
		}
		if err := model.WorkNodeID(wave.ID).Validate(); err != nil {
			return fail(ErrInvalid, "team wave id: %v", err)
		}
		for _, key := range wave.MemberKeys {
			if !members[key] || assigned[key] {
				return fail(ErrInvalid, "wave %s contains unknown or repeated member %s", wave.ID, key)
			}
			assigned[key] = true
		}
		waves[wave.ID] = wave
	}
	if len(assigned) != len(members) {
		return fail(ErrInvalid, "every team member must belong to exactly one wave")
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fail(ErrInvalid, "team wave dependency graph is cyclic")
		}
		if visited[id] {
			return nil
		}
		wave, ok := waves[id]
		if !ok {
			return fail(ErrInvalid, "unknown wave dependency %s", id)
		}
		visiting[id] = true
		for _, dependency := range wave.DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id], visited[id] = false, true
		return nil
	}
	for id := range waves {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkGraph(graph model.WorkGraph) error {
	if err := validateProcessProse(graph.Description, graph.Doc); err != nil {
		return err
	}
	if graph.CompilerVersion == "" || graph.EntryNodeID == "" || len(graph.Nodes) == 0 || len(graph.Nodes) > maxGraphNodes {
		return fail(ErrInvalid, "bounded graph, compiler version and entry node are required")
	}
	nodes := make(map[model.WorkNodeID]model.WorkNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if err := node.ID.Validate(); err != nil {
			return fail(ErrInvalid, "%v", err)
		}
		if _, exists := nodes[node.ID]; exists {
			return fail(ErrInvalid, "duplicate work node %s", node.ID)
		}
		if node.Retry.MaxAttempts > maxWorkAttempts {
			return fail(ErrInvalid, "work node %s exceeds retry cap %d", node.ID, maxWorkAttempts)
		}
		if err := validateRetryPolicy(node); err != nil {
			return err
		}
		if err := validateWorkNode(node); err != nil {
			return err
		}
		nodes[node.ID] = node
	}
	if _, ok := nodes[graph.EntryNodeID]; !ok {
		return fail(ErrInvalid, "work graph entry node does not exist")
	}
	adjacency := make(map[model.WorkNodeID][]model.WorkNodeID)
	incoming := make(map[model.WorkNodeID]int)
	for _, edge := range graph.Edges {
		if nodes[edge.From].ID == "" || nodes[edge.To].ID == "" || edge.From == edge.To {
			return fail(ErrInvalid, "work edge references an unknown or identical node")
		}
		if source := nodes[edge.From]; source.Kind == model.WorkNodeDecision && edge.Verdict != "" && !slices.Contains(source.Decision.PermittedAnswers, edge.Verdict) {
			return fail(ErrInvalid, "decision %s route %q is not a permitted answer", source.ID, edge.Verdict)
		}
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		incoming[edge.To]++
	}
	queue := make([]model.WorkNodeID, 0, len(nodes))
	degree := make(map[model.WorkNodeID]int, len(nodes))
	for id := range nodes {
		degree[id] = incoming[id]
		if degree[id] == 0 {
			queue = append(queue, id)
		}
	}
	var visited int
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range adjacency[id] {
			degree[next]--
			if degree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(nodes) {
		return fail(ErrInvalid, "ordinary work edges must be acyclic")
	}
	reachable := map[model.WorkNodeID]bool{graph.EntryNodeID: true}
	queue = []model.WorkNodeID{graph.EntryNodeID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[id] {
			if !reachable[next] {
				reachable[next] = true
				queue = append(queue, next)
			}
		}
	}
	if len(reachable) != len(nodes) {
		return fail(ErrInvalid, "all work nodes must be reachable from the entry")
	}
	for id, node := range nodes {
		if node.Kind == model.WorkNodeFork && len(adjacency[id]) < 2 {
			return fail(ErrInvalid, "fork %s requires at least two outgoing branches", id)
		}
		if node.Kind == model.WorkNodeJoin && incoming[id] < 2 {
			return fail(ErrInvalid, "join %s requires at least two incoming branches", id)
		}
		if node.Kind == model.WorkNodeEnd && len(adjacency[id]) != 0 {
			return fail(ErrInvalid, "end node %s cannot have outgoing edges", id)
		}
	}
	for _, id := range graph.Outcome.RequiredNodes {
		if nodes[id].ID == "" {
			return fail(ErrInvalid, "outcome references unknown node %s", id)
		}
	}
	return nil
}

func validateRetryPolicy(node model.WorkNode) error {
	retry := node.Retry
	if retry.Backoff < 0 || retry.AttemptBudget < 0 {
		return fail(ErrInvalid, "work node %s retry timing cannot be negative", node.ID)
	}
	if retry.MaxAttempts == 0 {
		if retry.Backoff != 0 || retry.AttemptBudget != 0 || len(retry.Retryable) != 0 {
			return fail(ErrInvalid, "work node %s retry fields require max attempts", node.ID)
		}
		return nil
	}
	if len(retry.Retryable) == 0 {
		return fail(ErrInvalid, "work node %s retry requires explicit failure classes", node.ID)
	}
	seen := make(map[string]bool, len(retry.Retryable))
	for _, class := range retry.Retryable {
		if seen[class] {
			return fail(ErrInvalid, "work node %s retry failure classes must be unique", node.ID)
		}
		seen[class] = true
		switch class {
		case model.RetryableProgramFailure, model.RetryableAgentRejection, model.RetryableHumanRejection:
		default:
			return fail(ErrInvalid, "work node %s has unsupported retry failure class %q", node.ID, class)
		}
	}
	return nil
}

func validateWorkNode(node model.WorkNode) error {
	if err := validateCaptureNames(node); err != nil {
		return err
	}
	if err := validateProcessProse(node.Description, node.Doc); err != nil {
		return err
	}
	switch node.Kind {
	case model.WorkNodeTask:
		if node.Performer == nil || node.Decision != nil || node.Join != nil || node.Wait != nil || node.End != nil {
			return fail(ErrInvalid, "task node %s requires exactly one performer", node.ID)
		}
		return validatePerformer(*node.Performer)
	case model.WorkNodeDecision:
		if node.Decision != nil && (len(node.Decision.Question) > 128<<10 || !utf8.ValidString(node.Decision.Question) || strings.ContainsRune(node.Decision.Question, 0)) {
			return fail(ErrInvalid, "decision question requires bounded valid text")
		}
		if node.Decision == nil || len(node.Decision.Audience) == 0 || len(node.Decision.PermittedAnswers) == 0 || node.Decision.ExpiresAfter <= 0 {
			return fail(ErrInvalid, "decision node %s requires bounded declared answers", node.ID)
		}
	case model.WorkNodeTaskComplete:
		if node.Performer != nil || node.Decision != nil || node.Join != nil || node.Wait != nil || node.End != nil || node.Stages != nil {
			return fail(ErrInvalid, "task completion node has incompatible configuration")
		}
	case model.WorkNodeFork:
		if node.Join != nil || node.Wait != nil || node.End != nil || node.Performer != nil {
			return fail(ErrInvalid, "fork node %s has incompatible configuration", node.ID)
		}
	case model.WorkNodeJoin:
		if node.Join == nil || (node.Join.Mode != model.JoinAll && node.Join.Mode != model.JoinAny) {
			return fail(ErrInvalid, "join node %s requires all or any policy", node.ID)
		}
	case model.WorkNodeWait:
		if err := validateWaitPolicy(node.Wait); err != nil {
			return err
		}
	case model.WorkNodeEnd:
		if node.End == nil || node.End.Outcome == model.WorkOutcomeNone || node.End.Outcome == model.WorkOutcomeUnknown {
			return fail(ErrInvalid, "end node %s requires a conclusive authored outcome", node.ID)
		}
	default:
		return fail(ErrInvalid, "work node %s has unsupported kind", node.ID)
	}
	return nil
}

func validatePerformer(performer model.Performer) error {
	if _, err := performerTimeout(performer); err != nil {
		return err
	}
	if contact := performer.Contact; contact != nil {
		cadence, err := time.ParseDuration(contact.Cadence)
		if err != nil || cadence <= 0 || len(contact.Cadence) > 128 || contact.Budget == 0 || contact.Budget > 10000 || strings.TrimSpace(contact.EscalationTarget) == "" || len(contact.EscalationTarget) > 1024 || !utf8.ValidString(contact.EscalationTarget) || strings.ContainsRune(contact.EscalationTarget, 0) {
			return fail(ErrInvalid, "contact schedule requires a positive duration, budget 1–10000, and bounded nonempty escalation target")
		}
	}
	populated := 0
	if performer.Agent != nil {
		populated++
	}
	if performer.Program != nil {
		populated++
	}
	if performer.Human != nil {
		populated++
	}
	if populated != 1 {
		return fail(ErrInvalid, "performer requires exactly one typed binding")
	}
	switch performer.Kind {
	case model.PerformerAgent:
		if performer.Agent == nil || (performer.Agent.AgentID == "" && performer.Agent.MemberKey == "" && performer.Agent.CreateDesired == nil) {
			return fail(ErrInvalid, "agent performer requires an exact binding or create intent")
		}
	case model.PerformerProgram:
		if performer.Program == nil || performer.Program.Profile.ProfileID == "" || performer.Program.Profile.RevisionID == "" || performer.Program.Profile.ContentHash == "" {
			return fail(ErrInvalid, "program performer requires a pinned profile revision")
		}
		if len(performer.Program.Arguments) > maxProgramArguments || len(performer.Program.Input) > maxProgramInputBytes || (len(performer.Program.Input) > 0 && !json.Valid(performer.Program.Input)) {
			return fail(ErrInvalid, "program performer arguments or input exceed bounded limits")
		}
		for _, argument := range performer.Program.Arguments {
			if len(argument) > maxProgramValueBytes || strings.ContainsRune(argument, '\x00') {
				return fail(ErrInvalid, "program performer argument is invalid or too large")
			}
		}
	case model.PerformerHuman:
		if err := validateHumanChoices(performer.Human); err != nil {
			return err
		}
		if performer.Human != nil && performer.Human.Operator && (performer.Human.AgentID != "" || performer.Human.RoleID != "") {
			return fail(ErrInvalid, "choose operator or agent/role human audience")
		}
		if performer.Human == nil || (!performer.Human.Operator && performer.Human.AgentID == "" && performer.Human.RoleID == "") || strings.TrimSpace(performer.Human.Prompt) == "" {
			return fail(ErrInvalid, "human performer requires audience and prompt")
		}
	default:
		return fail(ErrInvalid, "performer kind is unsupported")
	}
	return nil
}

func validateAutomation(condition model.AutomationCondition, action model.AutomationAction, policy model.OccurrencePolicy) error {
	populated := 0
	if condition.Schedule != nil {
		populated++
	}
	if condition.Trigger != nil {
		populated++
	}
	if condition.StandingOrder != nil {
		populated++
	}
	if populated != 1 {
		return fail(ErrInvalid, "automation condition requires exactly one typed configuration")
	}
	switch condition.Kind {
	case model.AutomationSchedule:
		if condition.Schedule == nil || (condition.Schedule.Interval > 0) == (strings.TrimSpace(condition.Schedule.Cron) != "") || (condition.Schedule.Interval > 0 && condition.Schedule.Interval < minimumScheduleCadence) || condition.Schedule.Timezone == "" {
			return fail(ErrInvalid, "schedule requires timezone and cron or interval of at least 30 seconds")
		}
		if _, err := time.LoadLocation(condition.Schedule.Timezone); err != nil {
			return fail(ErrInvalid, "schedule timezone is unavailable: %v", err)
		}
		if condition.Schedule.Cron != "" {
			if _, err := cronv3.ParseStandard(condition.Schedule.Cron); err != nil {
				return fail(ErrInvalid, "cron schedule is invalid: %v", err)
			}
		}
	case model.AutomationTrigger:
		if condition.Trigger == nil || strings.TrimSpace(condition.Trigger.SourceID) == "" || strings.TrimSpace(condition.Trigger.FactKind) == "" || len(condition.Trigger.Values) == 0 || condition.Trigger.Freshness <= 0 || condition.Trigger.Dwell < 0 || condition.Trigger.Cooldown < 0 || condition.Trigger.Debounce < 0 {
			return fail(ErrInvalid, "trigger requires named source, exact resource, values and bounded timing")
		}
		if err := validateAutomationFactResource(condition.Trigger.Resource); err != nil {
			return err
		}
		seenValues := map[string]bool{}
		for _, value := range condition.Trigger.Values {
			if strings.TrimSpace(value) == "" || seenValues[value] || condition.Trigger.FactKind == model.FactCICompleted && (value == "pending" || value == "unknown") {
				return fail(ErrInvalid, "trigger values must be unique conclusive values")
			}
			seenValues[value] = true
		}
	case model.AutomationStandingOrder:
		if condition.StandingOrder == nil || strings.TrimSpace(condition.StandingOrder.FactKind) == "" || condition.StandingOrder.DispatchDeadline <= 0 || condition.StandingOrder.Timing != model.StandingOrderSameContinuation {
			return fail(ErrInvalid, "standing order requires a supported same-continuation fact and dispatch deadline")
		}
		if _, err := regexp.Compile(condition.StandingOrder.Pattern); err != nil {
			return fail(ErrInvalid, "standing order pattern is invalid: %v", err)
		}
	default:
		return fail(ErrInvalid, "automation condition kind is unsupported")
	}
	variants := 0
	if action.Message != nil {
		variants++
	}
	if action.Work != nil {
		variants++
	}
	if action.Team != nil {
		variants++
	}
	if variants != 1 {
		return fail(ErrInvalid, "automation action requires exactly one typed action")
	}
	switch action.Kind {
	case model.AutomationSendMessage:
		if action.Message == nil || strings.TrimSpace(action.Message.Body) == "" {
			return fail(ErrInvalid, "message automation requires a body")
		}
		if condition.Kind == model.AutomationStandingOrder && len(action.Message.AgentIDs) == 0 && action.Message.GroupID == "" && action.Message.RoleID == "" {
			return fail(ErrInvalid, "standing guidance requires an explicit agent, group or role target")
		}
	case model.AutomationStartWork:
		if action.Work == nil || (action.Work.Definition == nil) == (action.Work.InlineGraph == nil) {
			return fail(ErrInvalid, "work automation requires one pinned definition or inline graph")
		}
	case model.AutomationDeployTeam:
		if action.Team == nil || action.Team.Definition.Kind != model.DefinitionTeam {
			return fail(ErrInvalid, "team automation requires a pinned team definition")
		}
	default:
		return fail(ErrInvalid, "automation action kind is unsupported")
	}
	if policy.ExpiresAfter <= 0 || policy.Deadline <= 0 || policy.Retry.MaxAttempts > maxWorkAttempts {
		return fail(ErrInvalid, "occurrence expiry, deadline and bounded retry are required")
	}
	switch policy.Overlap {
	case model.OverlapForbid, model.OverlapReplace:
		if policy.MaxActive > 1 {
			return fail(ErrInvalid, "forbid/replace overlap cannot exceed one active occurrence")
		}
	case model.OverlapAllow:
		if policy.MaxActive == 0 {
			return fail(ErrInvalid, "allow overlap requires a positive active limit")
		}
	default:
		return fail(ErrInvalid, "overlap policy is required")
	}
	if policy.Overlap == model.OverlapReplace && action.Kind != model.AutomationStartWork {
		return fail(ErrInvalid, "replace overlap is currently supported only for work actions")
	}
	if policy.MissedTicks != model.MissedTickSkip && policy.MissedTicks != model.MissedTickCoalesce {
		return fail(ErrInvalid, "missed tick policy is required")
	}
	if policy.OfflineDelivery != model.OfflineSkip && policy.OfflineDelivery != model.OfflineQueue {
		return fail(ErrInvalid, "offline delivery policy is required")
	}
	return nil
}

func graphNode(graph model.WorkGraph, id model.WorkNodeID) model.WorkNode {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node
		}
	}
	return model.WorkNode{}
}

func normalizedAttempts(value uint32) uint32 {
	if value == 0 {
		return 1
	}
	return value
}

func contentHash(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal validated orchestration content: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneRawMap(source map[string]json.RawMessage) map[string]json.RawMessage {
	if source == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		result[key] = append(json.RawMessage(nil), value...)
	}
	return result
}

func validateProcessProse(description, doc string) error {
	if len(description) > 16<<10 || len(doc) > 64<<10 || !utf8.ValidString(description) || !utf8.ValidString(doc) || strings.ContainsRune(description, 0) || strings.ContainsRune(doc, 0) {
		return fail(ErrInvalid, "process notes require valid text: description at most 16 KiB and documentation at most 64 KiB")
	}
	return nil
}
