package transport

import (
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// Observe refreshes durable runtime facts; snapshot remains a read-only query.
// Provider recovery evidence and native store references stay private.
func (h *Handler) observe(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		ExecutionID model.ExecutionID `json:"execution_id"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.application.Observe(r.Context(), app.ObserveRequest{Principal: p, ExecutionID: body.ExecutionID})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Execution        executionView               `json:"execution"`
		ObservedAt       time.Time                   `json:"observed_at"`
		Workload         ports.WorkloadObservedState `json:"workload"`
		Context          ports.ContextObservedState  `json:"context"`
		AttachmentActive bool                        `json:"attachment_active"`
		ExitCode         *int                        `json:"exit_code,omitempty"`
	}{projectExecution(result.Execution), result.Observation.ObservedAt, result.Observation.Workload,
		result.Observation.Context, result.Observation.AttachmentActive, result.Observation.ExitCode})
}
