package transport

import (
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerSandboxPreview(api app.SandboxPreviewAPI) {
	h.mux.HandleFunc("POST /v2/sandbox-profiles/preview", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Policy model.SandboxPolicy `json:"policy"`
		}
		if !decodeBoundedRequest(w, r, &body, 5<<20) {
			return
		}
		result, err := api.PreviewSandboxPolicy(r.Context(), principal, body.Policy)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
