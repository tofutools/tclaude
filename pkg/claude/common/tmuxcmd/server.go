package tmuxcmd

import (
	"fmt"
	"strconv"
	"strings"
)

// ServerPID returns the pid of the running tmux server. Combine it with
// NoStart to make it a pure probe: without NoStart some tmux versions start a
// server to answer the question.
//
// No server fails with ErrNoServer. A reply that is not a pid fails with
// ErrMalformedReply.
func (c Client) ServerPID() (int, error) {
	out, err := c.run("display-message", "-p", "#{pid}")
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(out))
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%w: server pid %q", ErrMalformedReply, text)
	}
	return pid, nil
}

// Version returns what `tmux -V` prints ("tmux 3.4"), for the tmux this
// client reaches. See clcommon.TmuxVersionAtLeast for reading it.
func (c Client) Version() (string, error) {
	out, err := c.run("-V")
	return strings.TrimSpace(string(out)), err
}

// StartPersistentServer starts the tmux server if none is running and tells
// it to stay up while it has no sessions.
//
// Both happen in one tmux command on purpose. A server started on its own
// holds no sessions and, with tmux's default exit-empty, exits the moment the
// starting client disconnects, before a second command could change the
// option. tmux only considers exiting once no client remains, so setting the
// option while the same client is still connected closes that window.
func (c Client) StartPersistentServer() error {
	_, err := c.run("start-server", ";", "set-option", "-g", "exit-empty", "off")
	return err
}

// KillServer stops the tmux server and with it every session and pane.
func (c Client) KillServer() error {
	_, err := c.run("kill-server")
	return err
}

// RunShell has the tmux server run a shell command and waits for it to
// finish. The command runs as a child of the server, in the server's
// environment, cgroup and sandbox rather than the caller's, which is the
// point: it is how a question is asked from where panes actually start.
//
// tmux expands its own format syntax in the command before running it: a '#'
// starts a format, so a literal one must be written "##". The caller escapes
// what it interpolates.
//
// With a client timeout the wait is abandoned at the deadline (ErrTimeout);
// the command itself keeps running on the server.
func (c Client) RunShell(command string) error {
	_, err := c.run("run-shell", command)
	return err
}

// GlobalEnvironment returns the server's global environment, the one new
// sessions inherit, as NAME=value entries. Variables marked as removed are
// left out.
func (c Client) GlobalEnvironment() ([]string, error) {
	out, err := c.run("show-environment", "-g")
	if err != nil {
		return nil, err
	}
	var env []string
	for _, line := range strings.Split(string(out), "\n") {
		// "-NAME" marks a variable removed from the environment.
		if line != "" && !strings.HasPrefix(line, "-") {
			env = append(env, line)
		}
	}
	return env, nil
}

// GlobalEnvironmentVariable reads one variable of the server's global
// environment. set is false when the server has no value for it; tmux reports
// a variable it has never heard of as an error.
func (c Client) GlobalEnvironmentVariable(name string) (value string, set bool, err error) {
	out, err := c.run("show-environment", "-g", name)
	if err != nil {
		return "", false, err
	}
	value, set = strings.CutPrefix(strings.TrimSpace(string(out)), name+"=")
	return value, set, nil
}

// SetGlobalEnvironment sets a variable in the server's global environment.
// Panes created afterwards see it; running ones do not.
func (c Client) SetGlobalEnvironment(name, value string) error {
	_, err := c.run("set-environment", "-g", name, value)
	return err
}

// UnsetGlobalEnvironment removes a variable from the server's global
// environment.
func (c Client) UnsetGlobalEnvironment(name string) error {
	_, err := c.run("set-environment", "-gu", name)
	return err
}

// CurrentSession returns the name of the session the calling process is
// running inside. It only has an answer for a process started in a tmux pane
// (it reads $TMUX through the tmux client); anywhere else it fails or names
// an unrelated session.
func (c Client) CurrentSession() (string, error) {
	out, err := c.run("display-message", "-p", "#{session_name}")
	return strings.TrimSpace(string(out)), err
}

// SwitchCurrentClient makes the terminal the calling process runs in show
// another session. Like CurrentSession it only works from inside a tmux pane.
func (c Client) SwitchCurrentClient(to SessionTarget) error {
	if err := named("switch-client", to.spec); err != nil {
		return err
	}
	_, err := c.run("switch-client", "-t", to.spec)
	return err
}
