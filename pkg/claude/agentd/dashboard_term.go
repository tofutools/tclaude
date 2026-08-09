package agentd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// In-browser terminal fallback for the dashboard's "open terminal" /
// "open window" actions, used when openTerminal can't pop a native
// GUI window — no DISPLAY/WAYLAND_DISPLAY (headless agentd), or no
// terminal emulator installed at all. Rather than erroring out,
// handleDashboardTermAPI / handleDashboardOpenWindowAPI point the
// dashboard at one of the WS routes below, which stream a real PTY
// straight into the page (the Preact terminal shell + vendored xterm.js).
//
// Ported from the former standalone `tclaude web` handleWS implementation,
// generalised to run an arbitrary `sh -c` command instead of a hardcoded tmux
// attach — see runPTYOverWS.

// termWSUpgrader upgrades the dashboard's terminal WebSocket requests.
// CheckOrigin always passes: checkDashboardAuth has already pinned the
// cookie + Origin (or accepted a pre-authed remote-mTLS request)
// before either WS handler reaches the upgrade, so a second Origin
// check here would only duplicate that logic — mirrors the equally
// permissive CheckOrigin in the former standalone `tclaude web` server.
var termWSUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// termSessionName builds the tmux session name backing an ad hoc
// browser terminal for (convID, which). Distinct prefix from real
// agent session names (which equal the human-chosen label), so it can
// never collide with one — and this session is never written to the
// sessions DB table, so it's invisible to every DB-row-driven
// dashboard surface (agent list, tray counts, worktree cleanup). It
// persists on the tclaude tmux server until killed manually or the
// host restarts; tclaude adds no reaper for it, matching how tmux
// sessions already behave everywhere else in this codebase.
//
// The identity is a hash of the *full* convID, not the display-shortened
// short8(convID): two conversations that share the same first 8 chars
// would otherwise hash to the same session name and `tmux new-session -A`
// would attach them to the same browser terminal.
func termSessionName(convID, which string) string {
	sum := sha256.Sum256([]byte(convID))
	return fmt.Sprintf("tclaude-term-%x-%s", sum[:8], which)
}

// handleDashboardTermWS is the in-browser fallback for "open
// terminal": it ensures (attach-or-create, so reconnects land back in
// the same shell) a tmux session at the agent's resolved directory and
// streams a PTY attached to it over the WebSocket.
//
//	GET /api/term-ws/{conv}?which=start|current|worktree
//
// Same threat model as the rest of /api/* — the dashboard cookie +
// Origin pin (or remote pre-auth) is the human-consent layer.
func handleDashboardTermWS(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/term-ws/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/term-ws/{conv}", http.StatusNotFound)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/term-ws/{conv}/"+parts[1], http.StatusNotFound)
		return
	}
	convSelector := parts[0]
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	which, ok := normaliseWhich(r.URL.Query().Get("which"))
	if !ok {
		http.Error(w, `which must be "start", "current", or "worktree"`, http.StatusBadRequest)
		return
	}
	start, current, worktree, _ := resolveDirs(res.ConvID)
	dir := pickDir(which, start, current, worktree)
	if dir == "" {
		http.Error(w, "no known "+which+" directory for "+short8(res.ConvID), http.StatusNotFound)
		return
	}
	name := termSessionName(res.ConvID, which)
	if err := session.RequireExternalTmuxServer(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	clientFlags := strings.TrimSpace(webTerminalTmuxFlags() + " " + session.ExternalTmuxNoStartFlag())
	cmd := fmt.Sprintf("tmux -L %s %s new-session -A -s %s -c %s",
		clcommon.TmuxSocketName, clientFlags, shellSingleQuote(name), shellSingleQuote(dir))
	runPTYOverWS(w, r, cmd, name, nil)
}

