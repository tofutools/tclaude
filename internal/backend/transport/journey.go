package transport

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) RegisterJourneyAPI(api app.JourneyAPI) error {
	if api == nil {
		return errors.New("journey application interface is required")
	}
	if h.journey != nil {
		return errors.New("journey API already registered")
	}
	h.journey = api
	h.mux.HandleFunc("POST /v2/history/refresh", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		Harness string `json:"harness"`
		Source  string `json:"source"`
	}) (any, error) {
		return api.RefreshHistory(ctx, app.RefreshHistoryRequest{Principal: p, Harness: b.Harness, SourceName: b.Source})
	}))
	h.mux.HandleFunc("POST /v2/history/search", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		Harness     string            `json:"harness"`
		WorkspaceID model.WorkspaceID `json:"workspace_id"`
		Query       string            `json:"query"`
		Archived    *bool             `json:"archived,omitempty"`
	}) (any, error) {
		return api.SearchHistory(ctx, app.SearchHistoryRequest{Principal: p, Harness: b.Harness, WorkspaceID: b.WorkspaceID, Query: b.Query, Archived: b.Archived})
	}))
	h.mux.HandleFunc("POST /v2/history/read", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		Selection model.HistorySelection `json:"selection"`
	}) (any, error) {
		return api.ReadHistory(ctx, app.ReadHistoryRequest{Principal: p, Selection: b.Selection})
	}))
	h.mux.HandleFunc("POST /v2/workspaces/register", journeyJSON(h, func(ctx context.Context, p model.Principal, b workspaceBody) (any, error) {
		return api.RegisterWorkspace(ctx, app.RegisterWorkspaceRequest{Context: b.context(p), ID: b.ID, Intent: b.Intent})
	}))
	h.mux.HandleFunc("POST /v2/workspaces/create", journeyJSON(h, func(ctx context.Context, p model.Principal, b workspaceBody) (any, error) {
		return api.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: b.context(p), ID: b.ID, Intent: b.Intent})
	}))
	h.mux.HandleFunc("GET /v2/workspaces/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.InspectWorkspace(r.Context(), app.InspectWorkspaceRequest{Principal: p, WorkspaceID: model.WorkspaceID(r.PathValue("id"))})
		journeyResult(w, result, err)
	})
	h.mux.HandleFunc("POST /v2/workspaces/remove", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		WorkspaceID      model.WorkspaceID `json:"workspace_id"`
		ExpectedRevision model.Revision    `json:"expected_revision"`
		Destructive      bool              `json:"destructive"`
	}) (any, error) {
		return api.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: b.context(p), WorkspaceID: b.WorkspaceID, ExpectedRevision: b.ExpectedRevision, Destructive: b.Destructive})
	}))
	h.mux.HandleFunc("POST /v2/shells", operationHandler(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		WorkspaceID      model.WorkspaceID `json:"workspace_id"`
		ExpectedRevision model.Revision    `json:"expected_revision"`
		Sandbox          model.SandboxMode `json:"sandbox"`
	}) (app.OperationResult, error) {
		return api.StartShell(ctx, app.StartShellRequest{Context: b.context(p), WorkspaceID: b.WorkspaceID, ExpectedRevision: b.ExpectedRevision, Sandbox: b.Sandbox})
	}))
	h.mux.HandleFunc("POST /v2/work", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		ID   model.WorkRunID   `json:"id"`
		Spec model.WorkRunSpec `json:"spec"`
	}) (any, error) {
		result, err := api.StartWork(ctx, app.StartWorkRequest{Context: b.context(p), ID: b.ID, Spec: b.Spec})
		return projectWork(result), err
	}))
	h.mux.HandleFunc("GET /v2/work/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.InspectWork(r.Context(), app.InspectWorkRequest{Principal: p, WorkRunID: model.WorkRunID(r.PathValue("id"))})
		journeyResult(w, projectWork(result), err)
	})
	h.mux.HandleFunc("POST /v2/work/evidence", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		WorkRunID        model.WorkRunID        `json:"work_run_id"`
		ExpectedRevision model.Revision         `json:"expected_revision"`
		Step             model.WorkStep         `json:"step"`
		Attempt          uint64                 `json:"attempt"`
		Kind             model.WorkEvidenceKind `json:"kind"`
		ArtifactRevision string                 `json:"artifact_revision"`
		Passed           *bool                  `json:"passed,omitempty"`
		Detail           string                 `json:"detail"`
	}) (any, error) {
		result, err := api.RecordWorkEvidence(ctx, app.RecordWorkEvidenceRequest{Context: b.context(p), ExpectedRunRevision: b.ExpectedRevision, WorkRunID: b.WorkRunID, Step: b.Step, Attempt: b.Attempt, Kind: b.Kind, ArtifactRevision: b.ArtifactRevision, Passed: b.Passed, Detail: b.Detail})
		return projectWork(result), err
	}))
	h.mux.HandleFunc("POST /v2/work/decision", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		WorkRunID        model.WorkRunID        `json:"work_run_id"`
		ExpectedRevision model.Revision         `json:"expected_revision"`
		Step             model.WorkStep         `json:"step"`
		Attempt          uint64                 `json:"attempt"`
		Decision         model.WorkDecisionKind `json:"decision"`
		Reason           string                 `json:"reason"`
	}) (any, error) {
		result, err := api.DecideWork(ctx, app.DecideWorkRequest{Context: b.context(p), ExpectedRunRevision: b.ExpectedRevision, WorkRunID: b.WorkRunID, Step: b.Step, Attempt: b.Attempt, Decision: b.Decision, Reason: b.Reason})
		return projectWork(result), err
	}))
	h.mux.HandleFunc("POST /v2/work/cancel", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		WorkRunID        model.WorkRunID `json:"work_run_id"`
		ExpectedRevision model.Revision  `json:"expected_revision"`
		Reason           string          `json:"reason"`
	}) (any, error) {
		result, err := api.CancelWork(ctx, app.CancelWorkRequest{Context: b.context(p), WorkRunID: b.WorkRunID, ExpectedRunRevision: b.ExpectedRevision, Reason: b.Reason})
		return projectWork(result), err
	}))
	h.mux.HandleFunc("POST /v2/history/metadata", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		ConversationID   model.ConversationID `json:"conversation_id"`
		ExpectedRevision model.Revision       `json:"expected_revision"`
		Title            string               `json:"title"`
		Archived         bool                 `json:"archived"`
	}) (any, error) {
		return api.SetConversationMetadata(ctx, app.SetConversationMetadataRequest{Context: b.context(p), ConversationID: b.ConversationID, ExpectedRevision: b.ExpectedRevision, Title: b.Title, Archived: b.Archived})
	}))
	h.mux.HandleFunc("POST /v2/workspaces/restore", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		WorkspaceID      model.WorkspaceID `json:"workspace_id"`
		ExpectedRevision model.Revision    `json:"expected_revision"`
	}) (any, error) {
		return api.RestoreCheckout(ctx, app.RestoreCheckoutRequest{Context: b.context(p), WorkspaceID: b.WorkspaceID, ExpectedRevision: b.ExpectedRevision})
	}))
	h.mux.HandleFunc("POST /v2/work/resolve", journeyJSON(h, func(ctx context.Context, p model.Principal, b struct {
		commandIdentity
		WorkRunID        model.WorkRunID `json:"work_run_id"`
		ExpectedRevision model.Revision  `json:"expected_revision"`
		Reason           string          `json:"reason"`
	}) (any, error) {
		result, err := api.ResolveWorkUncertainty(ctx, app.ResolveWorkUncertaintyRequest{Context: b.context(p), WorkRunID: b.WorkRunID, ExpectedRunRevision: b.ExpectedRevision, Reason: b.Reason})
		return projectWork(result), err
	}))
	return nil
}

