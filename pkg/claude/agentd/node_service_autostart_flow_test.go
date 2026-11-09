package agentd_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestNodeServiceAutostartKeepsObservedLaunchParams(t *testing.T) {
	f := newFlow(t)
	// Migrating from a hand-written unit of another name.
	env, probe := systemdUserEnvFor("process", "my-agentd.service", "[Service]\nExecStart=/x\n", "")
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
	require.True(t, containsSubstring(st.Autostart.Warnings, "my-agentd.service"), "warns that the old unit would start too: %v", st.Autostart.Warnings)
	unitFile, err := fs.ReadFile(auto.Paths.Unit)
	require.NoError(t, err)
	require.Contains(t, string(unitFile), "RestartPreventExitStatus=75")

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

func TestNodeServiceAutostartReinstallUnderOwnUnitKeepsItsEnv(t *testing.T) {
	f := newFlow(t)
	// agentd runs under the managed unit, its env only from agentd.env.
	env, probe := systemdUserEnvFor("process", "tclaude-agentd.service", "# Managed by tclaude\n", "# Managed by tclaude\nPATH=/usr/bin\nANTHROPIC_API_KEY=sk-1\n")
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, (&recordedRunner{}).run, func() {}))
	fs := agentd.NewMemFS()
	runner := &recordedRunner{out: map[string]string{"systemctl --user": "", "loginctl": "yes\n"}}
	auto := launcher.Autostart{GOOS: "linux", UID: 1000, User: "u", Paths: launcher.AutostartPaths("linux", "/home/u", ""), Run: runner.run, FS: fs}
	require.NoError(t, fs.WriteFile(auto.Paths.Unit, []byte("# Managed by tclaude\n"), 0o644))
	require.NoError(t, fs.WriteFile(auto.Paths.EnvFile, []byte("# Managed by tclaude\nPATH=/usr/bin\nANTHROPIC_API_KEY=sk-1\n"), 0o600))
	procEnv := map[string]string{"PATH": "/usr/bin", "ANTHROPIC_API_KEY": "sk-1"}
	t.Cleanup(agentd.SetAutostartForTest(auto, func(k string) (string, bool) { v, ok := procEnv[k]; return v, ok }))

	rec := fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "install", "serve_args": []string{"--dashboard-port", "9000"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	envFile, err := fs.ReadFile(auto.Paths.EnvFile)
	require.NoError(t, err)
	require.Contains(t, string(envFile), `ANTHROPIC_API_KEY="sk-1"`)
	unit, err := fs.ReadFile(auto.Paths.Unit)
	require.NoError(t, err)
	require.NotContains(t, string(unit), "EnvironmentFile=-/home/u/.config/tclaude/agentd.env", "own env file is not referenced twice")
}

func TestNodeServiceAutostartFromTUILaunchDropsTerminalFlags(t *testing.T) {
	f := newFlow(t)
	env := launcher.Env{GOOS: "linux", PID: 9, Args: []string{"/usr/local/bin/tclaude", "agentd", "serve", "--tui", "--own-tmux-server", "--dashboard-port", "7878"},
		Getenv: func(k string) string { return map[string]string{"TMUX": "x"}[k] }, ReadFile: func(string) ([]byte, error) { return nil, errors.New("none") }}
	t.Cleanup(agentd.SetLauncherForTest(env, (&recordedRunner{}).run, (&recordedRunner{}).run, func() {}))
	fs := agentd.NewMemFS()
	auto := launcher.Autostart{GOOS: "linux", User: "u", Paths: launcher.AutostartPaths("linux", "/home/u", ""), Run: (&recordedRunner{out: map[string]string{"": ""}}).run, FS: fs}
	t.Cleanup(agentd.SetAutostartForTest(auto, func(k string) (string, bool) { return "/usr/bin", k == "PATH" }))
	rec := fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "install"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var st agentd.NodeService
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, []string{"/usr/local/bin/tclaude", "agentd", "serve", "--dashboard-port", "7878"}, st.Autostart.Argv)
	require.True(t, containsSubstring(st.Autostart.Warnings, "--tui"), "%v", st.Autostart.Warnings)
	require.True(t, containsSubstring(st.Autostart.Warnings, "only PATH was captured"), "%v", st.Autostart.Warnings)
}
