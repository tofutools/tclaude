package common

import (
	"path/filepath"
	"testing"
)

func TestSpawnAttachmentsPrivateBaseUsesAgentReachableAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := filepath.Join(TclaudeAPIDir(), "spawn-attachments")
	if got := SpawnAttachmentsPrivateBase(); got != want {
		t.Fatalf("SpawnAttachmentsPrivateBase() = %q, want agent-reachable path %q", got, want)
	}
}

func TestLegacySpawnAttachmentsPrivateBaseRemainsProtectedDaemonData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := filepath.Join(TclaudeDataDir(), "spawn-attachments")
	if got := LegacySpawnAttachmentsPrivateBase(); got != want {
		t.Fatalf("LegacySpawnAttachmentsPrivateBase() = %q, want %q", got, want)
	}
}
