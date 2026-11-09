package agentd_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestNodeServiceAutostartKeepsObservedLaunchParams(t *testing.T) {
	f := newFlow(t)
	env, probe := systemdUserEnv("process")
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, (&recordedRunner{}).run, func() {}))
	fs := agentd.NewMemFS()
	runner := &recordedRunner{out: map[string]string{"systemctl --user is-enabled": "enabled\n", "systemctl --user": "", "loginctl show-user": "yes\n"}}
	auto := launcher.Autostart{GOOS: "linux", UID: 1000, User: "u", Paths: launcher.AutostartPaths("linux", "/home/u", ""), Run: runner.run, FS: fs}
	procEnv := map[string]string{"PATH": "/home/u/.local/bin:/usr/bin", "HUB_TOKEN": "s3cret"}
	t.Cleanup(agentd.SetAutostartForTest(auto, func(k string) (string, bool) { v, ok := procEnv[k]; return v, ok }))

	rec := fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "install"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "s3cret")
	var st agentd.NodeService
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.True(t, st.Autostart.Installed && st.Autostart.Managed && st.Autostart.Enabled, "%+v", st.Autostart)
	require.Equal(t, []string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--no-tray"}, st.Autostart.Argv, "default keeps the observed unit argv")
	require.Equal(t, []launcher.EnvVar{{Key: "HUB_TOKEN", Value: launcher.Redacted}, {Key: "PATH", Value: "/home/u/.local/bin:/usr/bin"}}, st.Autostart.Env)
	envFile, err := fs.ReadFile(auto.Paths.EnvFile)
	require.NoError(t, err)
	require.Contains(t, string(envFile), `HUB_TOKEN="s3cret"`, "kept env carries the real value into the 0600 env file")
	require.Equal(t, 0o600, int(fs.Modes[auto.Paths.EnvFile]))

	for _, bad := range [][]string{{"--tui"}, {"--bogus"}, {"positional"}, {"--print-human-token=always"}, {"--terminal", "x\nExecStartPre=/bin/evil"}} {
		rec = fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "install", "serve_args": bad})
		require.Equal(t, 400, rec.Code, "%q: %s", bad, rec.Body.String())
	}
	rec = fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "install", "serve_args": []string{"--dashboard-port", "9000"}, "keep_env": false})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, []string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--dashboard-port", "9000"}, st.Autostart.Argv)
	require.Equal(t, []launcher.EnvVar{{Key: "PATH", Value: "/home/u/.local/bin:/usr/bin"}}, st.Autostart.Env)

	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/node/service/autostart", map[string]any{"action": "uninstall"}), "reader"))
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "uninstall"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.False(t, st.Autostart.Installed)
	require.Empty(t, fs.Files)
	for _, c := range runner.called() {
		require.False(t, strings.Contains(c, "start ") || strings.Contains(c, "--now"), "autostart must not start a second agentd: %s", c)
	}
}

func TestNodeServiceAutostartRefusesHandWrittenUnit(t *testing.T) {
	f := newFlow(t)
	fs := agentd.NewMemFS()
	auto := launcher.Autostart{GOOS: "linux", User: "u", Paths: launcher.AutostartPaths("linux", "/home/u", ""), Run: (&recordedRunner{out: map[string]string{"": ""}}).run, FS: fs}
	require.NoError(t, fs.WriteFile(auto.Paths.Unit, []byte("[Service]\nExecStart=/mine\n"), 0o644))
	t.Cleanup(agentd.SetAutostartForTest(auto, func(string) (string, bool) { return "/usr/bin", true }))
	rec := fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "uninstall"})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "unit_not_managed")
	rec = fedHuman(t, f, "GET", "/v1/node/service", nil)
	require.Contains(t, rec.Body.String(), `"installed":true,"managed":false`)
}
