package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) registerGroupClone(api app.GroupCloneAPI) {
	h.mux.HandleFunc("POST /v2/groups/{id}/clone", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			RequestID               model.RequestID                  `json:"request_id"`
			ID                      model.GroupID                    `json:"id"`
			Name                    string                           `json:"name"`
			ExpectedGroupRevision   model.Revision                   `json:"expected_group_revision"`
			ExpectedDefaultRevision model.Revision                   `json:"expected_default_revision"`
			ExpectedMembers         map[model.AgentID]model.Revision `json:"expected_members"`
			CopyMembers             bool                             `json:"copy_members"`
			CopyDefault             bool                             `json:"copy_default"`
			MaxActiveMembers        int64                            `json:"max_active_members"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		out, err := api.CloneGroup(r.Context(), app.CloneGroupRequest{Context: app.RequestContext{Principal: p, RequestID: body.RequestID}, SourceID: model.GroupID(r.PathValue("id")), ID: body.ID, Name: body.Name, ExpectedGroupRevision: body.ExpectedGroupRevision, ExpectedDefaultRevision: body.ExpectedDefaultRevision, ExpectedMembers: body.ExpectedMembers, CopyMembers: body.CopyMembers, CopyDefault: body.CopyDefault, MaxActiveMembers: body.MaxActiveMembers})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}
