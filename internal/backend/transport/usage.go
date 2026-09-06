package transport

import (
	"context"
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
)

type usageAPI interface {
	RefreshUsage(context.Context, app.RefreshUsageRequest) (app.RefreshUsageResult, error)
	QueryUsage(context.Context, app.QueryUsageRequest) (app.UsageResult, error)
	QueryActivity(context.Context, app.QueryActivityRequest) (app.ActivityResult, error)
}

func (h *Handler) registerUsage(api usageAPI) {
	h.mux.HandleFunc("POST /v2/usage/refresh", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Target app.UsageTarget `json:"target"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.RefreshUsage(r.Context(), app.RefreshUsageRequest{Principal: principal, Target: body.Target})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("POST /v2/usage/query", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Filter app.UsageFilter `json:"filter"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.QueryUsage(r.Context(), app.QueryUsageRequest{Principal: principal, Filter: body.Filter})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	h.mux.HandleFunc("POST /v2/activity/query", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Filter app.ActivityFilter `json:"filter"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.QueryActivity(r.Context(), app.QueryActivityRequest{Principal: principal, Filter: body.Filter})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
