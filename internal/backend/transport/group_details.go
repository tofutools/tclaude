package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) registerGroupDetails(api app.GroupDetailsAPI) {
	h.mux.HandleFunc("PUT /v2/groups/{id}/details", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Details          model.GroupDetails `json:"details"`
			ExpectedRevision model.Revision     `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.SetGroupDetails(r.Context(), app.SetGroupDetailsRequest{Principal: p, ID: model.GroupID(r.PathValue("id")), Details: body.Details, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
