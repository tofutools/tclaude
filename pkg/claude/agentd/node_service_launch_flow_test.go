package agentd_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

type launchErrors struct {
	Code   string `json:"code"`
	Errors []struct {
		Path string `json:"path"`
		Code string `json:"code"`
	} `json:"errors"`
}

func getLaunch(t *testing.T, f *testharness.Flow) agentd.NodeServiceLaunch {
	t.Helper()
	rec := fedHuman(t, f, "GET", "/v1/node/service/launch", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var v agentd.NodeServiceLaunch
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

// shellLaunch runs agentd from a tmux pane with argv and the given
// environment, and fakes the autostart unit writer.
func shellLaunch(t *testing.T, argv []string, env map[string]string) (*agentd.MemFS, launcher.Autostart) {
	t.Helper()
	lenv := launcher.Env{GOOS: "linux", PID: 9, Args: argv,
		Getenv: func(k string) string { return map[string]string{"TMUX": "x"}[k] }, ReadFile: func(string) ([]byte, error) { return nil, errors.New("none") }}
	t.Cleanup(agentd.SetLauncherForTest(lenv, (&recordedRunner{}).run, (&recordedRunner{}).run, func() {}))
	environ := []string{}
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}
	t.Cleanup(agentd.SetSelfUpdateProcessForTest(argv, environ))
	fs := agentd.NewMemFS()
	auto := launcher.Autostart{GOOS: "linux", UID: 1000, User: "u", Paths: launcher.AutostartPaths("linux", "/home/u", ""), Run: (&recordedRunner{out: map[string]string{"": ""}}).run, FS: fs}
	t.Cleanup(agentd.SetAutostartForTest(auto, func(k string) (string, bool) { v, ok := env[k]; return v, ok }))
	return fs, auto
}

// managedUnitLaunch runs agentd under the systemd user unit tclaude wrote,
// with argv and env as its process saw them.
func managedUnitLaunch(t *testing.T, argv []string, env map[string]string) {
	t.Helper()
	lenv, probe := systemdUserEnvFor("process", "tclaude-agentd.service", "# Managed by tclaude\n", "# Managed by tclaude\nPATH=/usr/bin\n")
	t.Cleanup(agentd.SetLauncherForTest(lenv, probe.run, (&recordedRunner{}).run, func() {}))
	environ := []string{}
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}
	t.Cleanup(agentd.SetSelfUpdateProcessForTest(argv, environ))
}

