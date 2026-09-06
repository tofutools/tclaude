package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

const (
	orchestrationCompilerVersion = "1"
	maxGraphNodes                = 512
	maxWorkAttempts              = 100
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
		if err := validateWorkGraph(draft.Process.Graph); err != nil {
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
	}{strings.TrimSpace(draft.Name), draft.Kind, draft.SchemaVersion, draft.Source, draft.Parameters, draft.Team, draft.Process, closure}
	return model.DefinitionRevision{ID: draft.RevisionID, DefinitionID: draft.ID, ContentHash: contentHash(hashInput), SchemaVersion: draft.SchemaVersion, CompilerVersion: orchestrationCompilerVersion, Source: draft.Source, Parameters: append([]model.ParameterDeclaration(nil), draft.Parameters...), Team: draft.Team, Process: draft.Process, Dependencies: closure, Author: author}, nil
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
	if req.Sandbox == "" || len(req.EffectAuthority) == 0 {
		return ProgramProfileResult{}, fail(ErrInvalid, "program sandbox and effect authority are required")
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
	var closure []model.DefinitionRef
	if ref := req.Start.Definition; ref != nil {
		revision, err := s.store.DefinitionRevision(ctx, ref.RevisionID)
		if err != nil {
			return WorkRunResult{}, err
		}
		if revision.DefinitionID != ref.DefinitionID || revision.ContentHash != ref.ContentHash || ref.Kind != model.DefinitionProcess || revision.Process == nil {
			return WorkRunResult{}, fail(ErrConflict, "process definition reference is not the pinned revision")
		}
		graph = revision.Process.Graph
		closure = append(append([]model.DefinitionRef(nil), revision.Dependencies...), *ref)
		if err := validateParameterValues(revision.Parameters, req.Start.Parameters); err != nil {
			return WorkRunResult{}, err
		}
	} else {
		graph = *req.Start.InlineGraph
	}
	if err := validateWorkGraph(graph); err != nil {
		return WorkRunResult{}, err
	}
	if req.Start.Deadline.IsZero() || !req.Start.Deadline.After(s.now().UTC()) {
		return WorkRunResult{}, fail(ErrInvalid, "a future work deadline is required")
	}
	if err := s.validateProgramBindings(ctx, graph, req.Start.AuthorizedProgramProfiles, req.Context.Principal); err != nil {
		return WorkRunResult{}, err
	}
	authority := req.Context.Principal.Authority
	if req.Context.Principal.Kind == model.PrincipalOperator {
		authority = model.AuthoritySubject{Kind: model.AuthorityOperator}
	}
	if req.Context.Principal.Kind != model.PrincipalOperator {
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionStartWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: req.ID}}, s.now().UTC()); err != nil {
			return WorkRunResult{}, err
		}
	}
	now := s.now().UTC()
	entry := graphNode(graph, graph.EntryNodeID)
	run := model.WorkRun{ID: req.ID, RequestID: req.Context.RequestID, Requester: req.Context.Principal, Authority: authority, Delegation: req.Context.Principal.Delegation, Graph: &graph, DefinitionClosure: closure, Parameters: cloneRawMap(req.Start.Parameters), Scope: req.Start.Scope, AuthorizedPrograms: append([]model.ProgramProfileRef(nil), req.Start.AuthorizedProgramProfiles...), ControlState: model.WorkControlActive, State: model.WorkRunRunning, Deadline: req.Start.Deadline.UTC(), Revision: 1, CreatedAt: now, UpdatedAt: now}
	attempt, windows := s.initialActivation(req.ID, req.Start.Scope, entry, now, run.Deadline)
	run.NodeAttempts = []model.WorkNodeAttempt{attempt}
	record, _, err := s.store.CreateGraphWorkRun(ctx, run, windows)
	return WorkRunResult(record), err
}

func (s *Service) initialActivation(runID model.WorkRunID, scope model.WorkScope, node model.WorkNode, now, deadline time.Time) (model.WorkNodeAttempt, []model.DecisionWindow) {
	attempt := model.WorkNodeAttempt{Ref: model.WorkAttemptRef{RunID: runID, NodeID: node.ID, ActivationID: model.WorkActivationID(s.newID("activation_")), Attempt: 1}, State: model.NodeAttemptReady, Performer: node.Performer, ReadyAt: now, Deadline: deadline, RetryBudget: normalizedAttempts(node.Retry.MaxAttempts), CreatedAt: now, UpdatedAt: now}
	var audience []model.DecisionAudience
	var question string
	var answers []string
	var expires time.Time
	if node.Kind == model.WorkNodeDecision {
		audience, question, answers = append([]model.DecisionAudience(nil), node.Decision.Audience...), node.Name, append([]string(nil), node.Decision.PermittedAnswers...)
		expires = now.Add(node.Decision.ExpiresAfter)
	} else if node.Kind == model.WorkNodeTask && node.Performer != nil && node.Performer.Kind == model.PerformerHuman {
		human := node.Performer.Human
		if human.AgentID != "" {
			audience = append(audience, model.DecisionAudience{Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: human.AgentID}})
		}
		if human.RoleID != "" {
			audience = append(audience, model.DecisionAudience{RoleID: human.RoleID, GroupID: scope.GroupID})
		}
		question, answers, expires = human.Prompt, []string{"complete", "reject"}, deadline
	}
	if len(audience) == 0 {
		return attempt, nil
	}
	decisionID := model.DecisionID(s.newID("decision_"))
	attempt.State, attempt.DecisionID = model.NodeAttemptWaiting, decisionID
	window := model.DecisionWindow{ID: decisionID, Kind: model.DecisionWork, SourceRevision: 1, Attempt: attempt.Ref, Audience: audience, Question: question, PermittedAnswers: answers, ExpiresAt: expires, State: model.DecisionOpen, Revision: 1, CreatedAt: now, UpdatedAt: now}
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
	submission := model.DecisionSubmission{RequestID: req.Context.RequestID, DecisionID: req.DecisionID, ExpectedWindowRevision: req.ExpectedWindowRevision, Answer: req.Answer, Reason: req.Reason, EvidenceRefs: append([]model.WorkEvidenceID(nil), req.EvidenceRefs...), Actor: req.Context.Principal, SubmittedAt: s.now().UTC()}
	authority := model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionDecideWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: window.Window.Attempt.RunID}}
	record, err := s.store.SubmitDecision(ctx, submission, authority, s.now().UTC())
	return DecisionResult(record), err
}

