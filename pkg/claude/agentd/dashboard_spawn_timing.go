package agentd

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Browser durations are monotonic offsets from submit, not server timestamps.
// Accept only fixed numeric fields and a bounded label, never user prompt text.
func handleDashboardSpawnTiming(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !config.StartupTimingEnabled() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Label       string  `json:"label"`
		Closed      bool    `json:"closed"`
		Prepared    float64 `json:"prepared_ms"`
		Worktree    float64 `json:"worktree_ready_ms"`
		Attachments float64 `json:"attachments_ready_ms"`
		Request     float64 `json:"request_sent_ms"`
		Response    float64 `json:"response_received_ms"`
		Elapsed     float64 `json:"elapsed_ms"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || len(body.Label) > 256 {
		http.Error(w, "invalid timing report", http.StatusBadRequest)
		return
	}
	for _, duration := range []float64{body.Prepared, body.Worktree, body.Attachments, body.Request, body.Response, body.Elapsed} {
		if math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0 || duration > 86400000 {
			http.Error(w, "invalid timing duration", http.StatusBadRequest)
			return
		}
	}
	stage := "submit_failed"
	if body.Closed {
		stage = "dialog_close_requested"
	}
	slog.Info("startup timing", "component", "spawn_dialog", "stage", stage,
		"label", body.Label, "elapsed_ms", body.Elapsed,
		"prepared_ms", body.Prepared, "worktree_ready_ms", body.Worktree,
		"attachments_ready_ms", body.Attachments, "request_sent_ms", body.Request,
		"response_received_ms", body.Response)
	w.WriteHeader(http.StatusNoContent)
}
