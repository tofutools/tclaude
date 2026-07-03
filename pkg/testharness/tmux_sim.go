package testharness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// has-session liveness flows through cmd.Run()==nil (see
// session.IsTmuxSessionAlive), so the sim answers it with an exit-0 /
// exit-1 process. A bare exec.Command("true") PATH-resolves at Run()
// time — under `env -i` (empty PATH) LookPath fails and EVERY live
// session reads as dead, wedging the whole flow-test suite. Resolving
// true/false to ABSOLUTE paths once at init keeps the truthiness
// hermetic: exec.Command skips LookPath when the name contains a
// separator, so cmd.Path is taken verbatim regardless of $PATH.
var (
	trueBin  = resolveCoreutil("true")
	falseBin = resolveCoreutil("false")
	// echoBin backs the `display-message -p '#{pane_pid}'` query: the sim
	// answers it with `echo <pane-pid>` so the caller's .Output() reads the
	// pid back, the same hermetic-absolute-path discipline as true/false.
	echoBin = resolveCoreutil("echo")
)

// resolveCoreutil returns an absolute path to the named coreutil from a
// fixed candidate list, independent of $PATH. Falls back to the bare
// name (the legacy PATH-resolved behaviour) only when no candidate
// exists, so platforms without /usr/bin|/bin coreutils are no worse off
// than before.
func resolveCoreutil(name string) string {
	for _, dir := range []string{"/usr/bin", "/bin"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return name
}

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
// Only CCSim renders a footer (paneRenderer) — Codex has no remote control,
// so it is never captured for the /rc pill.
var (
	_ PaneSim      = (*CCSim)(nil)
	_ PaneSim      = (*CodexSim)(nil)
	_ paneRenderer = (*CCSim)(nil)
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
	// nextPID hands each newly-registered session a distinct, launch-unique
	// fake pane pid (mirrors tmux giving every new pane process a fresh OS
	// pid). Re-registering the same name — the test stand-in for a resume
	// reusing a conv-id-derived tmux name — therefore yields a DIFFERENT pid,
	// which is exactly what the soft-exit retry's livePanePID guard keys on.
	nextPID int
}

type tmuxSession struct {
	name    string
	cwd     string
	pane    PaneSim
	panePID int
}

func newTmuxSim() *TmuxSim {
	return &TmuxSim{
		sessions:      map[string]*tmuxSession{},
		buffers:       map[string]string{},
		commandCounts: map[string]int{},
	}
}

// Command satisfies the clcommon.Tmux interface. Returns a no-op
// exit-0 / exit-1 exec.Cmd (absolute-path true/false, hermetic under
// empty PATH — see trueBin/falseBin) whose Run() exits with the
// appropriate status; for verbs that mutate state (send-keys,
// kill-session), the mutation happens here before the cmd is returned.
func (t *TmuxSim) Command(args ...string) *exec.Cmd {
	if len(args) > 0 {
		t.mu.Lock()
		t.commandCounts[args[0]]++
		t.mu.Unlock()
	}
	switch {
	case len(args) >= 3 && args[0] == "has-session" && args[1] == "-t":
		if name := t.resolveTarget(args[2]); name != "" && t.IsAlive(name) {
			return exec.Command(trueBin)
		}
		return exec.Command(falseBin)
	case len(args) >= 4 && args[0] == "send-keys" && args[1] == "-t":
		t.routeSendKeys(args[2], args[3])
	case len(args) >= 3 && args[0] == "set-buffer":
		t.setBuffer(args[1:])
	case len(args) >= 3 && args[0] == "paste-buffer":
		t.pasteBuffer(args[1:])
	case len(args) >= 3 && args[0] == "kill-session" && args[1] == "-t":
		t.killSession(args[2])
	case len(args) >= 2 && args[0] == "display-message" && args[1] == "-p":
		return t.displayMessage(args)
	case len(args) >= 1 && args[0] == "capture-pane":
		return t.capturePane(args)
	}
	return exec.Command(trueBin)
}

// paneRenderer is the optional capability a PaneSim implements to answer
// `tmux capture-pane`: it returns the bytes the pane currently shows. CCSim
// renders a footer carrying the "/rc" remote-control pill when armed, so the
// daemon's on-demand readback (observeRemoteControl) has something to scan; a
// pane sim without it (CodexSim) captures as empty, which the reader treats as
// indeterminate — fine, since only remote-control-capable harnesses are ever
// observed.
type paneRenderer interface {
	RenderPane() string
}

// capturePane models `tmux capture-pane -p -e -J -t <target>`: it echoes the
// target pane's rendered content to stdout so the caller's .Output() reads it
// back (the same hermetic echoBin trick as displayMessage). A missing or
// no-longer-alive pane exits non-zero (falseBin) — exactly how real tmux fails
// the capture on a dead target — which observeRemoteControl reads as
// "unknown". A pane that doesn't implement paneRenderer echoes empty content.
func (t *TmuxSim) capturePane(args []string) *exec.Cmd {
	target := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-t" {
			target = args[i+1]
		}
	}
	name := t.resolveTarget(target)
	t.mu.Lock()
	s, ok := t.sessions[name]
	t.mu.Unlock()
	if !ok || s.pane == nil || !s.pane.IsAlive() {
		return exec.Command(falseBin)
	}
	if r, isRenderer := s.pane.(paneRenderer); isRenderer {
		return exec.Command(echoBin, r.RenderPane())
	}
	return exec.Command(echoBin, "")
}

