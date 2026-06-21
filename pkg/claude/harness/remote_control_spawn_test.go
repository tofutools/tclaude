package harness

import (
	"strings"
	"testing"
)

// TestResolveRemoteControl covers the spawn-time remote-control opt-in gate
// (JOH-258): it passes through for a harness with built-in Remote Access
// (Claude Code) on both true and false, rejects an opt-in for a harness without
// it (Codex), and leaves false alone for Codex (off is the default everywhere,
// so only an explicit true on a no-Remote-Access harness is an error).
func TestResolveRemoteControl(t *testing.T) {
	claude := MustGet(DefaultName)
	codex := MustGet(CodexName)

	if got, err := ResolveRemoteControl(claude, true); err != nil || got != true {
		t.Fatalf("ResolveRemoteControl(claude, true) = %v,%v; want true,nil", got, err)
	}
	if got, err := ResolveRemoteControl(claude, false); err != nil || got != false {
		t.Fatalf("ResolveRemoteControl(claude, false) = %v,%v; want false,nil", got, err)
	}
	if got, err := ResolveRemoteControl(codex, false); err != nil || got != false {
		t.Fatalf("ResolveRemoteControl(codex, false) = %v,%v; want false,nil (no opt-in, no error)", got, err)
	}
	if _, err := ResolveRemoteControl(codex, true); err == nil {
		t.Fatalf("ResolveRemoteControl(codex, true) must error — Codex has no built-in remote access")
	}
}

// TestCodexSpawner_IgnoresRemoteControl is the defence-in-depth check that the
// Codex spawner never emits a `--remote-control` even if a SpawnSpec carries
// RemoteControl=true (it shouldn't — the gate above rejects it first, but the
// spawner must not leak a flag Codex would reject). JOH-258.
func TestCodexSpawner_IgnoresRemoteControl(t *testing.T) {
	got := (codexSpawner{}).BuildCommand(SpawnSpec{RemoteControl: true})
	if strings.Contains(got, "--remote-control") {
		t.Fatalf("codex spawner must never emit --remote-control, got %q", got)
	}
}
