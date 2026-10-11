package federationcmd

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"testing"
)

func TestOfferImportLandingCLI(t *testing.T) {
	oldAvailable, oldRequest := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvailable, oldRequest })
	agent.DaemonAvailableImpl = func() bool { return true }
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.Equal(t, "POST", method)
		require.Contains(t, path, "/v1/federation/bundle-offers/offer/import")
		body := in.(map[string]any)
		require.Equal(t, "group_default", body["landing"])
		require.Equal(t, "", body["cwd"])
		require.Equal(t, false, body["apply"])
		return json.Unmarshal([]byte(`{"cwd":"/receiver/project","landing":{"cwd":"/receiver/project","reason":"group_default","candidates":[{"id":"group_default","cwd":"/receiver/project"}]}}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runOfferImport(&offerImportParams{ID: "offer", Peer: "peer", Landing: "group_default"}, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "Will start in /receiver/project (group_default)")
	require.Contains(t, stdout.String(), "Preview only")
}
