package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Name             string          `json:"name"`
		Members          []model.AgentID `json:"members"`
		ExpectedRevision model.Revision  `json:"expected_revision"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.application.UpdateGroup(r.Context(), app.UpdateGroupRequest{Context: p, ID: model.GroupID(r.PathValue("id")), Name: body.Name, Members: body.Members, ExpectedRevision: body.ExpectedRevision})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Group)
}
