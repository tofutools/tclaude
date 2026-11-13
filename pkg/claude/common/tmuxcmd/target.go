package tmuxcmd

import clcommon "github.com/tofutools/tclaude/pkg/claude/common"

// SessionTarget names a tmux session for a session-typed command
// (has-session, kill-session, list-clients, switch-client, detach-client -s).
type SessionTarget struct{ spec string }

// Session targets the session with exactly this name. tmux otherwise falls
// back to prefix matching, so a dead "repo" would resolve to a live "repo-2".
func Session(name string) SessionTarget {
	if name == "" {
		return SessionTarget{}
	}
	return SessionTarget{spec: clcommon.ExactTarget(name)}
}

// SessionID targets a session by its tmux id ("$3"). An id is never reused
// within a server's lifetime, so it keeps addressing the session a caller
// created even after its name has been released and taken again.
func SessionID(id string) SessionTarget { return SessionTarget{spec: id} }

// String is the tmux -t value.
func (t SessionTarget) String() string { return t.spec }

// PaneTarget names a pane for a pane-typed command (kill-pane, send-keys,
// display-message, capture-pane, pane-scoped options and hooks).
type PaneTarget struct{ spec string }

// PaneID targets a pane by its tmux id ("%12"), which is unique server-wide
// and is never reused while the server runs.
func PaneID(id string) PaneTarget { return PaneTarget{spec: id} }

// String is the tmux -t value.
func (t PaneTarget) String() string { return t.spec }

// ClientTarget names an attached tmux client by its tty path.
type ClientTarget struct{ spec string }

// ClientTTY targets the client attached from this tty ("/dev/pts/7").
func ClientTTY(tty string) ClientTarget { return ClientTarget{spec: tty} }

// String is the tmux -t / -c value.
func (t ClientTarget) String() string { return t.spec }
