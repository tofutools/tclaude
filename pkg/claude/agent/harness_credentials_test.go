package agent

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCredentialsCLIConfirmationAndChosenSet(t *testing.T) {
	oldAvail, oldReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = oldAvail, oldReq })
	DaemonAvailableImpl = func() bool { return true }
	calls := []string{}
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		calls = append(calls, path)
		require.Equal(t, "POST", method)
		require.True(t, opts.NoRetry)
		raw, err := json.Marshal(in)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"confirm_share":true`)
		require.NotContains(t, string(raw), `"credentials"`)
		return json.Unmarshal([]byte(`{"receipt":{"backup_id":"0123456789abcdef0123456789abcdef"},"peer_view":{"included":["node.credentials.receive"]}}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcInvalidArg, runCredentialOperation("push", &credentialParams{Harness: "codex", Node: "bob"}, &stdout, &stderr))
	require.Empty(t, calls)
	require.Zero(t, runCredentialOperation("push", &credentialParams{Harness: "codex,claude", Node: "bob node", ConfirmShare: true, JSON: true}, &stdout, &stderr))
	require.Equal(t, []string{"/v1/federation/peer/bob%20node/harnesses/credentials/push", "/v1/federation/peer/bob%20node/harnesses/credentials/push"}, calls)
	require.Contains(t, stdout.String(), "peer_view")
}
func TestCredentialsCLILocalRestoreAndListing(t *testing.T) {
	oldAvail, oldReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = oldAvail, oldReq })
	DaemonAvailableImpl = func() bool { return true }
	calls := []string{}
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		calls = append(calls, method+" "+path)
		return json.Unmarshal([]byte(`{"receipt":{"backup_id":"abc","backup_location":"/private"},"backups":[]}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runCredentialOperation("restore", &credentialParams{Harness: "codex"}, &stdout, &stderr))
	require.Zero(t, runCredentialOperation("ls", &credentialParams{Harness: "codex", Node: "bob"}, &stdout, &stderr))
	require.Equal(t, []string{"POST /v1/harnesses/credentials/restore", "GET /v1/federation/peer/bob/harnesses/credentials/backups?harness=codex"}, calls)
	require.Equal(t, rcInvalidArg, runCredentialOperation("restore", &credentialParams{Harness: "codex", Backup: "../x"}, &stdout, &stderr))
	require.NotPanics(t, func() { HarnessCmd() })
}
