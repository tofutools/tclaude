package transport

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// RegisterAgentAPI composes focused application interfaces before serving any
// requests. All identity and authorization decisions remain application-owned.
func (h *Handler) RegisterAgentAPI(agents app.AgentAPI, authority app.AuthorityAdminAPI) error {
	if agents == nil || authority == nil {
		return errors.New("agent and authority application interfaces are required")
	}
	if h.agents != nil {
		return errors.New("agent API already registered")
	}
	h.agents, h.authority = agents, authority
	h.mux.HandleFunc("GET /v2/identity", h.identity)
	h.mux.HandleFunc("GET /v2/inbox", h.inbox)
	h.mux.HandleFunc("POST /v2/inbox/{id}/read", h.readOwnMessage)
	h.mux.HandleFunc("POST /v2/status", h.scopedStatus)
	h.mux.HandleFunc("POST /v2/authority/explain", h.explainAuthority)
	h.mux.HandleFunc("GET /v2/authority", h.listAuthority)
	h.mux.HandleFunc("PUT /v2/authority/grants/{id}", h.putGrant)
	h.mux.HandleFunc("DELETE /v2/authority/grants/{id}", h.deleteGrant)
	h.registerAuthorityAdmin()
	return nil
}

func (h *Handler) identity(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	result, err := h.agents.WhoAmI(r.Context(), app.WhoAmIRequest{Principal: p})
	if err != nil {
		applicationError(w, err)
		return
	}
	var agent *agentView
	if result.Agent != nil {
		view := projectAgent(*result.Agent)
		agent = &view
	}
	writeJSON(w, http.StatusOK, struct {
		Kind         model.PrincipalKind            `json:"kind"`
		Agent        *agentView                     `json:"agent,omitempty"`
		Execution    executionView                  `json:"execution"`
		Conversation *model.ConversationAssociation `json:"conversation,omitempty"`
		Context      model.ContextReadiness         `json:"context"`
		Actions      []model.Action                 `json:"actions"`
		EvaluatedAt  time.Time                      `json:"evaluated_at"`
	}{result.Principal.Kind, agent, projectExecution(result.Execution), result.CurrentConversation, result.ContextReadiness, result.EffectiveActions, result.EvaluatedAt})
}

func (h *Handler) inbox(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	unread := false
	if len(q) > 0 {
		if len(q) != 1 || len(q["unread_only"]) != 1 {
			writeError(w, 400, "invalid_request")
			return
		}
		var err error
		unread, err = strconv.ParseBool(q.Get("unread_only"))
		if err != nil {
			writeError(w, 400, "invalid_request")
			return
		}
	}
	result, err := h.agents.ReadInbox(r.Context(), app.ReadInboxRequest{Principal: p, UnreadOnly: unread})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Messages []messageView `json:"messages"`
	}{projectMessages(result.Messages)})
}

func (h *Handler) readOwnMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body commandIdentity
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.application.MarkMessageRead(r.Context(), app.MarkMessageReadRequest{RequestContext: body.context(p), MessageID: model.MessageID(r.PathValue("id")), AgentID: p.AgentID})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectMessage(result.Message))
}

func (h *Handler) scopedStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Target model.ResourceSelector `json:"target"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.agents.ReadStatus(r.Context(), app.ReadStatusRequest{Principal: p, Target: body.Target})
	if err != nil {
		applicationError(w, err)
		return
	}
	executions := make([]executionView, 0, len(result.Executions))
	for _, execution := range result.Executions {
		executions = append(executions, projectExecution(execution))
	}
	writeJSON(w, http.StatusOK, struct {
		Agents       []agentView                     `json:"agents"`
		Executions   []executionView                 `json:"executions"`
		Associations []model.ConversationAssociation `json:"associations"`
	}{projectAgents(result.Agents), executions, result.Associations})
}

func (h *Handler) explainAuthority(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Action                 model.Action                `json:"action"`
		Resource               model.ResourceSelector      `json:"resource"`
		RequestedConfiguration *model.DesiredConfiguration `json:"requested_configuration,omitempty"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.agents.ExplainAuthority(r.Context(), app.AuthorityExplanationRequest{Principal: p, Action: body.Action, Resource: body.Resource, RequestedConfiguration: body.RequestedConfiguration})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Decision)
}

func (h *Handler) listAuthority(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	result, err := h.authority.ListAuthority(r.Context(), app.ListAuthorityRequest{Principal: p})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) putGrant(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Subject          model.AuthoritySubject    `json:"subject"`
		Action           model.Action              `json:"action"`
		Resource         model.ResourceSelector    `json:"resource"`
		Bounds           model.ConfigurationBounds `json:"bounds"`
		ExpiresAt        *time.Time                `json:"expires_at"`
		ExpectedRevision model.Revision            `json:"expected_revision"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.authority.PutGrant(r.Context(), app.PutGrantRequest{Principal: p, ExpectedRevision: body.ExpectedRevision, Grant: model.AuthorityGrant{ID: model.GrantID(r.PathValue("id")), Subject: body.Subject, Action: body.Action, Resource: body.Resource, Bounds: body.Bounds, ExpiresAt: body.ExpiresAt}})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Grant)
}

func (h *Handler) deleteGrant(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision model.Revision `json:"expected_revision"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	if err := h.authority.DeleteGrant(r.Context(), app.DeleteGrantRequest{Principal: p, GrantID: model.GrantID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision}); err != nil {
		applicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