// groupTermSessionName builds the tmux session name backing an ad hoc
// browser terminal opened in a GROUP's default directory. Distinct
// prefix from both real agent session names (the human-chosen label)
// and per-agent term sessions (termSessionName), so it can never
// collide with either. The identity is a hash of the group name, so
// re-opening the same group's terminal attaches back to the same shell
// (`tmux new-session -A`). Like termSessionName, this session is never
// written to the sessions DB table, so it stays invisible to every
// DB-row-driven dashboard surface.
func groupTermSessionName(groupName string) string {
	sum := sha256.Sum256([]byte(groupName))
	return fmt.Sprintf("tclaude-groupterm-%x", sum[:8])
}

// handleDashboardGroupTermWS is the group counterpart of
// handleDashboardTermWS: it opens an in-browser shell in a GROUP's
// default working directory (agent_groups.default_cwd) rather than a
// single agent's resolved dir. Backs the group ⚙ menu's "open web
// terminal" item.
//
//	GET /api/group-term-ws/{group}
//
// The directory is captured at FIRST open: groupTermSessionName keys the
// backing tmux session on the group name alone, and `tmux new-session -A`
// re-attaches an existing session in its original dir, ignoring the -c
// here. So changing the group's default_cwd (or renaming it) after a
// terminal was opened re-attaches the old shell in the old dir until that
// session is killed — the same first-open-wins behaviour termSessionName
// documents for the per-agent path.
//
// Same threat model as the rest of /api/* — the dashboard cookie +
// Origin pin (or remote pre-auth) is the human-consent layer.
func handleDashboardGroupTermWS(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/group-term-ws/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/group-term-ws/{group}", http.StatusNotFound)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/group-term-ws/{group}/"+parts[1], http.StatusNotFound)
		return
	}
	name := parts[0]
	if u, err := url.PathUnescape(name); err == nil {
		name = u
	}
	g, err := db.GetAgentGroupByName(name)
	if err != nil || g == nil {
		http.Error(w, "resolve group: "+name, http.StatusNotFound)
		return
	}
	dir := strings.TrimSpace(g.DefaultCwd)
	if dir == "" {
		http.Error(w, "group "+name+" has no default working directory set", http.StatusNotFound)
		return
	}
	sessName := groupTermSessionName(g.Name)
	if err := session.RequireExternalTmuxServer(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	clientFlags := strings.TrimSpace(webTerminalTmuxFlags() + " " + session.ExternalTmuxNoStartFlag())
	cmd := fmt.Sprintf("tmux -L %s %s new-session -A -s %s -c %s",
		clcommon.TmuxSocketName, clientFlags, shellSingleQuote(sessName), shellSingleQuote(dir))
	runPTYOverWS(w, r, cmd, sessName, nil)
}

// handleDashboardOpenWindowWS is the in-browser fallback for "open
// window": it streams a PTY running the exact same `tclaude session
// attach <label>` command openAttachCmd already builds for the native
// path, landing the human in the agent's live Claude Code TUI with no
// GUI required.
//
//	GET /api/open-window-ws/{conv}
//
// Same threat model as the rest of /api/* — the dashboard cookie +
// Origin pin (or remote pre-auth) is the human-consent layer.
func handleDashboardOpenWindowWS(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/open-window-ws/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/open-window-ws/{conv}", http.StatusNotFound)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/open-window-ws/{conv}/"+parts[1], http.StatusNotFound)
		return
	}
	convSelector := parts[0]
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	sess := pickAliveSession(res.ConvID)
	if sess == nil {
		http.Error(w, "no live tmux session for "+short8(res.ConvID), http.StatusNotFound)
		return
	}
	// Force the attach (tmux `attach-session -d`): it atomically detaches any
	// client already on this session before attaching ours. Without --force,
	// `tclaude session attach` sees the session still "attached in another
	// terminal", bails without attaching, and this PTY exits at once;
	// runPTYOverWS's teardown detach then drops the OLD window — so the new web
	// window flashes an "already attached" error while the previous window
	// silently closes. Detaching the old client is exactly what we want here
	// (opening a web window is an explicit "console on this agent HERE"
	// gesture), and doing it atomically as part of the attach needs no separate
	// detach/confirm round-trip. See openAttachCmdForce.
	//
	// Caveat — this is fully clean only when the displaced client is a NATIVE
	// terminal (no runPTYOverWS behind it). If the displaced client is ANOTHER
	// web window on this same session, `-d` detaches it and its runPTYOverWS
	// exits, whose teardown then runs a whole-session detachTmuxSession (see its
	// comment) that also drops the client we just attached — the new web window
	// blanks moments after opening. Closing that residual needs the per-client
	// (#{client_tty}) teardown detachTmuxSession already flags as future work;
	// until then this is still strictly better than the pre-fix behaviour and
	// correct for the common native-terminal case.
	h, _ := harness.Get(sess.Harness)
	runPTYOverWS(w, r, webTerminalAttachCmd(openAttachCmdForce(sess.ID)), sess.TmuxSession, h)
}

