package session

import (
	"strings"
	"testing"
)

// TestRunNew_UnknownHarnessErrors pins that --harness rejects an unknown
// value up front (harness.Resolve is the first thing runNew does, before
// any tmux work), rather than silently launching Claude Code.
func TestRunNew_UnknownHarnessErrors(t *testing.T) {
	err := RunNew(&NewParams{Harness: "definitely-not-a-harness"})
	if err == nil {
		t.Fatalf("an unknown --harness must error")
	}
	if !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("error should name the unknown harness, got: %v", err)
	}
}

// TestRunNew_CodexRejectsEffortBeforeSpawn pins that the codex harness's
// ModelCatalog is wired into the spawn path: --harness codex --effort high
// fails at validation (the codex reasoning mapping isn't wired yet), before
// runNew touches tmux. Guards that a typo'd/unsupported effort surfaces as
// a clean error rather than being forwarded to codex.
func TestRunNew_CodexRejectsEffortBeforeSpawn(t *testing.T) {
	err := RunNew(&NewParams{Harness: "codex", Effort: "high"})
	if err == nil {
		t.Fatalf("codex --effort high must error until the reasoning mapping is wired")
	}
	if !strings.Contains(err.Error(), "effort") {
		t.Fatalf("error should mention effort, got: %v", err)
	}
}
