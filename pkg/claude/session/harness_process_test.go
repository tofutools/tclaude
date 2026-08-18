package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsHarnessProcessName locks the harness-agnostic ancestor match the
// two process-tree walks (FindClaudePID, agentd's convIDForPID) share:
// every registered harness binary plus "node" matches; unrelated shell
// processes do not. "codex" matching is the JOH-206 / JOH-160 fix — a
// hard-coded claude/node check used to miss it.
func TestIsHarnessProcessName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"claude", true}, // Claude Code's own binary name
		{"node", true},   // Claude Code runs as a node process
		{"codex", true},  // OpenAI Codex CLI — the gap JOH-206 closes
		{"bash", false},
		{"sshd", false},
		{"sh", false},
		{"tclaude", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHarnessProcessName(tc.name); got != tc.want {
				t.Errorf("IsHarnessProcessName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsHarnessProcessNameRecognizesResolvedClaudeBinary(t *testing.T) {
	binDir := t.TempDir()
	claudeLink := filepath.Join(binDir, "claude")
	for _, version := range []string{"2.1.234", "2.1.235"} {
		target := filepath.Join(binDir, version)
		if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(claudeLink); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(target, claudeLink); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		if !IsHarnessProcessName(version) {
			t.Fatalf("IsHarnessProcessName(%q) = false, want true", version)
		}
	}
	if IsHarnessProcessName("9.9.999") {
		t.Fatal("unrelated version-shaped name matched Claude")
	}
}
