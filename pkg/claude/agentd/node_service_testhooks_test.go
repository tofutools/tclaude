package agentd

import (
	"context"
	"errors"

	"github.com/tofutools/tclaude/pkg/launcher"
)

// InstallLauncherTestDefaults keeps every agentd test away from the real
// systemctl and launchctl: detection sees a plain unsupervised process and
// both runners refuse. Tests that need a supervisor use SetLauncherForTest.
func InstallLauncherTestDefaults() {
	refuse := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("agentd tests: supervisor commands are suppressed")
	}
	launcherRunner, launcherRestartRunner = refuse, refuse
	launcherProcessEnv = func() launcher.Env {
		return launcher.Env{GOOS: "linux", PID: 1, Args: []string{"tclaude", "agentd", "serve"},
			Getenv: func(string) string { return "" }, ReadFile: func(string) ([]byte, error) { return nil, errors.New("suppressed") }}
	}
	restartDelay = 0
}

// SetLauncherForTest swaps the process context and both supervisor runners,
// clearing the cached detection and any pending restart. stop stands in for
// the daemon's graceful shutdown on the in-place re-exec path.
func SetLauncherForTest(env launcher.Env, probe, restart launcher.Runner, stop func()) func() {
	prevEnv, prevProbe, prevRestart := launcherProcessEnv, launcherRunner, launcherRestartRunner
	launcherProcessEnv = func() launcher.Env { return env }
	launcherRunner, launcherRestartRunner = probe, restart
	resetLauncherState()
	updateRestartState.Lock()
	prevStop, prevPath, prevReq := updateRestartState.stop, updateRestartState.path, updateRestartState.requested
	updateRestartState.stop, updateRestartState.path, updateRestartState.requested = stop, "/test/tclaude", false
	updateRestartState.Unlock()
	return func() {
		launcherProcessEnv, launcherRunner, launcherRestartRunner = prevEnv, prevProbe, prevRestart
		resetLauncherState()
		updateRestartState.Lock()
		updateRestartState.stop, updateRestartState.path, updateRestartState.requested = prevStop, prevPath, prevReq
		updateRestartState.Unlock()
	}
}

func resetLauncherState() {
	launcherState.Lock()
	launcherState.info, launcherState.pending = nil, ""
	launcherState.Unlock()
}

// ExecRestartRequestedForTest reports whether the in-place re-exec is armed.
func ExecRestartRequestedForTest() bool {
	updateRestartState.Lock()
	defer updateRestartState.Unlock()
	return updateRestartState.requested
}
