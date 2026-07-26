package common

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTclaudeTmuxSocketDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMUX_TMPDIR", base)
	got, err := TclaudeTmuxSocketDir()
	if err != nil {
		t.Fatal(err)
	}
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("canonicalize tmux base: %v", err)
	}
	want := filepath.Join(canonicalBase, fmt.Sprintf("tmux-%d", os.Getuid()))
	if got != want {
		t.Fatalf("tmux socket dir = %q, want %q", got, want)
	}

	t.Setenv("TMUX_TMPDIR", "relative")
	if _, err := TclaudeTmuxSocketDir(); err == nil {
		t.Fatal("relative TMUX_TMPDIR must be rejected")
	}
}
