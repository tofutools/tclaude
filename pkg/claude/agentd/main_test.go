package agentd_test

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
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
	// The soft-exit escalation ladder's last two rungs are kill(-pgid, SIGTERM)
	// then SIGKILL against the pane process. Only the first rung — tmux
	// kill-pane — is simulated; these two were the real syscalls in any test
	// that did not swap them, and the tmux simulator used to hand its first
	// session pane pid 1. kill(2) reads a negative pid as a process group, and
	// -1 is not "an unlikely group": it is the kernel's wildcard for every
	// process the caller may signal. So a test that let the ladder past
	// kill-pane SIGTERMed the test binary, its `go test` parent, and on a
	// developer's or agent's machine the shell around them — writing no
	// failure and no stack, so the whole event read as infrastructure trouble
	// (TCL-1035).
	//
	// BINARY-WIDE rather than in newFlow, and that is the point: newFlow lives
	// in package agentd_test, so a default there protects only the tests that
	// call it. The internal `package agentd` files compiled into this same
	// binary drive stopOneConvWithIntent directly with hand-rolled tmux stubs
	// and cannot call newFlow. TestMain is the only seam that covers both, and
	// it is where the terminal-spawn suppression above already sits for the
	// same reason.
	//
	// alive=false stands the ladder down before any signal is attempted. Tests
	// that assert on the signal rungs swap their own pair; their restore puts
	// this default back, never the real syscalls.
	restoreEscalationProcess := agentd.SetSoftExitEscalationProcessForTest(
		func(int) bool { return false },
		func(int, syscall.Signal) error { return nil },
	)
	restoreCodexProbe := session.SetCodexEffectiveConfigProbeForTest(
		func(string, []sandboxpolicy.EnvironmentEntry, string) (json.RawMessage, error) {
			return json.RawMessage(`{"config":{},"origins":{}}`), nil
		})
	code := m.Run()
	restoreCodexProbe()
	restoreEscalationProcess()
	os.Exit(code)
}
