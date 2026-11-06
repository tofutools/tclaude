package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A member@peer verb's reply says it is the peer's untrusted account, and
// what the remote verb left out.
func TestMemberVerbCLIShowsRemoteMarker(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	reply := `{"conv_id":"remote-conv","action":"stopped","remote":{"target":"builder@bob","untrusted":true},"remote_note":"the agent's worktree was kept"}`
	DaemonRequestImpl = func(_, _ string, _ any, out any, _ DaemonOpts) error {
		return json.Unmarshal([]byte(reply), out)
	}

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runStop(&stopParams{Selector: "builder@bob"}, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "Remote agent builder@bob")
	require.Contains(t, stdout.String(), "untrusted content")

	stdout.Reset()
	require.Equal(t, rcOK, runRetire(&retireParams{Selector: "builder@bob"}, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "untrusted content")
	require.Contains(t, stdout.String(), "note: the agent's worktree was kept")

	reply = `{"conv_id":"local-conv","action":"soft_stopped"}`
	stdout.Reset()
	require.Equal(t, rcOK, runStop(&stopParams{Selector: "worker"}, &stdout, &stderr))
	require.NotContains(t, stdout.String(), "untrusted", "a local reply carries no remote marker")
}
