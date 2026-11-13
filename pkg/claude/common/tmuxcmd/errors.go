package tmuxcmd

import (
	"errors"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

var (
	// ErrNotFound: tmux ran the command and said its target (a session,
	// window, pane or client) does not exist.
	ErrNotFound = errors.New("tmux target not found")
	// ErrNoServer: no tmux server is running on tclaude's socket. With no
	// server there are no sessions, so for a liveness question this is a
	// definite "not there", not a failure to ask.
	ErrNoServer = errors.New("no tmux server running")
	// ErrTimeout: the command did not answer in time, whichever way it was
	// sent. The outcome is unknown: a mutation may or may not have happened.
	ErrTimeout = errors.New("tmux command timeout")
	// ErrInvalidTarget: a verb was handed an empty target. tmux reads an
	// empty -t as "the current or most recent session, pane or client", so
	// sending it would act on something the caller never named. Nothing is
	// sent.
	ErrInvalidTarget = errors.New("empty tmux target")
)

// Error is a command tmux ran and rejected. It is an answer from tmux, as
// opposed to a failure to reach it. Use errors.Is with ErrNotFound or
// ErrNoServer for the common reasons, and Rejected to ask "did tmux say no".
type Error struct {
	// Command is the tmux command word, e.g. "kill-session".
	Command string
	// Message is tmux's own error text, trimmed; empty when tmux gave none.
	Message string
	// Err is the transport's error: an *exec.ExitError from a forked client
	// or a *clcommon.TmuxCommandError from control mode.
	Err error
}

func (e *Error) Error() string {
	var cmdErr *clcommon.TmuxCommandError
	if errors.As(e.Err, &cmdErr) || e.Message == "" {
		// The control-mode error already reads "tmux: <message>".
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Is classifies tmux's message so callers match a sentinel instead of text.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return notFoundMessage(e.Message)
	case ErrNoServer:
		return clcommon.TmuxNoServerMessage(e.Message)
	}
	return false
}

// Rejected reports whether err is tmux's own refusal of a command, as opposed
// to a failure to run it or a timeout. It is true for every non-zero exit of
// the tmux client, which includes the client failing to connect for a reason
// other than "no server" (a socket it may not open, a server of another
// version). Use errors.Is with ErrNotFound or ErrNoServer where only those
// two answers are acceptable.
func Rejected(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// notFoundMessage recognises tmux's "no such target" replies. They come from
// one place in tmux (cmd-find.c) and all read "can't find <kind>: <target>".
func notFoundMessage(msg string) bool {
	for line := range strings.SplitSeq(msg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "can't find ") {
			return true
		}
	}
	return false
}
