package federationcmd

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"testing"
)

func TestPeerAccessCLIPathsAndTTL(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var paths []string
	var bodies []any
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.True(t, opts.NoRetry)
		paths = append(paths, method+" "+path)
		bodies = append(bodies, in)
		return json.Unmarshal([]byte(`{"status":"pending"}`), out)
	}
	var stdout, stderr bytes.Buffer
	for _, p := range []*accessParams{{Action: "request", Node: "bob", Permission: "message.direct", GroupID: 7, TTL: "1h"}, {Action: "status", Node: "bob", ID: "abc"}, {Action: "list"}, {Action: "approve", ID: "abc", TTL: "0", GroupID: 7}, {Action: "deny", ID: "abc"}, {Action: "extend", ID: "abc", Seconds: 60}} {
		require.Zero(t, runAccess(p, &stdout, &stderr), stderr.String())
	}
	require.Equal(t, []string{"POST /v1/federation/peer/bob/peer-access-requests", "GET /v1/federation/peer/bob/peer-access-requests/abc", "GET /v1/federation/access-requests", "POST /v1/federation/access-requests/abc/decision", "POST /v1/federation/access-requests/abc/decision", "POST /v1/federation/access-requests/abc/decision"}, paths)
	require.Equal(t, 3600, bodies[0].(map[string]any)["grant_ttl_seconds"])
	require.Equal(t, 0, bodies[3].(map[string]any)["grant_ttl_seconds"])
	require.NotZero(t, runAccess(&accessParams{Action: "approve", Node: "bob", ID: "abc"}, &stdout, &stderr))
	require.NotZero(t, runAccess(&accessParams{Action: "request", Node: "bob", Permission: "message.direct", TTL: "-1s"}, &stdout, &stderr))
	require.Len(t, paths, 6)
}
