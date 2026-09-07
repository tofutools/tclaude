package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const maxRequestBytes = 1 << 20

type Handler struct {
	application    app.API
	agents         app.AgentAPI
	authority      app.AuthorityAdminAPI
	journey        app.JourneyAPI
	orchestration  app.OrchestrationAPI
	accessRequests app.AccessRequestAPI
	auth           Authenticator
	mux            *http.ServeMux
}

func NewHandler(application app.API, auth Authenticator) (*Handler, error) {
	if application == nil || auth == nil {
		return nil, errors.New("application and authenticator are required")
	}
	h := &Handler{application: application, auth: auth, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /v2/presentation", h.presentation)
	h.mux.HandleFunc("PUT /v2/presentation", h.presentation)
	h.mux.HandleFunc("POST /v2/agents", h.createAgent)
	h.mux.HandleFunc("POST /v2/groups", h.createGroup)
	h.mux.HandleFunc("PUT /v2/groups/{id}", h.updateGroup)
	h.mux.HandleFunc("GET /v2/snapshot", h.snapshot)
	h.mux.HandleFunc("GET /v2/attach", h.attach)
	h.mux.HandleFunc("POST /v2/observe", h.observe)
	if browser, ok := application.(app.DirectoryAPI); ok {
		h.registerDirectoryBrowser(browser)
	}
	h.registerCommands()
	h.registerCollaboration()
	if usage, ok := application.(usageAPI); ok {
		h.registerUsage(usage)
	}
	if catalog, ok := application.(app.ConfigurationCatalogAPI); ok {
		h.registerConfigurationCatalog(catalog)
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) caller(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p, err := h.auth.Authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return model.Principal{}, false
	}
	return p, true
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeBoundedRequest(w, r, target, maxRequestBytes)
}

func decodeBoundedRequest(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Code string `json:"code"`
	}{code})
}

// Application errors require a stable public code before transport exposes
// details. Native error text is never returned merely because it is an error.
func applicationError(w http.ResponseWriter, err error) {
	switch code := app.Code(err); code {
	case "forbidden":
		writeError(w, http.StatusForbidden, code)
	case "not_found":
		writeError(w, http.StatusNotFound, code)
	case "conflict", "uncertain":
		writeError(w, http.StatusConflict, code)
	case "invalid_request", "unsupported":
		writeError(w, http.StatusUnprocessableEntity, code)
	case "unavailable":
		writeError(w, http.StatusServiceUnavailable, code)
	default:
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req struct {
		TaskReference      string                             `json:"task_reference"`
		ParentAgentID      model.AgentID                      `json:"parent_agent_id"`
		CloneSourceAgentID model.AgentID                      `json:"clone_source_agent_id"`
		Notifications      model.AgentNotificationPreferences `json:"notifications"`

		ConfigurationProfile *model.ConfigurationProfileRef `json:"configuration_profile"`
		ConfigurationDefault string                         `json:"configuration_default"`
		ID                   model.AgentID                  `json:"id"`
		Name                 string                         `json:"name"`
		Desired              model.DesiredConfiguration     `json:"desired"`
	}
	if !decodeRequest(w, r, &req) {
		return
	}
	result, err := h.application.CreateAgent(r.Context(), app.CreateAgentRequest{Context: p, ID: req.ID, Name: req.Name, Desired: req.Desired, ConfigurationProfile: req.ConfigurationProfile, ConfigurationDefault: req.ConfigurationDefault, TaskReference: req.TaskReference, ParentAgentID: req.ParentAgentID, CloneSourceAgentID: req.CloneSourceAgentID, Notifications: req.Notifications})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, projectAgent(result.Agent))
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req struct {
		ID      model.GroupID   `json:"id"`
		Name    string          `json:"name"`
		Members []model.AgentID `json:"members"`
	}
	if !decodeRequest(w, r, &req) {
		return
	}
	result, err := h.application.CreateGroup(r.Context(), app.CreateGroupRequest{Context: p, ID: req.ID, Name: req.Name, Members: req.Members})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result.Group)
}

type executionView struct {
	Workload         model.ExecutionWorkloadKind `json:"workload"`
	ID               model.ExecutionID           `json:"id"`
	AgentID          model.AgentID               `json:"agent_id,omitempty"`
	ConversationID   model.ConversationID        `json:"conversation_id,omitempty"`
	Spec             model.ResolvedExecutionSpec `json:"spec"`
	State            model.ExecutionState        `json:"state"`
	ContextReadiness model.ContextReadiness      `json:"context_readiness"`
	Revision         model.Revision              `json:"revision"`
	CreatedAt        time.Time                   `json:"created_at"`
	UpdatedAt        time.Time                   `json:"updated_at"`
}

func projectExecution(e model.Execution) executionView {
	return executionView{Workload: e.Workload, ID: e.ID, AgentID: e.AgentID, ConversationID: e.ConversationID,
		Spec: e.Spec, State: e.State, ContextReadiness: e.ContextReadiness, Revision: e.Revision, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
}

func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	s, err := h.application.Snapshot(r.Context(), app.SnapshotRequest{Principal: p})
	if err != nil {
		applicationError(w, err)
		return
	}
	views := make([]executionView, 0, len(s.Executions))
	for _, e := range s.Executions {
		views = append(views, projectExecution(e))
	}
	operations := make([]operationView, 0, len(s.Operations))
	for _, operation := range s.Operations {
		operations = append(operations, projectOperation(operation))
	}
	workRuns := make([]workResultView, 0, len(s.WorkRuns))
	for _, run := range s.WorkRuns {
		result := app.WorkRunResult{Run: run}
		for _, evidence := range s.WorkEvidence {
			if evidence.WorkRunID == run.ID {
				result.Evidence = append(result.Evidence, evidence)
			}
		}
		workRuns = append(workRuns, projectWork(result))
	}
	writeJSON(w, http.StatusOK, struct {
		Conversations []model.Conversation            `json:"conversations"`
		Associations  []model.ConversationAssociation `json:"associations"`
		Revision      model.Revision                  `json:"revision"`
		Agents        []agentView                     `json:"agents"`
		Groups        []model.Group                   `json:"groups"`
		Executions    []executionView                 `json:"executions"`
		Operations    []operationView                 `json:"operations"`
		Messages      []messageView                   `json:"messages"`
		Workspaces    []app.WorkspaceView             `json:"workspaces"`
		WorkspaceUses []model.WorkspaceUse            `json:"workspace_uses"`
		WorkRuns      []workResultView                `json:"work_runs"`
	}{s.Conversations, s.Associations, s.Revision, projectAgents(s.Agents), s.Groups, views, operations, projectMessages(s.Messages), s.Workspaces, s.WorkspaceUses, workRuns})
}
