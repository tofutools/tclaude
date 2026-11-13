package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// swapTerminalLoginEnv makes the login shell answer with login (or fail
// with err) and counts the questions. The answer goes through
// launcher.TerminalEnv's own seed and merge.
func swapTerminalLoginEnv(t *testing.T, login []string, err error) *int {
	t.Helper()
	asked := 0
	prev := terminalLoginEnv
	terminalLoginEnv = func(_ context.Context, environ []string) ([]string, error) {
		asked++
		if err != nil {
			return nil, err
		}
		if login == nil {
			return nil, nil
		}
		return launcher.MergeTerminalEnv(launcher.SessionSeed(runtime.GOOS, environ), login, environ), nil
	}
	t.Cleanup(func() { terminalLoginEnv = prev })
	return &asked
}

// captureSlog sends slog to a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// newToolchainFirst reports a PATH that finds the login shell's Go 1.27.4
// first; the old directory may stay after it, as a fallback.
func newToolchainFirst(path string) bool {
	dirs := strings.Split(path, ":")
	i, j := slices.Index(dirs, "/opt/go-1.27.4/bin"), slices.Index(dirs, "/opt/go-1.27.3/bin")
	return i == 0 && (j < 0 || j > i)
}

// A restart takes the login shell's environment as it is now: the new
// toolchain's PATH, without the stale profile variable the old shell set,
// while the launch's own variables stay (the terminal and tmux pane it runs
// in, tclaude's markers and operator token, the launch profile's).
func TestRestartEnvironRefreshesFromTheLoginShell(t *testing.T) {
	asked := swapTerminalLoginEnv(t, []string{"HOME=/h", "PATH=/opt/go-1.27.4/bin:/usr/bin", "EDITOR=vim"}, nil)
	logs := captureSlog(t)
	environ := []string{
		"HOME=/h", "PATH=/opt/go-1.27.3/bin:/usr/bin", "GOROOT=/opt/go-1.27.3",
		"TMUX=/tmp/tmux-1/default,1,0", "SHLVL=2", "TERM_PROGRAM=iTerm.app",
		"TCLAUDE_HUMAN_TOKEN=tok", "TCLAUDE_CONFIG_DIR=/c", "ANTHROPIC_BASE_URL=http://p",
	}
	profile := &config.LaunchProfile{Env: []config.LaunchEnv{{Key: "ANTHROPIC_BASE_URL", Value: "http://p"}}}
	got := envMap(restartEnviron(context.Background(), environ, launcher.Info{Kind: launcher.KindShell}, profile))
	if *asked != 1 {
		t.Fatalf("login shell asked %d times", *asked)
	}
	if !newToolchainFirst(got["PATH"]) {
		t.Errorf("PATH = %q, want the login shell's Go first", got["PATH"])
	}
	for k, want := range map[string]string{
		"EDITOR": "vim",
		"TMUX":   "/tmp/tmux-1/default,1,0", "SHLVL": "2", "TERM_PROGRAM": "iTerm.app",
		"TCLAUDE_HUMAN_TOKEN": "tok", "TCLAUDE_CONFIG_DIR": "/c", "ANTHROPIC_BASE_URL": "http://p",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for _, k := range []string{"GOROOT", launcher.LoginEnvErrorVar, loginEnvAppliedVar} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q, want it unset", k, v)
		}
	}
	if !strings.Contains(logs.String(), "refreshed the environment from the login shell") {
		t.Errorf("no log of the refresh:\n%s", logs)
	}
}

// The LaunchAgent restarted by a relaunch keeps its launchd variables and
// is marked as having the login environment, so it does not re-exec for it.
func TestRestartEnvironKeepsTheLaunchdJob(t *testing.T) {
	swapTerminalLoginEnv(t, []string{"PATH=/fresh/bin"}, nil)
	environ := []string{"PATH=/usr/bin", "XPC_SERVICE_NAME=" + launcher.LaunchdLabel, "XPC_FLAGS=0x0"}
	got := envMap(restartEnviron(context.Background(), environ, launcher.Info{Kind: launcher.KindLaunchd}, nil))
	if got["XPC_SERVICE_NAME"] != launcher.LaunchdLabel || got["XPC_FLAGS"] != "0x0" || got[loginEnvAppliedVar] != "1" || !strings.HasPrefix(got["PATH"], "/fresh/bin:") {
		t.Errorf("env %v", got)
	}
}

// A systemd unit's fresh start is the unit's environment, never the login
// shell's, so its in-place restart keeps that too.
func TestRestartEnvironKeepsTheSystemdUnitsEnvironment(t *testing.T) {
	for _, kind := range []string{launcher.KindSystemdUser, launcher.KindSystemdSystem} {
		asked := swapTerminalLoginEnv(t, []string{"PATH=/fresh/bin"}, nil)
		environ := []string{"PATH=/usr/bin", "INVOCATION_ID=abc"}
		if got := restartEnviron(context.Background(), environ, launcher.Info{Kind: kind}, nil); !slices.Equal(got, environ) || *asked != 0 {
			t.Errorf("%s: got %q, asked %d", kind, got, *asked)
		}
	}
}

