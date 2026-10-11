package federationcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestRemoteActionCLIPathsAndNoRetry(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var calls []string
	var bodies []any
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.True(t, opts.NoRetry)
		calls = append(calls, method+" "+path)
		bodies = append(bodies, in)
		return json.Unmarshal([]byte("{\"status\":\"accepted\"}"), out)
	}
	var stdout, stderr bytes.Buffer
	for _, p := range []*actionParams{
		{Action: "spawn", Node: "bob", Group: "work & tools", Brief: "task data; $(literal)", Profile: "safe"},
		{Action: "spawn-status", Node: "bob", Job: 7},
		{Action: "stop", Node: "bob", Agent: "agt_source", Force: true},
		{Action: "retire", Node: "bob", Agent: "agt_source"},
		{Action: "clone", Node: "bob", Agent: "agt_source", FollowUp: "do task"},
		{Action: "move", Node: "bob", Agent: "agt_source", Group: "receiver"},
		{Action: "teleport", Node: "bob", Agent: "agt_source", Group: "receiver", Clone: true},
		{Action: "message", Node: "bob", Agent: "agt_source", Body: "hello"},
		{Action: "resume", Node: "bob", Agent: "agt_source"},
		{Action: "restart", Node: "bob", Agent: "agt_source"},
		{Action: "sandbox-restart", Node: "bob", Agent: "agt_source", Sandbox: "unlock"},
	} {
		require.Zero(t, runAction(p, &stdout, &stderr), stderr.String())
	}
	require.Equal(t, []string{
		"POST /v1/federation/peer/bob/groups/work%20&%20tools/spawn",
		"GET /v1/federation/peer/bob/spawn-requests/7",
		"POST /v1/federation/peer/bob/agents/agt_source/stop?force=1",
		"POST /v1/federation/peer/bob/agents/agt_source/retire",
		"POST /v1/federation/peer/bob/agents/agt_source/clone",
		"POST /v1/federation/peer/bob/agents/agt_source/move",
		"POST /v1/federation/peer/bob/agents/agt_source/teleport",
		"POST /v1/federation/peer/bob/operator-message",
		"POST /v1/federation/peer/bob/agents/agt_source/resume",
		"POST /v1/federation/peer/bob/agents/agt_source/restart",
		"POST /v1/federation/peer/bob/agents/agt_source/sandbox-restart"}, calls)
	require.Equal(t, map[string]any{"action": "unlock"}, bodies[10])
	require.Equal(t, "task data; $(literal)", bodies[0].(map[string]any)["brief"])
	require.Equal(t, map[string]any{"group": "receiver"}, bodies[5])
	require.NotZero(t, runAction(&actionParams{Action: "move", Node: "bob", Agent: "agt_source"}, &stdout, &stderr))
	require.NotZero(t, runAction(&actionParams{Action: "sandbox-restart", Node: "bob", Agent: "agt_source"}, &stdout, &stderr))
	require.Len(t, calls, 11, "invalid destination or sandbox direction cannot issue a request")
}
