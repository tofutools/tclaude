package session

import (
	"os"
	"sync"
	"testing"
)

// TestMain scrubs the launch-identity environment a tclaude-managed pane
// exports to its own harness (TCL-573): a developer or agent running
// `go test` inside a managed session inherits a real
// TCLAUDE_EXIT_GENERATION, and the SessionEnd hook path reads that
// variable to detect stale predecessor observations — so the inherited
// value makes unrelated exit-audit tests observe THIS pane's launch
// generation and reject their own fixtures as stale. Tests that assert
// the stale-detection behavior set the variable themselves via t.Setenv,
// which restores this scrubbed default afterwards.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("TCLAUDE_EXIT_GENERATION")
	// Keep this fixture lazy: the Darwin smoke helper runs TestMain inside a
	// Seatbelt that denies temporary-file writes, and it never needs the
	// Linux-only constructed-root projection this source supports.
	var bashEnvOnce sync.Once
	var bashEnvPath string
	var bashEnvErr error
	tclaudeLayerConstructedRootBashEnvSource = func() (string, error) {
		bashEnvOnce.Do(func() {
			bashEnv, err := os.CreateTemp("", "tclaude-test-bash-env-")
			if err != nil {
				bashEnvErr = err
				return
			}
			bashEnvPath = bashEnv.Name()
			if _, err := bashEnv.WriteString(tclaudeLayerConstructedRootBashEnv(
				tclaudeLayerConstructedRootTclaudeBin,
			)); err != nil {
				bashEnvErr = err
			}
			if err := bashEnv.Close(); bashEnvErr == nil && err != nil {
				bashEnvErr = err
			}
			if bashEnvErr != nil {
				_ = os.Remove(bashEnvPath)
				bashEnvPath = ""
			}
		})
		return bashEnvPath, bashEnvErr
	}
	// Gated namespace smokes render bubblewrap arguments in this Go test
	// process but launch a separately built tclaude binary. Without this seam,
	// the constructed root would project the test executable as /usr/bin/tclaude
	// and the smoke would test the wrong program. Ordinary unit-test runs keep
	// the production resolver and therefore continue exercising os.Executable.
	if binary := os.Getenv("TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"); binary != "" {
		tclaudeLayerTclaudeCLIPath = func() string { return binary }
	}
	code := m.Run()
	if bashEnvPath != "" {
		_ = os.Remove(bashEnvPath)
	}
	os.Exit(code)
}
