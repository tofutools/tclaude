package session

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
	// Gated namespace smokes render bubblewrap arguments in this Go test
	// process but launch a separately built tclaude binary. Without this seam,
	// the constructed root would project the test executable as /usr/bin/tclaude
	// and the smoke would test the wrong program. Ordinary unit-test runs keep
	// the production resolver and therefore continue exercising os.Executable.
	if binary := os.Getenv("TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"); binary != "" {
		tclaudeLayerTclaudeCLIPath = func() string { return binary }
	}
	// A harness-builtin Codex launch with no sandbox asks a real Codex for its
	// managed requirements. Tests launch none: they report no requirements.
	codexRequirementsReader = func(string, []sandboxpolicy.EnvironmentEntry) (json.RawMessage, error) {
		return json.RawMessage(`{"requirements":null}`), nil
	}
	os.Exit(m.Run())
}
