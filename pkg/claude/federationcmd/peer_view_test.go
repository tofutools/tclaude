package federationcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNodeSummaryCLIFanoutAndErrors(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var active, peak atomic.Int32
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		if path == "/v1/federation/status?summary=1" {
			peers := []map[string]any{}
			for i := 0; i < 6; i++ {
				peers = append(peers, map[string]any{"instance_id": fmt.Sprintf("inst_%d", i), "label": fmt.Sprintf("node-%d", i), "trusted": true, "level": "restricted"})
			}
			peers = append(peers, map[string]any{"instance_id": "untrusted", "trusted": false})
			raw, _ := json.Marshal(map[string]any{"instance_id": "local", "name": "Self", "peers": peers})
			return json.Unmarshal(raw, out)
		}
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if !opts.NoRetry {
			return fmt.Errorf("summary must not retry")
		}
		if strings.Contains(path, "inst_2/") {
			return &agent.DaemonError{Status: 504, Code: "peer_unreachable", Msg: "unreachable", Raw: []byte(`{"code":"peer_unreachable","reason":"peer_timeout","last_seen":"2026-10-09T00:00:00Z"}`)}
		}
		return json.Unmarshal([]byte(`{"shared_groups":1,"shared_agents":2,"waiting_for_input":1,"peer_view":{"omitted":[{"feature":"health"}]}}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, runNodes(&nodesParams{Summary: true, JSON: true}, &stdout, &stderr), stderr.String())
	var rows []nodeSummaryRow
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rows))
	require.Len(t, rows, 7)
	require.Equal(t, "local", rows[0].InstanceID)
	require.Equal(t, "inst_2", rows[3].InstanceID)
	require.Contains(t, string(rows[3].Error), "peer_timeout")
	require.Contains(t, string(rows[1].Summary), "omitted")
	require.LessOrEqual(t, peak.Load(), int32(4))
	stdout.Reset()
	require.Zero(t, runNodes(&nodesParams{Node: "node-1", JSON: true}, &stdout, &stderr), stderr.String())
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "inst_1", rows[0].InstanceID)
	stdout.Reset()
	require.Zero(t, runStatusOptions(&statusParams{Summary: true, JSON: true}, &stdout, &stderr), stderr.String())
	require.NotContains(t, stdout.String(), "peer_grants")
	require.NotContains(t, stdout.String(), "outbox")
}
func TestPeerViewCLIErrorContract(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.Equal(t, "/v1/federation/peer/bob/snapshot?scope=visible", path)
		return &agent.DaemonError{Status: 403, Code: "not_trusted", Raw: []byte(`{"code":"not_trusted"}`)}
	}
	var stdout, stderr bytes.Buffer
	require.NotZero(t, runPeerView(&viewParams{Node: "bob"}, "snapshot?scope=visible", &stdout, &stderr))
	require.Contains(t, stderr.String(), `"code":"not_trusted"`)
	require.Empty(t, stdout.String())
	for _, name := range []string{"summary", "node"} {
		require.NotNil(t, nodesCmd().Flags().Lookup(name))
	}
	require.NotNil(t, statusCmd().Flags().Lookup("summary"))
}
