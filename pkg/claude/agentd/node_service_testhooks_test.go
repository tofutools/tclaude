package agentd

import (
	"context"
	"errors"
	"os"
	"sync"

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
	autostartFactory = func() launcher.Autostart {
		return launcher.Autostart{GOOS: "linux", UID: 1000, User: "tester",
			Paths: launcher.AutostartPaths("linux", "/nonexistent-test-home", ""), Run: refuse, FS: NewMemFS()}
	}
	autostartLookupEnv = func(string) (string, bool) { return "", false }
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

// MemFS is an in-memory launcher.FS for autostart tests.
type MemFS struct {
	mu    sync.Mutex
	Files map[string][]byte
	Modes map[string]os.FileMode
}

func NewMemFS() *MemFS {
	return &MemFS{Files: map[string][]byte{}, Modes: map[string]os.FileMode{}}
}

func (m *MemFS) ReadFile(p string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.Files[p]; ok {
		return append([]byte(nil), b...), nil
	}
	return nil, os.ErrNotExist
}

func (m *MemFS) WriteFile(p string, b []byte, mode os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Files[p], m.Modes[p] = append([]byte(nil), b...), mode
	return nil
}

func (m *MemFS) MkdirAll(string, os.FileMode) error { return nil }

func (m *MemFS) Remove(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Files[p]; !ok {
		return os.ErrNotExist
	}
	delete(m.Files, p)
	return nil
}

// SetAutostartForTest swaps the Autostart (paths, FS, runner) and the
// environment lookup planAutostart reads values from.
func SetAutostartForTest(a launcher.Autostart, lookupEnv func(string) (string, bool)) func() {
	prevFactory, prevLookup := autostartFactory, autostartLookupEnv
	autostartFactory = func() launcher.Autostart { return a }
	autostartLookupEnv = lookupEnv
	return func() { autostartFactory, autostartLookupEnv = prevFactory, prevLookup }
}

// PerformRestartForTest runs what serve does after it returns for a
// restart, with the process exit captured instead of taken.
func PerformRestartForTest() (exited bool, code int, err error) {
	prev := restartExit
	restartExit = func(c int) { exited, code = true, c }
	defer func() { restartExit = prev }()
	err = performSelfUpdateRestart()
	return exited, code, err
}
