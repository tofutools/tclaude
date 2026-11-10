package agentd_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func testAppLauncher(goos string) (launcher.AppLauncher, *agentd.MemFS, *recordedRunner) {
	fs := agentd.NewMemFS()
	run := &recordedRunner{out: map[string]string{"": ""}}
	return launcher.AppLauncher{GOOS: goos, Paths: launcher.AppLauncherPathsFor(goos, "/home/u", ""), Run: run.run, FS: fs,
		Current: launcher.AppLauncherSpec{Exe: "/home/u/.local/bin/tclaude", Version: "v2.0.0", PATH: "/home/u/.local/bin:/usr/bin"}}, fs, run
}

func TestNodeLauncherInstallStatusUninstall(t *testing.T) {
	f := newFlow(t)
	for _, goos := range []string{"linux", "darwin"} {
		l, fs, _ := testAppLauncher(goos)
		restore := agentd.SetAppLauncherForTest(l)

		rec := fedHuman(t, f, "GET", "/v1/node/launcher", nil)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var st agentd.NodeAppLauncher
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		require.True(t, st.Supported)
		require.False(t, st.Installed)
		require.Equal(t, "full", st.Access)
		require.NotNil(t, st.Warnings)

		rec = fedHuman(t, f, "POST", "/v1/node/launcher/install", map[string]any{})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		require.True(t, st.Installed && st.Managed && !st.Stale, "%+v", st)
		require.True(t, st.Options.StartDaemon, "start_daemon defaults on")
		require.Equal(t, "/home/u/.local/bin/tclaude", st.Exe)
		require.Len(t, fs.Files, 3, "%s: %v", goos, fs.Files)

		rec = fedHuman(t, f, "POST", "/v1/node/launcher/install", map[string]any{"start_daemon": false})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		require.False(t, st.Options.StartDaemon)

		for _, bad := range []map[string]any{{"start_daemon": "yes"}, {"exe": "/tmp/evil"}} {
			rec = fedHuman(t, f, "POST", "/v1/node/launcher/install", bad)
			require.Equal(t, 400, rec.Code, "%v: %s", bad, rec.Body.String())
		}
		rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/node/launcher/uninstall", map[string]any{}), "reader"))
		require.Equal(t, 403, rec.Code, "agents may not manage the launcher")

		rec = fedHuman(t, f, "POST", "/v1/node/launcher/uninstall", map[string]any{})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		require.False(t, st.Installed)
		require.Empty(t, fs.Files)
		restore()
	}
}

func TestNodeLauncherRefusesHandMadeLauncher(t *testing.T) {
	f := newFlow(t)
	l, fs, _ := testAppLauncher("linux")
	t.Cleanup(agentd.SetAppLauncherForTest(l))
	require.NoError(t, fs.WriteFile(l.Paths.Desktop, []byte("[Desktop Entry]\nExec=/mine\n"), 0o644))
	for _, path := range []string{"/v1/node/launcher/install", "/v1/node/launcher/uninstall"} {
		rec := fedHuman(t, f, "POST", path, map[string]any{})
		require.Equal(t, 409, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "launcher_not_managed")
	}
	rec := fedHuman(t, f, "GET", "/v1/node/launcher", nil)
	require.Contains(t, rec.Body.String(), `"installed":true,"managed":false`)
	got, err := fs.ReadFile(l.Paths.Desktop)
	require.NoError(t, err)
	require.Equal(t, "[Desktop Entry]\nExec=/mine\n", string(got))
}

// agentd start rewrites a managed launcher that names another binary, with
// its options, and leaves a missing or hand-made one alone.
func TestNodeLauncherRewrittenWhenStale(t *testing.T) {
	newFlow(t)
	l, _, _ := testAppLauncher("darwin")
	old := l
	old.Current.Exe, old.Current.Version = "/old/bin/tclaude", "v1.0.0"
	spec := old.Current
	spec.Options.StartDaemon = false
	require.NoError(t, old.Install(t.Context(), spec))
	t.Cleanup(agentd.SetAppLauncherForTest(l))
	require.True(t, l.Status().Stale)

	agentd.RefreshStaleAppLauncherForTest()
	st := l.Status()
	require.False(t, st.Stale, "%+v", st)
	require.Equal(t, "/home/u/.local/bin/tclaude", st.Exe)
	require.Equal(t, "v2.0.0", st.Version)
	require.False(t, st.Options.StartDaemon, "options survive the rewrite")

	l2, fs2, _ := testAppLauncher("linux")
	t.Cleanup(agentd.SetAppLauncherForTest(l2))
	agentd.RefreshStaleAppLauncherForTest()
	require.Empty(t, fs2.Files, "never installs one that is not there")
}

