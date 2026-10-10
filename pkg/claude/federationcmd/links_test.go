package federationcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestFederationLinksCLIExactFilterAndOutput(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.Equal(t, "GET", method)
		require.Equal(t, "/v1/federation/links?group=builders+%26+tools", path)
		return json.Unmarshal([]byte(`{"groups":[{"group_id":42,"name":"builders & tools","federation_links":[{"peer":"inst_a","label":"bob\u001b]52;attack\u0007","kind":"grant","direction":"in","level":"restricted","pool":"rigs","slugs":["routes.consume"],"online":true,"last_seen":"2026-10-10T00:00:00Z"},{"peer":"inst_b","label":"carol","kind":"route","direction":"out","level":"restricted","remote":"api","online":false}]},{"group_id":43,"name":"quiet","federation_links":[]}]}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runLinks(&linksParams{Group: "builders & tools"}, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "routes.consume")
	require.Contains(t, stdout.String(), "rigs")
	require.Contains(t, stdout.String(), "api")
	require.Contains(t, stdout.String(), "online")
	require.Contains(t, stdout.String(), "offline")
	require.Contains(t, stdout.String(), "2026-10-10T00:00:00Z")
	require.Contains(t, stdout.String(), "no links")
	require.NotContains(t, stdout.String(), "\x1b")
	require.NotContains(t, stdout.String(), "\a")
	stdout.Reset()
	require.Zero(t, runLinks(&linksParams{Group: "builders & tools", JSON: true}, &stdout, &stderr))
	var result struct {
		Groups []federationGroupLinks `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.Len(t, result.Groups, 2)
	require.Equal(t, int64(42), result.Groups[0].GroupID)
	require.Len(t, result.Groups[0].Links, 2)
}
