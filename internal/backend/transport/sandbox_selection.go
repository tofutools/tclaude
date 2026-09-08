package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) registerSandboxSelection(api app.SandboxSelectionAPI) {
	h.mux.HandleFunc("POST /v2/sandbox-profiles/selection", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Scopes []model.SandboxScopeSelection `json:"scopes"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.ResolveLaunchSandbox(r.Context(), principal, body.Scopes)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
