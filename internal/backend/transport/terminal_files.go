package transport

import (
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerTerminalFiles(files app.TerminalFileAPI) {
	h.mux.HandleFunc("POST /v2/terminal-files", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var in struct {
			RequestID   model.RequestID   `json:"request_id"`
			ExecutionID model.ExecutionID `json:"execution_id"`
			Filename    string            `json:"filename"`
			Content     []byte            `json:"content"`
		}
		if !decodeBoundedRequest(w, r, &in, 12<<20) {
			return
		}
		result, err := files.StageTerminalFile(r.Context(), app.StageTerminalFileRequest{Context: app.RequestContext{Principal: principal, RequestID: in.RequestID}, ExecutionID: in.ExecutionID, Filename: in.Filename, Content: in.Content})
		if result.Operation.ID == "" {
			if err != nil {
				applicationError(w, err)
				return
			}
			applicationError(w, app.ErrUncertain)
			return
		}
		// A durable operation outcome is a successful read of the receipt, even
		// when publication was refused or uncertain. Clients must inspect State.
		writeJSON(w, http.StatusOK, struct {
			Operation operationView      `json:"operation"`
			File      model.TerminalFile `json:"file"`
			Repeated  bool               `json:"repeated"`
		}{projectOperation(result.Operation), result.File, result.Repeated})
	})
}
