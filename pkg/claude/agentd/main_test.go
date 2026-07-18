package agentd_test

import (
	"errors"
	"os"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// TestMain installs a binary-wide default for the terminal-spawning
// seam, so any test that reaches an openTerminal call site without
// swapping its own stub gets a deterministic "could not open" error —
// the same degraded path a headless CI host exercises — instead of
// popping a real terminal window onto a developer desktop (TCL-584).
//
// Tests that assert on the open path still swap their own
// recorder/stub via agentd.SetOpenTerminalForTest; their restore puts
// this default back, never the real launcher. terminal.OpenWithCommand
// additionally refuses all test binaries outright, so this default is
// the first of two layers, not the only one.
//
// One TestMain governs the whole test binary, including the internal
// `package agentd` test files compiled alongside this external
// package.
func TestMain(m *testing.M) {
	agentd.SetOpenTerminalForTest(func(string) error {
		return errors.New("agentd tests: terminal spawn suppressed by default (TCL-584); swap agentd.SetOpenTerminalForTest to observe the open path")
	})
	os.Exit(m.Run())
}
