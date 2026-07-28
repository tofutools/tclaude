package agent

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReincarnateHelpDocumentsHarnessSpecificContextPolicy(t *testing.T) {
	long := reincarnateCmd().Long
	assert.Contains(t, long, "primarily a Claude Code context-management tool")
	assert.Contains(t, long, "Codex CLI has effective, efficient automatic compaction")
	assert.Contains(t, long, "run to full context and auto-compact")
	assert.Contains(t, long, "Do not reincarnate a Codex agent merely to free context space")
	assert.Contains(t, long, "explicit human request")
}

func TestRunReincarnatePrintsOfflineAgentError(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/agent/worker/reincarnate", path)
		assert.Equal(t, map[string]any{"follow_up": "continue from the handoff"}, in)
		return &DaemonError{
			Status: http.StatusServiceUnavailable,
			Code:   "no_tmux",
			Msg:    "cannot reincarnate worker: the agent is offline. Reincarnation can only run on a live agent; resume it first with `tclaude agent resume worker`.",
		}
	}

	var stdout, stderr bytes.Buffer
	rc := runReincarnate(
		&reincarnateParams{FollowUp: "continue from the handoff", Target: "worker"},
		strings.NewReader(""), &stdout, &stderr)

	require.Equal(t, rcIOFailure, rc)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "Error: cannot reincarnate worker: the agent is offline.")
	assert.Contains(t, stderr.String(), "tclaude agent resume worker")
}