// displayMessage models `tmux display-message -p -t <target> '#{pane_pid}'`:
// it echoes the target pane's launch-unique fake pid to stdout so the
// caller's .Output() reads it back. A missing or no-longer-alive session
// exits non-zero (falseBin) — exactly how real tmux fails the query when
// the session is gone — which livePanePID reads as pid 0. Only #{pane_pid}
// is modelled; any other format string would still get the pid (the only
// field the daemon ever asks for).
func (t *TmuxSim) displayMessage(args []string) *exec.Cmd {
	target := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-t" {
			target = args[i+1]
		}
	}
	name := t.resolveTarget(target)
	t.mu.Lock()
	s, ok := t.sessions[name]
	t.mu.Unlock()
	if !ok || (s.pane != nil && !s.pane.IsAlive()) {
		return exec.Command(falseBin)
	}
	return exec.Command(echoBin, strconv.Itoa(s.panePID))
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

// resolveTarget models tmux's target-session resolution for a -t argument
// (cmd-find): strip any ":window.pane" suffix, then an optional leading
// '=' pins EXACT name matching; a bare name resolves exact-first with a
// unique-prefix fallback. The prefix fallback is deliberately modelled —
// it is the production footgun clcommon.ExactTarget exists to avoid (a
// dead name silently resolving to a live "-N" namesake), so a dropped '='
// shows up here as a flow-test failure instead of a wrong-pane delivery
// in production. Returns the resolved session-table key, or "" when
// nothing matches (an ambiguous prefix errors in real tmux; the sim
// treats it as no match).
func (t *TmuxSim) resolveTarget(target string) string {
	name := strings.SplitN(target, ":", 2)[0]
	exact := strings.HasPrefix(name, "=")
	name = strings.TrimPrefix(name, "=")
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.sessions[name]; ok {
		return name
	}
	if exact || name == "" {
		return ""
	}
	found := ""
	for k := range t.sessions {
		if strings.HasPrefix(k, name) {
			if found != "" {
				return "" // ambiguous — real tmux errors out
			}
			found = k
		}
	}
	return found
}

// normalizeTarget is the form send-keys targets are LOGGED in: the '='
// exactness marker is resolution detail, not identity, so it is stripped —
// assertions keep matching "name:0.0" whether production sent the target
// bare or '='-pinned.
func normalizeTarget(target string) string {
	return strings.TrimPrefix(target, "=")
}

// routeSendKeys logs the call and forwards text to the attached pane
// sim. Target is "<sessionName>:0.0" or bare "<sessionName>", optionally
// '='-pinned; resolution goes through resolveTarget.
func (t *TmuxSim) routeSendKeys(target, text string) {
	sessName := t.resolveTarget(target)
	t.mu.Lock()
	t.sentLog = append(t.sentLog, SentKey{Target: normalizeTarget(target), Text: text})
	s, ok := t.sessions[sessName]
	t.mu.Unlock()
	if ok && s.pane != nil {
		s.pane.Receive(text)
	}
}

// killSession removes the targeted session from the alive table and tears
// down its attached pane sim (mirrors tmux dropping the foreground
// process). Target resolution mirrors real tmux (see resolveTarget).
func (t *TmuxSim) killSession(target string) {
	name := t.resolveTarget(target)
	if name == "" {
		return
	}
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
	t.nextPID++
	t.sessions[name] = &tmuxSession{name: name, cwd: cwd, pane: pane, panePID: t.nextPID}
}

// MarkAlive registers a session without an attached pane sim. Used for
// scenarios that need a live session entry but don't drive simulator
// behavior. Has-session returns true; send-keys is logged but
// silently dropped.
func (t *TmuxSim) MarkAlive(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.sessions[name]; !ok {
		t.nextPID++
		t.sessions[name] = &tmuxSession{name: name, panePID: t.nextPID}
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
