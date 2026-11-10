package processcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// --remote reaches the peer's process runs through its peer view, and what
// the peer answers is marked untrusted and stripped of control characters.
func TestProcessRemoteRunsGoThroughThePeerView(t *testing.T) {
	var paths []string
	var bodies []any
	stubProcessRuntime(t, func(method, path string, in, out any, _ agent.DaemonOpts) error {
		paths = append(paths, method+" "+path)
		bodies = append(bodies, in)
		switch path {
		case "/v1/federation/peer/bob/v1/process/runs?group=crew":
			return json.Unmarshal([]byte(`{"runs":[{"id":"run_\u001b[31mx","templateRef":"gate@1","status":"waiting","scope":{"kind":"group","group":"crew"}}]}`), out)
		default:
			return json.Unmarshal([]byte(`{"id":"run_peer","status":"waiting","scope":{"kind":"group","group":"crew","owner":"peer:inst"}}`), out)
		}
	})

	var stdout, stderr bytes.Buffer
	require.NoError(t, runProcessRunsLs(&processRunsLsParams{Group: "crew", Remote: "bob"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "Remote process runs of bob, as that node reports it (untrusted content)")
	assert.NotContains(t, stdout.String(), "\x1b", "control characters from the peer are stripped")
	assert.Contains(t, stdout.String(), "run_[31mx")

	stdout.Reset()
	require.NoError(t, runProcessShowRemote(&processShowParams{RunID: "run_peer", Remote: "bob", JSON: true}, &stdout, &stderr))
	var shown map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &shown))
	assert.Equal(t, map[string]any{"target": "bob", "untrusted": true}, shown["remote"])

	stdout.Reset()
	require.NoError(t, runProcessRun(&processRunParams{TemplateID: "gate", Group: "crew", Remote: "bob"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "Created process run run_peer")
	assert.Contains(t, stdout.String(), "tclaude process show run_peer --remote bob")

	assert.Equal(t, []string{
		http.MethodGet + " /v1/federation/peer/bob/v1/process/runs?group=crew",
		http.MethodGet + " /v1/federation/peer/bob/v1/process/runs/run_peer",
		http.MethodPost + " /v1/federation/peer/bob/v1/process/runs",
	}, paths)

	err := runProcessRun(&processRunParams{TemplateID: "gate", Remote: "bob"}, &stdout, &stderr)
	require.Error(t, err, "a remote start names a group or an agent")
	assert.Len(t, paths, 3, "and is refused before any request")
}

// The manage verbs take --remote too, through the same peer view.
func TestProcessRemoteManageVerbsGoThroughThePeerView(t *testing.T) {
	var paths []string
	stubProcessRuntime(t, func(method, path string, _, out any, _ agent.DaemonOpts) error {
		paths = append(paths, method+" "+path)
		return json.Unmarshal([]byte(`{"changed":true,"started":true,"run":{"id":"run_peer","status":"waiting"}}`), out)
	})
	var stdout, stderr bytes.Buffer
	require.NoError(t, runProcessMutation(&processRunMutationParams{RunID: "run_peer", Remote: "bob"}, "/pause", nil, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "Remote process runs of bob, as that node reports it (untrusted content)")
	require.NoError(t, runProcessDecide(&processDecideParams{RunID: "run_peer", Node: "choose", Verdict: "approve", Remote: "bob"}, &stdout, &stderr))
	require.NoError(t, runProcessAssign(&processAssignParams{RunID: "run_peer", Node: "work", Agent: "agt_x", Remote: "bob"}, &stdout, &stderr))
	require.NoError(t, runProcessResolveBlocked(&processResolveBlockedParams{RunID: "run_peer", Node: "work", Attempt: 1, Action: "skip", Remote: "bob"}, &stdout, &stderr))
	assert.Equal(t, []string{
		http.MethodPost + " /v1/federation/peer/bob/v1/process/runs/run_peer/pause",
		http.MethodPost + " /v1/federation/peer/bob/v1/process/runs/run_peer/decide",
		http.MethodPost + " /v1/federation/peer/bob/v1/process/runs/run_peer/nodes/work/assign",
		http.MethodPost + " /v1/federation/peer/bob/v1/process/runs/run_peer/resolve-blocked",
	}, paths)
}
