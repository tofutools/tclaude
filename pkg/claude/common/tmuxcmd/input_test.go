package tmuxcmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder is a client that records every command instead of sending it.
func recorder(calls *[][]string, fail func(args []string) error) Client {
	return NewClient(TransportFunc(func(_ context.Context, args []string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		if fail != nil {
			return nil, fail(args)
		}
		return nil, nil
	}))
}

func TestInputVerbArgv(t *testing.T) {
	pane := PaneID("%7")
	tests := []struct {
		name string
		call func(Client)
		want [][]string
	}{
		{"keys by name", func(c Client) { _ = c.SendKeys(pane, "Escape", "C-c") },
			[][]string{{"send-keys", "-t", "%7", "Escape", "C-c"}}},
		{"literal text is never a key name", func(c Client) { _ = c.SendLiteral(Session("repo").FirstPane(), "Enter") },
			[][]string{{"send-keys", "-l", "-t", "=repo:0.0", "Enter"}}},
		{"raw bytes as hex", func(c Client) { _ = c.SendBytes(pane, []byte{0x1b, '[', 'M'}) },
			[][]string{{"send-keys", "-t", "%7", "-H", "1b", "5b", "4d"}}},
		{"no bytes sends nothing", func(c Client) { _ = c.SendBytes(pane, nil) }, nil},
		{"a key run is one chained command", func(c Client) {
			_ = c.SendKeySequence(pane, []Key{{Name: "Enter"}, {Literal: "a;"}, {Literal: "-x"}})
		}, [][]string{{"send-keys", "-t", "%7", "Enter", ";", "send-keys", "-t", "%7", "-l", "--", `a\;`, ";", "send-keys", "-t", "%7", "-l", "--", "-x"}}},
		{"an empty key run sends nothing", func(c Client) { _ = c.SendKeySequence(pane, nil) }, nil},
		{"enter copy mode", func(c Client) { _ = c.EnterCopyMode(pane) },
			[][]string{{"copy-mode", "-e", "-t", "%7"}}},
		{"copy-mode command", func(c Client) { _ = c.CopyModeCommand(pane, "cancel", 1) },
			[][]string{{"send-keys", "-t", "%7", "-X", "cancel"}}},
		{"repeated copy-mode command", func(c Client) { _ = c.CopyModeCommand(pane, "scroll-up", 10) },
			[][]string{{"send-keys", "-t", "%7", "-X", "-N", "10", "scroll-up"}}},
		{"capture the screen", func(c Client) { _, _ = c.CapturePane(pane, CaptureOptions{}) },
			[][]string{{"capture-pane", "-p", "-J", "-t", "%7"}}},
		{"capture with escapes", func(c Client) { _, _ = c.CapturePane(pane, CaptureOptions{Escapes: true}) },
			[][]string{{"capture-pane", "-p", "-e", "-J", "-t", "%7"}}},
		{"capture with history", func(c Client) { _, _ = c.CapturePane(pane, CaptureOptions{History: 40}) },
			[][]string{{"capture-pane", "-p", "-J", "-S", "-40", "-t", "%7"}}},
		{"capture rows as displayed", func(c Client) { _, _ = c.CapturePane(pane, CaptureOptions{History: 200, Wrapped: true}) },
			[][]string{{"capture-pane", "-p", "-S", "-200", "-t", "%7"}}},
		{"resize", func(c Client) { _ = c.ResizeWindow(WindowID("@2"), 120, 40) },
			[][]string{{"resize-window", "-t", "@2", "-x", "120", "-y", "40"}}},
		{"pin size", func(c Client) { _ = c.PinWindowSize(WindowID("@2")) },
			[][]string{{"resize-window", "-t", "@2"}}},
		{"root key binding", func(c Client) { _ = c.BindRootKey("S-Right", "run-shell", "-b", "x") },
			[][]string{{"bind-key", "-n", "S-Right", "run-shell", "-b", "x"}}},
		{"paste", func(c Client) { _ = c.PasteText(pane, "buf", "a\nb", nil) },
			[][]string{{"set-buffer", "-b", "buf", "a\nb"}, {"paste-buffer", "-d", "-p", "-r", "-b", "buf", "-t", "%7"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls [][]string
			tt.call(recorder(&calls, nil))
			assert.Equal(t, tt.want, calls)
		})
	}
}

func TestPaneFromSpec(t *testing.T) {
	for spec, want := range map[string]string{
		"%42":        "%42",
		"=%42":       "%42",
		"repo:0.0":   "=repo:0.0",
		"=repo:0.0":  "=repo:0.0",
		"repo":       "=repo",
		"":           "",
		"my.repo:0":  "=my.repo:0",
		"=my-repo:1": "=my-repo:1",
	} {
		assert.Equal(t, want, PaneFromSpec(spec).String(), spec)
	}
}

// A paste that is refused or fails after its text was staged must not leave
// the staged buffer behind on the server.
func TestPasteTextRemovesItsBufferWhenThePasteDoesNotHappen(t *testing.T) {
	refused := errors.New("pane is waiting on a human")
	var calls [][]string
	err := recorder(&calls, nil).PasteText(PaneID("%7"), "buf", "text", func() error { return refused })
	require.ErrorIs(t, err, refused)
	assert.Equal(t, [][]string{{"set-buffer", "-b", "buf", "text"}, {"delete-buffer", "-b", "buf"}}, calls,
		"the check runs after staging and before the paste")

	calls = nil
	broken := errors.New("paste failed")
	err = recorder(&calls, func(args []string) error {
		if args[0] == "paste-buffer" {
			return broken
		}
		return nil
	}).PasteText(PaneID("%7"), "buf", "text", nil)
	require.ErrorIs(t, err, broken)
	assert.Equal(t, "delete-buffer", calls[len(calls)-1][0])

	calls = nil
	err = recorder(&calls, func([]string) error { return broken }).PasteText(PaneID("%7"), "buf", "text", nil)
	require.ErrorIs(t, err, broken)
	assert.Len(t, calls, 1, "nothing to remove when staging itself failed")
}

func TestTransportReceivesClientFlagsAndDeadline(t *testing.T) {
	var got []string
	var hadDeadline bool
	c := NewClient(TransportFunc(func(ctx context.Context, args []string) ([]byte, error) {
		got = args
		_, hadDeadline = ctx.Deadline()
		return []byte("/dev/pts/3\n"), nil
	}))
	ttys, err := c.NoStart().WithTimeout(time.Minute).ClientTTYs(Session("repo"))
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/pts/3"}, ttys)
	assert.Equal(t, []string{"-N", "list-clients", "-t", "=repo", "-F", "#{client_tty}"}, got)
	assert.True(t, hadDeadline, "the client's timeout is the transport's deadline")

	_, err = c.ClientTTYs(Session("repo"))
	require.NoError(t, err)
	assert.False(t, hadDeadline, "no timeout, no deadline")
}

func TestTransportDeadlineIsErrTimeout(t *testing.T) {
	c := NewClient(TransportFunc(func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	_, err := c.WithTimeout(time.Millisecond).HasSession(Session("repo"))
	require.ErrorIs(t, err, ErrTimeout)
	assert.False(t, Rejected(err))
}

func TestTransportRejectionIsAnAnswer(t *testing.T) {
	c := NewClient(TransportFunc(func(context.Context, []string) ([]byte, error) {
		return nil, &Error{Command: "has-session", Message: "can't find session: repo", Err: errors.New("exit status 1")}
	}))
	exists, err := c.HasSession(Session("repo"))
	require.NoError(t, err)
	assert.False(t, exists)
	require.ErrorIs(t, c.KillSession(Session("repo")), ErrNotFound)
}

// Only a trailing ';' is special to tmux's argv parser, and only it is
// escaped: the text must arrive in the pane exactly as written.
func TestLiteralArgEscapesOnlyTheCommandSeparator(t *testing.T) {
	assert.Equal(t, `hello\;`, literalArg("hello;"))
	assert.Equal(t, `\\;`, literalArg(`\;`))
	assert.Equal(t, "; kill-server", literalArg("; kill-server"))
	assert.Equal(t, "", literalArg(""))
}
