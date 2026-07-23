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

// TestRunNew_CodexRejectsClaudeModelBeforeSpawn pins that the codex
// harness's ModelCatalog is wired into the spawn path: --harness codex
// --model opus (a Claude Code model) fails at validation, before runNew
// touches tmux, rather than being forwarded to `codex --model`. (Effort,
// by contrast, is now accepted for codex and mapped to its reasoning
// scale — see TestCodexModels / TestCodexReasoningEffort in the harness
// package; so a valid effort is NOT a pre-spawn error anymore.)
func TestRunNew_CodexRejectsClaudeModelBeforeSpawn(t *testing.T) {
	err := RunNew(&NewParams{Harness: "codex", Model: "opus"})
	if err == nil {
		t.Fatalf("codex --model opus (a Claude Code model) must error before spawn")
	}
	if !strings.Contains(err.Error(), "Claude Code model") {
		t.Fatalf("error should explain it's a Claude Code model, got: %v", err)
	}
}

func TestRunNew_ToolGovernanceIsOpenCodeOnly(t *testing.T) {
	for _, harnessName := range []string{"claude", "codex"} {
		err := RunNew(&NewParams{Harness: harnessName, ToolGovernance: "ask"})
		if err == nil || !strings.Contains(err.Error(), "tool-governance") {
			t.Fatalf("RunNew(%s, --tools ask) error = %v, want tool-governance rejection", harnessName, err)
		}
	}

	err := RunNew(&NewParams{Harness: "opencode", ToolGovernance: "sometimes"})
	if err == nil || !strings.Contains(err.Error(), "tool-governance") {
		t.Fatalf("RunNew(opencode, --tools sometimes) error = %v, want validation rejection", err)
	}
}
