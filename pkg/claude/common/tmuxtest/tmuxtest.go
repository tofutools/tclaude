// Package tmuxtest helps tests stand in for the tmux transport
// (clcommon.Tmux).
//
// Most test doubles in the tree were written as a function from a tmux argv
// to an *exec.Cmd, often a real process whose exit status or output is the
// reply. FromCommand keeps that shape usable: it turns such a function into a
// clcommon.Tmux. It also serves tests that run a real tmux binary on a
// private socket. Production code never builds a tmux process this way; the
// guard test in pkg/claude/common/tmuxcmd keeps it so.
package tmuxtest

import (
	"context"
	"os/exec"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// CommandFunc builds the process that answers one tmux command.
type CommandFunc func(args ...string) *exec.Cmd

// Tmux is a clcommon.Tmux that answers every command by running the process
// its CommandFunc builds.
type Tmux struct {
	command CommandFunc
	// sessions answers ListSessions; nil reports no sessions.
	sessions func() (map[string]struct{}, error)
}

// FromCommand returns a transport that runs command's process for each tmux
// command and reports its outcome the way the real transport does: stdout on
// success, *clcommon.TmuxCommandError on a non-zero exit.
func FromCommand(command CommandFunc) Tmux { return Tmux{command: command} }

// WithSessions makes ListSessions answer from list.
func (t Tmux) WithSessions(list func() (map[string]struct{}, error)) Tmux {
	t.sessions = list
	return t
}

// Run runs the process for one command.
func (t Tmux) Run(ctx context.Context, args []string) ([]byte, error) {
	return Run(ctx, t.command(args...))
}

// AttachArgv is the process's own argv.
func (t Tmux) AttachArgv(args []string) ([]string, error) { return Argv(t.command(args...)) }

// ListSessions answers from WithSessions, or reports no sessions.
func (t Tmux) ListSessions() (map[string]struct{}, error) {
	if t.sessions == nil {
		return nil, nil
	}
	return t.sessions()
}

// Run runs cmd as the answer to one tmux command. A cmd that could not even
// be built (exec.Cmd.Err) fails with that error.
func Run(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	return clcommon.RunTmuxClient(ctx, cmd)
}

// Argv is the argv a terminal would run for cmd.
func Argv(cmd *exec.Cmd) ([]string, error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	return append([]string{cmd.Path}, cmd.Args[1:]...), nil
}