// spawnFocusWSPath builds the /api/spawn-focus-ws/{label} path the
// spawn endpoint hands back (as focus_ws) when auto-focus targets the browser
// or could not pop a native window. Label-keyed, not conv-keyed — see
// handleDashboardSpawnFocusWS.
func spawnFocusWSPath(label string) string {
	return "/api/spawn-focus-ws/" + url.PathEscape(label)
}

// handleDashboardSpawnFocusWS is the browser target/fallback for spawn
// auto-focus. The response points the dashboard here when web terminals are
// configured, or when a native open fails, instead of silently opening
// nothing while claiming success — see spawnOutcome.FocusMode / handleGroupSpawn.
//
// Label-keyed rather than conv-keyed, like pending_focus.go's attach:
// a freshly-spawned pane may not have a conv-id yet (a gated Codex
// spawn, or a CC spawn whose hook hasn't landed). A deferred OpenCode response
// can even precede the pane; waitForDashboardSpawnFocusSession bridges that
// bounded pending-launch interval.
//
//	GET /api/spawn-focus-ws/{label}
//
// Same threat model as the rest of /api/* — the dashboard cookie +
// Origin pin (or remote pre-auth) is the human-consent layer.
func handleDashboardSpawnFocusWS(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	label := strings.TrimPrefix(r.URL.Path, "/api/spawn-focus-ws/")
	if u, err := url.PathUnescape(label); err == nil {
		label = u
	}
	if label == "" {
		http.Error(w, "expected /api/spawn-focus-ws/{label}", http.StatusNotFound)
		return
	}
	sess := waitForDashboardSpawnFocusSession(r, label)
	if sess == nil {
		http.Error(w, "no tmux pane for "+label, http.StatusNotFound)
		return
	}
	h, _ := harness.Get(sess.Harness)
	runPTYOverWS(w, r, webTerminalAttachCmd(openAttachCmd(label)), sess.TmuxSession, h)
}

// waitForDashboardSpawnFocusSession bridges the one spawn shape where the
// browser-focus response can precede the pane: deferred OpenCode server boot.
// Keep the websocket handshake pending while the label is still a durable
// pending-spawn reservation, bounded beyond OpenCode's startup timeout plus
// the short pane fork. Ordinary missing labels still fail immediately, and a
// canceled/failed pending spawn stops waiting as soon as its reservation goes.
func waitForDashboardSpawnFocusSession(r *http.Request, label string) *db.SessionRow {
	const paneForkGrace = 3 * time.Second
	deadline := time.NewTimer(openCodeStartupTimeout + paneForkGrace)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		sess, err := db.LoadSession(label)
		if err == nil && sess != nil && sess.TmuxSession != "" {
			return sess
		}
		pending, pendingErr := db.GetPendingSpawn(label)
		if pendingErr != nil || pending == nil {
			return nil
		}
		select {
		case <-r.Context().Done():
			return nil
		case <-deadline.C:
			return nil
		case <-ticker.C:
		}
	}
}

