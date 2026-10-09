package agent

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/harnessops"
	"testing"
)

func TestHarnessOperationsCLIValidationAndRemoteCopy(t *testing.T) {
	oldAvail, oldReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = oldAvail, oldReq })
	DaemonAvailableImpl = func() bool { return true }
	calls := 0
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		calls++
		require.Equal(t, "POST", method)
		require.Equal(t, "/v1/federation/peer/bob%20node/harnesses/operations", path)
		require.True(t, opts.NoRetry)
		require.Equal(t, harnessops.Request{Action: "install", Harness: "codex", CopyCredentials: true, OverwriteCredentials: true}, in)
		return json.Unmarshal([]byte(`{"id":"0123456789abcdef0123456789abcdef","state":"running","warnings":["Target agents act as you"],"peer_view":{"included":["node.harnesses.install"]}}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcInvalidArg, runHarnessOperation("install", &harnessOperationParams{Harness: "codex", CopyCredentials: true}, &stdout, &stderr))
	require.Zero(t, calls)
	require.Equal(t, rcInvalidArg, runHarnessOperation("update", &harnessOperationParams{Harness: "codex", Now: true, WhenIdle: true}, &stdout, &stderr))
	require.Zero(t, calls)
	require.Zero(t, runHarnessOperation("install", &harnessOperationParams{Harness: "codex", Node: "bob node", CopyCredentials: true, OverwriteCredentials: true, NoWait: true, JSON: true}, &stdout, &stderr))
	require.Contains(t, stdout.String(), "peer_view")
	require.Contains(t, stderr.String(), "Target agents act as you")
}
func TestHarnessOperationsCLIBulkNodes(t *testing.T) {
	oldAvail, oldReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = oldAvail, oldReq })
	DaemonAvailableImpl = func() bool { return true }
	paths := []string{}
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		paths = append(paths, path)
		if method == "GET" {
			return json.Unmarshal([]byte(`{"peers":[{"instance_id":"trusted","trusted":true},{"instance_id":"untrusted","trusted":false}]}`), out)
		}
		return json.Unmarshal([]byte(`{"id":"0123456789abcdef0123456789abcdef","state":"running"}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runHarnessOperation("update", &harnessOperationParams{All: true, AllNodes: true, NoWait: true, JSON: true}, &stdout, &stderr))
	require.Equal(t, []string{"/v1/federation/status?summary=1", "/v1/harnesses/operations", "/v1/federation/peer/trusted/harnesses/operations"}, paths)
	require.Contains(t, stdout.String(), "trusted")
}