type workspaceBody struct {
	commandIdentity
	ID     model.WorkspaceID     `json:"id"`
	Intent model.WorkspaceIntent `json:"intent"`
}

func journeyJSON[B any](h *Handler, call func(context.Context, model.Principal, B) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body B
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := call(r.Context(), p, body)
		journeyResult(w, result, err)
	}
}

func journeyResult(w http.ResponseWriter, result any, err error) {
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Public attribution identifies the actor, not its private authentication
// generation or the durable internal delegation used for later effects.
type workActor struct {
	Kind          model.PrincipalKind `json:"kind"`
	AgentID       model.AgentID       `json:"agent_id,omitempty"`
	ExecutionID   model.ExecutionID   `json:"execution_id,omitempty"`
	AutomationRun string              `json:"automation_run,omitempty"`
}

func projectWorkActor(p model.Principal) workActor {
	return workActor{p.Kind, p.AgentID, p.ExecutionID, p.AutomationRun}
}

type workAttemptView struct {
	Step        model.WorkStep         `json:"step"`
	Attempt     uint64                 `json:"attempt"`
	OperationID model.OperationID      `json:"operation_id"`
	State       model.WorkAttemptState `json:"state"`
	StartedAt   time.Time              `json:"started_at"`
	SettledAt   *time.Time             `json:"settled_at,omitempty"`
}
type workRunView struct {
	ID                model.WorkRunID      `json:"id"`
	RequestID         model.RequestID      `json:"request_id"`
	Requester         workActor            `json:"requester"`
	Spec              model.WorkRunSpec    `json:"spec"`
	WorkspaceUseID    model.WorkspaceUseID `json:"workspace_use_id,omitempty"`
	WorkerExecutionID model.ExecutionID    `json:"worker_execution_id,omitempty"`
	State             model.WorkRunState   `json:"state"`
	Attempts          []workAttemptView    `json:"attempts"`
	Revision          model.Revision       `json:"revision"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
}
type workEvidenceView struct {
	ID               model.WorkEvidenceID   `json:"id"`
	WorkRunID        model.WorkRunID        `json:"work_run_id"`
	Step             model.WorkStep         `json:"step"`
	Attempt          uint64                 `json:"attempt"`
	Kind             model.WorkEvidenceKind `json:"kind"`
	Reporter         workActor              `json:"reporter"`
	ArtifactRevision string                 `json:"artifact_revision"`
	Passed           *bool                  `json:"passed,omitempty"`
	Detail           string                 `json:"detail"`
	RecordedAt       time.Time              `json:"recorded_at"`
	Revision         model.Revision         `json:"revision"`
}
type workDecisionView struct {
	WorkRunID model.WorkRunID        `json:"work_run_id"`
	Step      model.WorkStep         `json:"step"`
	Attempt   uint64                 `json:"attempt"`
	Decision  model.WorkDecisionKind `json:"decision"`
	Decider   workActor              `json:"decider"`
	Reason    string                 `json:"reason"`
	DecidedAt time.Time              `json:"decided_at"`
	Revision  model.Revision         `json:"revision"`
}
type workResultView struct {
	Run      workRunView        `json:"run"`
	Evidence []workEvidenceView `json:"evidence"`
	Decision *workDecisionView  `json:"decision,omitempty"`
}

func projectWork(result app.WorkRunResult) workResultView {
	run := result.Run
	attempts := make([]workAttemptView, 0, len(run.Attempts))
	for _, a := range run.Attempts {
		attempts = append(attempts, workAttemptView{a.Step, a.Attempt, a.OperationID, a.State, a.StartedAt, a.SettledAt})
	}
	evidence := make([]workEvidenceView, 0, len(result.Evidence))
	for _, e := range result.Evidence {
		evidence = append(evidence, workEvidenceView{e.ID, e.WorkRunID, e.Step, e.Attempt, e.Kind, projectWorkActor(e.Reporter), e.ArtifactRevision, e.Passed, e.Detail, e.RecordedAt, e.Revision})
	}
	view := workResultView{Run: workRunView{run.ID, run.RequestID, projectWorkActor(run.Requester), run.Spec, run.WorkspaceUseID, run.WorkerExecutionID, run.State, attempts, run.Revision, run.CreatedAt, run.UpdatedAt}, Evidence: evidence}
	if d := result.Decision; d != nil {
		view.Decision = &workDecisionView{d.WorkRunID, d.Step, d.Attempt, d.Decision, projectWorkActor(d.Decider), d.Reason, d.DecidedAt, d.Revision}
	}
	return view
}