// termWSHook lets the env-gated real-browser terminal smoke observe and
// redirect runPTYOverWS without touching the HTTP routes above it: the smoke
// swaps the tmux/attach command for a deterministic PTY program and counts
// PTY starts, applied resizes, and teardowns from outside the browser
// (TCL-490). Same seam discipline as openTerminal / clcommon.Default — a
// package-level variable that is nil in production and swapped only from
// testhooks (SetTermWSHookForTest).
type termWSHook struct {
	// RewriteCommand replaces the shell command (and associated tmux session)
	// a terminal WebSocket route hands to runPTYOverWS.
	RewriteCommand func(shellCommand, tmuxSession string) (string, string)
	// OnStart observes the PTY child process right after pty.Start.
	OnStart func(proc *os.Process)
	// OnResize observes each resize actually APPLIED to the PTY (only after a
	// successful pty.Setsize, so a smoke can never pass on failed resizes).
	OnResize func(cols, rows int)
	// OnTeardown observes the completed per-connection teardown (detach +
	// PTY close + process-group hangup + reap) — exactly once per PTY.
	OnTeardown func()
}

var termWSTestHook *termWSHook

// termResizeMsg is sent from the browser when the xterm instance
// resizes. Mirrors the former standalone `tclaude web` resize payload.
type termResizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// parseTermResize classifies one WebSocket frame for the PTY bridge. isResize
// is true for any {"type":"resize",...} text frame — those are consumed by the
// bridge and never written to the PTY, valid or not, matching the pre-existing
// behaviour. size is non-nil only when the dimensions are usable (positive and
// within the uint16 range a PTY winsize can carry).
func parseTermResize(messageType int, data []byte) (size *pty.Winsize, isResize bool) {
	if messageType != websocket.TextMessage {
		return nil, false
	}
	var msg termResizeMsg
	if json.Unmarshal(data, &msg) != nil || msg.Type != "resize" {
		return nil, false
	}
	if msg.Cols <= 0 || msg.Rows <= 0 || msg.Cols > 0xffff || msg.Rows > 0xffff {
		return nil, true
	}
	return &pty.Winsize{Cols: uint16(msg.Cols), Rows: uint16(msg.Rows)}, true
}

// initialResizeWait bounds how long runPTYOverWS holds the command back
// waiting for the client's opening resize message. Every client of these
// routes (the dashboard widget, the standalone terminal page, the remote TUI)
// sends its size immediately on open, so in practice this wait is a few
// milliseconds plus one network round trip; the timeout only covers a client
// that dies mid-handshake or predates the resize protocol. Var, not const, so
// tests can shrink it.
var initialResizeWait = 1 * time.Second

// defaultPTYWinsize is the fallback size for a PTY whose client never said how
// big it is. 80x24 is the least-wrong guess; what must never happen is
// starting the command on a 0x0 PTY — a tmux client that reads that before the
// first real resize lands renders a minimal-width window, and if it also
// misses the resize's SIGWINCH (see winchProcessGroup) it stays that way.
var defaultPTYWinsize = pty.Winsize{Cols: 80, Rows: 24}

// detachTmuxSession asks the tmux server to detach every client attached to
// tmuxSession; the clients drop their view and the session keeps running,
// detached. This is the reliable way to make closing the web window/term modal
// actually detach on the tmux level: it commands the always-running tmux server
// directly (`tmux -L tclaude detach-client -s …`), so it works even though the
// tmux client is a forked child that a PTY-close hangup / process-group SIGHUP
// did not reliably tear down in the field (see hangupProcessGroup). Best-effort
// — a missing session/server just makes tmux exit non-zero, which we ignore.
//
// This detaches ALL of the session's clients, so if the agent also has a native
// terminal window attached, closing the web view detaches that too. That's an
// accepted simplification for now; detaching only our own client (by its PTY
// tty, which tmux exposes as #{client_tty}) is a deliberate future refinement.
func detachTmuxSession(tmuxSession string, h *harness.Harness) {
	if tmuxSession == "" {
		return
	}
	_ = clcommon.TmuxCommand("detach-client", "-s", clcommon.ExactTarget(tmuxSession)).Run()
	session.CancelTmuxScrollback(tmuxSession, h)
}

