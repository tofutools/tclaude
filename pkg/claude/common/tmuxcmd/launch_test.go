package tmuxcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replying is a client that records every command and answers each with out.
func replying(calls *[][]string, out string, err error) Client {
	return NewClient(TransportFunc(func(_ context.Context, args []string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		return []byte(out), err
	}))
}

func TestSessionSpecArgs(t *testing.T) {
	tests := []struct {
		name string
		spec SessionSpec
		want []string
	}{
		{"shell in a directory", SessionSpec{Name: "term", Dir: "/work"},
			[]string{"new-session", "-d", "-s", "term", "-c", "/work"}},
		{"sized pane running a program", SessionSpec{Name: "agent", Dir: "/work", Width: 200, Height: 50, Command: []string{"sh", "/tmp/launch.sh", "marker"}},
			[]string{"new-session", "-d", "-s", "agent", "-c", "/work", "-x", "200", "-y", "50", "sh", "/tmp/launch.sh", "marker"}},
		{"id requested", SessionSpec{Name: "job", Dir: "/work", Width: 200, Height: 50, PrintID: true, Command: []string{"sh", "x"}},
			[]string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", "job", "-c", "/work", "-x", "200", "-y", "50", "sh", "x"}},
		{"no directory", SessionSpec{Name: "bridge", Width: 200, Height: 50, Command: []string{"sh", "-c", "sleep 300"}},
			[]string{"new-session", "-d", "-s", "bridge", "-x", "200", "-y", "50", "sh", "-c", "sleep 300"}},
		{"a size needs both dimensions", SessionSpec{Name: "half", Width: 200},
			[]string{"new-session", "-d", "-s", "half"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.spec.Args())
			var calls [][]string
			_, err := replying(&calls, "", nil).NewSession(tt.spec)
			require.NoError(t, err)
			assert.Equal(t, [][]string{tt.want}, calls, "NewSession sends exactly what Args reports")
		})
	}
}

func TestNewSession(t *testing.T) {
	var calls [][]string
	id, err := replying(&calls, "$7\n", nil).NoStart().NewSession(SessionSpec{Name: "job", PrintID: true})
	require.NoError(t, err)
	assert.Equal(t, "$7", id)
	assert.Equal(t, "-N", calls[0][0], "no-start goes before the command word")

	duplicate := &Error{Command: "new-session", Message: "duplicate session: job"}
	_, err = replying(&calls, "", duplicate).NewSession(SessionSpec{Name: "job"})
	require.ErrorContains(t, err, "duplicate session: job", "tmux's explanation travels in the error")
	assert.True(t, Rejected(err))

	calls = nil
	_, err = replying(&calls, "", nil).NewSession(SessionSpec{})
	require.ErrorIs(t, err, ErrInvalidTarget)
	assert.Empty(t, calls)
}

func TestAttachCommand(t *testing.T) {
	fake := script(t, "exit 0")
	argv, err := Local().AttachCommand(Session("repo"), AttachOptions{})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"attach-session", "-t", "=repo"}}, fake.calls)
	assert.Equal(t, "/bin/sh", argv[0], "the argv is the seam's own command, ready to run on a terminal")

	fake = script(t, "exit 0")
	_, err = Local().AttachCommand(Session("repo"), AttachOptions{
		DetachOthers: true, IgnoreSize: true, ClientFlags: []string{"-T", "sync"},
	})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"-T", "sync", "attach-session", "-d", "-f", "ignore-size", "-t", "=repo"}}, fake.calls,
		"client flags go before the command word")

	fake = script(t, "exit 0")
	_, err = Local().AttachCommand(Session(""), AttachOptions{})
	require.ErrorIs(t, err, ErrInvalidTarget)
	assert.Empty(t, fake.calls)
}

func TestServerVerbs(t *testing.T) {
	var calls [][]string
	pid, err := replying(&calls, "4242\n", nil).NoStart().ServerPID()
	require.NoError(t, err)
	assert.Equal(t, 4242, pid)
	assert.Equal(t, [][]string{{"-N", "display-message", "-p", "#{pid}"}}, calls)

	for _, reply := range []string{"", "0\n", "-3\n", "12 34\n", "pid\n"} {
		_, err = replying(&calls, reply, nil).ServerPID()
		require.ErrorIs(t, err, ErrMalformedReply, "%q", reply)
	}

	noServer := &Error{Command: "display-message", Message: "no server running on /tmp/tmux-1000/tclaude", Err: errors.New("exit status 1")}
	_, err = replying(&calls, "", noServer).ServerPID()
	require.ErrorIs(t, err, ErrNoServer)

	calls = nil
	c := replying(&calls, "", nil)
	require.NoError(t, c.StartPersistentServer())
	require.NoError(t, c.KillServer())
	require.NoError(t, c.RunShell("true"))
	require.NoError(t, c.SwitchCurrentClient(Session("next")))
	require.NoError(t, c.SetGlobalEnvironment("LANG", "en_GB.UTF-8"))
	require.NoError(t, c.UnsetGlobalEnvironment("LANG"))
	assert.Equal(t, [][]string{
		{"start-server", ";", "set-option", "-g", "exit-empty", "off"},
		{"kill-server"},
		{"run-shell", "true"},
		{"switch-client", "-t", "=next"},
		{"set-environment", "-g", "LANG", "en_GB.UTF-8"},
		{"set-environment", "-gu", "LANG"},
	}, calls)

	version, err := replying(&calls, "tmux 3.4\n", nil).Version()
	require.NoError(t, err)
	assert.Equal(t, "tmux 3.4", version)
	assert.Equal(t, []string{"-V"}, calls[len(calls)-1])

	name, err := replying(&calls, "repo\n", nil).CurrentSession()
	require.NoError(t, err)
	assert.Equal(t, "repo", name)
	assert.Equal(t, []string{"display-message", "-p", "#{session_name}"}, calls[len(calls)-1])
}

func TestGlobalEnvironment(t *testing.T) {
	var calls [][]string
	env, err := replying(&calls, "PATH=/usr/bin\n-LANG\nTERM=xterm\n", nil).GlobalEnvironment()
	require.NoError(t, err)
	assert.Equal(t, []string{"PATH=/usr/bin", "TERM=xterm"}, env, "a removed variable is not part of the environment")

	value, set, err := replying(&calls, "DIR=/sys/fs/cgroup/x\n", nil).GlobalEnvironmentVariable("DIR")
	require.NoError(t, err)
	assert.True(t, set)
	assert.Equal(t, "/sys/fs/cgroup/x", value)
	assert.Equal(t, []string{"show-environment", "-g", "DIR"}, calls[len(calls)-1])

	_, set, err = replying(&calls, "-DIR\n", nil).GlobalEnvironmentVariable("DIR")
	require.NoError(t, err)
	assert.False(t, set)

	value, set, err = replying(&calls, "DIR=\n", nil).GlobalEnvironmentVariable("DIR")
	require.NoError(t, err)
	assert.True(t, set, "set to empty is still set")
	assert.Empty(t, value)

	_, set, err = replying(&calls, "DIRECTORY=/x\n", nil).GlobalEnvironmentVariable("DIR")
	require.NoError(t, err)
	assert.False(t, set, "another variable sharing the prefix is not this one")
}