// The operator takes the running launch as the profile, edits it, and the
// autostart unit is written from it; secrets stay out of config.json and the
// unit, and only reach the 0600 env file.
func TestLaunchProfileUseWhatIsRunningAndInstall(t *testing.T) {
	f := newFlow(t)
	argv := []string{"/usr/local/bin/tclaude", "agentd", "serve", "--no-tray", "-s", "/tmp/x.sock", "--dashboard-port", "8123", "--persist-operator-token", "--log-level", "debug"}
	env := map[string]string{"PATH": "/usr/bin", "ANTHROPIC_API_KEY": "sk-secret-1", "LANG": "C.UTF-8", "HOME": "/home/u", "RANDOM_SHELL_VAR": "1",
		"SSH_AUTH_SOCK": "/tmp/ssh-x/agent.1", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus", "XDG_SESSION_ID": "3", "TCLAUDE_HUMAN_TOKEN": "op-tok", "TMUX_PANE": "%1"}
	fs, auto := shellLaunch(t, argv, env)

	v := getLaunch(t, f)
	require.Nil(t, v.Profile)
	require.Equal(t, "none", v.Version)
	require.True(t, v.Owned)
	require.Equal(t, "none", v.Comparison.Baseline)
	require.Equal(t, "unconfigured", v.Comparison.State)
	args := []string{}
	for _, fl := range v.Suggested.Flags {
		args = append(args, fl.Arg)
	}
	require.Equal(t, []string{"--no-tray", "--socket=/tmp/x.sock", "--dashboard-port=8123", "--persist-operator-token", "--log-level=debug"}, args, "flags normalized to one token each; token-placement flags are not secrets")
	for _, fl := range v.Suggested.Flags {
		require.False(t, fl.Secret, fl.Name)
	}
	keys := []string{}
	for _, e := range v.Suggested.Env {
		keys = append(keys, e.Key+":"+e.Mode)
	}
	require.Equal(t, []string{"ANTHROPIC_API_KEY:inherit", "LANG:set", "PATH:inherit"}, keys, "only relevant variables, sorted, secrets and PATH inherited; never the login session's or a pane's")

	// Every suggested flag is accepted as is.
	rec := fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": map[string]any{"flags": args, "env": []any{}, "inherit_env": []any{}}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	require.Equal(t, "not_applied", v.Comparison.State, "a daemon started by hand is not what the profile describes")
	rec = fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": nil})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, launcher.Redacted, v.Suggested.Env[0].Value)
	require.True(t, v.Suggested.Env[0].Secret)

	// Every bad row is named by its path.
	rec = fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": map[string]any{
		"flags":       []string{"--no-tray", "--wizard", "--tui", "--bogus", "--terminal=<redacted>"},
		"env":         []map[string]string{{"key": "ANTHROPIC_API_KEY", "value": "sk"}, {"key": "LANG", "value": "C"}, {"key": "bad key", "value": "1"}},
		"inherit_env": []string{"LANG", "HOME", "PATH"},
	}})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	var bad launchErrors
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bad))
	got := map[string]string{}
	for _, e := range bad.Errors {
		got[e.Path] = e.Code
	}
	require.Equal(t, map[string]string{
		"profile.flags[2]":       "invalid_flag",
		"profile.flags[3]":       "invalid_flag",
		"profile.flags[4]":       "redacted_value",
		"profile.env[0].value":   "secret_value",
		"profile.env[2].key":     "invalid_key",
		"profile.inherit_env[0]": "duplicate_key",
		"profile.inherit_env[1]": "reserved_key",
	}, got)

	// "Use what's running", with one flag removed and one variable missing.
	profile := map[string]any{"flags": []string{"--no-tray", "--dashboard-port=8123"}, "env": []map[string]string{{"key": "LANG", "value": "C.UTF-8"}}, "inherit_env": []string{"ANTHROPIC_API_KEY", "PATH", "NOT_SET"}}
	rec = fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": profile, "version": "none"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	require.NotEqual(t, "none", v.Version)
	require.Equal(t, []string{"NOT_SET"}, v.Missing)
	require.Equal(t, "not_applied", v.Comparison.State)

	// Once agentd runs under the unit tclaude wrote, it is compared.
	managedUnitLaunch(t, argv, env)
	v = getLaunch(t, f)
	require.Equal(t, "profile", v.Comparison.Baseline)
	require.Equal(t, "differs", v.Comparison.State)
	require.Equal(t, []selfupdate.LaunchDifference{
		{Kind: "flag", Change: "added", Name: "--log-level", After: "--log-level=debug"},
		{Kind: "flag", Change: "added", Name: "--persist-operator-token", After: "--persist-operator-token"},
		{Kind: "flag", Change: "added", Name: "--socket", After: "--socket=/tmp/x.sock"},
		{Kind: "env", Change: "removed", Name: "NOT_SET"},
	}, v.Comparison.Differences)
	raw, err := os.ReadFile(config.ConfigPath())
	require.NoError(t, err)
	require.NotContains(t, string(raw), "sk-secret-1", "config.json never holds a secret value")
	saved, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, []string{"ANTHROPIC_API_KEY", "PATH", "NOT_SET"}, saved.ServiceLaunch().InheritEnv)

	// A stale version is refused with the current view.
	rec = fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": profile, "version": "none"})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "launch_profile_conflict")
	require.Contains(t, rec.Body.String(), v.Version)

	// A non-secret value that changed shows before and after locally.
	t.Cleanup(agentd.SetSelfUpdateProcessForTest(argv, []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-secret-2", "LANG=sv_SE.UTF-8", "NOT_SET=1"}))
	v = getLaunch(t, f)
	require.Contains(t, v.Comparison.Differences, selfupdate.LaunchDifference{Kind: "env", Change: "changed", Name: "LANG", Before: "C.UTF-8", After: "sv_SE.UTF-8"})
	require.NotContains(t, func() string { b, _ := json.Marshal(v); return string(b) }(), "sk-secret")

	// Autostart follows the profile; its own options are refused.
	rec = fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "install", "serve_args": []string{}})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "launch_profile_set")
	rec = fedHuman(t, f, "POST", "/v1/node/service/autostart", map[string]any{"action": "install"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var st agentd.NodeService
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, []string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--no-tray", "--dashboard-port=8123"}, st.Autostart.Argv, "the unit's own executable, the profile's flags")
	unit, err := fs.ReadFile(auto.Paths.Unit)
	require.NoError(t, err)
	require.NotContains(t, string(unit), "sk-secret", "the unit carries no value inline")
	envFile, err := fs.ReadFile(auto.Paths.EnvFile)
	require.NoError(t, err)
	require.Contains(t, string(envFile), `ANTHROPIC_API_KEY="sk-secret-2"`, "inherited values come from the environment agentd started with")
	require.Contains(t, string(envFile), `NOT_SET="1"`)
	require.Contains(t, string(envFile), `LANG="C.UTF-8"`)
	require.Equal(t, 0o600, int(fs.Modes[auto.Paths.EnvFile]))

	// Clearing it brings back the observed-launch default.
	rec = fedHuman(t, f, "PUT", "/v1/node/service/launch", map[string]any{"profile": nil})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Nil(t, getLaunch(t, f).Profile)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Nil(t, cfg.Service)
}

// Under a unit tclaude did not write, the profile is never applied; the
// comparison is with that unit, which is what a restart uses.
func TestLaunchProfileComparesWithAnUnownedUnit(t *testing.T) {
	f := newFlow(t)
	env, probe := systemdUserEnvFor("process", "ansible-agentd.service", "[Service]\nExecStart=/x\n", "")
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, (&recordedRunner{}).run, func() {}))
	t.Cleanup(agentd.SetSelfUpdateProcessForTest([]string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--dashboard-port=9"}, []string{"PATH=/usr/bin"}))
	v := getLaunch(t, f)
	require.False(t, v.Owned)
	require.Equal(t, "unit", v.Comparison.Baseline)
	require.Equal(t, []selfupdate.LaunchDifference{
		{Kind: "flag", Change: "added", Name: "--dashboard-port", After: "--dashboard-port=9"},
		{Kind: "flag", Change: "removed", Name: "--no-tray", Before: "--no-tray"},
		{Kind: "env", Change: "removed", Name: "HUB_TOKEN"},
	}, v.Comparison.Differences)
	require.True(t, containsSubstring(v.Warnings, "tclaude never rewrites it"), "%v", v.Warnings)
}

// Peers: node.read sees keys only; node.update.settings may only change
// what an existing profile inherits; an unrestricted peer is the operator.
func TestLaunchProfilePeersSeeKeysOnly(t *testing.T) {
	fh := newFedHarness(t)
	shellLaunch(t, []string{"/usr/local/bin/tclaude", "agentd", "serve"}, map[string]string{"PATH": "/usr/bin", "LANG": "C.UTF-8", "OPENAI_API_KEY": "sk-o"})
	require.NoError(t, config.Save(&config.Config{Service: &config.ServiceConfig{Launch: &config.LaunchProfile{
		Flags: []string{"--no-tray"}, Env: []config.LaunchEnv{{Key: "LANG", Value: "C.UTF-8"}}, InheritEnv: []string{"PATH"}}}}))
	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method string, body any) (int, string) {
		rec := testharness.Serve(peer, testharness.JSONRequest(t, method, "/api/node/service/launch", body))
		return rec.Code, rec.Body.String()
	}
	grant := func(slug string) {
		require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: slug}))
	}
	code, _ := call("GET", nil)
	require.Equal(t, 403, code)
	grant(agentd.PermNodeRead)
	code, body := call("GET", nil)
	require.Equal(t, 200, code, body)
	var v agentd.NodeServiceLaunch
	require.NoError(t, json.Unmarshal([]byte(body), &v))
	require.Equal(t, "read", v.Access)
	require.Equal(t, []config.LaunchEnv{{Key: "LANG"}}, v.Profile.Env)
	for _, e := range v.Suggested.Env {
		require.Empty(t, e.Value, e.Key)
	}
	require.NotContains(t, body, "C.UTF-8")
	require.NotContains(t, body, "/usr/bin")
	code, _ = call("PUT", map[string]any{"profile": nil})
	require.Equal(t, 403, code)

	grant(agentd.PermNodeUpdateSettings)
	code, body = call("PUT", map[string]any{"profile": map[string]any{"flags": []string{"--no-tray", "--pprof"}, "env": []map[string]string{{"key": "LANG"}}, "inherit_env": []string{"PATH"}}})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "local_only_setting")
	code, body = call("PUT", map[string]any{"profile": nil})
	require.Equal(t, 403, code, body)
	code, body = call("PUT", map[string]any{"profile": map[string]any{"flags": []string{"--no-tray"}, "env": []map[string]string{{"key": "LANG"}}, "inherit_env": []string{"PATH", "OPENAI_API_KEY"}}})
	require.Equal(t, 200, code, body)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, []config.LaunchEnv{{Key: "LANG", Value: "C.UTF-8"}}, cfg.ServiceLaunch().Env, "the stored value the peer never saw is kept")
	require.Equal(t, []string{"PATH", "OPENAI_API_KEY"}, cfg.ServiceLaunch().InheritEnv)

	setFedTrustLevel(t, fh, "unrestricted")
	code, body = call("GET", nil)
	require.Equal(t, 200, code)
	require.Contains(t, body, "C.UTF-8")
}

