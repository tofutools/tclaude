package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// An operator can split a managed session and leave the split active after
// scrolling the harness pane. The detach hook must target the harness pane,
// not tmux's active-pane shorthand, or later agent send-keys remain trapped in
// copy mode. run-hooks exercises the same server-side client-detached command
// with no client attached, without needing a terminal emulator in the test.
func TestConfigureTmuxScrollback_RealTmuxHookCancelsInactiveHarnessPane(t *testing.T) {
	tmux := withIsolatedRealTmux(t)
	require.NoError(t, tmux.Command("new-session", "-d", "-s", "scroll-hook", "sleep", "300").Run())
	require.NoError(t, tmux.Command("split-window", "-d", "-t", "=scroll-hook:0.0", "sleep", "300").Run())
	require.NoError(t, tmux.Command("select-pane", "-t", "=scroll-hook:0.1").Run())

	enableTmuxMouseScrollback("scroll-hook")
	require.NoError(t, tmux.Command("copy-mode", "-t", "=scroll-hook:0.0").Run())

	mode, err := tmux.Command("display-message", "-p", "-t", "=scroll-hook:0.0", "#{pane_mode}").Output()
	require.NoError(t, err)
	require.Equal(t, "copy-mode", strings.TrimSpace(string(mode)))

	require.NoError(t, tmux.Command("set-hook", "-R", "-t", "=scroll-hook:", "client-detached").Run())
	mode, err = tmux.Command("display-message", "-p", "-t", "=scroll-hook:0.0", "#{pane_in_mode}").Output()
	require.NoError(t, err)
	require.Equal(t, "0", strings.TrimSpace(string(mode)))
}
