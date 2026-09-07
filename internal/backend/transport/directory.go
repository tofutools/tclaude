package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"net/http"
	"strconv"
)

func (h *Handler) registerDirectoryBrowser(browser app.DirectoryAPI) {
	h.mux.HandleFunc("GET /v2/directories", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		query := r.URL.Query()
		limit := 0
		var err error
		if query.Has("limit") {
			limit, err = strconv.Atoi(query.Get("limit"))
			if err != nil {
				applicationError(w, app.ErrInvalid)
				return
			}
		}
		hidden := false
		if query.Has("hidden") {
			hidden, err = strconv.ParseBool(query.Get("hidden"))
			if err != nil {
				applicationError(w, app.ErrInvalid)
				return
			}
		}
		result, err := browser.BrowseDirectory(r.Context(), app.BrowseDirectoryRequest{Principal: principal, Path: query.Get("path"), After: query.Get("after"), Limit: limit, IncludeHidden: hidden})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
