package tmuxcmd

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// Client issues tmux commands. The zero value and Local() talk to tclaude's
// own tmux server with no deadline. A Client is a small value: the With*
// methods return a modified copy and never change the receiver, so a
// configured Client can be shared freely.
type Client struct {
	timeout time.Duration
	timer   Timer
	noStart bool
}

// Timer starts a deadline timer: it returns the channel that fires when the
// time is up and a stop function. It exists so a test can fire a timeout
// causally instead of waiting for one.
type Timer func(time.Duration) (fired <-chan time.Time, stop func())

// Local returns the client for the tmux server on this host.
func Local() Client { return Client{} }

// WithTimeout bounds every command the returned client issues. A tmux client
// can connect and then never return; on any path where something waits on the
// answer (a request handler, a per-agent delivery worker, a held lock) an
// unbounded command turns that into a permanent stall. A command that
// overruns fails with ErrTimeout. d <= 0 means no deadline.
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
	if errors.As(err, &exitErr) && !errors.Is(err, ErrTimeout) {
		return &Error{Command: command, Message: strings.TrimSpace(stderr), Err: err}
	}
	return err
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