// Peers: node.read reads, node.update.settings installs and uninstalls.
func TestNodeLauncherPeerAccess(t *testing.T) {
	fh := newFedHarness(t)
	l, fs, _ := testAppLauncher("linux")
	t.Cleanup(agentd.SetAppLauncherForTest(l))
	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) (int, string) {
		rec := testharness.Serve(peer, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	grant := func(slug string) {
		require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: slug}))
	}
	code, _ := call("GET", "/api/node/launcher", nil)
	require.Equal(t, 403, code)
	grant(agentd.PermNodeRead)
	code, body := call("GET", "/api/node/launcher", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"access":"read"`)
	code, _ = call("POST", "/api/node/launcher/install", map[string]any{})
	require.Equal(t, 403, code)
	require.Empty(t, fs.Files)

	grant(agentd.PermNodeUpdateSettings)
	code, body = call("POST", "/api/node/launcher/install", map[string]any{"serve_args": []string{"--dashboard-port=8123", "--pprof"}})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "local_only_setting")
	code, body = call("POST", "/api/node/launcher/install", map[string]any{"start_daemon": true, "serve_args": []string{"--dashboard-port=8123", "--no-tray", "--persist-operator-token"}})
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"access":"manage"`)
	require.True(t, strings.Contains(body, `"managed":true`), body)
	code, body = call("POST", "/api/node/launcher/uninstall", map[string]any{})
	require.Equal(t, 200, code, body)
	require.Empty(t, fs.Files)
}

// The serve flags the launcher starts agentd with: tclaude's defaults, the
// launcher's own setting, or the launch profile, which overrides it.
func TestNodeLauncherServeArgs(t *testing.T) {
	f := newFlow(t)
	l, fs, _ := testAppLauncher("linux")
	t.Cleanup(agentd.SetAppLauncherForTest(l))
	script := func() string {
		b, err := fs.ReadFile(l.Paths.Script)
		require.NoError(t, err)
		return string(b)
	}
	install := func(body map[string]any) agentd.NodeAppLauncher {
		t.Helper()
		rec := fedHuman(t, f, "POST", "/v1/node/launcher/install", body)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var st agentd.NodeAppLauncher
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		return st
	}

	st := install(map[string]any{})
	require.Equal(t, "default", st.ServeArgsSource)
	require.Equal(t, []string{"--persist-operator-token", "--dashboard-port=7878", "--no-print-human-token"}, st.ServeArgs)
	require.Equal(t, st.ServeArgs, st.DefaultServeArgs)
	require.Equal(t, st.ServeArgs, st.InstalledServeArgs)
	require.Contains(t, script(), "--start-daemon -- '--persist-operator-token' '--dashboard-port=7878' '--no-print-human-token'")

	st = install(map[string]any{"serve_args": []string{"--dashboard-port=8123", "--no-tray"}})
	require.Equal(t, "launcher", st.ServeArgsSource)
	require.Equal(t, []string{"--dashboard-port=8123", "--no-tray"}, st.InstalledServeArgs)
	require.True(t, containsSubstring(st.Warnings, "do not persist the operator token"), "%v", st.Warnings)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, []string{"--dashboard-port=8123", "--no-tray"}, cfg.AppLauncherServeArgs())

	// Omitted keeps the setting.
	st = install(map[string]any{"start_daemon": true})
	require.Equal(t, "launcher", st.ServeArgsSource)

	for _, bad := range [][]string{{"--bogus"}, {"--tui"}, {"positional"}, {"--terminal=x'; evil; '"}, {"--print-human-token=always"}} {
		rec := fedHuman(t, f, "POST", "/v1/node/launcher/install", map[string]any{"serve_args": bad})
		require.Equal(t, 400, rec.Code, "%q: %s", bad, rec.Body.String())
	}

	// A launch profile overrides it; changing the profile rewrites the launcher.
	rec := fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": map[string]any{"flags": []string{"--persist-operator-token", "--dashboard-port=9000"}, "env": []any{}, "inherit_env": []string{}}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, script(), "'--dashboard-port=9000'", "the profile change rewrote the launcher")
	rec = fedHuman(t, f, "GET", "/v1/node/launcher", nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, "profile", st.ServeArgsSource)
	require.False(t, st.Stale)
	st = install(map[string]any{"serve_args": []string{"--no-tray"}})
	require.True(t, containsSubstring(st.Warnings, "launch profile is set"), "%v", st.Warnings)

	// null goes back to the defaults once the profile is gone.
	rec = fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": nil})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	st = install(map[string]any{"serve_args": nil})
	require.Equal(t, "default", st.ServeArgsSource)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Nil(t, cfg.AppLauncherServeArgs())
}

func TestNodeLauncherWarnsAboutAnInMemoryOperatorToken(t *testing.T) {
	f := newFlow(t)
	l, _, _ := testAppLauncher("linux")
	t.Cleanup(agentd.SetAppLauncherForTest(l))
	t.Cleanup(agentd.SetOperatorTokenStoreForTest("ephemeral"))
	rec := fedHuman(t, f, "GET", "/v1/node/launcher", nil)
	var st agentd.NodeAppLauncher
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, "ephemeral", st.OperatorTokenStore)
	require.True(t, containsSubstring(st.Warnings, "--persist-operator-token"), "%v", st.Warnings)
	agentd.SetOperatorTokenStoreForTest("file")
	rec = fedHuman(t, f, "GET", "/v1/node/launcher", nil)
	require.NotContains(t, rec.Body.String(), "in memory only")
}
