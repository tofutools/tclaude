package federationcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestHumanInboxCLIPaths(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var calls []string
	var bodies []any
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.True(t, opts.NoRetry)
		calls = append(calls, method+" "+path)
		bodies = append(bodies, in)
		return json.Unmarshal([]byte(`{}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runHumanInbox(&humanInboxParams{Peer: "lab"}, &stdout, &stderr))
	require.Zero(t, runHumanInboxReply(&humanInboxReplyParams{Peer: "lab", ID: 7, Body: "go"}, &stdout, &stderr))
	require.Zero(t, runHumanInboxDecide(&humanInboxDecideParams{Peer: "lab", ID: "req/1", Decision: "deny"}, &stdout, &stderr))
	require.NotZero(t, runHumanInboxDecide(&humanInboxDecideParams{Peer: "lab", ID: "req1", Decision: "always"}, &stdout, &stderr))
	require.NotZero(t, runHumanInboxReply(&humanInboxReplyParams{Peer: "lab", ID: 7}, &stdout, &stderr))
	require.Equal(t, []string{
		"GET /v1/federation/peer/lab/human-inbox",
		"POST /v1/federation/peer/lab/human-inbox/reply",
		"POST /v1/federation/peer/lab/human-inbox/access/req%2F1",
	}, calls)
	require.Equal(t, map[string]any{"id": int64(7), "body": "go"}, bodies[1])
	require.Equal(t, map[string]any{"decision": "deny"}, bodies[2])
}
