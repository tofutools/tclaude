package transport

import (
	"encoding/json"
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerProcessSnippets(api app.ProcessSnippetAPI) {
	h.mux.HandleFunc("GET /v2/process-snippets", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		out, err := api.ListProcessSnippets(r.Context(), p)
		if err != nil {
			applicationError(w, err)
			return
		}
		views := make([]processSnippetView, 0, len(out))
		for _, item := range out {
			views = append(views, projectProcessSnippet(item))
		}
		writeJSON(w, http.StatusOK, views)
	})
	h.mux.HandleFunc("POST /v2/process-snippets/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			RequestID        model.RequestID `json:"request_id"`
			Action           string          `json:"action"`
			Name             string          `json:"name"`
			Selection        json.RawMessage `json:"selection"`
			ExpectedRevision model.Revision  `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		out, err := api.WriteProcessSnippet(r.Context(), app.ProcessSnippetRequest{Context: app.RequestContext{Principal: p, RequestID: body.RequestID}, ID: r.PathValue("id"), Action: body.Action, Name: body.Name, Selection: body.Selection, ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectProcessSnippet(out))
	})
}
