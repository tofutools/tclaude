package common

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestTmuxArgsForceUTF8(t *testing.T) {
	if got := TmuxArgs("list-sessions"); !slices.Equal(got, []string{"-u", "-L", "tclaude", "list-sessions"}) {
		t.Errorf("TmuxArgs = %v", got)
	}
}

// TestListSessionsWithoutALocale runs a real tmux on a private socket with
// no locale in the environment, as agentd has under launchd. Without -u the
// tmux client prints the tab in `list-sessions -F` as '_', no session name
// matches, and every agent reads as offline. The test fails without -u.
func TestListSessionsWithoutALocale(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("", "tx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if len(filepath.Join(dir, "tmux-00000", TmuxSocketName)) > 100 {
		t.Skip("temp dir too long for a unix socket path")
	}
	t.Setenv("TMUX_TMPDIR", dir)
	// A client inside tmux (TMUX set, even empty) trusts UTF-8 on its own,
	// which would hide the bug when the tests run in a tmux pane.
	for _, k := range []string{"TMUX", "LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	tmux := LiveTmux{}
	if _, err := tmux.Run(context.Background(), []string{"new-session", "-d", "-s", "abc123", "sleep", "60"}); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	t.Cleanup(func() { _, _ = tmux.Run(context.Background(), []string{"kill-server"}) })

	alive, err := tmux.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := alive["abc123"]; !ok || len(alive) != 1 {
		t.Fatalf("alive = %v, want exactly abc123", alive)
	}
}
