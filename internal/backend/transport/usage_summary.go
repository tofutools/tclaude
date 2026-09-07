package transport

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/app"
	"net/http"
)

type usageSummaryAPI interface {
	SummarizeUsage(context.Context, app.UsageSummaryRequest) (app.UsageSummaryResult, error)
}

func (h *Handler) registerUsageSummary(api usageSummaryAPI) {
	h.mux.HandleFunc("POST /v2/usage/summary", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Filter app.UsageSummaryFilter `json:"filter"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := api.SummarizeUsage(r.Context(), app.UsageSummaryRequest{Principal: principal, Filter: body.Filter})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
