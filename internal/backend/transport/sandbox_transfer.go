package transport

import (
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func (h *Handler) registerSandboxTransfer(api app.SandboxTransferAPI) {
	h.mux.HandleFunc("POST /v2/sandbox-profiles/export", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Ref model.SandboxProfileRef `json:"ref"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.ExportSandboxBundle(r.Context(), principal, body.Ref)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("POST /v2/sandbox-profiles/import/inspect", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var bundle sandboxpolicy.Bundle
		if !decodeBoundedRequest(w, r, &bundle, 18<<20) {
			return
		}
		result, err := api.InspectSandboxBundle(r.Context(), principal, bundle)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("POST /v2/sandbox-profiles/import", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			Bundle     sandboxpolicy.Bundle         `json:"bundle"`
			Selections []app.SandboxImportSelection `json:"selections"`
		}
		if !decodeBoundedRequest(w, r, &body, 18<<20) {
			return
		}
		result, err := api.ImportSandboxProfiles(r.Context(), app.ImportSandboxProfilesRequest{Context: body.context(principal), Bundle: body.Bundle, Selections: body.Selections})
		if err != nil {
			applicationError(w, err)
			return
		}
		profiles := make([]any, 0, len(result.Profiles))
		for _, entry := range result.Profiles {
			profiles = append(profiles, projectSandboxProfile(entry))
		}
		writeJSON(w, http.StatusOK, struct {
			Root     model.SandboxProfileRef
			Profiles []any
			Repeated bool
		}{result.Root, profiles, result.Repeated})
	})
}