// The shell does not answer: restart with the environment agentd started
// with, log it, and hand the failure to the restarted daemon, which posts
// it in Messages (reportLoginEnvFailure).
func TestRestartEnvironFallsBackWhenTheLoginShellFails(t *testing.T) {
	swapTerminalLoginEnv(t, nil, errors.New("/bin/zsh did not finish within 30s"))
	logs := captureSlog(t)
	environ := []string{"PATH=/old/bin", "GOROOT=/old/go"}
	got := restartEnviron(context.Background(), environ, launcher.Info{Kind: launcher.KindDetached}, nil)
	want := append(slices.Clone(environ), launcher.LoginEnvErrorVar+"=restarting: /bin/zsh did not finish within 30s")
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if !strings.Contains(logs.String(), "could not get a fresh environment from the login shell") ||
		!strings.Contains(logs.String(), "did not finish within 30s") {
		t.Errorf("no log of the fallback:\n%s", logs)
	}
	// An empty answer is a failure too.
	swapTerminalLoginEnv(t, nil, nil)
	if got := envMap(restartEnviron(context.Background(), environ, launcher.Info{}, nil)); got["PATH"] != "/old/bin" || got[launcher.LoginEnvErrorVar] == "" {
		t.Errorf("empty answer: %v", got)
	}
}

// restartedDaemonEnvVar runs the test binary as the restarted daemon
// (TestRestartedDaemonEnvProcess): it prints its environment, what it hands
// a spawned agent, and the exports a new agent pane runs with.
const restartedDaemonEnvVar = "TCLAUDE_TEST_RESTARTED_DAEMON_ENV"

type restartedDaemonEnv struct {
	Environ, Spawn []string
	PaneExports    string
}

func TestRestartedDaemonEnvProcess(t *testing.T) {
	if os.Getenv(restartedDaemonEnvVar) == "" {
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(restartedDaemonEnv{
		Environ: os.Environ(), Spawn: spawnEnvWithoutOperatorToken(), PaneExports: clcommon.BuildEnvExports(nil),
	})
}

// The operator restarts a daemon that has been up since before the shell
// moved to a new Go: the restarted daemon runs with the login shell's PATH,
// and so do the agents it spawns and their panes, without the operator
// token.
func TestDaemonRestartRunsWithTheLoginShellsEnvironment(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	swapTerminalLoginEnv(t, []string{"HOME=" + home, "PATH=/opt/go-1.27.4/bin:/usr/bin"}, nil)

	prevEnviron, prevExec, prevExit := launchEnviron, execSelfUpdate, restartExit
	launcherState.Lock()
	prevInfo := launcherState.info
	launcherState.info = &launcher.Info{Kind: launcher.KindDetached, RestartMethod: launcher.RestartExec}
	launcherState.Unlock()
	updateRestartState.Lock()
	prevRequested, prevPath, prevStart, prevPlan := updateRestartState.requested, updateRestartState.path, updateRestartState.startPath, updateRestartState.plan
	updateRestartState.requested, updateRestartState.path, updateRestartState.startPath, updateRestartState.plan = true, "/bin/tclaude", "/bin/tclaude", restartPlan{}
	updateRestartState.Unlock()
	t.Cleanup(func() {
		launchEnviron, execSelfUpdate, restartExit = prevEnviron, prevExec, prevExit
		launcherState.Lock()
		launcherState.info = prevInfo
		launcherState.Unlock()
		updateRestartState.Lock()
		updateRestartState.requested, updateRestartState.path, updateRestartState.startPath, updateRestartState.plan = prevRequested, prevPath, prevStart, prevPlan
		updateRestartState.Unlock()
	})
	launchEnviron = []string{"HOME=" + home, "PATH=/opt/go-1.27.3/bin:/usr/bin", "GOROOT=/opt/go-1.27.3", "TCLAUDE_HUMAN_TOKEN=operator-secret"}
	restartExit = func(c int) { t.Fatalf("restart exited with %d, want an exec", c) }

	var out []byte
	execSelfUpdate = func(path string, _ []string, env []string) error {
		if path != "/bin/tclaude" {
			t.Errorf("exec %s", path)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestRestartedDaemonEnvProcess$")
		cmd.Env = append(slices.Clone(env), restartedDaemonEnvVar+"=1")
		var err error
		out, err = cmd.Output()
		return err
	}
	if err := performSelfUpdateRestart(); err != nil {
		t.Fatal(err)
	}
	var got restartedDaemonEnv
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&got); err != nil {
		t.Fatalf("restarted daemon printed %q: %v", out, err)
	}
	daemon, spawn := envMap(got.Environ), envMap(got.Spawn)
	if !newToolchainFirst(daemon["PATH"]) || spawn["PATH"] != daemon["PATH"] {
		t.Errorf("PATH: daemon %q, spawned agent %q; want the login shell's Go first", daemon["PATH"], spawn["PATH"])
	}
	if !strings.Contains(got.PaneExports, "/opt/go-1.27.4/bin") {
		t.Errorf("a new pane's exports lack the fresh PATH: %s", got.PaneExports)
	}
	if _, ok := daemon["GOROOT"]; ok {
		t.Error("the stale GOROOT survived the restart")
	}
	// The operator stays authenticated across the restart, as before; no
	// agent or pane ever sees the token.
	if daemon["TCLAUDE_HUMAN_TOKEN"] != "operator-secret" {
		t.Error("the restarted daemon lost the operator token it was started with")
	}
	if _, ok := spawn["TCLAUDE_HUMAN_TOKEN"]; ok || strings.Contains(got.PaneExports, "operator-secret") {
		t.Error("the operator token leaked into an agent's environment")
	}
}
