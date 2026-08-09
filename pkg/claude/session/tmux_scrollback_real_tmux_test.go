package session

import (
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
)

// An operator can split a managed session and leave the split active after
// scrolling the harness pane. The detach hook must target the harness pane,
// not tmux's active-pane shorthand, or later agent send-keys remain trapped in
// copy mode. A real PTY-backed attach/detach exercises the server hook and also
// proves that installing it preserves occupied global hook-array slots.
func TestConfigureTmuxScrollback_RealTmuxHookCancelsInactiveHarnessPane(t *testing.T) {
	tmux := withIsolatedRealTmux(t)
	require.NoError(t, tmux.Command("new-session", "-d", "-s", "scroll-hook", "sleep", "300").Run())
	require.NoError(t, tmux.Command("split-window", "-d", "-t", "=scroll-hook:0.0", "sleep", "300").Run())
	require.NoError(t, tmux.Command("select-pane", "-t", "=scroll-hook:0.1").Run())
	require.NoError(t, tmux.Command("set-hook", "-g", "client-detached[0]", "display-message global-zero").Run())
	require.NoError(t, tmux.Command("set-hook", "-g", "client-detached[100]", "display-message global-hundred").Run())

	enableTmuxMouseScrollback("scroll-hook")
	enableTmuxMouseScrollback("scroll-hook") // repeat configuration must not append the hook again
	hooks, err := tmux.Command("show-hooks", "-g", "client-detached").Output()
	require.NoError(t, err)
	require.Contains(t, string(hooks), "client-detached[0] display-message global-zero")
	require.Contains(t, string(hooks), "client-detached[100] display-message global-hundred")
	require.Equal(t, 1, strings.Count(string(hooks), tmuxScrollDetachHookMarker))

	attach := tmux.Command("attach-session", "-t", "=scroll-hook")
	terminal, err := pty.Start(attach)
	require.NoError(t, err)
	t.Cleanup(func() { _ = terminal.Close() })
	deadline := time.Now().Add(5 * time.Second)
	attached := false
	for time.Now().Before(deadline) {
		out, listErr := tmux.Command("list-clients", "-F", "#{session_name}").Output()
		if listErr == nil && strings.Contains(string(out), "scroll-hook") {
			attached = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, attached, "tmux client did not attach")
	require.NoError(t, tmux.Command("copy-mode", "-t", "=scroll-hook:0.0").Run())

	mode, err := tmux.Command("display-message", "-p", "-t", "=scroll-hook:0.0", "#{pane_mode}").Output()
	require.NoError(t, err)
	require.Equal(t, "copy-mode", strings.TrimSpace(string(mode)))

	require.NoError(t, tmux.Command("detach-client", "-s", "=scroll-hook").Run())
	_ = terminal.Close()
	_ = attach.Wait()
	mode, err = tmux.Command("display-message", "-p", "-t", "=scroll-hook:0.0", "#{pane_in_mode}").Output()
	require.NoError(t, err)
	require.Equal(t, "0", strings.TrimSpace(string(mode)))
}