// hangupProcessGroup sends SIGHUP to the whole process group led by proc, not
// just proc itself. It is a teardown BACKSTOP — the reliable detach is
// detachTmuxSession (which commands the tmux server directly); this just makes
// sure the wrapper process and anything it forked actually exit if that did not
// already bring them down.
//
// Why the group and not just proc: runPTYOverWS's child is `sh -c "exec tclaude
// session attach …"` (open-window) or `sh -c "tmux new-session …"` (open-term).
// In the open-window case the wrapper — sh, exec-replaced by tclaude, so the
// same pid as proc — FORKS the tmux client as a child, so a SIGHUP to proc
// alone misses it. pty.Start started the wrapper with Setsid, so it leads a
// process group whose pgid == pid; a kill to the negative pid reaches proc AND
// that forked tmux client. (On its own this signal proved unreliable for
// detaching the client in the field — hence detachTmuxSession — but it is still
// a cheap, correct way to reap the process tree.) The tmux SERVER is a separate
// long-running daemon outside this group, so the underlying session keeps
// running.
//
// Targeting the negative pid is safe even if Setsid somehow didn't take: a
// process group with id == proc.Pid exists only while proc actually leads one,
// so the worst case is ESRCH — it can never reach agentd's own group. If the
// group send fails (e.g. everything already exited), fall back to signaling
// proc directly so behaviour never regresses below the old single-PID signal.
func hangupProcessGroup(proc *os.Process) {
	if proc == nil {
		return
	}
	if err := syscall.Kill(-proc.Pid, syscall.SIGHUP); err != nil {
		_ = proc.Signal(syscall.SIGHUP)
	}
}

// winchProcessGroup delivers SIGWINCH to the PTY's whole process group after a
// resize was applied. The kernel raises SIGWINCH only when TIOCSWINSZ actually
// CHANGES the size, which leaves one stuck state: a tmux client that read the
// pre-resize tty size and missed the change's signal while it was still
// starting up (its handler not yet installed) keeps rendering the old size,
// and a client re-sending the same size to repair exactly that — as the
// dashboard's post-open refit does — never generates another signal. The
// explicit group signal makes every resize message a full re-sync regardless
// of whether the kernel considered it a change. Group not pid for the same
// reason as hangupProcessGroup: the tmux client may be a forked child of the
// wrapper. SIGWINCH's default action is to be ignored, so over-delivery to the
// wrapper is harmless, and the same negative-pid safety argument applies.
func winchProcessGroup(proc *os.Process) {
	if proc == nil {
		return
	}
	if err := syscall.Kill(-proc.Pid, syscall.SIGWINCH); err != nil {
		_ = proc.Signal(syscall.SIGWINCH)
	}
}

// webTerminalTmuxFlags are the tmux client flags for a terminal rendered by the
// dashboard's xterm.js, spelled for the `sh -c` command strings the PTY sites
// build. The browser terminal loads a linkHandler for OSC 8, so it can honestly
// claim the hyperlink capability tmux gates hyperlink passthrough on — without
// it, a link a harness draws with label text (rather than a bare URL) arrives as
// dead text, because tmux keeps the target in its grid and never emits it.
//
// A PTY site that reaches tmux through `tclaude session attach` cannot spell the
// flag here — the wrapper builds the tmux argv itself — and uses
// webTerminalAttachCmd instead.
func webTerminalTmuxFlags() string {
	return "-T " + clcommon.TmuxHyperlinksFeature
}

