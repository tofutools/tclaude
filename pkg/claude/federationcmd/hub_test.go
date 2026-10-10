package federationcmd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"testing"
)

func TestHubCLIUsesLocalRoutesWithoutMutationRetry(t *testing.T) {
	oldAvail, oldRequest := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldRequest })
	agent.DaemonAvailableImpl = func() bool { return true }
	tests := []struct {
		resource     string
		p            hubResourceParams
		method, path string
	}{
		{"admins", hubResourceParams{Action: "add", Instance: "inst_id", Capabilities: []string{"hub.health.read"}}, "POST", "admins"},
		{"admins", hubResourceParams{Action: "remove", Instance: "inst_id"}, "DELETE", "admins/inst_id"},
		{"settings", hubResourceParams{Action: "reset", Key: "max_connections"}, "PATCH", "settings"},
		{"identity", hubResourceParams{Action: "recover", Old: "inst_old", New: "inst_new", Apply: true, Fingerprint: "verified"}, "POST", "identity/recover"},
		{"logs", hubResourceParams{Action: "tail", MaxEntries: 20, Cursor: "opaque"}, "GET", "logs?cursor=opaque&max_entries=20"},
	}
	for _, tt := range tests {
		t.Run(tt.resource+tt.p.Action, func(t *testing.T) {
			called := false
			agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
				called = true
				require.Equal(t, tt.method, method)
				require.Equal(t, "/v1/federation/hub/"+tt.path, path)
				require.True(t, opts.NoRetry)
				return nil
			}
			var stdout, stderr bytes.Buffer
			require.Zero(t, runHubResource(tt.resource, &tt.p, &stdout, &stderr), stderr.String())
			require.True(t, called)
		})
	}
}
