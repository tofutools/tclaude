package agentd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// In-browser terminal fallback for the dashboard's "open terminal" /
// "open window" actions, used when openTerminal can't pop a native
// GUI window — no DISPLAY/WAYLAND_DISPLAY (headless agentd), or no
// terminal emulator installed at all. Rather than erroring out,
// handleDashboardTermAPI / handleDashboardOpenWindowAPI point the
// dashboard at one of the WS routes below, which stream a real PTY
// straight into the page (js/modal-term.js + vendored xterm.js).
//
// Ported from the deprecated pkg/claude/web package's handleWS
// (server.go), generalised to run an arbitrary `sh -c` command instead
// of a hardcoded tmux attach — see runPTYOverWS.

// termWSUpgrader upgrades the dashboard's terminal WebSocket requests.
// CheckOrigin always passes: checkDashboardAuth has already pinned the
// cookie + Origin (or accepted a pre-authed remote-mTLS request)
// before either WS handler reaches the upgrade, so a second Origin
// check here would only duplicate that logic — mirrors the equally
// permissive CheckOrigin in pkg/claude/web/server.go.
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
	cmd := fmt.Sprintf("tmux -L %s new-session -A -s %s -c %s",
		clcommon.TmuxSocketName, shellSingleQuote(name), shellSingleQuote(dir))
	runPTYOverWS(w, r, cmd, name)
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
	runPTYOverWS(w, r, openAttachCmdForce(sess.ID), sess.TmuxSession)
}

// spawnFocusWSPath builds the /api/spawn-focus-ws/{label} path the
// spawn endpoint hands back (as focus_ws) when auto-focus could not
// pop a native window. Label-keyed, not conv-keyed — see
// handleDashboardSpawnFocusWS.
func spawnFocusWSPath(label string) string {
	return "/api/spawn-focus-ws/" + url.PathEscape(label)
}

// handleDashboardSpawnFocusWS is the in-browser fallback for spawn
// auto-focus: when executeSpawn's focusSpawn closure can't pop a
// native terminal window (no DISPLAY/WAYLAND_DISPLAY, or no terminal
// emulator installed), the spawn response points the dashboard here
// instead of silently opening nothing while claiming success — see
// spawnOutcome.FocusMode / handleGroupSpawn.
//
// Label-keyed rather than conv-keyed, like pending_focus.go's attach:
// a freshly-spawned pane may not have a conv-id yet (a gated Codex
// spawn, or a CC spawn whose hook hasn't landed), but its label is
// known the moment the pane exists.
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
	sess, err := db.LoadSession(label)
	if err != nil || sess == nil || sess.TmuxSession == "" {
		http.Error(w, "no tmux pane for "+label, http.StatusNotFound)
		return
	}
	runPTYOverWS(w, r, openAttachCmd(label), sess.TmuxSession)
}

// termResizeMsg is sent from the browser when the xterm instance
// resizes. Mirrors pkg/claude/web/server.go's resizeMsg.
type termResizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

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
func detachTmuxSession(tmuxSession string) {
	if tmuxSession == "" {
		return
	}
	_ = clcommon.TmuxCommand("detach-client", "-s", tmuxSession).Run()
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

// runPTYOverWS upgrades the request to a WebSocket and pumps a PTY
// running `sh -c shellCommand` over it: PTY output → binary WS
// messages, WS messages → PTY input, except a {"type":"resize",...}
// JSON text message, which resizes the PTY instead of being written to
// it. Ported from the deprecated pkg/claude/web package's handleWS,
// generalised to take an arbitrary command instead of a hardcoded
// `tmux attach-session`. Callers must call checkDashboardAuth before
// reaching here — this function performs no auth of its own.
//
// tmuxSession is the tmux session this PTY attaches to (the agent's
// `spwn-…` / ad hoc `tclaude-term-…` session, on the `-L tclaude` server).
// On teardown it is handed to detachTmuxSession so closing the modal actually
// detaches on the tmux level. Pass "" when there is no associated session
// (then teardown falls back to the process-group SIGHUP alone).
func runPTYOverWS(w http.ResponseWriter, r *http.Request, shellCommand, tmuxSession string) {
	conn, err := termWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	cmd := exec.Command("sh", "-c", shellCommand)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, "Error: %v\r\n", err))
		return
	}
	defer func() {
		// Reliable detach first: tell the tmux server to drop the session's
		// clients. Then tear down the PTY/process tree (the SIGHUP is a
		// backstop — see hangupProcessGroup).
		detachTmuxSession(tmuxSession)
		_ = ptmx.Close()
		hangupProcessGroup(cmd.Process)
		_ = cmd.Wait()
	}()

	// Closing ptmx unblocks the PTY->WS reader; closing conn unblocks the
	// WS->PTY reader. Whichever pump exits first runs this once, so the
	// other side can never stay blocked and wg.Wait() always completes —
	// e.g. when the PTY EOFs (shell/tmux exited) the WS->PTY goroutine
	// would otherwise stay parked in conn.ReadMessage() forever. The
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

	// WebSocket -> PTY (input + resize)
	go func() {
		defer wg.Done()
		defer closeBoth()
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.TextMessage {
				var msg termResizeMsg
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
					if msg.Cols > 0 && msg.Rows > 0 {
						_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(msg.Cols), Rows: uint16(msg.Rows)})
					}
					continue
				}
			}
			_, _ = ptmx.Write(data)
		}
	}()

	wg.Wait()
}
