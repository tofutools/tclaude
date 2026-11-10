package common

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/tmuxctl"
)

// listSessionsFormat is the list-sessions format both the exec and the
// control-mode paths read. Fixed-charset fields come first and the free-text
// session name last, separated by plain spaces, so no delimiter has to survive
// a tmux client's output sanitising (a TAB turned into '_' under a non-UTF-8
// locale is what took every agent offline once) and a name containing spaces
// still parses. pane_dead is the active pane of the session's current window.
const listSessionsFormat = "#{session_id} #{pane_dead} #{session_name}"

// errSessionListFormat marks a list-sessions line that does not have the shape
// listSessionsFormat produces. It is an error rather than a skipped line: a
// listing we cannot read must not turn into "every agent is offline".
var errSessionListFormat = errors.New("tmux list-sessions: unrecognised line")

// sessionListEntry is one parsed list-sessions line.
type sessionListEntry struct {
	name string
	dead bool
}

// parseSessionList reads listSessionsFormat lines. Blank lines are ignored;
// anything else that is not `$<id> <0|1|> <name>` fails the whole listing.
func parseSessionList(lines []string) ([]sessionListEntry, error) {
	out := make([]sessionListEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, rest, ok := strings.Cut(line, " ")
		if !ok || !validSessionID(id) {
			return nil, fmt.Errorf("%w: %q", errSessionListFormat, truncateLine(line))
		}
		dead, name, ok := strings.Cut(rest, " ")
		if !ok || (dead != "" && dead != "0" && dead != "1") || name == "" {
			return nil, fmt.Errorf("%w: %q", errSessionListFormat, truncateLine(line))
		}
		out = append(out, sessionListEntry{name: name, dead: dead == "1"})
	}
	return out, nil
}

func validSessionID(id string) bool {
	if len(id) < 2 || id[0] != '$' {
		return false
	}
	for _, r := range id[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func truncateLine(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// aliveSessions is the ListSessions result: every session whose active pane
// is not retained-dead, minus the control client's hidden session.
func aliveSessions(entries []sessionListEntry) map[string]struct{} {
	alive := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.dead || e.name == tmuxctl.SessionName {
			continue
		}
		alive[e.name] = struct{}{}
	}
	return alive
}

// hasForeignSession reports whether entries holds any session other than the
// control client's own — the condition for a control connection to be worth
// having (and for it not to be the only thing keeping the server up).
func hasForeignSession(entries []sessionListEntry) bool {
	for _, e := range entries {
		if e.name != tmuxctl.SessionName {
			return true
		}
	}
	return false
}

// ControlModeEnvVar switches agentd's tmux control mode: "0", "off", "false"
// or "no" keep every query on the exec path.
const ControlModeEnvVar = "TCLAUDE_TMUX_CONTROL_MODE"

// ControlModeDisabledByEnv reports whether ControlModeEnvVar turns control
// mode off. ok is false when the variable is unset or not recognised.
func ControlModeDisabledByEnv() (disabled, ok bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ControlModeEnvVar))) {
	case "0", "off", "false", "no":
		return true, true
	case "1", "on", "true", "yes":
		return false, true
	}
	return false, false
}

// controlReconnectBackoff bounds how often the exec path may try to bring the
// control connection up again after a failed attempt.
const controlReconnectBackoff = 2 * time.Second

// ControlTmux answers the queries agentd makes in bulk over one long-lived
// tmux control-mode connection, and everything else — and every query while
// the connection is down — through the embedded LiveTmux exec path. A failed
// control-mode query falls back to exec for that call, so the worst case is
// today's behaviour, never an empty "everything is offline" answer.
//
// The connection is opened lazily, only after an exec listing has seen a
// session other than the hidden control session: the control client never
// starts a tmux server (see tmuxctl), and it closes itself again once its own
// session is the last one, so the server still exits when empty.
type ControlTmux struct {
	// exec builds the fallback tmux subprocesses (LiveTmux in production).
	exec   func(args ...string) *exec.Cmd
	client *tmuxctl.Client

	mu          sync.Mutex
	connecting  bool
	lastAttempt time.Time
	lastErr     string
	now         func() time.Time
}

// NewControlTmux returns a ControlTmux over the tclaude socket.
func NewControlTmux() *ControlTmux {
	return newControlTmux(LiveTmux{}.Command, tmuxctl.Config{BaseArgs: TmuxArgs(), ExitWhenAlone: true})
}

func newControlTmux(execCmd func(args ...string) *exec.Cmd, cfg tmuxctl.Config) *ControlTmux {
	return &ControlTmux{exec: execCmd, client: tmuxctl.New(cfg), now: time.Now}
}

