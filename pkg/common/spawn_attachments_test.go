package common

import (
	"path/filepath"
	"testing"
)

func TestSpawnAttachmentsPrivateBaseUsesProtectedDaemonData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := filepath.Join(TclaudeDataDir(), "spawn-attachments")
	if got := SpawnAttachmentsPrivateBase(); got != want {
		t.Fatalf("SpawnAttachmentsPrivateBase() = %q, want protected path %q", got, want)
	}
}
