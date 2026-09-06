package transport

import (
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerConfigurationCatalog(catalog app.ConfigurationCatalogAPI) {
	h.mux.HandleFunc("POST /v2/configuration-profiles", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			ID               model.ConfigurationProfileID         `json:"id"`
			RevisionID       model.ConfigurationProfileRevisionID `json:"revision_id"`
			ExpectedRevision model.Revision                       `json:"expected_revision"`
			Name             string                               `json:"name"`
			Desired          model.DesiredConfiguration           `json:"desired"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := catalog.SaveConfigurationProfile(r.Context(), app.SaveConfigurationProfileRequest{Context: body.context(principal), ID: body.ID, RevisionID: body.RevisionID, ExpectedRevision: body.ExpectedRevision, Name: body.Name, Desired: body.Desired})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("GET /v2/configuration-profiles", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := catalog.ListConfigurationProfiles(r.Context(), principal)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("GET /v2/configuration-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := catalog.GetConfigurationProfile(r.Context(), principal, model.ConfigurationProfileRef{ProfileID: model.ConfigurationProfileID(r.PathValue("id")), RevisionID: model.ConfigurationProfileRevisionID(r.URL.Query().Get("revision_id")), ContentHash: r.URL.Query().Get("content_hash")})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