func (s *Service) validateProgramBindings(ctx context.Context, graph model.WorkGraph, authorized []model.ProgramProfileRef, principal model.Principal) error {
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
				if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: principal, Action: requirement.Action, Resource: requirement.Resource, RequestedConfiguration: requirement.RequestedConfiguration}, s.now().UTC()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) SaveAutomationRule(ctx context.Context, req SaveAutomationRuleRequest) (AutomationRuleResult, error) {
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
	rule := model.AutomationRule{ID: req.ID, Name: strings.TrimSpace(req.Name), HeadRevisionID: req.RevisionID, Enabled: req.Enabled, Revision: req.ExpectedRevision + 1, CreatedAt: now, UpdatedAt: now}
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
	seen := make(map[model.AgentID]bool, len(req.Recipients))
	for _, id := range req.Recipients {
		if seen[id] {
			continue
		}
		seen[id] = true
		recipients = append(recipients, model.OccurrenceRecipient{AgentID: id, Disposition: model.RecipientPending})
	}
	occurrence := model.AutomationOccurrence{ID: req.OccurrenceID, RuleID: req.RuleID, RuleRevisionID: record.Head.ID, SourceOccurrenceKey: "manual:" + req.SourceOccurrenceKey, RequestID: req.Context.RequestID, Requester: req.Context.Principal, ScheduledAt: now, EligibleAt: now, ExpiresAt: expires, State: model.OccurrencePending, Recipients: recipients, Revision: 1, CreatedAt: now, UpdatedAt: now}
	created, _, err := s.store.MaterializeOccurrence(ctx, occurrence, req.ExpectedRuleRevision)
	return OccurrenceResult{Occurrence: created.Occurrence}, err
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
		if strings.TrimSpace(parameter.Name) == "" || seen[parameter.Name] {
			return fail(ErrInvalid, "parameter names must be non-empty and unique")
		}
		seen[parameter.Name] = true
		switch parameter.Type {
		case model.ParameterString, model.ParameterNumber, model.ParameterBoolean, model.ParameterObject, model.ParameterArray:
		default:
			return fail(ErrInvalid, "parameter %s has unsupported type", parameter.Name)
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
		if _, ok := values[declaration.Name]; !ok && declaration.Required && len(declaration.Default) == 0 {
			return fail(ErrInvalid, "required parameter %s is missing", declaration.Name)
		}
	}
	for name, raw := range values {
		declaration, ok := declared[name]
		if !ok {
			return fail(ErrInvalid, "parameter %s is not declared", name)
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return fail(ErrInvalid, "parameter %s is invalid JSON", name)
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
			return fail(ErrInvalid, "parameter %s does not match declared type %s", name, declaration.Type)
		}
	}
	return nil
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
		if strings.TrimSpace(member.Key) == "" || members[member.Key] {
			return fail(ErrInvalid, "team member keys must be non-empty and unique")
		}
		members[member.Key] = true
	}
	briefs := make(map[string]model.TeamBriefing, len(team.Briefings))
	for _, brief := range team.Briefings {
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

func validateWorkNode(node model.WorkNode) error {
	switch node.Kind {
	case model.WorkNodeTask:
		if node.Performer == nil || node.Decision != nil || node.Join != nil || node.Wait != nil || node.End != nil {
			return fail(ErrInvalid, "task node %s requires exactly one performer", node.ID)
		}
		return validatePerformer(*node.Performer)
	case model.WorkNodeDecision:
		if node.Decision == nil || len(node.Decision.Audience) == 0 || len(node.Decision.PermittedAnswers) == 0 || node.Decision.ExpiresAfter <= 0 {
			return fail(ErrInvalid, "decision node %s requires bounded declared answers", node.ID)
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
		if node.Wait == nil || (node.Wait.Duration <= 0) == (strings.TrimSpace(node.Wait.Until) == "") {
			return fail(ErrInvalid, "wait node %s requires exactly one bounded wake condition", node.ID)
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
	case model.PerformerHuman:
		if performer.Human == nil || (performer.Human.AgentID == "" && performer.Human.RoleID == "") || strings.TrimSpace(performer.Human.Prompt) == "" {
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
	case model.AutomationTrigger:
		if condition.Trigger == nil || strings.TrimSpace(condition.Trigger.FactKind) == "" || condition.Trigger.Freshness <= 0 {
			return fail(ErrInvalid, "trigger requires fact kind and bounded freshness")
		}
	case model.AutomationStandingOrder:
		if condition.StandingOrder == nil || strings.TrimSpace(condition.StandingOrder.FactKind) == "" || condition.StandingOrder.DispatchDeadline <= 0 || (condition.StandingOrder.Timing != model.StandingOrderSameContinuation && condition.StandingOrder.Timing != model.StandingOrderNextTurn) {
			return fail(ErrInvalid, "standing order requires fact, timing and dispatch deadline")
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
