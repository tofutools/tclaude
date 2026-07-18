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
	// sleepBin lets a flow test model a tmux client process that starts but
	// never answers promptly. Production callers must bound that subprocess;
	// otherwise one stuck has-session/send-keys holds a per-target delivery
	// worker forever. As with true/false/echo, keep the path hermetic.
	sleepBin = resolveCoreutil("sleep")
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
	// paste injectTextAndSubmit uses to land multi-line text.
	buffers map[string]string
	// commandCounts records every Command(verb, …) invocation by verb
	// (args[0]) — exposed via CommandCount for regression tests that
	// pin "snapshot collapses to ONE list-sessions and ZERO
	// has-session calls". An accessor rather than a getter on the
	// whole map so the lock stays internal.
	commandCounts   map[string]int
	mutationTargets map[string][]string
	// commandFaults are one-shot subprocess faults, keyed by tmux verb. They
	// are consumed before the verb's ordinary simulated behavior: fail makes
	// Run exit 1; hang makes it run `sleep` for the requested duration. These
	// model the two real delivery failures that matter at this boundary while
	// keeping the queue/database paths fully production.
	commandFaults map[string][]tmuxCommandFault
	// nextPID hands each newly-registered session a distinct, launch-unique
	// fake pane pid (mirrors tmux giving every new pane process a fresh OS
	// pid). Re-registering the same name — the test stand-in for a resume
	// reusing a conv-id-derived tmux name — therefore yields a DIFFERENT pid,
	// which is exactly what the soft-exit retry's livePanePID guard keys on.
	nextPID int
}

type tmuxCommandFault struct {
	fail bool
	hang time.Duration
}

type tmuxSession struct {
	name           string
	paneID         string
	cwd            string
	pane           PaneSim
	panePID        int
	remainOnExit   bool
	paneDiedHook   string
	paneDead       bool
	exitStatus     string
	exitSignal     string
	exitGeneration string
}

