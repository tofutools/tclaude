package agent

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPeerViewCLIReadAndProjection(t *testing.T) {
	old := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = old })
	calls := 0
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		calls++
		require.Equal(t, "GET", method)
		require.Equal(t, 20*time.Second, opts.Timeout)
		require.True(t, opts.NoRetry)
		require.Equal(t, "/v1/federation/peer/my%20node/groups/team%2Fone", path)
		return json.Unmarshal([]byte(`{"group":{"members":[{"agent_id":"agt_1","title":"x\u001b[31m\nrow","online":false,"state":{}}]},"peer_view":{"omitted":[{"feature":"groups.presence"}]}}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runPeerListing("my node", "agents", "team/one", true, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), `"peer_view"`)
	require.Contains(t, stdout.String(), `groups.presence`)
	stdout.Reset()
	require.Zero(t, runPeerListing("my node", "agents", "team/one", false, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "not reported")
	require.NotContains(t, stdout.String(), "\x1b")
	require.NotContains(t, stdout.String(), "offline")
	before := calls
	for _, tail := range []string{"/snapshot", "../snapshot", "groups/%2e%2e/config", "https://example.com/snapshot", "snapshot#fragment", "groups//name"} {
		require.Error(t, ReadPeerView("bob", tail, new(any)))
	}
	require.Equal(t, before, calls)
}
