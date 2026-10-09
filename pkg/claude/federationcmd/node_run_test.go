package federationcmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/noderun"
)

func TestNodeRunCLIFanoutSkipsOfflineWithoutRetry(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var mu sync.Mutex
	paths := []string{}
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.True(t, opts.NoRetry)
		if path == "/v1/federation/status?summary=1" {
			return json.Unmarshal([]byte(`{"peers":[{"instance_id":"online","trusted":true,"online":true},{"instance_id":"offline","trusted":true,"online":false},{"instance_id":"untrusted","trusted":false,"online":true}]}`), out)
		}
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		require.Equal(t, "POST", method)
		require.Equal(t, "printf 'hello; $(literal)'", in.(noderun.Request).Script)
		if strings.Contains(path, "online/") {
			return &agent.DaemonError{Status: 502, Code: "peer_unreachable", Raw: []byte(`{"reason":"peer_offline"}`)}
		}
		raw, _ := json.Marshal(noderun.Job{ID: strings.Repeat("a", 32), State: "running", TimeoutSeconds: 30, ExitCode: -1})
		return json.Unmarshal(raw, out)
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, runNodeRuns(&nodeRunParams{All: true, NoWait: true, JSON: true}, []string{"printf 'hello; $(literal)'"}, &stdout, &stderr), stderr.String())
	var rows []nodeRunOutcome
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rows))
	require.Len(t, rows, 3)
	require.Equal(t, "running", rows[0].State)
	require.Equal(t, "skipped", rows[1].State)
	require.Equal(t, "skipped", rows[2].State)
	require.ElementsMatch(t, []string{"/v1/node/run", "/v1/federation/peer/online/node/run"}, paths)
}

func TestNodeRunCLIJobAndBoundedLog(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	id := strings.Repeat("a", 32)
	badChunk := false
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.Equal(t, "GET", method)
		require.Nil(t, in)
		if strings.Contains(path, "/logs?") {
			chunk := noderun.LogChunk{Data: []byte("full \x1b[31moutput"), EOF: true, NextOffset: 16}
			chunk.NextOffset = int64(len(chunk.Data))
			if badChunk {
				chunk.NextOffset++
			}
			raw, _ := json.Marshal(chunk)
			return json.Unmarshal(raw, out)
		}
		raw, _ := json.Marshal(noderun.Job{ID: id, State: "completed", TimeoutSeconds: 30})
		return json.Unmarshal(raw, out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runNodeRuns(&nodeRunParams{Node: []string{"bob"}, Job: id, Log: "stdout"}, nil, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "output")
	require.NotContains(t, stdout.String(), "\x1b")
	badChunk = true
	stdout.Reset()
	require.Equal(t, 1, runNodeRuns(&nodeRunParams{Job: id, Log: "stdout", JSON: true}, nil, &stdout, &stderr))
	require.Contains(t, stdout.String(), "invalid bounded log reply")
}