func newTmuxSim() *TmuxSim {
	return &TmuxSim{
		sessions:        map[string]*tmuxSession{},
		buffers:         map[string]string{},
		commandCounts:   map[string]int{},
		mutationTargets: map[string][]string{},
		commandFaults:   map[string][]tmuxCommandFault{},
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
		if fault, ok := t.takeCommandFault(args[0]); ok {
			if fault.fail {
				return exec.Command(falseBin)
			}
			return exec.Command(sleepBin, strconv.FormatFloat(fault.hang.Seconds(), 'f', 3, 64))
		}
	}
	switch {
	case len(args) >= 3 && args[0] == "has-session" && args[1] == "-t":
		if name := t.resolveTarget(args[2], false); name != "" && t.hasSession(name) {
			return exec.Command(trueBin)
		}
		return exec.Command(falseBin)
	case len(args) >= 1 && args[0] == "list-sessions":
		// The bounded production snapshot uses Command + Output rather than
		// the unbounded ListSessions interface. Echo the same live names so
		// flow tests exercise the real timeout/parse path.
		t.mu.Lock()
		names := make([]string, 0, len(t.sessions))
		for name := range t.sessions {
			names = append(names, name)
		}
		t.mu.Unlock()
		alive := names[:0]
		for _, name := range names {
			if t.IsAlive(name) {
				alive = append(alive, name)
			}
		}
		return exec.Command(echoBin, strings.Join(alive, "\n"))
	case len(args) >= 4 && args[0] == "send-keys" && args[1] == "-t":
		t.routeSendKeys(args[2], args[3])
	case len(args) >= 5 && args[0] == "send-keys" && args[1] == "-l" && args[2] == "-t":
		// Literal mode prevents text such as "Enter" or "C-c" from being
		// interpreted as tmux key names. The pane simulator already treats
		// routed text literally; account for the extra production flag here.
		t.routeSendKeys(args[3], args[4])
	case len(args) >= 3 && args[0] == "set-buffer":
		t.setBuffer(args[1:])
	case len(args) >= 3 && args[0] == "paste-buffer":
		t.pasteBuffer(args[1:])
	case len(args) >= 3 && args[0] == "kill-session" && args[1] == "-t":
		t.recordMutationTarget("kill-session", args[2])
		t.killSession(args[2])
	case len(args) >= 3 && args[0] == "kill-pane" && args[1] == "-t":
		t.recordMutationTarget("kill-pane", args[2])
		t.killSession(args[2])
	case len(args) >= 1 && args[0] == "show-hooks":
		return exec.Command(echoBin, "pane-died")
	case len(args) >= 2 && args[0] == "display-message" && args[1] == "-p":
		return t.displayMessage(args)
	case len(args) >= 1 && args[0] == "capture-pane":
		return t.capturePane(args)
	case len(args) >= 6 && args[0] == "set-option" && args[1] == "-p" && args[2] == "-u" && args[3] == "-t":
		name := t.resolveTarget(args[4], true)
		if name == "" {
			return exec.Command(falseBin)
		}
		t.mu.Lock()
		if s := t.sessions[name]; s != nil && args[5] == "@tclaude_exit_generation" {
			s.exitGeneration = ""
		}
		t.mu.Unlock()
		return exec.Command(trueBin)
	case len(args) >= 6 && args[0] == "set-option" && args[1] == "-p" && args[2] == "-t":
		name := t.resolveTarget(args[3], true)
		if name == "" {
			return exec.Command(falseBin)
		}
		t.mu.Lock()
		if s := t.sessions[name]; s != nil && args[4] == "remain-on-exit" {
			s.remainOnExit = args[5] == "on"
		} else if s != nil && args[4] == "@tclaude_exit_generation" {
			s.exitGeneration = args[5]
		}
		t.mu.Unlock()
		return exec.Command(trueBin)
	case len(args) >= 6 && args[0] == "set-hook" && args[1] == "-p" && args[2] == "-t":
		name := t.resolveTarget(args[3], true)
		if name == "" || args[4] != "pane-died" {
			return exec.Command(falseBin)
		}
		t.mu.Lock()
		if s := t.sessions[name]; s != nil {
			s.paneDiedHook = args[5]
		}
		t.mu.Unlock()
		return exec.Command(trueBin)
	case len(args) >= 6 && args[0] == "set-hook" && args[1] == "-p" && args[2] == "-u" && args[3] == "-t":
		name := t.resolveTarget(args[4], true)
		if name == "" || args[5] != "pane-died" {
			return exec.Command(falseBin)
		}
		t.mu.Lock()
		if s := t.sessions[name]; s != nil {
			s.paneDiedHook = ""
		}
		t.mu.Unlock()
		return exec.Command(trueBin)
	case len(args) >= 3 && args[0] == "set-option" && args[1] == "-t":
		// Pane-typed like the real command, so the production set-option
		// sites' ExactTarget(name)+":" form is exercised through the same
		// target-resolution rules as send-keys (a colon-less '=' pin
		// would silently no-op or misroute in real tmux — see
		// resolveTarget). The option itself isn't modelled; only whether
		// the target resolves.
		if name := t.resolveTarget(args[2], true); name != "" && t.IsAlive(name) {
			return exec.Command(trueBin)
		}
		return exec.Command(falseBin)
	case len(args) >= 3 && args[0] == "list-panes" && args[1] == "-t":
		return t.listPanes(args[2])
	}
	return exec.Command(trueBin)
}

func (t *TmuxSim) recordMutationTarget(verb, target string) {
	t.mu.Lock()
	t.mutationTargets[verb] = append(t.mutationTargets[verb], target)
	t.mu.Unlock()
}

func (t *TmuxSim) MutationTargets(verb string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.mutationTargets[verb]...)
}

// FailNextCommand makes the next invocation of verb return exit status 1
// without applying its ordinary simulated behavior. Faults queue FIFO so a
// flow can describe a short sequence of failures deterministically.
func (t *TmuxSim) FailNextCommand(verb string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commandFaults[verb] = append(t.commandFaults[verb], tmuxCommandFault{fail: true})
}

