package transport

import (
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) registerGroupDisband(api app.GroupDisbandAPI) {
	h.mux.HandleFunc("POST /v2/groups/{id}/disband", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			RequestID        model.RequestID `json:"request_id"`
			ExpectedRevision model.Revision  `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		out, err := api.DisbandGroup(r.Context(), app.DisbandGroupRequest{Context: app.RequestContext{Principal: p, RequestID: body.RequestID}, ID: model.GroupID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			var blocked *app.GroupDisbandBlockedError
			if errors.As(err, &blocked) {
				writeJSON(w, http.StatusConflict, map[string]any{"code": "group_busy", "message": blocked.Error(), "blocker": blocked})
				return
			}
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}
