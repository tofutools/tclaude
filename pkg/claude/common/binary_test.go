package common

import (
	"os"
	"path/filepath"
	"testing"
)

// The tclaude CLI and the standalone tclaude-agentd daemon link the same
// code, and the daemon builds command lines that run tclaude subcommands
// (session attach, session exit-callback). Resolving those to the running
// executable is right only in tclaude itself; in a sibling binary it would
// re-invoke that binary with subcommands it does not have.
func TestResolveTclaudePath(t *testing.T) {
	// An empty PATH makes the exec.LookPath fallback deterministic, so a
	// tclaude installed on the developer's machine cannot mask a wrong answer.
	writeExecutable := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Run("tclaude itself uses the running executable", func(t *testing.T) {
		t.Setenv("PATH", "")
		dir := t.TempDir()
		self := filepath.Join(dir, "tclaude")
		writeExecutable(t, self)
		// A sibling exists and is deliberately ignored: the tclaude binary
		// must stay pinned to the exact build that is running.
		if got := resolveTclaudePath(self, true); got != self {
			t.Errorf("resolveTclaudePath(%q, true) = %q, want %q", self, got, self)
		}
	})

	t.Run("sibling binary prefers a tclaude installed beside it", func(t *testing.T) {
		t.Setenv("PATH", "")
		dir := t.TempDir()
		self := filepath.Join(dir, "tclaude-agentd")
		sibling := filepath.Join(dir, "tclaude")
		writeExecutable(t, self)
		writeExecutable(t, sibling)
		if got := resolveTclaudePath(self, false); got != sibling {
			t.Errorf("resolveTclaudePath(%q, false) = %q, want %q", self, got, sibling)
		}
	})

	t.Run("a directory named tclaude is not mistaken for the binary", func(t *testing.T) {
		t.Setenv("PATH", "")
		dir := t.TempDir()
		self := filepath.Join(dir, "tclaude-agentd")
		writeExecutable(t, self)
		if err := os.Mkdir(filepath.Join(dir, "tclaude"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if got := resolveTclaudePath(self, false); got != "tclaude" {
			t.Errorf("resolveTclaudePath(%q, false) = %q, want bare %q", self, got, "tclaude")
		}
	})

	t.Run("sibling binary falls back to PATH", func(t *testing.T) {
		selfDir := t.TempDir()
		pathDir := t.TempDir()
		self := filepath.Join(selfDir, "tclaude-agentd")
		onPath := filepath.Join(pathDir, "tclaude")
		writeExecutable(t, self)
		writeExecutable(t, onPath)
		t.Setenv("PATH", pathDir)
		if got := resolveTclaudePath(self, false); got != onPath {
			t.Errorf("resolveTclaudePath(%q, false) = %q, want %q", self, got, onPath)
		}
	})

	t.Run("bare name when nothing resolves", func(t *testing.T) {
		t.Setenv("PATH", "")
		dir := t.TempDir()
		self := filepath.Join(dir, "tclaude-agentd")
		writeExecutable(t, self)
		if got := resolveTclaudePath(self, false); got != "tclaude" {
			t.Errorf("resolveTclaudePath(%q, false) = %q, want bare %q", self, got, "tclaude")
		}
	})

	t.Run("unknown executable path falls through to PATH", func(t *testing.T) {
		pathDir := t.TempDir()
		onPath := filepath.Join(pathDir, "tclaude")
		writeExecutable(t, onPath)
		t.Setenv("PATH", pathDir)
		if got := resolveTclaudePath("", true); got != onPath {
			t.Errorf(`resolveTclaudePath("", true) = %q, want %q`, got, onPath)
		}
	})
}

func TestMarkSelfNotTclaude(t *testing.T) {
	t.Cleanup(func() { selfIsTclaude = true })
	if !selfIsTclaude {
		t.Fatal("selfIsTclaude should default to true so the tclaude binary is unaffected")
	}
	MarkSelfNotTclaude()
	if selfIsTclaude {
		t.Error("MarkSelfNotTclaude did not clear selfIsTclaude")
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no special chars passes through unchanged",
			args: []string{"/usr/local/bin/tclaude", "session", "attach"},
			want: "/usr/local/bin/tclaude session attach",
		},
		{
			name: "bare command passes through unchanged",
			args: []string{"tclaude", "status-bar"},
			want: "tclaude status-bar",
		},
		{
			// Regression guard for JOH-32: a binary path containing spaces must
			// stay a single shell token instead of splitting on the space.
			name: "path with spaces is single-quoted",
			args: []string{"/Users/First Last/go/bin/tclaude", "session", "attach"},
			want: "'/Users/First Last/go/bin/tclaude' session attach",
		},
		{
			name: "embedded single quote is escaped",
			args: []string{"/home/o'brien/bin/tclaude", "status-bar"},
			want: `'/home/o'\''brien/bin/tclaude' status-bar`,
		},
		{
			name: "empty",
			args: nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellJoin(tt.args); got != tt.want {
				t.Errorf("shellJoin(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
