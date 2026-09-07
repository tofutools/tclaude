package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) registerGroupCapacity(api app.GroupCapacityAPI) {
	h.mux.HandleFunc("PUT /v2/groups/{id}/capacity", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			MaxActiveMembers int64          `json:"max_active_members"`
			ExpectedRevision model.Revision `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.SetGroupCapacity(r.Context(), app.SetGroupCapacityRequest{Principal: p, ID: model.GroupID(r.PathValue("id")), MaxActiveMembers: body.MaxActiveMembers, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
