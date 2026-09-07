package transport

import (
	"context"
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
)

type launchSupportAPI interface {
	LaunchSupport(context.Context, app.LaunchSupportRequest) (app.LaunchSupportResult, error)
}

func (h *Handler) registerLaunchSupport() {
	api, ok := h.application.(launchSupportAPI)
	if !ok {
		return
	}
	h.mux.HandleFunc("GET /v2/launch-support", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := api.LaunchSupport(r.Context(), app.LaunchSupportRequest{Principal: principal, Harness: r.URL.Query().Get("harness")})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
