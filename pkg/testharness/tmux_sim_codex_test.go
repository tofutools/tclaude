package testharness

import (
	"testing"
)

// TestTmuxSim_RoutesToCodexSim proves the PaneSim extraction lets one
// TmuxSim drive a *CodexSim with the same send-keys / has-session /
// kill-session machinery it uses for *CCSim — the harness-agnostic
// routing Phase-2 needs, exercised here without the Codex parser.
func TestTmuxSim_RoutesToCodexSim(t *testing.T) {
	home := t.TempDir()
	tm := newTmuxSim()
	cx := NewCodexSim(t, home, "/work")
	if err := cx.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tm.Register("codex-sess", cx.Cwd, cx)

	// has-session reflects the CodexSim's alive flag.
	if !tm.IsAlive("codex-sess") {
		t.Fatal("registered live CodexSim should be alive")
	}

	// send-keys + Enter route through Command into the rollout.
	tm.Command("send-keys", "-t", "codex-sess:0.0", "deploy the change")
	tm.Command("send-keys", "-t", "codex-sess:0.0", "Enter")
	envs := readRollout(t, cx.RolloutPath)
	if got := messageText(t, findByType(envs, "response_item")); got != "deploy the change" {
		t.Errorf("routed message = %q, want %q", got, "deploy the change")
	}

	// kill-session tears the sim down → has-session goes false.
	tm.Command("kill-session", "-t", "codex-sess")
	if tm.IsAlive("codex-sess") {
		t.Error("CodexSim should be dead after kill-session")
	}
}
