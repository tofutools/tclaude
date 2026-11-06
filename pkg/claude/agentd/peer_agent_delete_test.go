package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The redaction reaches an environment wherever the export records one, and
// a process's PATH, but keeps names, provenance and number or flag fields.
func TestRedactPeerEnvironmentValues(t *testing.T) {
	var doc any
	require.NoError(t, json.Unmarshal([]byte(`{
		"spawn": {"env": {"TOKEN": "s3cret-a"}, "environment": [{"name": "B", "value": "s3cret-b"}]},
		"sandbox": {"effective": {"pre_launch": [{"name": "gh", "script": "export GH_TOKEN=s3cret-d", "exports": ["GH_TOKEN"]}, {"name": "x", "script": "s3cret-e"}], "environment": [{"name": "C", "value": "s3cret-c"}], "provenance": {"environment": {"C": {"kind": "profile", "name": "dev"}}}},
			"environment_group_id": 7, "refresh_group_environment": true},
		"running": {"live_process": {"path": "/s3cret/bin"}, "execution_boundary": {"path": {"host": "/s3cret/host"}}, "cwd": "/work"}
	}`), &doc))
	var redacted []string
	redactPeerEnvironmentValues(doc, "", false, &redacted)
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NotContains(t, string(out), "s3cret")
	for _, kept := range []string{`"TOKEN"`, `"name":"B"`, `"kind":"profile"`, `"environment_group_id":7`, `"refresh_group_environment":true`, `"cwd":"/work"`, `"exports":["GH_TOKEN"]`, `"name":"gh"`} {
		require.Contains(t, string(out), kept)
	}
	require.ElementsMatch(t, []string{"spawn.env", "spawn.environment", "sandbox.effective.environment", "sandbox.effective.pre_launch.script", "running.live_process.path", "running.execution_boundary.path"}, redacted)
}
