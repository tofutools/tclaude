package transport

import (
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerConfigurationCatalog(catalog app.ConfigurationCatalogAPI) {
	if transfer, ok := catalog.(app.ConfigurationTransferAPI); ok {
		h.registerConfigurationTransfer(transfer)
	}
	h.mux.HandleFunc("POST /v2/configuration-profiles/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			ExpectedRevision model.Revision `json:"expected_revision"`
			Archived         *bool          `json:"archived"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if body.Archived == nil {
			applicationError(w, app.ErrInvalid)
			return
		}
		result, err := catalog.SetConfigurationProfileArchived(r.Context(), app.SetConfigurationProfileArchivedRequest{Context: body.context(principal), ID: model.ConfigurationProfileID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision, Archived: *body.Archived})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	h.mux.HandleFunc("POST /v2/configuration-profiles/{id}/availability", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			ExpectedRevision model.Revision `json:"expected_revision"`
			Disabled         *bool          `json:"disabled"`
			Reason           *string        `json:"reason"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if body.Disabled == nil {
			applicationError(w, app.ErrInvalid)
			return
		}
		result, err := catalog.SetConfigurationProfileAvailability(r.Context(), app.SetConfigurationProfileAvailabilityRequest{Context: body.context(principal), ID: model.ConfigurationProfileID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision, Disabled: *body.Disabled, Reason: body.Reason})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	h.mux.HandleFunc("GET /v2/configuration-defaults", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := catalog.GetConfigurationDefaults(r.Context(), principal)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("POST /v2/configuration-defaults", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			ExpectedRevision model.Revision                           `json:"expected_revision"`
			Global           *model.ConfigurationProfileRef           `json:"global"`
			Harnesses        map[string]model.ConfigurationProfileRef `json:"harnesses"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := catalog.SaveConfigurationDefaults(r.Context(), app.SaveConfigurationDefaultsRequest{Context: body.context(principal), ExpectedRevision: body.ExpectedRevision, Global: body.Global, Harnesses: body.Harnesses})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
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
			Startup          *model.ProfileStartup                `json:"startup"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := catalog.SaveConfigurationProfile(r.Context(), app.SaveConfigurationProfileRequest{Context: body.context(principal), ID: body.ID, RevisionID: body.RevisionID, ExpectedRevision: body.ExpectedRevision, Name: body.Name, Desired: body.Desired, Startup: body.Startup})
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
