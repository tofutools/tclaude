package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"net/http"
)

func (h *Handler) registerConfigurationTransfer(api app.ConfigurationTransferAPI) {
	h.mux.HandleFunc("POST /v2/configuration-transfer/inspect", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var bundle app.ConfigurationBundle
		if !decodeRequest(w, r, &bundle) {
			return
		}
		result, err := api.InspectConfigurationBundle(r.Context(), p, bundle)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("POST /v2/configuration-transfer/import", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			Bundle     app.ConfigurationBundle            `json:"bundle"`
			Selections []app.ConfigurationImportSelection `json:"selections"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.ImportConfigurations(r.Context(), app.ImportConfigurationsRequest{Context: body.context(p), Bundle: body.Bundle, Selections: body.Selections})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
