package agentd_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestNodeServiceFleetGrantsReadManageAndKeepServeFlagsLocal(t *testing.T) {
	fh := newFedHarness(t)
	env, probe := systemdUserEnv("process")
	restart := &recordedRunner{out: map[string]string{"": ""}}
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, restart.run, func() { t.Error("supervised restart must not stop for re-exec") }))
	runner := &recordedRunner{out: map[string]string{"systemctl --user is-enabled": "enabled\n", "systemctl --user": "", "loginctl show-user": "yes\n"}}
	auto := launcher.Autostart{GOOS: "linux", UID: 1000, User: "u", Paths: launcher.AutostartPaths("linux", "/home/u", ""), Run: runner.run, FS: agentd.NewMemFS()}
	procEnv := map[string]string{"PATH": "/usr/bin", "HUB_TOKEN": "s3cret"}
	t.Cleanup(agentd.SetAutostartForTest(auto, func(k string) (string, bool) { v, ok := procEnv[k]; return v, ok }))

	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) (int, agentd.NodeService, string) {
		rec := testharness.Serve(peer, testharness.JSONRequest(t, method, path, body))
		var st agentd.NodeService
		_ = json.Unmarshal(rec.Body.Bytes(), &st)
		return rec.Code, st, rec.Body.String()
	}
	grant := func(slug string) {
		require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: slug}))
	}
	revoke := func(slug string) {
		_, err := db.DeleteFederationPeerGrant(fh.peer.id.ID(), slug, "")
		require.NoError(t, err)
	}

	code, _, _ := call("GET", "/api/node/service", nil)
	require.Equal(t, 403, code, "reading needs node.read")
	grant(agentd.PermNodeUpdateSettings)
	code, _, _ = call("POST", "/api/node/service/restart", map[string]any{})
	require.Equal(t, 403, code, "a change also needs node.read, which guards the returned view")
	revoke(agentd.PermNodeUpdateSettings)

	// node.read: the launcher without environment values, read only.
	grant(agentd.PermNodeRead)
	code, st, body := call("GET", "/api/node/service", nil)
	require.Equal(t, 200, code, body)
	require.Equal(t, "read", st.Access)
	require.Equal(t, launcher.KindSystemdUser, st.Launcher.Kind)
	require.Equal(t, []launcher.EnvVar{{Key: "PATH"}, {Key: "HUB_TOKEN"}}, st.Launcher.Env)
	require.NotContains(t, body, "/usr/bin")
	require.Contains(t, st.Redacted, "launcher.env")
	code, _, _ = call("POST", "/api/node/service/restart", map[string]any{})
	require.Equal(t, 403, code, "node.read cannot restart")
	code, _, _ = call("POST", "/api/node/service/autostart", map[string]any{"action": "install"})
	require.Equal(t, 403, code, "node.read cannot install autostart")
	require.Empty(t, restart.called())

	// node.update.settings: restart and autostart with the observed flags.
	grant(agentd.PermNodeUpdateSettings)
	code, _, body = call("POST", "/api/node/service/autostart", map[string]any{"action": "install", "serve_args": []string{"--dashboard-port", "1"}})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "local_only_setting")
	code, st, body = call("POST", "/api/node/service/autostart", map[string]any{"action": "install"})
	require.Equal(t, 200, code, body)
	require.Equal(t, "manage", st.Access)
	require.True(t, st.Autostart.Installed)
	require.NotContains(t, body, "/usr/bin", "the unit's environment is withheld too")
	code, _, body = call("POST", "/api/node/service/restart", map[string]any{})
	require.Equal(t, 202, code, body)
	require.Eventually(t, func() bool { return len(restart.called()) == 1 }, 5*time.Second, 5*time.Millisecond, "systemctl restart never ran")

	// An unrestricted peer acts as the node's operator.
	setFedTrustLevel(t, fh, "unrestricted")
	code, st, _ = call("GET", "/api/node/service", nil)
	require.Equal(t, 200, code)
	require.Equal(t, "full", st.Access)
	require.Contains(t, st.Launcher.Env, launcher.EnvVar{Key: "PATH", Value: "/usr/bin"})
	require.Empty(t, st.Redacted)
	code, _, body = call("POST", "/api/node/service/autostart", map[string]any{"action": "install", "serve_args": []string{"--no-tray"}})
	require.Equal(t, 200, code, body)

	rec := fedHuman(t, fh.f, "GET", "/v1/federation/audit", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "node.service.autostart")
	require.Contains(t, rec.Body.String(), "operator@"+fh.peer.id.ID())
}
