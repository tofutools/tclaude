package transport

import (
	"mime"
	"net/http"
	"strconv"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) registerExecutionFiles(files app.ExecutionFileAPI) {
	h.mux.HandleFunc("GET /v2/execution-files", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		if len(q) != 2 || len(q["execution_id"]) != 1 || len(q["path"]) != 1 {
			applicationError(w, app.ErrInvalid)
			return
		}
		file, err := files.ReadExecutionFile(r.Context(), app.ReadExecutionFileRequest{Principal: principal, ExecutionID: model.ExecutionID(q.Get("execution_id")), Path: q.Get("path")})
		if err != nil {
			applicationError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Filename}))
		w.Header().Set("Content-Length", strconv.Itoa(len(file.Content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(file.Content)
	})
}