// Command builds an exec.Cmd for the tmux binary: everything that is not
// migrated to control mode keeps forking one tmux process per call.
func (t *ControlTmux) Command(args ...string) *exec.Cmd { return t.exec(args...) }

// Client exposes the control client, for notification subscribers.
func (t *ControlTmux) Client() *tmuxctl.Client { return t.client }

// Close tears the control connection down.
func (t *ControlTmux) Close() { t.client.Close() }

// ControlModeStatus is the operator-facing view of which path tmux queries
// take.
type ControlModeStatus struct {
	Enabled     bool      `json:"enabled"`
	Connected   bool      `json:"connected"`
	Reason      string    `json:"reason"`
	Since       time.Time `json:"since"`
	Connects    int64     `json:"connects"`
	Requests    int64     `json:"requests"`
	Failures    int64     `json:"failures"`
	LastConnErr string    `json:"last_connect_error,omitempty"`
}

// Status reports the control connection's state.
func (t *ControlTmux) Status() ControlModeStatus {
	st := t.client.State()
	t.mu.Lock()
	lastErr := t.lastErr
	t.mu.Unlock()
	return ControlModeStatus{Enabled: true, Connected: st.Connected, Reason: st.Reason, Since: st.Since,
		Connects: st.Connects, Requests: st.Requests, Failures: st.Failures, LastConnErr: lastErr}
}

// TmuxControlModeStatus reports how Default answers tmux queries.
func TmuxControlModeStatus() ControlModeStatus {
	if ct, ok := Default.(*ControlTmux); ok {
		return ct.Status()
	}
	return ControlModeStatus{Reason: "control mode off: queries use one tmux process each"}
}

// ListSessions answers over control mode when connected, else (and on any
// control-mode failure) over exec.
func (t *ControlTmux) ListSessions() (map[string]struct{}, error) {
	if t.client.Connected() {
		lines, err := t.client.Run("list-sessions", "-F", listSessionsFormat)
		if err == nil {
			entries, perr := parseSessionList(lines)
			if perr == nil {
				return aliveSessions(entries), nil
			}
			slog.Warn("tmux control mode: unreadable list-sessions reply, reconnecting", "error", perr, "module", "tmuxctl")
			t.client.Disconnect("protocol error: unreadable list-sessions reply")
		} else if !errors.Is(err, tmuxctl.ErrNotConnected) {
			slog.Debug("tmux control mode: list-sessions failed, using exec", "error", err, "module", "tmuxctl")
		}
	}
	entries, err := execListSessionEntries(t.exec)
	if err != nil {
		return nil, err
	}
	if hasForeignSession(entries) {
		t.maybeConnect()
	}
	return aliveSessions(entries), nil
}

// maybeConnect starts one background connection attempt, rate-limited after a
// failure.
func (t *ControlTmux) maybeConnect() {
	t.mu.Lock()
	if t.connecting || t.client.Connected() || (t.lastErr != "" && t.now().Sub(t.lastAttempt) < controlReconnectBackoff) {
		t.mu.Unlock()
		return
	}
	t.connecting = true
	t.lastAttempt = t.now()
	t.mu.Unlock()
	go func() {
		err := t.client.Connect()
		t.mu.Lock()
		t.connecting = false
		prev := t.lastErr
		if err != nil {
			t.lastErr = err.Error()
		} else {
			t.lastErr = ""
		}
		t.mu.Unlock()
		if err != nil && err.Error() != prev {
			slog.Info("tmux control mode: connect failed, using exec fallback", "error", err, "module", "tmuxctl")
		}
	}()
}

// ListSessions forks one tmux list-sessions command and returns session names
// whose active pane is not retained-dead. Non-zero exit (typically
// "no server running on …" when the tmux server is down) collapses to
// an empty set with nil error — the snapshot semantics are the same as
// "every session is offline". An output line that is not in the expected
// format is an error, not an empty set.
func (l LiveTmux) ListSessions() (map[string]struct{}, error) {
	entries, err := execListSessionEntries(l.Command)
	if err != nil {
		return nil, err
	}
	return aliveSessions(entries), nil
}

func execListSessionEntries(command func(args ...string) *exec.Cmd) ([]sessionListEntry, error) {
	out, err := command("list-sessions", "-F", listSessionsFormat).Output()
	if err != nil {
		// `tmux ls` exits non-zero when there is no server. Treat that
		// as the empty set rather than an error — it is the normal
		// "nothing is running" state, not a probe failure.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, nil
		}
		return nil, err
	}
	return parseSessionList(strings.Split(string(out), "\n"))
}
