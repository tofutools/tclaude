package agent

import (
	"errors"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// standaloneRelaunchResponse mirrors agentd.StandaloneRelaunchPrepared (the
// agent package cannot import agentd).
type standaloneRelaunchResponse struct {
	Warnings []string `json:"warnings,omitempty"`
	Changes  []string `json:"changes,omitempty"`
}

// The standalone relaunch paths in conv (conv resume, the watch TUI) reach
// agentd through these, installed here because conv cannot import this
// package. No retries: with the daemon down, the human's resume goes ahead
// under the stored policy at once rather than waiting out a restart window.
func init() {
	conv.StandaloneRelaunchPrepare = func(convID string) ([]string, []string, error) {
		var out standaloneRelaunchResponse
		err := DaemonRequestImpl(http.MethodPost, "/v1/relaunch-sandbox/prepare",
			map[string]any{"conv_id": convID}, &out, DaemonOpts{NoRetry: true})
		var refused *DaemonError
		if err != nil && !errors.As(err, &refused) {
			return nil, nil, conv.ErrAgentdUnreachable
		}
		if err != nil {
			return nil, nil, err
		}
		return out.Changes, out.Warnings, nil
	}
	conv.StandaloneRelaunchSettle = func(convID string, launched bool) []string {
		var out standaloneRelaunchResponse
		if err := DaemonRequestImpl(http.MethodPost, "/v1/relaunch-sandbox/settle",
			map[string]any{"conv_id": convID, "launched": launched}, &out, DaemonOpts{NoRetry: true}); err != nil {
			return []string{"could not tell agentd how the relaunch ended: " + err.Error()}
		}
		return out.Warnings
	}
}
