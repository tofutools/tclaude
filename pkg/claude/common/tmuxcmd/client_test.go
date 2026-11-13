package tmuxcmd

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// scriptedTmux is a clcommon.Tmux whose every command runs the given shell
// script, and which records the argv it was asked for.
type scriptedTmux struct {
	script string
	calls  [][]string
}

func (s *scriptedTmux) Command(args ...string) *exec.Cmd {
	s.calls = append(s.calls, args)
	return exec.Command("/bin/sh", "-c", s.script)
}

func (s *scriptedTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func script(t *testing.T, sh string) *scriptedTmux {
	t.Helper()
	fake := &scriptedTmux{script: sh}
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })
	return fake
}

func TestVerbArgv(t *testing.T) {
	tests := []struct {
		name string
		call func(Client)
		want []string
	}{
		{"has-session pins the name exactly", func(c Client) { _, _ = c.HasSession(Session("repo")) },
			[]string{"has-session", "-t", "=repo"}},
		{"session id is passed as is", func(c Client) { _, _ = c.HasSession(SessionID("$3")) },
			[]string{"has-session", "-t", "$3"}},
		{"kill-session", func(c Client) { _ = c.KillSession(Session("repo")) },
			[]string{"kill-session", "-t", "=repo"}},
		{"kill-pane takes a bare pane id", func(c Client) { _ = c.KillPane(PaneID("%7")) },
			[]string{"kill-pane", "-t", "%7"}},
		{"client ttys", func(c Client) { _, _ = c.ClientTTYs(Session("repo")) },
			[]string{"list-clients", "-t", "=repo", "-F", "#{client_tty}"}},
		{"all clients", func(c Client) { _, _ = c.ListAllClients("#{session_name} #{client_tty}") },
			[]string{"list-clients", "-F", "#{session_name} #{client_tty}"}},
		{"detach one client", func(c Client) { _ = c.DetachClient(ClientTTY("/dev/pts/4")) },
			[]string{"detach-client", "-t", "/dev/pts/4"}},
		{"detach a session's clients", func(c Client) { _ = c.DetachSessionClients(Session("repo")) },
			[]string{"detach-client", "-s", "=repo"}},
		{"switch client", func(c Client) { _ = c.SwitchClient(ClientTTY("/dev/pts/4"), Session("next")) },
			[]string{"switch-client", "-c", "/dev/pts/4", "-t", "=next"}},
		{"no-start goes before the command word", func(c Client) { _ = c.NoStart().KillSession(Session("repo")) },
			[]string{"-N", "kill-session", "-t", "=repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := script(t, "exit 0")
			tt.call(Local())
			require.Len(t, fake.calls, 1)
			assert.Equal(t, tt.want, fake.calls[0])
		})
	}
}

func TestHasSessionTellsAnAnswerFromAFailureToAsk(t *testing.T) {
	script(t, "exit 0")
	exists, err := Local().HasSession(Session("repo"))
	require.NoError(t, err)
	assert.True(t, exists)

	script(t, "echo \"can't find session: repo\" >&2; exit 1")
	exists, err = Local().HasSession(Session("repo"))
	require.NoError(t, err, "tmux saying no is an answer")
	assert.False(t, exists)

	fake := &missingBinaryTmux{}
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })
	exists, err = Local().HasSession(Session("repo"))
	require.Error(t, err, "tmux that cannot be run is not an answer")
	assert.False(t, exists)
	assert.False(t, Rejected(err))
}

type missingBinaryTmux struct{}

func (missingBinaryTmux) Command(...string) *exec.Cmd {
	return exec.Command("/nonexistent/tclaude-test-tmux")
}
func (missingBinaryTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		stderr      string
		notFound    bool
		noServer    bool
		wantMessage string
	}{
		{"can't find session: repo", true, false, "can't find session: repo"},
		{"can't find pane: %9", true, false, "can't find pane: %9"},
		{"can't find window: @2", true, false, "can't find window: @2"},
		{"no server running on /tmp/tmux-1000/tclaude", false, true, "no server running on /tmp/tmux-1000/tclaude"},
		{"error connecting to /tmp/tmux-1000/tclaude (No such file or directory)", false, true,
			"error connecting to /tmp/tmux-1000/tclaude (No such file or directory)"},
		{"duplicate session: repo", false, false, "duplicate session: repo"},
		{"", false, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.stderr, func(t *testing.T) {
			script(t, "printf '%s\\n' \""+tt.stderr+"\" >&2; exit 1")
			err := Local().KillSession(Session("repo"))
			require.Error(t, err)
			assert.True(t, Rejected(err))
			assert.Equal(t, tt.notFound, errors.Is(err, ErrNotFound))
			assert.Equal(t, tt.noServer, errors.Is(err, ErrNoServer))
			assert.False(t, errors.Is(err, ErrTimeout))
			var tmuxErr *Error
			require.ErrorAs(t, err, &tmuxErr)
			assert.Equal(t, "kill-session", tmuxErr.Command)
			assert.Equal(t, tt.wantMessage, tmuxErr.Message)
			// The exec path's exit error stays reachable, so code that still
			// asks clcommon.IsTmuxCommandFailure keeps working.
			assert.True(t, clcommon.IsTmuxCommandFailure(err))
			if tt.wantMessage != "" {
				assert.Contains(t, err.Error(), tt.wantMessage)
			}
		})
	}
}

func TestListRepliesDropBlankLines(t *testing.T) {
	script(t, "printf '/dev/pts/1\\n\\n/dev/pts/2\\n'")
	ttys, err := Local().ClientTTYs(Session("repo"))
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/pts/1", "/dev/pts/2"}, ttys)

	script(t, "exit 0")
	ttys, err = Local().ClientTTYs(Session("repo"))
	require.NoError(t, err)
	assert.Empty(t, ttys, "no attached client is an empty list, not one empty tty")
}

// A hung tmux client is killed and reaped when the deadline fires. The test
// fires the timer itself once the child has started, so it never waits for
// the wall-clock deadline.
func TestTimeoutKillsAndReapsTheCommand(t *testing.T) {
	script(t, "echo wedged >&2; exec sleep 600")
	armed := make(chan time.Duration, 1)
	fire := make(chan time.Time, 1)
	client := Local().WithTimeout(time.Hour).WithTimer(func(d time.Duration) (<-chan time.Time, func()) {
		armed <- d
		return fire, func() {}
	})
	done := make(chan error, 1)
	go func() {
		_, err := client.HasSession(Session("repo"))
		done <- err
	}()
	select {
	case d := <-armed:
		assert.Equal(t, time.Hour, d)
	case <-time.After(30 * time.Second):
		t.Fatal("command never started its deadline timer")
	}
	fire <- time.Time{}
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrTimeout)
		assert.False(t, Rejected(err), "a timeout is not tmux's answer")
	case <-time.After(30 * time.Second):
		t.Fatal("timed-out command was not killed and reaped")
	}
}

func TestTimeoutDoesNotChangeAPromptAnswer(t *testing.T) {
	script(t, "exit 1")
	exists, err := Local().WithTimeout(time.Minute).HasSession(Session("repo"))
	require.NoError(t, err)
	assert.False(t, exists)
}
