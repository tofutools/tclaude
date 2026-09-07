package transport

import (
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerSandboxProfiles(api app.SandboxProfileAPI) {
	h.mux.HandleFunc("GET /v2/sandbox-profiles", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		archived := r.URL.Query().Get("include_archived")
		if archived != "" && archived != "true" && archived != "false" {
			applicationError(w, app.ErrInvalid)
			return
		}
		result, err := api.ListSandboxProfiles(r.Context(), principal, archived == "true")
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("GET /v2/sandbox-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.GetSandboxProfile(r.Context(), principal, model.SandboxProfileID(r.PathValue("id")))
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectSandboxProfile(result))
	})
	h.mux.HandleFunc("POST /v2/sandbox-profiles", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			commandIdentity
			ID               model.SandboxProfileID `json:"id"`
			ExpectedRevision model.Revision         `json:"expected_revision"`
			Name             string                 `json:"name"`
			Policy           model.SandboxPolicy    `json:"policy"`
		}
		// Policy text is bounded at 4 MiB after canonical encoding. The outer
		// envelope and JSON whitespace have their own finite transport headroom.
		if !decodeBoundedRequest(w, r, &body, 5<<20) {
			return
		}
		result, err := api.SaveSandboxProfile(r.Context(), app.SaveSandboxProfileRequest{Context: body.context(principal), ID: body.ID, ExpectedRevision: body.ExpectedRevision, Name: body.Name, Policy: body.Policy})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectSandboxProfile(result))
	})
	h.mux.HandleFunc("POST /v2/sandbox-profiles/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
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
		result, err := api.SetSandboxProfileArchived(r.Context(), app.SetSandboxProfileArchivedRequest{Context: body.context(principal), ID: model.SandboxProfileID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision, Archived: *body.Archived})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectSandboxProfile(result))
	})
	h.mux.HandleFunc("POST /v2/sandbox-profiles/inspect", func(w http.ResponseWriter, r *http.Request) {
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
		result, err := api.InspectSandboxClosure(r.Context(), principal, body.Ref)
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

type sandboxRevisionProjection struct {
	Ref       model.SandboxProfileRef
	Number    model.Revision
	Policy    model.SandboxPolicy
	Author    collaborationActor
	RequestID model.RequestID
	CreatedAt time.Time
}

func projectSandboxProfile(result app.SandboxProfileResult) any {
	return struct {
		Profile  model.SandboxProfile
		Revision sandboxRevisionProjection
		Repeated bool
	}{result.Profile, sandboxRevisionProjection{result.Revision.Ref, result.Revision.Number, result.Revision.Policy, projectCollaborationActor(result.Revision.Author), result.Revision.RequestID, result.Revision.CreatedAt}, result.Repeated}
}
