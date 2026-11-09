package agentd_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type recordedRunner struct {
	mu    sync.Mutex
	out   map[string]string
	fail  error
	calls []string
}

func (r *recordedRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, cmd)
	if r.fail != nil {
		return nil, r.fail
	}
	for prefix, out := range r.out {
		if strings.HasPrefix(cmd, prefix) {
			return []byte(out), nil
		}
	}
	return nil, errors.New("unexpected command " + cmd)
}

func (r *recordedRunner) called() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func systemdUserEnv(killMode string) (launcher.Env, *recordedRunner) {
	return systemdUserEnvFor(killMode, "tclaude-agentd.service", "[Service]\nExecStart=/x\n", "")
}

// systemdUserEnvFor is agentd running as the main process of a user unit
// named name, whose file holds unitFile. A non-empty envFileContent makes
// the unit's only environment tclaude's own agentd.env.
func systemdUserEnvFor(killMode, name, unitFile, envFileContent string) (launcher.Env, *recordedRunner) {
	unit := "/home/u/.config/systemd/user/" + name
	files := map[string]string{
		"/proc/4242/cgroup": "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + name + "\n",
		unit:                unitFile,
	}
	envFiles := ""
	if envFileContent != "" {
		files["/home/u/.config/tclaude/agentd.env"] = envFileContent
		envFiles = "EnvironmentFiles=/home/u/.config/tclaude/agentd.env (ignore_errors=no)\n"
	}
	env := launcher.Env{GOOS: "linux", PID: 4242, UID: 1000, EUID: 1000,
		Args:   []string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--no-tray"},
		Getenv: func(k string) string { return map[string]string{"INVOCATION_ID": "x"}[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}}
	inline := "Environment=PATH=/usr/bin HUB_TOKEN=s3cret\n"
	if envFileContent != "" {
		inline = ""
	}
	probe := &recordedRunner{out: map[string]string{"systemctl --user show " + name: "Id=" + name + "\nMainPID=4242\n" +
		"ExecStart={ path=/home/u/.local/bin/tclaude ; argv[]=/home/u/.local/bin/tclaude agentd serve --no-tray ; ignore_errors=no }\n" +
		inline + envFiles + "Restart=on-failure\nKillMode=" + killMode + "\nFragmentPath=" + unit + "\n"}}
	return env, probe
}

func TestNodeServiceStatusReportsSystemdLauncherWithSecretsRedacted(t *testing.T) {
	f := newFlow(t)
	env, probe := systemdUserEnv("process")
	restart := &recordedRunner{out: map[string]string{"": ""}}
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, restart.run, func() { t.Error("supervised restart must not stop for re-exec") }))

	rec := fedHuman(t, f, "GET", "/v1/node/service", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "s3cret")
	var st agentd.NodeService
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, launcher.KindSystemdUser, st.Launcher.Kind)
	require.Equal(t, "tclaude-agentd.service", st.Launcher.Unit)
	require.Equal(t, []string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--no-tray"}, st.Launcher.Argv)
	require.Equal(t, []launcher.EnvVar{{Key: "PATH", Value: "/usr/bin"}, {Key: "HUB_TOKEN", Value: launcher.Redacted}}, st.Launcher.Env)
	require.False(t, st.Launcher.ManagedByTclaude)
	require.Equal(t, launcher.RestartSupervisor, st.Launcher.RestartMethod)

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()
	rec = testharness.Serve(dash, testharness.JSONRequest(t, "GET", "/api/node/service", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"kind":"systemd-user"`)

	// Agents have no path to it.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/node/service", nil), "reader"))
	require.Equal(t, 403, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/node/service/restart", map[string]any{}), "reader"))
	require.Equal(t, 403, rec.Code)
}

func TestNodeServiceRestartGoesThroughSystemd(t *testing.T) {
	f := newFlow(t)
	env, probe := systemdUserEnv("process")
	restart := &recordedRunner{out: map[string]string{"": ""}}
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, restart.run, func() { t.Error("supervised restart must not stop for re-exec") }))

	rec := fedHuman(t, f, "POST", "/v1/node/service/restart", map[string]any{"argv": []string{"x"}})
	require.Equal(t, 400, rec.Code, "restart takes no parameters")
	rec = fedHuman(t, f, "POST", "/v1/node/service/restart", map[string]any{})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var res agentd.NodeRestartResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	require.Equal(t, agentd.NodeRestartResult{Method: launcher.RestartSupervisor, Launcher: launcher.KindSystemdUser}, res)
	require.Eventually(t, func() bool { return len(restart.called()) == 1 }, 5*time.Second, 5*time.Millisecond, "systemctl restart never ran")
	require.Equal(t, []string{"systemctl --user --no-block restart tclaude-agentd.service"}, restart.called())
	require.False(t, agentd.ExecRestartRequestedForTest())

	// The self-updater's hook takes the same path and does not double-restart.
	method, err := agentd.RequestDaemonRestart("self-update")
	require.NoError(t, err)
	require.Equal(t, launcher.RestartSupervisor, method)
	rec = fedHuman(t, f, "GET", "/v1/node/service", nil)
	require.Contains(t, rec.Body.String(), `"restart_pending":"supervisor"`)
	require.Len(t, restart.called(), 1)
}

func TestNodeServiceRestartFallsBackToInPlaceExecWhenSystemdRefuses(t *testing.T) {
	f := newFlow(t)
	env, probe := systemdUserEnv("process")
	restart := &recordedRunner{fail: errors.New("access denied")}
	stopped := make(chan struct{}, 1)
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, restart.run, func() { stopped <- struct{}{} }))

	rec := fedHuman(t, f, "POST", "/v1/node/service/restart", nil)
	require.Equal(t, 202, rec.Code, rec.Body.String())
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon never stopped for the in-place re-exec fallback")
	}
	require.True(t, agentd.ExecRestartRequestedForTest())
	rec = fedHuman(t, f, "GET", "/v1/node/service", nil)
	require.Contains(t, rec.Body.String(), `"restart_pending":"exec"`)
}

func TestNodeServiceRestartKeepsTmuxAliveUnderControlGroupKillMode(t *testing.T) {
	f := newFlow(t)
	env, probe := systemdUserEnv("control-group")
	restart := &recordedRunner{out: map[string]string{"": ""}}
	stopped := make(chan struct{}, 1)
	t.Cleanup(agentd.SetLauncherForTest(env, probe.run, restart.run, func() { stopped <- struct{}{} }))

	rec := fedHuman(t, f, "GET", "/v1/node/service", nil)
	require.Contains(t, rec.Body.String(), "KillMode=control-group")
	rec = fedHuman(t, f, "POST", "/v1/node/service/restart", nil)
	require.Equal(t, 202, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"method":"exec"`)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon never stopped for the in-place re-exec")
	}
	require.Empty(t, restart.called(), "a unit restart would kill the agents' tmux server")
}