// HangNextCommand makes the next invocation of verb remain in Run for d
// before exiting successfully, without applying its ordinary simulated
// behavior. Use a duration longer than the production timeout under test.
func (t *TmuxSim) HangNextCommand(verb string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commandFaults[verb] = append(t.commandFaults[verb], tmuxCommandFault{hang: d})
}

func (t *TmuxSim) takeCommandFault(verb string) (tmuxCommandFault, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	faults := t.commandFaults[verb]
	if len(faults) == 0 {
		return tmuxCommandFault{}, false
	}
	fault := faults[0]
	if len(faults) == 1 {
		delete(t.commandFaults, verb)
	} else {
		t.commandFaults[verb] = faults[1:]
	}
	return fault, true
}

// listPanes models `tmux list-panes -t <target> -F '#{pane_pid}'` for the
// sim's one-pane sessions: it echoes the resolved session's pane pid (the
// only field production asks for, via ParsePIDFromTmux) and exits non-zero
// on a missing/dead target like real tmux. Resolution uses the pane-typed
// rules — deliberately stricter than real list-panes (window-typed, whose
// colon-less '=name' would still prefix-fall-back onto the session table
// un-pinned), so only a session-exact target form passes.
func (t *TmuxSim) listPanes(target string) *exec.Cmd {
	name := t.resolveTarget(target, true)
	t.mu.Lock()
	s, ok := t.sessions[name]
	t.mu.Unlock()
	if !ok || (s.pane != nil && !s.pane.IsAlive()) {
		return exec.Command(falseBin)
	}
	return exec.Command(echoBin, strconv.Itoa(s.panePID))
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
	name := t.resolveTarget(target, true)
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

// displayMessage models the pane_pid and pane_current_path formats used by
// production lifecycle code. A missing or no-longer-alive session exits
// non-zero (falseBin), exactly like real tmux.
func (t *TmuxSim) displayMessage(args []string) *exec.Cmd {
	target := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-t" {
			target = args[i+1]
		}
	}
	name := t.resolveTarget(target, true)
	t.mu.Lock()
	s, ok := t.sessions[name]
	if !ok {
		t.mu.Unlock()
		return exec.Command(falseBin)
	}
	dead := !t.sessionPaneAlive(s)
	remainOnExit, cwd, paneID, panePID := s.remainOnExit, s.cwd, s.paneID, s.panePID
	exitStatus, exitSignal, exitGeneration := s.exitStatus, s.exitSignal, s.exitGeneration
	t.mu.Unlock()
	if dead && !remainOnExit {
		return exec.Command(falseBin)
	}
	format := args[len(args)-1]
	if format == "#{pane_current_path}" {
		return exec.Command(echoBin, cwd)
	}
	if format == "#{pane_dead}" {
		if dead {
			return exec.Command(echoBin, "1")
		}
		return exec.Command(echoBin, "0")
	}
	if format == "#{pane_id}" {
		return exec.Command(echoBin, paneID)
	}
	if format == "#{pane_dead}|#{pane_pid}" {
		deadValue := "0"
		if dead {
			deadValue = "1"
		}
		return exec.Command(echoBin, deadValue+"|"+strconv.Itoa(panePID))
	}
	if format == "#{session_name}|#{pane_id}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}" {
		deadValue := "0"
		if dead {
			deadValue = "1"
		}
		return exec.Command(echoBin, name+"|"+paneID+"|"+deadValue+"|"+exitStatus+"|"+exitSignal+"|"+exitGeneration)
	}
	if format == "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}" {
		deadValue := "0"
		if dead {
			deadValue = "1"
		}
		return exec.Command(echoBin, name+"|"+paneID+"|"+strconv.Itoa(panePID)+"|"+deadValue+"|"+exitStatus+"|"+exitSignal+"|"+exitGeneration)
	}
	return exec.Command(echoBin, strconv.Itoa(panePID))
}

func (t *TmuxSim) sessionPaneAlive(s *tmuxSession) bool {
	return s != nil && !s.paneDead && (s.pane == nil || s.pane.IsAlive())
}

func (t *TmuxSim) hasSession(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[name]
	return s != nil && (t.sessionPaneAlive(s) || s.remainOnExit)
}

