package transport

import (
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerSandboxDefaults(api app.SandboxDefaultsAPI) {
	h.mux.HandleFunc("GET /v2/sandbox-defaults", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.GetSandboxDefaults(r.Context(), principal)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("POST /v2/sandbox-defaults", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			Global           *model.SandboxProfileID                   `json:"global"`
			Groups           *map[model.GroupID]model.SandboxProfileID `json:"groups"`
			ExpectedRevision model.Revision                            `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if body.Global == nil || body.Groups == nil {
			applicationError(w, app.ErrInvalid)
			return
		}
		result, err := api.SaveSandboxDefaults(r.Context(), app.SaveSandboxDefaultsRequest{Context: body.context(principal), Global: *body.Global, Groups: *body.Groups, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
