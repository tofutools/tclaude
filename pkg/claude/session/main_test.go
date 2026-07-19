package session

import (
	"os"
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
	os.Exit(m.Run())
}