// setBuffer models `tmux set-buffer -b <name> <data>` — it stores the
// trailing data argument under the named buffer. injectTextAndSubmit stages
// multiline input this way before pasting it.
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

// resolveTarget models tmux's target resolution for a -t argument
// (cmd-find): strip any ":window.pane" suffix, then an optional leading
// '=' pins EXACT name matching; a bare name resolves exact-first with a
// unique-prefix fallback. Two real-tmux behaviors are deliberately
// modelled because they are the production footguns clcommon.ExactTarget
// and its doc comment exist to avoid — each shows up here as a flow-test
// failure instead of a wrong-pane delivery in production:
//
//  1. The prefix fallback: a dead bare name silently resolving to a live
//     "-N" namesake (a dropped '=').
//  2. Pane-typed parsing: for a pane-typed command (paneTyped=true —
//     send-keys, display-message, capture-pane, paste-buffer), a
//     COLON-LESS target lands whole in the pane slot where tmux never
//     strips the '=', so "=name" hunts a pane literally named "=name"
//     and matches nothing (a '=' in the wrong position). Session-typed
//     commands (has-session, kill-session) parse the same target as a
//     session name and DO strip the '='.
//
// Returns the resolved session-table key, or "" when nothing matches (an
// ambiguous prefix errors in real tmux; the sim treats it as no match).
func (t *TmuxSim) resolveTarget(target string, paneTyped bool) string {
	if strings.HasPrefix(strings.TrimPrefix(target, "="), "%") {
		paneID := strings.TrimPrefix(strings.TrimPrefix(target, "="), "%")
		pid, err := strconv.Atoi(paneID)
		if err != nil {
			return ""
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		for name, session := range t.sessions {
			if strings.TrimPrefix(session.paneID, "%") == strconv.Itoa(pid) {
				return name
			}
		}
		return ""
	}
	name, _, hadColon := strings.Cut(target, ":")
	if paneTyped && !hadColon && strings.HasPrefix(name, "=") {
		return ""
	}
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
// '='-pinned; resolution goes through resolveTarget (pane-typed, like
// real send-keys).
func (t *TmuxSim) routeSendKeys(target, text string) {
	sessName := t.resolveTarget(target, true)
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
	name := t.resolveTarget(target, false)
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
	t.sessions[name] = &tmuxSession{name: name, cwd: cwd, pane: pane, paneID: "%" + strconv.Itoa(t.nextPID), panePID: t.nextPID}
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
		t.sessions[name] = &tmuxSession{name: name, paneID: "%" + strconv.Itoa(t.nextPID), panePID: t.nextPID}
	}
}

// SetPaneIdentityForTest gives a simulated pane independently controlled tmux
// pane and operating-system process identities.
func (t *TmuxSim) SetPaneIdentityForTest(name, paneID string, panePID int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.sessions[name]; s != nil {
		s.paneID, s.panePID = paneID, panePID
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

// MarkPaneDead retains a pane-shaped corpse with exact tmux status/signal
// evidence, matching pane-local remain-on-exit behavior.
func (t *TmuxSim) MarkPaneDead(name string, exitCode *int, signal string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[name]
	if s == nil {
		return
	}
	s.remainOnExit = true
	s.paneDead = true
	s.exitStatus = ""
	if exitCode != nil {
		s.exitStatus = strconv.Itoa(*exitCode)
	}
	s.exitSignal = signal
}

// SetPaneExitGeneration binds retained-pane evidence to one simulated launch.
func (t *TmuxSim) SetPaneExitGeneration(name, generation string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.sessions[name]; s != nil {
		s.exitGeneration = generation
	}
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
	defer t.mu.Unlock()
	return t.sessionPaneAlive(t.sessions[name])
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
		entries := append([]SentKey(nil), t.sentLog...)
		t.mu.Unlock()
		for _, sk := range entries {
			if strings.Contains(sk.Text, contains) && (sk.Target == target || t.resolveTarget(sk.Target, true) == t.resolveTarget(target, true)) {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
