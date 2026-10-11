package agent

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestHarnessAvailabilityCLI(t *testing.T) {
	oldAvail, oldReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = oldAvail, oldReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		require.Equal(t, "GET", method)
		require.Equal(t, "/v1/federation/peer/bob%20node/harnesses/availability?refresh=1", path)
		require.True(t, opts.NoRetry)
		require.Equal(t, 20*time.Second, opts.Timeout)
		return json.Unmarshal([]byte(`{"schema":1,"harnesses":[{"name":"codex","installed":true,"path":"/bad\u001b]52;clipboard\u0007\npath","version_status":"unknown","usable":null,"credential_present":null}],"peer_view":{"included":["node.harnesses"]}}`), out)
	}
	var stdout, stderr bytes.Buffer
	p := &harnessLsParams{Node: "bob node", Refresh: true}
	require.Zero(t, runHarnessLs(p, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "unknown")
	require.NotContains(t, stdout.String(), "\x1b")
	require.NotContains(t, stdout.String(), "\npath")
	p.JSON = true
	stdout.Reset()
	require.Zero(t, runHarnessLs(p, &stdout, &stderr))
	require.Contains(t, stdout.String(), "peer_view")
}
