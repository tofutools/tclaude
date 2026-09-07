package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) registerGroupHierarchy(api app.GroupHierarchyAPI) {
	h.mux.HandleFunc("PUT /v2/groups/{id}/parent", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			RequestID        model.RequestID `json:"request_id"`
			ParentGroupID    model.GroupID   `json:"parent_group_id"`
			ExpectedRevision model.Revision  `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		out, err := api.SetGroupParent(r.Context(), app.SetGroupParentRequest{Context: app.RequestContext{Principal: p, RequestID: body.RequestID}, ID: model.GroupID(r.PathValue("id")), ParentGroupID: body.ParentGroupID, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}