// webTerminalAttachCmd carries the same OSC 8 opt-in across the one process hop
// `tclaude session attach` adds, by exporting it for exactly that command.
//
// The assignment is a prefix on the command rather than an entry in the PTY's
// own environment on purpose. A browser terminal is an interactive shell the
// operator runs things in, and anything started there — a daemon restart, say —
// would inherit a process-wide copy and hand it to native terminal attaches it
// later opens, re-enabling hyperlinks on the very terminals whose renderer we
// know nothing about. Scoping it to the exec'd command keeps the claim attached
// to the client it is true of. `VAR=value exec cmd` exports VAR to cmd.
func webTerminalAttachCmd(attachCommand string) string {
	return clcommon.TmuxClientFeaturesEnv + "=" + clcommon.TmuxHyperlinksFeature + " " + attachCommand
}

// runPTYOverWS upgrades the request to a WebSocket and pumps a PTY
// running `sh -c shellCommand` over it: PTY output → binary WS
// messages, WS messages → PTY input, except a {"type":"resize",...}
// JSON text message, which resizes the PTY instead of being written to
// it. Ported from the former standalone `tclaude web` handleWS implementation,
// generalised to take an arbitrary command instead of a hardcoded
// `tmux attach-session`. Callers must call checkDashboardAuth before
// reaching here — this function performs no auth of its own.
//
// tmuxSession is the tmux session this PTY attaches to (the agent's
// `spwn-…` / ad hoc `tclaude-term-…` session, on the `-L tclaude` server).
// On teardown it is handed to detachTmuxSession so closing the modal actually
// detaches on the tmux level. tmuxHarness identifies a managed agent whose
// harness uses native tmux scrollback; ad hoc terminals pass nil. Pass "" when
// there is no associated session (then teardown falls back to the process-group
// SIGHUP alone).
func runPTYOverWS(w http.ResponseWriter, r *http.Request, shellCommand, tmuxSession string, tmuxHarness *harness.Harness) {
	hook := termWSTestHook
	if hook != nil && hook.RewriteCommand != nil {
		shellCommand, tmuxSession = hook.RewriteCommand(shellCommand, tmuxSession)
	}
	conn, err := upgradeTerminalWebSocket(w, r)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	// One goroutine owns conn reads for the whole connection so frames can be
	// consumed both before the PTY exists (the initial-size wait below) and
	// after, in order. It exits when the connection errors (any teardown path
	// closes conn) or when runPTYOverWS returns (readerDone, for early-exit
	// paths where nothing is draining frames).
	type wsFrame struct {
		messageType int
		data        []byte
	}
	frames := make(chan wsFrame)
	readerDone := make(chan struct{})
	defer close(readerDone)
	go func() {
		defer close(frames)
		for {
			messageType, data, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			select {
			case frames <- wsFrame{messageType, data}:
			case <-readerDone:
				return
			}
		}
	}()

	// Hold the command back (briefly) until the client has said how big its
	// terminal is, so the PTY is born at the right size instead of 0x0. The
	// kernel raises SIGWINCH only on an actual size CHANGE, so a tmux client
	// starting on a 0x0 PTY could read that size and — if the real resize
	// landed while its signal handling was still being installed — keep it,
	// rendering a minimal-width window until the next genuine resize. Starting
	// at the final size removes that race entirely. Input frames arriving
	// before the size are rare (the resize is the first thing every client
	// sends) but preserved in order.
	var initial *pty.Winsize
	var pending []wsFrame
	waitTimer := time.NewTimer(initialResizeWait)
	defer waitTimer.Stop()
initialSize:
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return // client went away before the command ever started
			}
			size, isResize := parseTermResize(frame.messageType, frame.data)
			if isResize {
				if size != nil {
					initial = size
					break initialSize
				}
				continue
			}
			pending = append(pending, frame)
		case <-waitTimer.C:
			break initialSize
		}
	}

	cmd := exec.Command("sh", "-c", shellCommand)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	startSize := defaultPTYWinsize
	if initial != nil {
		startSize = *initial
	}
	ptmx, err := pty.StartWithSize(cmd, &startSize)
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, "Error: %v\r\n", err))
		return
	}
	// Attach/detach/resize at INFO on purpose (TCL-1136): these are the
	// events that permanently change a managed pane's size under tmux's
	// `window-size latest`, and the size-drift investigation had no way to
	// tell after the fact whether ANYTHING had ever attached. The size in the
	// attach line is what the pane is being fitted to the moment the client
	// arrives.
	attachedAt := time.Now()
	slog.Info("browser terminal attached",
		"tmux_session", tmuxSession, "path", r.URL.Path,
		"size", fmt.Sprintf("%dx%d", startSize.Cols, startSize.Rows))
	if hook != nil && hook.OnStart != nil {
		hook.OnStart(cmd.Process)
	}
	// The initial size was applied (by StartWithSize) exactly like a resize
	// message's Setsize, so the smoke's applied-resize observer sees it too.
	if hook != nil && hook.OnResize != nil && initial != nil {
		hook.OnResize(int(startSize.Cols), int(startSize.Rows))
	}
	defer func() {
		// Reliable detach first: tell the tmux server to drop the session's
		// clients. Then tear down the PTY/process tree (the SIGHUP is a
		// backstop — see hangupProcessGroup).
		detachTmuxSession(tmuxSession, tmuxHarness)
		_ = ptmx.Close()
		hangupProcessGroup(cmd.Process)
		_ = cmd.Wait()
		slog.Info("browser terminal detached",
			"tmux_session", tmuxSession, "path", r.URL.Path,
			"attached_for", time.Since(attachedAt).Round(time.Second).String())
		if hook != nil && hook.OnTeardown != nil {
			hook.OnTeardown()
		}
	}()

	// Closing ptmx unblocks the PTY->WS pump; closing conn unblocks the
	// connection-reader goroutine, whose channel close in turn ends the
	// WS->PTY pump. Whichever pump exits first runs this once, so the
	// other side can never stay blocked and wg.Wait() always completes —
	// e.g. when the PTY EOFs (shell/tmux exited) the reader would
	// otherwise stay parked in conn.ReadMessage() forever. The
	// outer defers (conn.Close, then detachTmuxSession + ptmx.Close + a
	// process-group SIGHUP + cmd.Wait) still run afterwards; the double
	// close is a harmless no-op. The underlying tmux session lives on —
	// detach-client drops our CLIENT (and any others) but never touches the
	// tmux server daemon, so the session keeps running detached.
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = ptmx.Close()
			_ = conn.Close()
		})
	}

	// Input that arrived while the command was being sized and started.
	for _, frame := range pending {
		_, _ = ptmx.Write(frame.data)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// PTY -> WebSocket
	go func() {
		defer wg.Done()
		defer closeBoth()
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// WebSocket -> PTY (input + resize). Runs until the reader goroutine
	// closes frames (which it does once the connection errors — every
	// teardown path closes conn), so the reader can never stay blocked on a
	// frame nobody is receiving.
	go func() {
		defer wg.Done()
		defer closeBoth()
		for frame := range frames {
			size, isResize := parseTermResize(frame.messageType, frame.data)
			if isResize {
				if size != nil {
					// Best-effort as before (a failed resize never kills the
					// stream); the hook fires only for APPLIED resizes.
					if err := pty.Setsize(ptmx, size); err == nil {
						winchProcessGroup(cmd.Process)
						slog.Info("browser terminal resized",
							"tmux_session", tmuxSession, "path", r.URL.Path,
							"size", fmt.Sprintf("%dx%d", size.Cols, size.Rows))
						if hook != nil && hook.OnResize != nil {
							hook.OnResize(int(size.Cols), int(size.Rows))
						}
					}
				}
				continue
			}
			_, _ = ptmx.Write(frame.data)
		}
	}()

	wg.Wait()
}

// upgradeTerminalWebSocket carries response headers already written by the
// dashboard auth gate into Gorilla's hijacked 101 response. This is
// load-bearing for restart grace: Set-Cookie rotates an old session to the new
// process token during the WebSocket handshake.
func upgradeTerminalWebSocket(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return termWSUpgrader.Upgrade(w, r, w.Header().Clone())
}
