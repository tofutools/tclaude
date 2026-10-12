package federationcmd

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"strings"
	"testing"
	"time"
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

func TestHubStaleCLIRequiresConfirmationOfPreview(t *testing.T) {
	oldAvail, oldRequest := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldRequest })
	agent.DaemonAvailableImpl = func() bool { return true }
	for _, answer := range []string{"no", "", "yes"} {
		calls := 0
		agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
			calls++
			require.Equal(t, "POST", method)
			require.Equal(t, "/v1/federation/hub/admissions/cleanup", path)
			require.True(t, opts.NoRetry)
			if calls == 1 {
				require.NoError(t, json.Unmarshal([]byte(`{"days":30,"cutoff":"2026-09-01T00:00:00Z","instances":[{"instance":"inst_old","name":"old"}]}`), out))
			} else {
				body := in.(map[string]any)
				require.Equal(t, true, body["apply"])
				require.Equal(t, []string{"inst_old"}, body["instances"])
				require.Equal(t, 30, body["days"])
				require.Equal(t, "2026-09-01T00:00:00Z", body["cutoff"].(time.Time).Format(time.RFC3339))
			}
			return nil
		}
		var stdout, stderr bytes.Buffer
		require.Zero(t, runHubStale(&hubStaleParams{Days: 30}, strings.NewReader(answer), &stdout, &stderr))
		require.Contains(t, stdout.String(), "inst_old")
		require.Contains(t, stderr.String(), "Type yes")
		want := 1
		if answer == "yes" {
			want = 2
		}
		require.Equal(t, want, calls)
	}
}
