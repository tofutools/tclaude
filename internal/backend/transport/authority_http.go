package transport

import (
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerAuthorityAdmin() {
	h.mux.HandleFunc("PUT /v2/authority/denials/{id}", h.putDenial)
	h.mux.HandleFunc("DELETE /v2/authority/denials/{id}", h.deleteDenial)
	h.mux.HandleFunc("PUT /v2/authority/roles/{id}", h.putRole)
	h.mux.HandleFunc("PUT /v2/authority/roles/{id}/assignments", h.putAssignment)
	h.mux.HandleFunc("DELETE /v2/authority/roles/{id}/assignments", h.deleteAssignment)
	h.mux.HandleFunc("PUT /v2/groups/{id}/owner", h.setGroupOwner)
	h.mux.HandleFunc("GET /v2/executions/{id}/access", h.accessStatus)
	h.mux.HandleFunc("POST /v2/executions/{id}/access/revoke", h.revokeAccess)
}
func (h *Handler) putRole(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Name             string         `json:"name"`
		Description      string         `json:"description"`
		Brief            string         `json:"brief"`
		Actions          []model.Action `json:"actions"`
		ExpectedRevision model.Revision `json:"expected_revision"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.authority.PutRole(r.Context(), app.PutRoleRequest{Principal: p, ExpectedRevision: body.ExpectedRevision, Role: model.Role{ID: model.RoleID(r.PathValue("id")), Name: body.Name, Description: body.Description, Brief: body.Brief, Actions: body.Actions}})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Role)
}

type assignmentBody struct {
	Subject          model.AuthoritySubject    `json:"subject"`
	Resource         model.ResourceSelector    `json:"resource"`
	Bounds           model.ConfigurationBounds `json:"bounds"`
	ExpectedRevision model.Revision            `json:"expected_revision"`
}

func (b assignmentBody) assignment(id string) model.RoleAssignment {
	return model.RoleAssignment{RoleID: model.RoleID(id), Subject: b.Subject, Resource: b.Resource, Bounds: b.Bounds}
}
func (h *Handler) putAssignment(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body assignmentBody
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.authority.PutRoleAssignment(r.Context(), app.PutRoleAssignmentRequest{Principal: p, Assignment: body.assignment(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Assignment)
}
func (h *Handler) deleteAssignment(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body assignmentBody
	if !decodeRequest(w, r, &body) {
		return
	}
	if err := h.authority.DeleteRoleAssignment(r.Context(), app.DeleteRoleAssignmentRequest{Principal: p, Assignment: body.assignment(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision}); err != nil {
		applicationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handler) setGroupOwner(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		OwnerAgentID     model.AgentID             `json:"owner_agent_id"`
		OwnerAgentIDs    []model.AgentID           `json:"owner_agent_ids"`
		ExpectedRevision model.Revision            `json:"expected_revision"`
		Bounds           model.ConfigurationBounds `json:"bounds"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.authority.SetGroupOwner(r.Context(), app.SetGroupOwnerRequest{Principal: p, GroupID: model.GroupID(r.PathValue("id")), OwnerAgentID: body.OwnerAgentID, OwnerAgentIDs: body.OwnerAgentIDs, ExpectedGroupRevision: body.ExpectedRevision, Bounds: body.Bounds})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Group)
}

type accessView struct {
	ExecutionID model.ExecutionID          `json:"execution_id"`
	State       model.ExecutionAccessState `json:"state"`
	Revision    model.Revision             `json:"revision"`
	ExpiresAt   time.Time                  `json:"expires_at"`
}

func projectAccess(access model.ExecutionAccessBinding) accessView {
	return accessView{access.ExecutionID, access.State, access.Revision, access.ExpiresAt}
}
func (h *Handler) accessStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	result, err := h.authority.ExecutionAccessStatus(r.Context(), app.ExecutionAccessStatusRequest{Principal: p, ExecutionID: model.ExecutionID(r.PathValue("id"))})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectAccess(result.Access))
}
func (h *Handler) revokeAccess(w http.ResponseWriter, r *http.Request) {
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
	result, err := h.authority.RevokeExecutionAccess(r.Context(), app.RevokeExecutionAccessRequest{Principal: p, ExecutionID: model.ExecutionID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectAccess(result.Access))
}
