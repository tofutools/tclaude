package agentd

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestLauncherPATHFallbackDirs(t *testing.T) {
	exists := map[string]bool{"/opt/homebrew/bin": true}
	isDir := func(p string) bool { return exists[p] }
	got := withLauncherFallbackPATH("darwin", []string{"/usr/bin", "/bin"}, isDir)
	if !slices.Equal(got, []string{"/usr/bin", "/bin", "/opt/homebrew/bin"}) {
		t.Errorf("darwin: %v", got)
	}
	// Already there: not repeated.
	got = withLauncherFallbackPATH("darwin", []string{"/opt/homebrew/bin", "/usr/bin"}, isDir)
	if !slices.Equal(got, []string{"/opt/homebrew/bin", "/usr/bin"}) {
		t.Errorf("darwin, present: %v", got)
	}
	if got := withLauncherFallbackPATH("linux", []string{"/usr/bin"}, isDir); !slices.Equal(got, []string{"/usr/bin"}) {
		t.Errorf("linux: %v", got)
	}
}

func TestLookPathIn(t *testing.T) {
	isExec := func(p string) bool { return p == "/opt/homebrew/bin/tmux" }
	if got := lookPathIn("tmux", []string{"/usr/bin", "relative", "/opt/homebrew/bin"}, isExec); got != "/opt/homebrew/bin/tmux" {
		t.Errorf("got %q", got)
	}
	if got := lookPathIn("tmux", []string{"/usr/bin"}, isExec); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestWarnTmuxMissing(t *testing.T) {
	var out bytes.Buffer
	warnTmuxMissing(&out, func(string) (string, error) { return "/usr/bin/tmux", nil })
	if out.Len() != 0 {
		t.Errorf("warned with tmux present: %q", out.String())
	}
	warnTmuxMissing(&out, func(string) (string, error) { return "", errors.New("not found") })
	if !strings.Contains(out.String(), "tmux is not on agentd's PATH") {
		t.Errorf("out %q", out.String())
	}
}
