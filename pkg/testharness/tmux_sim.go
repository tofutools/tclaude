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

// PaneSim is the behaviour a TmuxSim needs from whatever harness
// simulator is attached to a pane: it receives keystrokes, reports
// whether it is still alive, and tears down on kill-session. Both
// *CCSim and *CodexSim implement it, so one TmuxSim drives either
// harness's sim unchanged — the routing / has-session / kill-session
// machinery is harness-agnostic.
type PaneSim interface {
	Receive(text string)
	IsAlive() bool
	Shutdown()
}

// Both harness simulators satisfy PaneSim, so TmuxSim routes to either.
var (
	_ PaneSim = (*CCSim)(nil)
	_ PaneSim = (*CodexSim)(nil)
)

// TmuxSim is the test-time stand-in for clcommon.LiveTmux. It
// satisfies the clcommon.Tmux interface: owns a sessions table,
// routes send-keys to the registered pane sim's Receive, answers
// has-session against the alive flag, and removes sessions on
// kill-session. Tests assign a *TmuxSim to clcommon.Default at
// setup; t.Cleanup restores the production singleton.
//
// One pane per session. Multi-pane / multi-window scenarios aren't
// modelled; add them when a real test needs them.
type TmuxSim struct {
	mu       sync.Mutex
	sessions map[string]*tmuxSession
	sentLog  []SentKey
	// buffers models tmux's named paste buffers. set-buffer stores into
	// it; paste-buffer reads it back and routes the content to the
	// target pane's sim — the test-time stand-in for the bracketed
	// paste injectMultilineAndSubmit uses to land multi-line text.
	buffers map[string]string
	// commandCounts records every Command(verb, …) invocation by verb
	// (args[0]) — exposed via CommandCount for regression tests that
	// pin "snapshot collapses to ONE list-sessions and ZERO
	// has-session calls". An accessor rather than a getter on the
	// whole map so the lock stays internal.
	commandCounts map[string]int
}

type tmuxSession struct {
	name string
	cwd  string
	pane PaneSim
}

func newTmuxSim() *TmuxSim {
	return &TmuxSim{
		sessions:      map[string]*tmuxSession{},
		buffers:       map[string]string{},
		commandCounts: map[string]int{},
	}
}

// Command satisfies the clcommon.Tmux interface. Returns a no-op
// `true`/`false` exec.Cmd whose Run() exits with the appropriate
// status; for verbs that mutate state (send-keys, kill-session),
// the mutation happens here before the cmd is returned.
func (t *TmuxSim) Command(args ...string) *exec.Cmd {
	if len(args) > 0 {
		t.mu.Lock()
		t.commandCounts[args[0]]++
		t.mu.Unlock()
	}
	switch {
	case len(args) >= 3 && args[0] == "has-session" && args[1] == "-t":
		if t.IsAlive(args[2]) {
			return exec.Command("true")
		}
		return exec.Command("false")
	case len(args) >= 4 && args[0] == "send-keys" && args[1] == "-t":
		t.routeSendKeys(args[2], args[3])
	case len(args) >= 3 && args[0] == "set-buffer":
		t.setBuffer(args[1:])
	case len(args) >= 3 && args[0] == "paste-buffer":
		t.pasteBuffer(args[1:])
	case len(args) >= 3 && args[0] == "kill-session" && args[1] == "-t":
		t.killSession(args[2])
	}
	return exec.Command("true")
}

// setBuffer models `tmux set-buffer -b <name> <data>` — it stores the
// trailing data argument under the named buffer. injectMultilineAndSubmit
// stages the group startup context this way before pasting it.
func (t *TmuxSim) setBuffer(args []string) {
	name := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-b" {
			name = args[i+1]
		}
	}
	// The data is the final positional argument.
	data := args[len(args)-1]
	t.mu.Lock()
	t.buffers[name] = data
	t.mu.Unlock()
}

// pasteBuffer models `tmux paste-buffer [-dpr] -b <name> -t <target>`.
// It looks up the named buffer and routes its contents to the target
// pane exactly as a send-keys would — production uses bracketed paste
// here so multi-line text lands as one block; the simulator just
// needs the text to reach the CCSim. With -d the buffer is dropped.
func (t *TmuxSim) pasteBuffer(args []string) {
	name, target := "", ""
	del := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-b":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "-t":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		case "-d":
			del = true
		}
	}
	t.mu.Lock()
	text, ok := t.buffers[name]
	if del {
		delete(t.buffers, name)
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	t.routeSendKeys(target, text)
}

// routeSendKeys logs the call and forwards text to the attached pane
// sim. Target is "<sessionName>:0.0" or bare "<sessionName>"; we strip
// the pane suffix before lookup.
func (t *TmuxSim) routeSendKeys(target, text string) {
	t.mu.Lock()
	t.sentLog = append(t.sentLog, SentKey{Target: target, Text: text})
	sessName := strings.SplitN(target, ":", 2)[0]
	s, ok := t.sessions[sessName]
	t.mu.Unlock()
	if ok && s.pane != nil {
		s.pane.Receive(text)
	}
}

// killSession removes the session from the alive table and tears down
// its attached pane sim (mirrors tmux dropping the foreground process).
func (t *TmuxSim) killSession(name string) {
	t.mu.Lock()
	s, ok := t.sessions[name]
	delete(t.sessions, name)
	t.mu.Unlock()
	if ok && s.pane != nil {
		s.pane.Shutdown()
	}
}

// Register attaches a pane sim (a *CCSim or *CodexSim) to a tmux
// session name. The sim owns its own alive state; once it exits,
// has-session returns false even if the session entry is still in the
// table.
func (t *TmuxSim) Register(name, cwd string, pane PaneSim) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessions[name] = &tmuxSession{name: name, cwd: cwd, pane: pane}
}

// MarkAlive registers a session without an attached pane sim. Used for
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

// MarkOffline drops the session entry. The attached pane sim (if any)
// keeps its in-memory state — Resume can find it via CCRegistry and
// re-Register under a new tmux name. To hard-kill, use kill-session
// via Command (TmuxSim shuts the sim down).
func (t *TmuxSim) MarkOffline(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, name)
}

// ListSessions satisfies clcommon.Tmux for snapshot-shaped callers.
// Walks the same alive predicate as IsAlive over every registered
// session and returns the names that pass — so a snapshot taken via
// this method is identical to N IsAlive calls, just in one trip.
// Recorded under "list-sessions" so a regression test can pin "the
// snapshot fired ONE list-sessions and ZERO has-session calls".
func (t *TmuxSim) ListSessions() (map[string]struct{}, error) {
	t.mu.Lock()
	t.commandCounts["list-sessions"]++
	names := make([]string, 0, len(t.sessions))
	for k := range t.sessions {
		names = append(names, k)
	}
	t.mu.Unlock()
	alive := map[string]struct{}{}
	for _, n := range names {
		if t.IsAlive(n) {
			alive[n] = struct{}{}
		}
	}
	return alive, nil
}

// CommandCount returns how many times Command was invoked with the
// given verb (args[0]) since the sim was created. Exposed for
// regression tests that pin call-count invariants — e.g. one
// dashboard snapshot fires ONE list-sessions and ZERO has-session.
func (t *TmuxSim) CommandCount(verb string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.commandCounts[verb]
}

// IsAlive reports whether the session is registered and (when a pane
// sim is attached) the sim is still processing input.
func (t *TmuxSim) IsAlive(name string) bool {
	t.mu.Lock()
	s, ok := t.sessions[name]
	t.mu.Unlock()
	if !ok {
		return false
	}
	if s.pane == nil {
		return true
	}
	return s.pane.IsAlive()
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
