package testharness

import (
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SentKey records one tmux send-keys invocation. The TmuxSim retains a
// log for back-compat assertions (WaitForSendKeys); the v2 preferred
// shape is to read the .jsonl directly, since the daemon's job is to
// land turns there.
type SentKey struct {
	Target string
	Text   string
}

// TmuxSim is the rewire replacement for clcommon.TmuxCommand. It owns
// a sessions table, routes send-keys to the registered CCSim's
// Receive, answers has-session against the alive flag, and removes
// sessions on kill-session.
//
// One pane per session. Multi-pane / multi-window scenarios aren't
// modelled; add them when a real test needs them.
type TmuxSim struct {
	mu       sync.Mutex
	sessions map[string]*tmuxSession
	sentLog  []SentKey
}

type tmuxSession struct {
	name string
	cwd  string
	cc   *CCSim
}

func newTmuxSim() *TmuxSim {
	return &TmuxSim{sessions: map[string]*tmuxSession{}}
}

// Command is the rewire replacement signature for clcommon.TmuxCommand.
// Returns a no-op `true`/`false` exec.Cmd whose Run() exits with the
// appropriate status; for verbs that mutate state (send-keys,
// kill-session), the mutation happens here before the cmd is returned.
func (t *TmuxSim) Command(args ...string) *exec.Cmd {
	switch {
	case len(args) >= 3 && args[0] == "has-session" && args[1] == "-t":
		if t.IsAlive(args[2]) {
			return exec.Command("true")
		}
		return exec.Command("false")
	case len(args) >= 4 && args[0] == "send-keys" && args[1] == "-t":
		t.routeSendKeys(args[2], args[3])
	case len(args) >= 3 && args[0] == "kill-session" && args[1] == "-t":
		t.killSession(args[2])
	}
	return exec.Command("true")
}

// routeSendKeys logs the call and forwards text to the attached CCSim.
// Target is "<sessionName>:0.0" or bare "<sessionName>"; we strip the
// pane suffix before lookup.
func (t *TmuxSim) routeSendKeys(target, text string) {
	t.mu.Lock()
	t.sentLog = append(t.sentLog, SentKey{Target: target, Text: text})
	sessName := strings.SplitN(target, ":", 2)[0]
	s, ok := t.sessions[sessName]
	t.mu.Unlock()
	if ok && s.cc != nil {
		s.cc.Receive(text)
	}
}

// killSession removes the session from the alive table and tears down
// its attached CCSim (mirrors tmux dropping the foreground process).
func (t *TmuxSim) killSession(name string) {
	t.mu.Lock()
	s, ok := t.sessions[name]
	delete(t.sessions, name)
	t.mu.Unlock()
	if ok && s.cc != nil {
		s.cc.Shutdown()
	}
}

// Register attaches a CCSim to a tmux session name. The CC owns its
// own alive state; once it /exits, has-session returns false even if
// the session entry is still in the table.
func (t *TmuxSim) Register(name, cwd string, cc *CCSim) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessions[name] = &tmuxSession{name: name, cwd: cwd, cc: cc}
}

// MarkAlive registers a session without an attached CCSim. Used for
// scenarios that need a live session entry but don't drive simulator
// behavior. Has-session returns true; send-keys is logged but
// silently dropped.
func (t *TmuxSim) MarkAlive(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.sessions[name]; !ok {
		t.sessions[name] = &tmuxSession{name: name}
	}
}

// MarkOffline drops the session entry. The attached CCSim (if any)
// keeps its in-memory state — Resume can find it via CCRegistry and
// re-Register under a new tmux name. To hard-kill, use kill-session
// via Command (TmuxSim shuts the CC down).
func (t *TmuxSim) MarkOffline(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, name)
}

// IsAlive reports whether the session is registered and (when a CC is
// attached) the CC is still processing input.
func (t *TmuxSim) IsAlive(name string) bool {
	t.mu.Lock()
	s, ok := t.sessions[name]
	t.mu.Unlock()
	if !ok {
		return false
	}
	if s.cc == nil {
		return true
	}
	return s.cc.IsAlive()
}

// Sessions returns a snapshot of registered session names.
func (t *TmuxSim) Sessions() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.sessions))
	for k := range t.sessions {
		out = append(out, k)
	}
	return out
}

// Sent returns a snapshot of every send-keys recorded so far.
func (t *TmuxSim) Sent() []SentKey {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SentKey, len(t.sentLog))
	copy(out, t.sentLog)
	return out
}

// WaitForSendKeys blocks until at least one send-keys recorded so far
// matches target and contains substring `contains`. Returns true on
// match, false on timeout. Kept for back-compat; new assertions
// should prefer reading the .jsonl via FreshConvRow.
func (t *TmuxSim) WaitForSendKeys(target, contains string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		for _, sk := range t.sentLog {
			if sk.Target == target && strings.Contains(sk.Text, contains) {
				t.mu.Unlock()
				return true
			}
		}
		t.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
