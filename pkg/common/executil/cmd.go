package executil

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// DefaultGracePeriod is the time between SIGTERM and SIGKILL.
const DefaultGracePeriod = 5 * time.Second

// Cmd wraps exec.Cmd with graceful shutdown: on context cancellation, SIGTERM is sent
// to the process and its children (process group). If they don't exit within GracePeriod,
// SIGKILL is sent to the entire group.
type Cmd struct {
	*exec.Cmd
	ctx         context.Context
	gracePeriod time.Duration
	done        chan struct{}
}

// CommandContext creates a Cmd with the default 5-second grace period.
func CommandContext(ctx context.Context, name string, args ...string) *Cmd {
	return CommandContextWithGrace(ctx, DefaultGracePeriod, name, args...)
}

// CommandContextWithGrace creates a Cmd with a custom grace period.
func CommandContextWithGrace(ctx context.Context, gracePeriod time.Duration, name string, args ...string) *Cmd {
	return &Cmd{
		Cmd:         exec.Command(name, args...),
		ctx:         ctx,
		gracePeriod: gracePeriod,
		done:        make(chan struct{}),
	}
}

// Start starts the command and launches the context-watcher goroutine.
func (c *Cmd) Start() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	setup(c.Cmd)
	if err := c.Cmd.Start(); err != nil {
		return err
	}
	go c.watch()
	return nil
}

// Wait waits for the command to exit and signals the context watcher to stop.
func (c *Cmd) Wait() error {
	err := c.Cmd.Wait()
	close(c.done)
	return err
}

// Run starts the command and waits for it to finish.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Output runs the command and returns its stdout output.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var b bytes.Buffer
	c.Stdout = &b
	err := c.Run()
	return b.Bytes(), err
}

// CombinedOutput runs the command and returns combined stdout+stderr output.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	err := c.Run()
	return b.Bytes(), err
}
