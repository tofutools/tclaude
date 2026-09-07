package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) registerGroupConfiguration(api app.GroupConfigurationAPI) {
	h.mux.HandleFunc("GET /v2/groups/{id}/configuration", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		out, err := api.GetGroupConfiguration(r.Context(), p, model.GroupID(r.PathValue("id")))
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	h.mux.HandleFunc("PUT /v2/groups/{id}/configuration", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Environment      model.Environment              `json:"environment"`
			Profile          *model.ConfigurationProfileRef `json:"profile"`
			ExpectedRevision model.Revision                 `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		out, err := api.SetGroupConfiguration(r.Context(), app.SetGroupConfigurationRequest{Principal: p, GroupID: model.GroupID(r.PathValue("id")), Environment: body.Environment, Profile: body.Profile, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	h.mux.HandleFunc("POST /v2/groups/{id}/agents", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Environment             model.Environment `json:"environment"`
			RequestID               model.RequestID   `json:"request_id"`
			ID                      model.AgentID     `json:"id"`
			Name                    string            `json:"name"`
			ExpectedGroupRevision   model.Revision    `json:"expected_group_revision"`
			ExpectedDefaultRevision model.Revision    `json:"expected_default_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		out, err := api.CreateGroupMember(r.Context(), app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: p, RequestID: body.RequestID}, GroupID: model.GroupID(r.PathValue("id")), Environment: body.Environment, ID: body.ID, Name: body.Name, ExpectedGroupRevision: body.ExpectedGroupRevision, ExpectedDefaultRevision: body.ExpectedDefaultRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}
