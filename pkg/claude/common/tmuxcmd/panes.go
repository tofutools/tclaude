package tmuxcmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMalformedReply: tmux answered, but not in the shape the verb asked for.
// It is never read as "the target is gone".
var ErrMalformedReply = errors.New("malformed tmux reply")

// Display expands a tmux format against a pane and returns the result
// without its trailing newline (display-message -p).
//
// Prefer a typed verb (PaneStatus, PanePID) where one exists: tmux 3.7
// answers a target that does not exist with every field empty instead of an
// error, and the typed verbs turn that into ErrNotFound.
func (c Client) Display(p PaneTarget, format string) (string, error) {
	if err := named("display-message", p.spec); err != nil {
		return "", err
	}
	out, err := c.run("display-message", "-p", "-t", p.spec, format)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// PanePID returns the pid of the process tmux started in the pane. tmux knows
// it from the moment the pane exists, which makes it the reliable "is this
// still the same process" signal.
func (c Client) PanePID(p PaneTarget) (int, error) {
	out, err := c.Display(p, "#{pane_pid}")
	if err != nil {
		return 0, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return 0, notFound("display-message", p.spec)
	}
	pid, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("%w: pane pid %q", ErrMalformedReply, out)
	}
	return pid, nil
}

// PaneStatus is what tmux knows about one pane's identity and life.
type PaneStatus struct {
	// Session is the name of the session the pane belongs to.
	Session string
	// PaneID is the pane's server-unique id ("%12").
	PaneID string
	// PID is the pid of the pane's process, 0 when tmux reported none.
	PID int
	// Dead is true for a pane whose process exited but which tmux retains
	// (remain-on-exit) so the exit can be inspected.
	Dead bool
	// ExitStatus is the dead process's exit status as tmux printed it, or
	// empty: for a live pane, a pane killed by a signal, or a dead pane whose
	// child tmux has not reaped yet.
	ExitStatus string
	// ExitSignal is the signal that killed the dead process, or empty.
	ExitSignal string
	// ExitGeneration is tclaude's launch identity for the pane, the
	// OptionExitGeneration pane option. Empty when it was never set.
	ExitGeneration string
}

// paneStatusFormat is the one format PaneStatus reads. The simulator in
// pkg/testharness models it by this constant.
const paneStatusFormat = "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{" + OptionExitGeneration + "}"

// PaneStatusFormat is the display-message format PaneStatus sends.
func PaneStatusFormat() string { return paneStatusFormat }

// PaneStatus reads a pane's identity, liveness and exit evidence in one tmux
// call, so the fields are a consistent snapshot of the same pane.
//
// A target tmux does not know fails with ErrNotFound on every tmux version.
// A reply that does not parse fails with ErrMalformedReply.
func (c Client) PaneStatus(p PaneTarget) (PaneStatus, error) {
	out, err := c.Display(p, paneStatusFormat)
	if err != nil {
		return PaneStatus{}, err
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 7 {
		return PaneStatus{}, fmt.Errorf("%w: pane status has %d fields", ErrMalformedReply, len(parts))
	}
	if parts[0] == "" && parts[1] == "" {
		// tmux 3.7 no longer fails display-message for a missing target: it
		// exits 0 and expands every field empty.
		return PaneStatus{}, notFound("display-message", p.spec)
	}
	st := PaneStatus{
		Session: parts[0], PaneID: parts[1], Dead: parts[3] == "1",
		ExitStatus: parts[4], ExitSignal: parts[5], ExitGeneration: parts[6],
	}
	if pid, err := strconv.Atoi(parts[2]); err == nil && pid > 0 {
		st.PID = pid
	}
	return st, nil
}

// PaneInfo is one row of AllPanes.
type PaneInfo struct {
	Session        string
	PaneID         string
	PID            int
	Dead           bool
	ExitGeneration string
}

// allPanesFormat is the one format AllPanes reads. The simulator in
// pkg/testharness models it by this constant.
const allPanesFormat = "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{" + OptionExitGeneration + "}"

// AllPanesFormat is the list-panes format AllPanes sends.
func AllPanesFormat() string { return allPanesFormat }

// AllPanes lists every pane on the server in one call, in tmux's order. A
// retained dead pane is listed with Dead set; a killed session is absent.
// Rows tmux printed in another shape are skipped.
func (c Client) AllPanes() ([]PaneInfo, error) {
	rows, err := c.ListAllPanes(allPanesFormat)
	if err != nil {
		return nil, err
	}
	panes := make([]PaneInfo, 0, len(rows))
	for _, row := range rows {
		parts := strings.Split(strings.TrimSpace(row), "|")
		if len(parts) != 5 || parts[0] == "" {
			continue
		}
		pid, _ := strconv.Atoi(parts[2])
		panes = append(panes, PaneInfo{
			Session: parts[0], PaneID: parts[1], PID: pid, Dead: parts[3] == "1", ExitGeneration: parts[4],
		})
	}
	return panes, nil
}

// ListAllPanes returns one line per pane on the server, rendered with the
// tmux format. Prefer AllPanes when identity and liveness are what is needed.
func (c Client) ListAllPanes(format string) ([]string, error) {
	return c.list("list-panes", "-a", "-F", format)
}

// ListPanes returns one line per pane of a window, rendered with the format.
func (c Client) ListPanes(w WindowTarget, format string) ([]string, error) {
	if err := named("list-panes", w.spec); err != nil {
		return nil, err
	}
	return c.list("list-panes", "-t", w.spec, "-F", format)
}

// ListSessions returns one line per session on the server, rendered with the
// tmux format. Sessions with a retained dead pane are included. With no
// server running it fails with ErrNoServer, which for most callers is simply
// "no sessions".
func (c Client) ListSessions(format string) ([]string, error) {
	return c.list("list-sessions", "-F", format)
}

// ListAllWindows returns one line per window on the server, rendered with the
// tmux format.
func (c Client) ListAllWindows(format string) ([]string, error) {
	return c.list("list-windows", "-a", "-F", format)
}

func (c Client) list(args ...string) ([]string, error) {
	out, err := c.run(args...)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// notFound builds the ErrNotFound a verb reports when tmux answered a missing
// target without an error of its own.
func notFound(command, target string) error {
	return &Error{Command: command, Message: "can't find target: " + target, Err: ErrNotFound}
}
