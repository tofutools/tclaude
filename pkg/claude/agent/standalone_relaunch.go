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
		if err == nil {
			return out.Changes, out.Warnings, nil
		}
		var refused *DaemonError
		switch {
		case !errors.As(err, &refused):
			return nil, nil, &conv.StandaloneRelaunchUnavailable{Warning: conv.AgentdNotRunning}
		case refused.Status == http.StatusUnauthorized || refused.Status == http.StatusForbidden:
			// The routes are the operator's. A caller agentd cannot confirm
			// as the operator (no current operator token, or an agent's pane)
			// still resumes, as it could before, under the stored policy.
			return nil, nil, &conv.StandaloneRelaunchUnavailable{Warning: conv.AgentdNotOperator}
		case refused.Status == http.StatusNotFound || refused.Status == http.StatusMethodNotAllowed:
			return nil, nil, &conv.StandaloneRelaunchUnavailable{Warning: conv.AgentdTooOld}
		}
		return nil, nil, err
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
