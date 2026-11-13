package tmuxcmd

import (
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// SessionSpec describes a session for NewSession to create.
type SessionSpec struct {
	// Name is the session's name. tmux refuses a name it still lists,
	// including one whose pane is dead but retained.
	Name string
	// Dir is the working directory the pane starts in. Empty leaves it to
	// tmux (the directory of the process that reaches tmux).
	Dir string
	// Width and Height pin the pane's size in cells. Zero leaves tmux's
	// default-size, which the operator's tmux.conf can change.
	Width, Height int
	// Command is the program the pane runs and its arguments. A single
	// element is passed to a shell by tmux; several are executed directly
	// with no shell in between. Empty starts the default shell.
	Command []string
	// PrintID makes NewSession return the new session's id ("$3").
	PrintID bool
}

// Args is the tmux command NewSession sends for the spec. It is exposed for
// callers that must bound its size before sending: the tmux client rejects a
// command over about 16KB with an opaque "command too long".
func (s SessionSpec) Args() []string {
	args := []string{"new-session", "-d"}
	if s.PrintID {
		args = append(args, "-P", "-F", "#{session_id}")
	}
	args = append(args, "-s", s.Name)
	if s.Dir != "" {
		args = append(args, "-c", s.Dir)
	}
	if s.Width > 0 && s.Height > 0 {
		args = append(args, "-x", strconv.Itoa(s.Width), "-y", strconv.Itoa(s.Height))
	}
	return append(args, s.Command...)
}

// NewSession creates a detached session running the spec's command and
// returns its id when the spec asks for one.
//
// The pane's process is started by the tmux server, so it inherits the
// server's environment, not the caller's. A failure carries tmux's own
// explanation ("duplicate session", "bad session name") in the error.
func (c Client) NewSession(spec SessionSpec) (id string, err error) {
	if err := named("new-session", spec.Name); err != nil {
		return "", err
	}
	out, err := c.run(spec.Args()...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// AttachOptions shapes the client AttachCommand describes.
type AttachOptions struct {
	// DetachOthers detaches every other client of the session on attach.
	DetachOthers bool
	// IgnoreSize keeps this client's size from affecting the session's: it is
	// a viewer of the pane, not a terminal the pane is sized for.
	IgnoreSize bool
	// ClientFlags are tmux client flags that go before the command word, as
	// clcommon.TmuxClientFeatureArgs builds them ("-T", "hyperlinks").
	ClientFlags []string
}

// AttachCommand returns the argv of the process that, given a terminal,
// attaches it to the session: the program and its arguments, ready for a PTY
// or for the calling process's own stdio.
//
// This is the one place the package hands back a command to run instead of
// running it. Attaching is not a request with a reply: it is a long-lived
// interactive process that owns a terminal, forwards keystrokes and window
// size changes and paints the pane until the user detaches, so it needs a TTY
// the caller holds. Everything else goes through the verbs.
//
// The argv is whatever reaches this client's server from here: the local tmux
// binary today, and a remote shell or exec command for a runner elsewhere.
// tmux exits with status 1 on an ordinary detach.
func (c Client) AttachCommand(s SessionTarget, opts AttachOptions) ([]string, error) {
	if err := named("attach-session", s.spec); err != nil {
		return nil, err
	}
	var args []string
	if c.noStart {
		args = append(args, "-N")
	}
	args = append(args, opts.ClientFlags...)
	args = append(args, "attach-session")
	if opts.DetachOthers {
		args = append(args, "-d")
	}
	if opts.IgnoreSize {
		args = append(args, "-f", "ignore-size")
	}
	args = append(args, "-t", s.spec)
	cmd := clcommon.TmuxCommand(args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	return append([]string{cmd.Path}, cmd.Args[1:]...), nil
}
