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
			ProfileID               model.ConfigurationProfileID `json:"profile_id"`
			Launch                  *app.GroupMemberLaunch       `json:"launch"`
			ConfigurationOverrides  *model.ConfigurationOptions  `json:"configuration_overrides"`
			Labels                  *model.AgentDisplayLabels    `json:"labels"`
			Environment             model.Environment            `json:"environment"`
			RequestID               model.RequestID              `json:"request_id"`
			ID                      model.AgentID                `json:"id"`
			Name                    string                       `json:"name"`
			ExpectedGroupRevision   model.Revision               `json:"expected_group_revision"`
			ExpectedDefaultRevision model.Revision               `json:"expected_default_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		out, err := api.CreateGroupMember(r.Context(), app.CreateGroupMemberRequest{ProfileID: body.ProfileID, Launch: body.Launch, ConfigurationOverrides: body.ConfigurationOverrides, Labels: body.Labels, Context: app.RequestContext{Principal: p, RequestID: body.RequestID}, GroupID: model.GroupID(r.PathValue("id")), Environment: body.Environment, ID: body.ID, Name: body.Name, ExpectedGroupRevision: body.ExpectedGroupRevision, ExpectedDefaultRevision: body.ExpectedDefaultRevision})
		// Once admitted, failed/uncertain native effects still have a safe durable
		// receipt. Return it immediately so the operator can inspect the outcome.
		if err != nil && (out.Operation == nil || out.Operation.Operation.ID == "") {
			applicationError(w, err)
			return
		}
		var operation *operationView
		if out.Operation != nil {
			value := projectOperation(out.Operation.Operation)
			operation = &value
		}
		writeJSON(w, http.StatusOK, struct {
			Agent     model.Agent
			Group     model.Group
			Repeated  bool
			Operation *operationView `json:",omitempty"`
		}{out.Agent, out.Group, out.Repeated, operation})
	})
}
