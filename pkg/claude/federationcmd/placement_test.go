package federationcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestSpawnPlacementCLIRequestAndExplanation(t *testing.T) {
	available, request := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = available, request })
	agent.DaemonAvailableImpl = func() bool { return true }
	var sent map[string]any
	agent.DaemonRequestImpl = func(method, path string, in, out any, _ agent.DaemonOpts) error {
		require.Equal(t, http.MethodPost, method)
		require.Equal(t, "/v1/federation/spawn-requests", path)
		sent = in.(map[string]any)
		*out.(*json.RawMessage) = json.RawMessage(`{"envelope_id":"abcdefghijklmno","to":"builders@bob","state":"accepted","hub_connected":true,"placement":{"prefer":"least-loaded","selected":"inst-bob","candidates":[{"peer":"bob","instance":"inst-bob","group":"builders","eligible":true},{"peer":"carol","instance":"inst-carol","eligible":false,"reason":"node metadata stale or warming"}]}}`)
		return nil
	}
	var stdout, stderr bytes.Buffer
	rc := runSpawnRequest(&spawnRequestParams{Node: "group:rigs", Group: "builders", Require: "harness=codex", Brief: "work"}, &stdout, &stderr)
	require.Zero(t, rc, stderr.String())
	require.Equal(t, "group:rigs", sent["node"])
	require.Equal(t, "harness=codex", sent["require"])
	require.Contains(t, stdout.String(), "builders@bob: selected")
	require.Contains(t, stdout.String(), "carol: node metadata stale or warming")
	stdout.Reset()
	stderr.Reset()
	agent.DaemonRequestImpl = func(method, path string, in, out any, _ agent.DaemonOpts) error {
		return &agent.DaemonError{Status: 409, Code: "no_candidate", Msg: "no node", Raw: []byte(`{"code":"no_candidate","error":"no node","placement":{"prefer":"least-loaded","candidates":[{"peer":"bob","instance":"inst-bob","eligible":false,"reason":"CPU load unavailable"}]}}`)}
	}
	require.NotZero(t, runSpawnRequest(&spawnRequestParams{Node: "auto", Brief: "work"}, &stdout, &stderr))
	require.Contains(t, stdout.String(), "CPU load unavailable")
	require.Contains(t, stderr.String(), "no node")
	cmd := spawnRequestCmd()
	for _, name := range []string{"node", "group", "require", "prefer", "json"} {
		require.NotNil(t, cmd.Flags().Lookup(name))
	}
}
