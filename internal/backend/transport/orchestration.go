package transport

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) RegisterOrchestrationAPI(api app.OrchestrationAPI) error {
	if api == nil {
		return errors.New("orchestration application interface is required")
	}
	if h.orchestration != nil {
		return errors.New("orchestration API already registered")
	}
	h.orchestration = api
	h.mux.HandleFunc("POST /v2/definitions/validate", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		Draft app.DefinitionDraft `json:"draft"`
	}) (any, error) {
		result, err := api.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: p, Draft: b.Draft})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/definitions", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		Draft            app.DefinitionDraft `json:"draft"`
		ExpectedRevision model.Revision      `json:"expected_revision"`
	}) (any, error) {
		result, err := api.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: b.context(p), Draft: b.Draft, ExpectedRevision: b.ExpectedRevision})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/processes", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		ID    model.WorkRunID `json:"id"`
		Start model.WorkStart `json:"start"`
	}) (any, error) {
		result, err := api.StartProcess(ctx, app.StartProcessRequest{Context: b.context(p), ID: b.ID, Start: b.Start})
		return projectWork(result), err
	}))
	h.mux.HandleFunc("POST /v2/processes/evidence", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		Attempt             model.WorkAttemptRef   `json:"attempt"`
		ExpectedRunRevision model.Revision         `json:"expected_run_revision"`
		Kind                model.WorkEvidenceKind `json:"kind"`
		ArtifactRevision    string                 `json:"artifact_revision"`
		Passed              *bool                  `json:"passed"`
		Disposition         model.WorkOutcome      `json:"disposition"`
		Detail              string                 `json:"detail"`
	}) (any, error) {
		result, err := api.RecordNodeEvidence(ctx, app.RecordNodeEvidenceRequest{Context: b.context(p), Attempt: b.Attempt, ExpectedRunRevision: b.ExpectedRunRevision, Kind: b.Kind, ArtifactRevision: b.ArtifactRevision, Passed: b.Passed, Disposition: b.Disposition, Detail: b.Detail})
		return projectWork(result), err
	}))
	h.mux.HandleFunc("POST /v2/decisions/submit", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		DecisionID             model.DecisionID       `json:"decision_id"`
		ExpectedWindowRevision model.Revision         `json:"expected_window_revision"`
		Answer                 string                 `json:"answer"`
		Reason                 string                 `json:"reason"`
		EvidenceRefs           []model.WorkEvidenceID `json:"evidence_refs"`
	}) (any, error) {
		result, err := api.SubmitDecision(ctx, app.SubmitDecisionRequest{Context: b.context(p), DecisionID: b.DecisionID, ExpectedWindowRevision: b.ExpectedWindowRevision, Answer: b.Answer, Reason: b.Reason, EvidenceRefs: b.EvidenceRefs})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/program-profiles", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		ID               model.ProgramProfileID           `json:"id"`
		RevisionID       model.ProgramProfileRevisionID   `json:"revision_id"`
		Name             string                           `json:"name"`
		ExpectedRevision model.Revision                   `json:"expected_revision"`
		Executable       string                           `json:"executable"`
		ArgumentPrefix   []string                         `json:"argument_prefix"`
		Environment      map[string]string                `json:"environment"`
		WorkingDirectory string                           `json:"working_directory"`
		Sandbox          model.SandboxMode                `json:"sandbox"`
		Timeout          time.Duration                    `json:"timeout"`
		OutputLimitBytes int64                            `json:"output_limit_bytes"`
		EffectAuthority  []model.ProgramEffectRequirement `json:"effect_authority"`
	}) (any, error) {
		result, err := api.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: b.context(p), ID: b.ID, RevisionID: b.RevisionID, Name: b.Name, ExpectedRevision: b.ExpectedRevision, Executable: b.Executable, ArgumentPrefix: b.ArgumentPrefix, Environment: b.Environment, WorkingDirectory: b.WorkingDirectory, Sandbox: b.Sandbox, Timeout: b.Timeout, OutputLimitBytes: b.OutputLimitBytes, EffectAuthority: b.EffectAuthority})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/automation/rules", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		ID               model.AutomationRuleID         `json:"id"`
		RevisionID       model.AutomationRuleRevisionID `json:"revision_id"`
		Name             string                         `json:"name"`
		ExpectedRevision model.Revision                 `json:"expected_revision"`
		Enabled          bool                           `json:"enabled"`
		Owner            model.AuthoritySubject         `json:"owner"`
		Delegation       model.AutomationDelegation     `json:"delegation"`
		Condition        model.AutomationCondition      `json:"condition"`
		Action           model.AutomationAction         `json:"action"`
		Policy           model.OccurrencePolicy         `json:"policy"`
		Dependencies     []model.DefinitionRef          `json:"dependencies"`
	}) (any, error) {
		result, err := api.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: b.context(p), ID: b.ID, RevisionID: b.RevisionID, Name: b.Name, ExpectedRevision: b.ExpectedRevision, Enabled: b.Enabled, Owner: b.Owner, Delegation: b.Delegation, Condition: b.Condition, Action: b.Action, Policy: b.Policy, Dependencies: b.Dependencies})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/automation/run", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		RuleID               model.AutomationRuleID `json:"rule_id"`
		ExpectedRuleRevision model.Revision         `json:"expected_rule_revision"`
		OccurrenceID         model.OccurrenceID     `json:"occurrence_id"`
		SourceOccurrenceKey  string                 `json:"source_occurrence_key"`
		Recipients           []model.AgentID        `json:"recipients"`
	}) (any, error) {
		result, err := api.RunRuleNow(ctx, app.RunRuleNowRequest{Context: b.context(p), RuleID: b.RuleID, ExpectedRuleRevision: b.ExpectedRuleRevision, OccurrenceID: b.OccurrenceID, SourceOccurrenceKey: b.SourceOccurrenceKey, Recipients: b.Recipients})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/teams/deploy", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		DeploymentID  model.DeploymentID      `json:"deployment_id"`
		Instantiation model.TeamInstantiation `json:"instantiation"`
	}) (any, error) {
		result, err := api.DeployTeam(ctx, app.DeployTeamRequest{Context: b.context(p), DeploymentID: b.DeploymentID, Instantiation: b.Instantiation})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/teams/rebrief", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		DeploymentID     model.DeploymentID  `json:"deployment_id"`
		ExpectedRevision model.Revision      `json:"expected_revision"`
		Definition       model.DefinitionRef `json:"definition"`
	}) (any, error) {
		result, err := api.RebriefDeployment(ctx, app.RebriefDeploymentRequest{Context: b.context(p), DeploymentID: b.DeploymentID, ExpectedRevision: b.ExpectedRevision, Definition: b.Definition})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/teams/advance-phase", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		DeploymentID     model.DeploymentID `json:"deployment_id"`
		ExpectedRevision model.Revision     `json:"expected_revision"`
	}) (any, error) {
		result, err := api.AdvanceAdvisoryPhase(ctx, app.AdvanceAdvisoryPhaseRequest{Context: b.context(p), DeploymentID: b.DeploymentID, ExpectedRevision: b.ExpectedRevision})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("POST /v2/teams/stand-down", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		DeploymentID     model.DeploymentID `json:"deployment_id"`
		ExpectedRevision model.Revision     `json:"expected_revision"`
		Reason           string             `json:"reason"`
	}) (any, error) {
		result, err := api.StandDownDeployment(ctx, app.StandDownDeploymentRequest{Context: b.context(p), DeploymentID: b.DeploymentID, ExpectedRevision: b.ExpectedRevision, Reason: b.Reason})
		return projectOrchestration(result), err
	}))
	h.mux.HandleFunc("GET /v2/definitions/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.GetDefinition(r.Context(), app.GetDefinitionRequest{Principal: p, DefinitionID: model.DefinitionID(r.PathValue("id"))})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/definitions", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.ListDefinitions(r.Context(), app.ListDefinitionsRequest{Principal: p, Kind: model.DefinitionKind(r.URL.Query().Get("kind")), IncludeTombstoned: r.URL.Query().Get("include_tombstoned") == "true"})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/program-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.GetProgramProfile(r.Context(), app.GetProgramProfileRequest{Principal: p, ID: model.ProgramProfileID(r.PathValue("id"))})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/program-profiles", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.ListProgramProfiles(r.Context(), app.ListProgramProfilesRequest{Principal: p, IncludeTombstoned: r.URL.Query().Get("include_tombstoned") == "true"})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/decisions/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.GetDecision(r.Context(), app.GetDecisionRequest{Principal: p, DecisionID: model.DecisionID(r.PathValue("id"))})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/decisions", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.ListPendingDecisions(r.Context(), app.ListPendingDecisionsRequest{Principal: p})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/automation/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.GetAutomationRule(r.Context(), app.GetAutomationRuleRequest{Principal: p, ID: model.AutomationRuleID(r.PathValue("id"))})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/automation/rules", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.ListAutomationRules(r.Context(), app.ListAutomationRulesRequest{Principal: p, IncludeTombstoned: r.URL.Query().Get("include_tombstoned") == "true"})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/automation/occurrences", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.ListOccurrences(r.Context(), app.ListOccurrencesRequest{Principal: p, RuleID: model.AutomationRuleID(r.URL.Query().Get("rule_id"))})
		journeyResult(w, projectOrchestration(result), err)
	})
	h.mux.HandleFunc("GET /v2/teams/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.GetTeamDeployment(r.Context(), app.GetTeamDeploymentRequest{Principal: p, DeploymentID: model.DeploymentID(r.PathValue("id"))})
		journeyResult(w, projectOrchestration(result), err)
	})
	return nil
}
