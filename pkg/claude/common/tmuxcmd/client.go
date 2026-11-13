package tmuxcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
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
// on it.
//
// args is the command as tmux's own client takes it after the socket flags:
// the command word and its arguments, preceded by any client-level flag such
// as -N. A command tmux ran and rejected is reported as *Error carrying
// tmux's message; any other error means the command could not be delivered
// or its outcome is unknown. Run returns promptly once ctx is done.
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
// The timeout governs a forked tmux client. A command answered over agentd's
// control-mode connection is bounded by that connection's own reply deadline
// instead, and reports ErrTimeout the same way.
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
	if c.transport != nil {
		return c.runVia(c.transport, full)
	}
	// Over control mode the reply is bounded by the connection's own deadline.
	if out, err, ok := clcommon.TryTmuxControl(full...); ok {
		return out, wrapError(args[0], "", err)
	}
	cmd := clcommon.TmuxCommand(full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := c.wait(cmd, &stderr); err != nil {
		return nil, wrapError(args[0], stderr.String(), err)
	}
	return stdout.Bytes(), nil
}

// runVia sends one command through a transport other than the local one,
// turning the client's timeout into the context's deadline.
func (c Client) runVia(t Transport, args []string) ([]byte, error) {
	ctx := context.Background()
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	out, err := t.Run(ctx, args)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && !errors.Is(err, ErrTimeout) {
		return nil, fmt.Errorf("%w after %s: %w", ErrTimeout, c.timeout, err)
	}
	return out, err
}

// wait runs cmd to completion, killing and reaping it if the client's timeout
// passes first. Reaping before returning matters: callers release latches and
// locks on return, and must not do so while the child still runs.
func (c Client) wait(cmd *exec.Cmd, stderr *bytes.Buffer) error {
	if c.timeout <= 0 {
		return cmd.Run()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	startTimer := c.timer
	if startTimer == nil {
		startTimer = realTimer
	}
	fired, stop := startTimer(c.timeout)
	defer stop()
	select {
	case err := <-done:
		return err
	case <-fired:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("%w after %s: %s", ErrTimeout, c.timeout, detail)
		}
		return fmt.Errorf("%w after %s", ErrTimeout, c.timeout)
	}
}

func realTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { _ = t.Stop() }
}

// wrapError turns a transport error into the package's error model: tmux's
// own refusal becomes *Error, everything else passes through unchanged.
func wrapError(command, stderr string, err error) error {
	if err == nil {
		return nil
	}
	var cmdErr *clcommon.TmuxCommandError
	if errors.As(err, &cmdErr) {
		return &Error{Command: command, Message: strings.TrimSpace(cmdErr.Message()), Err: err}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &Error{Command: command, Message: strings.TrimSpace(stderr), Err: err}
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
