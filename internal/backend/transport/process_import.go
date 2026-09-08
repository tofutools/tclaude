package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/processimport"
	"net/http"
)

func (h *Handler) registerProcessImport(api app.ProcessImportAPI) {
	h.mux.HandleFunc("POST /v2/process-import/inspect", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Source string `json:"source"`
		}
		if !decodeBoundedRequest(w, r, &body, 8<<20) {
			return
		}
		result, err := api.InspectProcessImport(r.Context(), p, body.Source)
		journeyResult(w, result, err)
	})
	h.mux.HandleFunc("POST /v2/process-import/convert", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Source   string                           `json:"source"`
			ID       model.DefinitionID               `json:"id"`
			Bindings map[string]processimport.Binding `json:"bindings"`
		}
		if !decodeBoundedRequest(w, r, &body, 8<<20) {
			return
		}
		result, err := api.ConvertProcessImport(r.Context(), app.ConvertProcessImportRequest{Principal: p, Source: body.Source, ID: body.ID, Bindings: body.Bindings})
		journeyResult(w, struct {
			Draft struct {
				app.DefinitionDraft
				Parameters []parameterDeclarationView
			}
			Notices []string
		}{Draft: struct {
			app.DefinitionDraft
			Parameters []parameterDeclarationView
		}{result.Draft, projectParameters(result.Draft.Parameters)}, Notices: result.Notices}, err)
	})
}
