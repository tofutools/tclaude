package tmuxcmd

// HasSession reports whether tmux lists the session. A session whose pane
// died but is retained (remain-on-exit) is still listed, so this answers "is
// the name taken", not "is something running there".
//
// The error is nil whenever tmux answered, including "no" and "no server".
// A non-nil error (ErrTimeout, tmux missing) means the question could not be
// asked, and the bool is then false without meaning "gone".
func (c Client) HasSession(s SessionTarget) (bool, error) {
	_, err := c.run("has-session", "-t", s.spec)
	if err == nil {
		return true, nil
	}
	if Rejected(err) {
		return false, nil
	}
	return false, err
}

// KillSession destroys the session and every pane in it. Killing a session
// that is already gone fails with ErrNotFound.
func (c Client) KillSession(s SessionTarget) error {
	_, err := c.run("kill-session", "-t", s.spec)
	return err
}

// KillPane destroys one pane. When it is the session's last pane, tmux
// destroys the session with it.
func (c Client) KillPane(p PaneTarget) error {
	_, err := c.run("kill-pane", "-t", p.spec)
	return err
}

// ClientTTYs returns the tty of every client attached to the session, one
// per terminal window showing it. Empty means nobody is looking at it.
func (c Client) ClientTTYs(s SessionTarget) ([]string, error) {
	return c.ListClients(s, "#{client_tty}")
}

// ListClients returns one line per client attached to the session, rendered
// with the tmux format. Prefer ClientTTYs when the tty is all that is needed.
func (c Client) ListClients(s SessionTarget, format string) ([]string, error) {
	out, err := c.run("list-clients", "-t", s.spec, "-F", format)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// ListAllClients returns one line per client attached to any session on the
// server, rendered with the tmux format.
func (c Client) ListAllClients(format string) ([]string, error) {
	out, err := c.run("list-clients", "-F", format)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// DetachClient detaches one attached client. The session and whatever runs
// in it are untouched; only that terminal's view goes away.
func (c Client) DetachClient(client ClientTarget) error {
	_, err := c.run("detach-client", "-t", client.spec)
	return err
}

// DetachSessionClients detaches every client attached to the session.
func (c Client) DetachSessionClients(s SessionTarget) error {
	_, err := c.run("detach-client", "-s", s.spec)
	return err
}

// SwitchClient moves an attached client to another session, so the terminal
// window that showed one session now shows the other.
func (c Client) SwitchClient(client ClientTarget, to SessionTarget) error {
	_, err := c.run("switch-client", "-c", client.spec, "-t", to.spec)
	return err
}
