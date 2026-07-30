package agentd_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	// Resolving a filtered Codex launch reads Codex's effective config by
	// executing the real binary. CI shards that run these flows have no Codex
	// install, so without a default every Codex filtered flow would refuse for
	// a missing executable rather than exercising the path under test. This
	// default answers with Codex's own unconfigured shape — no provider
	// override, no base URL, no remotely delivered layer — which resolves to
	// the first-party API route. Tests needing a different route swap their own.
	restoreCodexProbe := session.SetCodexEffectiveConfigProbeForTest(
		func(string, []sandboxpolicy.EnvironmentEntry) (json.RawMessage, error) {
			return json.RawMessage(`{"config":{},"origins":{}}`), nil
		})
	code := m.Run()
	restoreCodexProbe()
	os.Exit(code)
}
