package session

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
)

func TestDetachSizeNormalizationRealTmuxHookAndAttachedGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmux := withIsolatedRealTmux(t)
	require.NoError(t, tmux.Command("set-hook", "-g", "client-detached[0]", "display-message operator-zero").Run())
	require.NoError(t, tmux.Command("set-hook", "-g", "client-detached[100]", "display-message operator-hundred").Run())
	require.NoError(t, launchDetachedTmuxSession("detach-size", t.TempDir(), "exec sleep 300"),
		"detached creation must install the normalization hook before any client attaches")

	var installers sync.WaitGroup
	for range 40 {
		installers.Add(1)
		go func() {
			defer installers.Done()
			ConfigureTmuxDetachNormalization("detach-size")
		}()
	}
	installers.Wait()
	hooks, err := tmux.Command("show-hooks", "-g", "client-detached").Output()
	require.NoError(t, err)
	require.Contains(t, string(hooks), "client-detached[0] display-message operator-zero")
	require.Contains(t, string(hooks), "client-detached[100] display-message operator-hundred")
	require.Equal(t, 1, strings.Count(string(hooks), tmuxDetachNormalizeOption))

	// Replacing the global hook array removes our entry. The next attach setup
	// must inspect the actual hooks and repair it without replacing the new
	// operator entry.
	require.NoError(t, tmux.Command("set-hook", "-g", "client-detached[0]", "display-message operator-replacement").Run())
	ConfigureTmuxDetachNormalization("detach-size")
	hooks, err = tmux.Command("show-hooks", "-g", "client-detached").Output()
	require.NoError(t, err)
	require.Contains(t, string(hooks), "client-detached[0] display-message operator-replacement")
	require.Equal(t, 1, strings.Count(string(hooks), tmuxDetachNormalizeOption))

	attach := tmux.Command("attach-session", "-t", "=detach-size")
	terminal, err := pty.StartWithSize(attach, &pty.Winsize{Cols: 155, Rows: 39})
	require.NoError(t, err)
	t.Cleanup(func() { _ = terminal.Close() })
	require.Eventually(t, func() bool {
		out, listErr := tmux.Command("list-clients", "-F", "#{session_name}").Output()
		return listErr == nil && strings.Contains(string(out), "detach-size")
	}, 5*time.Second, 10*time.Millisecond, "tmux client did not attach")

	// The callback helper's condition is evaluated inside tmux: while a viewer
	// remains, the window must retain that viewer's size.
	attachedSize := realTmuxWindowSize(t, tmux, "detach-size")
	require.NotEqual(t, "200x50", attachedSize)
	NormalizeTmuxPaneAfterDetach("detach-size")
	require.Equal(t, attachedSize, realTmuxWindowSize(t, tmux, "detach-size"))

	// A raw tmux detach has no returning tclaude caller. The global hook must
	// still normalize immediately rather than waiting for agentd's sweep.
	require.NoError(t, tmux.Command("detach-client", "-s", "=detach-size").Run())
	_ = terminal.Close()
	_ = attach.Wait()
	require.Eventually(t, func() bool {
		return realTmuxWindowSize(t, tmux, "detach-size") == "200x50"
	}, 5*time.Second, 10*time.Millisecond, "detach hook did not restore canonical size")
}

func realTmuxWindowSize(t *testing.T, tmux isolatedRealTmux, sessionName string) string {
	t.Helper()
	out, err := tmux.Command("display-message", "-p", "-t", "="+sessionName+":",
		"#{window_width}x#{window_height}").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}
