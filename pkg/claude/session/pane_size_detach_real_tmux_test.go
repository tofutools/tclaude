package session

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDetachSizeNormalizationRealTmuxHookAndAttachedGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmux := withIsolatedRealTmux(t)
	require.NoError(t, tmux.Command("set-hook", "-g", "client-detached[0]", "display-message operator-zero").Run())
	require.NoError(t, tmux.Command("set-hook", "-g", "client-detached[100]", "display-message operator-hundred").Run())
	// A crash-prone legacy global hook from an earlier tclaude must be swept
	// away by the first configure call on an upgraded binary.
	require.NoError(t, tmux.Command("set-hook", "-g",
		"client-detached["+tmuxDetachNormalizeHookIndex+"]", "display-message legacy-normalize").Run())
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
	globalHooks, err := tmux.Command("show-hooks", "-g", "client-detached").Output()
	require.NoError(t, err)
	require.Contains(t, string(globalHooks), "client-detached[0] display-message operator-zero")
	require.Contains(t, string(globalHooks), "client-detached[100] display-message operator-hundred")
	require.NotContains(t, string(globalHooks), "client-detached["+tmuxDetachNormalizeHookIndex+"]",
		"legacy global normalization hook must be removed")
	sessionHooks, err := tmux.Command("show-hooks", "-t", "=detach-size:", "client-detached").Output()
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(sessionHooks), "client-detached["+tmuxDetachNormalizeHookIndex+"]"))

	// Re-running configure must stay idempotent and keep operator hooks.
	ConfigureTmuxDetachNormalization("detach-size")
	sessionHooks, err = tmux.Command("show-hooks", "-t", "=detach-size:", "client-detached").Output()
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(sessionHooks), "client-detached["+tmuxDetachNormalizeHookIndex+"]"))

	// Control mode is a real tmux client without a terminal-emulator dependency,
	// so this stays stable on headless CI while exercising the same server hook.
	attach := tmux.Command("-C", "attach-session", "-t", "=detach-size")
	stdin, err := attach.StdinPipe()
	require.NoError(t, err)
	var attachStderr bytes.Buffer
	attach.Stdout = io.Discard
	attach.Stderr = &attachStderr
	require.NoError(t, attach.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		if attach.Process != nil {
			_ = attach.Process.Kill()
		}
	})
	attached := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, listErr := tmux.Command("list-clients", "-F", "#{session_name}").Output()
		if listErr == nil && strings.Contains(string(out), "detach-size") {
			attached = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !attached {
		t.Fatalf("tmux control client did not attach: %s", attachStderr.String())
	}

	// Control-mode clients do not impose terminal dimensions, so perturb the
	// window explicitly. The callback helper's condition is evaluated inside
	// tmux: while a viewer remains, the window must retain this size.
	require.NoError(t, tmux.Command("resize-window", "-t", "=detach-size:", "-x", "155", "-y", "39").Run())
	attachedSize := realTmuxWindowSize(t, tmux, "detach-size")
	require.Equal(t, "155x39", attachedSize)
	NormalizeTmuxPaneAfterDetach("detach-size")
	require.Equal(t, attachedSize, realTmuxWindowSize(t, tmux, "detach-size"))

	// A raw tmux detach has no returning tclaude caller. The global hook must
	// still normalize immediately rather than waiting for agentd's sweep.
	require.NoError(t, tmux.Command("detach-client", "-s", "=detach-size").Run())
	_ = stdin.Close()
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
