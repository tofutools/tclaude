package agentd_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/updatescripts"
)

func TestNodeUpdateScriptsReportPendingAndRunUnderNodeUpdate(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.ResetNodeUpdateScriptsForTest())
	// Given: this node's operator configured a JSON status script with an
	// update, and a plain-text status script without one.
	require.NoError(t, config.Save(&config.Config{Updates: &config.UpdatesConfig{Scripts: []config.UpdateScriptConfig{
		{Name: "tools", Label: "Dev tools", Status: `if [ -f "$HOME/updated" ]; then echo '{"items":[{"name":"jq","current":"1.8","latest":"1.8","update_available":false}]}'; else echo '{"summary":"1 pending","items":[{"name":"jq","current":"1.7","latest":"1.8","update_available":true}]}'; fi`, Update: `touch "$HOME/updated"; echo upgraded jq`},
		{Name: "os", Status: "echo 'OS current'"},
	}}}))
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	get := func() (int, updatescripts.Overview) {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/node/update-scripts", nil))
		var o updatescripts.Overview
		_ = json.Unmarshal(rec.Body.Bytes(), &o)
		return rec.Code, o
	}
	code, _ := get()
	require.Equal(t, 403, code, "a peer needs node.update")
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeUpdate}))

	// When: the peer reads the overview, the never-checked scripts are checked.
	var o updatescripts.Overview
	require.Eventually(t, func() bool {
		code, o = get()
		return code == 200 && len(o.Scripts) == 2 && o.Scripts[0].State == "ok" && o.Scripts[1].State == "ok"
	}, 5*time.Second, 20*time.Millisecond)
	tools := o.Scripts[0]
	require.Equal(t, "Dev tools", tools.Label)
	require.Equal(t, "1 pending", tools.Summary)
	require.True(t, *tools.UpdateAvailable)
	require.Equal(t, "1.8", tools.Items[0].Latest)
	require.Equal(t, "OS current", o.Scripts[1].Summary)
	require.False(t, *o.Scripts[1].UpdateAvailable)
	require.False(t, o.Scripts[1].HasUpdate)

	// A command never comes from the request.
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/node/update-scripts", map[string]any{"action": "update", "name": "tools", "command": "rm -rf /"}))
	require.Equal(t, 400, rec.Code)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/node/update-scripts", map[string]any{"action": "update", "name": "os"}))
	require.Equal(t, 400, rec.Code, "a script without an update command cannot be run")

	// When: the peer runs the update, it succeeds and the status re-checks.
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/node/update-scripts", map[string]any{"action": "update", "name": "tools"}))
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var job updatescripts.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job))
	require.Eventually(t, func() bool {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/node/update-scripts/jobs/"+job.ID, nil))
		return rec.Code == 200 && json.Unmarshal(rec.Body.Bytes(), &job) == nil && job.State != "running"
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "succeeded", job.State, job.Error)
	require.Equal(t, "upgraded jq", job.Output)
	require.Equal(t, "operator@"+fh.peer.id.ID(), job.Actor)
	require.Eventually(t, func() bool {
		code, o = get()
		return code == 200 && o.Scripts[0].State == "ok" && o.Scripts[0].UpdateAvailable != nil && !*o.Scripts[0].UpdateAvailable
	}, 5*time.Second, 20*time.Millisecond)
}
