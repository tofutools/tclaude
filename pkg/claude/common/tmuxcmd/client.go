package tmuxcmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxctl"
)

// Client issues tmux commands. The zero value and Local() talk to tclaude's
// own tmux server with no deadline. A Client is a small value: the With*
// methods return a modified copy and never change the receiver, so a
// configured Client can be shared freely.
type Client struct {
	transport Transport
	timeout   time.Duration
	timer     Timer
	noStart   bool
}

// Timer starts a deadline timer: it returns the channel that fires when the
// time is up and a stop function. It exists so a test can fire a timeout
// causally instead of waiting for one.
type Timer func(time.Duration) (fired <-chan time.Time, stop func())

// Local returns the client for the tmux server on this host.
func Local() Client { return Client{} }

// Transport carries one tmux command to a tmux server and brings back what
// it printed. It is the single method a new way of reaching tmux (a runner
// on another host, a pod, a test double) has to provide; every verb is built
// on it. clcommon.Tmux, the transport of the local server, satisfies it.
//
// args is the command as tmux's own client takes it after the socket flags:
// the command word and its arguments, preceded by any client-level flag such
// as -N. A command tmux ran and rejected is reported as *Error or
// *clcommon.TmuxCommandError carrying tmux's message; any other error means
// the command could not be delivered or its outcome is unknown. Run returns
// promptly once ctx is done, having stopped whatever it started; what it
// returns then, its own error, ctx.Err() or a *clcommon.TmuxAbortedError
// (whose Stderr is quoted in the timeout), is reported to the caller as
// ErrTimeout when the client's timeout is what ended ctx.
//
// A runner implements this and, to offer terminals, Attacher. clcommon.Tmux
// is the same seam as the local server's transports spell it, with the
// session listing agentd's liveness snapshot reads added.
type Transport interface {
	Run(ctx context.Context, args []string) (stdout []byte, err error)
}

// TransportFunc adapts a function to Transport.
type TransportFunc func(ctx context.Context, args []string) ([]byte, error)

// Run calls f.
func (f TransportFunc) Run(ctx context.Context, args []string) ([]byte, error) { return f(ctx, args) }

// NewClient returns a client that sends its commands through t instead of to
// the tmux server on this host.
func NewClient(t Transport) Client { return Client{transport: t} }

// WithTimeout bounds every command the returned client issues. A tmux client
// can connect and then never return; on any path where something waits on the
// answer (a request handler, a per-agent delivery worker, a held lock) an
// unbounded command turns that into a permanent stall. A command that
// overruns fails with ErrTimeout. d <= 0 means no deadline.
//
// The timeout is handed to the transport as its context's deadline. A command
// answered over agentd's control-mode connection is bounded by that
// connection's own reply deadline instead, and reports ErrTimeout the same
// way.
func (c Client) WithTimeout(d time.Duration) Client {
	c.timeout = d
	return c
}

// WithTimer replaces how the timeout's timer is started. Tests only.
func (c Client) WithTimer(t Timer) Client {
	c.timer = t
	return c
}

// NoStart makes the returned client refuse to start a tmux server (tmux -N):
// a command fails instead of creating a server as a side effect. It is used
// where the server must be one that something else owns, so that a command
// racing a server exit cannot start a new one in the caller's cgroup.
func (c Client) NoStart() Client {
	c.noStart = true
	return c
}

// run issues one tmux command and returns its stdout. args starts with the
// tmux command word.
func (c Client) run(args ...string) ([]byte, error) {
	full := args
	if c.noStart {
		full = append([]string{"-N"}, args...)
	}
	ctx := context.Background()
	if c.timeout > 0 {
		var stop func()
		ctx, stop = c.deadline(ctx)
		defer stop()
	}
	out, err := c.via().Run(ctx, full)
	if err == nil {
		return out, nil
	}
	err = wrapError(args[0], err)
	// An answer from tmux stays an answer even if it arrived late: an error
	// must not read as both "tmux said no" and "we do not know".
	if !Rejected(err) && errors.Is(context.Cause(ctx), errDeadline) && !errors.Is(err, ErrTimeout) {
		var aborted *clcommon.TmuxAbortedError
		if errors.As(err, &aborted) {
			if aborted.Stderr != "" {
				return nil, fmt.Errorf("%w after %s: %s", ErrTimeout, c.timeout, aborted.Stderr)
			}
			return nil, fmt.Errorf("%w after %s", ErrTimeout, c.timeout)
		}
		return nil, fmt.Errorf("%w after %s: %w", ErrTimeout, c.timeout, err)
	}
	return nil, err
}

// via is the transport this client's commands go through.
func (c Client) via() Transport {
	if c.transport != nil {
		return c.transport
	}
	// Resolved per command, not when the client is built: tests and agentd's
	// startup replace clcommon.Default after clients exist.
	return clcommon.Default
}

// errDeadline is the cancellation cause of a context whose client timeout
// passed, which tells that apart from any other reason a transport stopped.
var errDeadline = errors.New("tmux client timeout passed")

// deadline derives the context one command runs under from the client's
// timeout. The transport stops, and reaps whatever it started, when the
// context ends; callers release latches and locks on return and must not do
// so while a child of theirs still runs.
func (c Client) deadline(ctx context.Context) (context.Context, func()) {
	if c.timer == nil {
		return context.WithTimeoutCause(ctx, c.timeout, errDeadline)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	fired, stopTimer := c.timer(c.timeout)
	finished := make(chan struct{})
	go func() {
		select {
		case <-fired:
			cancel(errDeadline)
		case <-finished:
		}
	}()
	return ctx, func() {
		close(finished)
		stopTimer()
		cancel(nil)
	}
}

// wrapError turns a transport's error into the package's error model: tmux's
// own refusal becomes *Error, a control-mode reply timeout becomes
// ErrTimeout, everything else passes through unchanged.
func wrapError(command string, err error) error {
	if err == nil {
		return nil
	}
	if Rejected(err) {
		return err
	}
	var cmdErr *clcommon.TmuxCommandError
	if errors.As(err, &cmdErr) {
		return &Error{Command: command, Message: strings.TrimSpace(cmdErr.Message()), Err: err}
	}
	if errors.Is(err, tmuxctl.ErrTimeout) && !errors.Is(err, ErrTimeout) {
		// One timeout sentinel, whichever way the command was sent.
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	return err
}

// named rejects an empty target before anything is sent; see ErrInvalidTarget.
func named(command string, specs ...string) error {
	for _, spec := range specs {
		if spec == "" {
			return fmt.Errorf("tmux %s: %w", command, ErrInvalidTarget)
		}
	}
	return nil
}

// lines splits a reply into its non-empty lines.
func lines(out []byte) []string {
	var res []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			res = append(res, line)
		}
	}
	return res
}