// After an update restart, the job also records how the daemon compares
// with its launch profile (keys only).
func TestSelfUpdateRecordsTheProfileAsExpectedLaunch(t *testing.T) {
	f := newFlow(t)
	host := &selfUpdateHost{}
	host.install(t)
	dir := testutil.CanonicalTempDir(t)
	bin := fakeBinary(t, dir, "bin/tclaude", "v1.0.0")
	argv := []string{bin, "agentd", "serve", "--no-tray"}
	managedUnitLaunch(t, argv, map[string]string{"PATH": "/usr/bin", "LANG": "C"})
	svc := selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "release"}})
	require.NoError(t, config.Save(&config.Config{Service: &config.ServiceConfig{Launch: &config.LaunchProfile{
		Flags: []string{"--no-tray"}, Env: []config.LaunchEnv{{Key: "LANG", Value: "en_US.UTF-8"}}, InheritEnv: []string{}}}}))
	rec := fedHuman(t, f, "POST", "/v1/node/update", map[string]any{"action": "apply", "version": "v1.1.0"})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	fedEventuallyWithin(t, "apply to restart", 5*time.Second, func() bool {
		j := svc.Status().Job
		return j != nil && j.State == "restarting"
	})
	restarted := selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: bin, Version: "v1.1.0", Method: "release"}})
	agentd.FinishRestartedUpdateForTest(restarted)
	job := restarted.Status().Job
	require.Equal(t, "identical", job.Launch.State, job.Launch.Differences)
	require.Equal(t, &selfupdate.ExpectedLaunch{Baseline: "profile", State: "differs", Differences: []selfupdate.LaunchDifference{{Kind: "env", Change: "changed", Name: "LANG"}}}, job.Launch.Expected)
	require.True(t, containsSubstring(job.Warnings, "agentd does not run as its launch profile says"), "%v", job.Warnings)
}
