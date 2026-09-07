package transport

import (
	"context"
	"net/http"
	"strconv"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
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
		writeJSON(w, http.StatusOK, struct {
			app.RefreshUsageResult
			Observation usageObservationView
		}{result, projectUsage(result.Observation)})
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
		observations := make([]usageObservationView, 0, len(result.Observations))
		for _, observation := range result.Observations {
			observations = append(observations, projectUsage(observation))
		}
		writeJSON(w, http.StatusOK, struct {
			Observations []usageObservationView
			NextCursor   string
		}{observations, result.NextCursor})
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

// ExactValue preserves int64 source readings for clients whose JSON number type
// cannot represent every integer. Value remains for existing numeric clients.
type usageCounterView struct {
	model.UsageCounter
	ExactValue string
}
type usageObservationView struct {
	model.UsageObservation
	Counters []usageCounterView
}

func projectUsage(observation model.UsageObservation) usageObservationView {
	out := usageObservationView{UsageObservation: observation, Counters: make([]usageCounterView, 0, len(observation.Counters))}
	for _, counter := range observation.Counters {
		out.Counters = append(out.Counters, usageCounterView{counter, strconv.FormatInt(counter.Value, 10)})
	}
	return out
}
