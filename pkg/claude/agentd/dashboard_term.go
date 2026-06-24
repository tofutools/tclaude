package agentd

import (
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
func termSessionName(convID, which string) string {
	return "tclaude-term-" + short8(convID) + "-" + which
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
	runPTYOverWS(w, r, cmd)
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
	runPTYOverWS(w, r, openAttachCmd(sess.ID))
}

// termResizeMsg is sent from the browser when the xterm instance
// resizes. Mirrors pkg/claude/web/server.go's resizeMsg.
type termResizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// runPTYOverWS upgrades the request to a WebSocket and pumps a PTY
// running `sh -c shellCommand` over it: PTY output → binary WS
// messages, WS messages → PTY input, except a {"type":"resize",...}
// JSON text message, which resizes the PTY instead of being written to
// it. Ported from the deprecated pkg/claude/web package's handleWS,
// generalised to take an arbitrary command instead of a hardcoded
// `tmux attach-session`. Callers must call checkDashboardAuth before
// reaching here — this function performs no auth of its own.
func runPTYOverWS(w http.ResponseWriter, r *http.Request, shellCommand string) {
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
		_ = ptmx.Close()
		_ = cmd.Process.Signal(syscall.SIGHUP)
		_ = cmd.Wait()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// PTY -> WebSocket
	go func() {
		defer wg.Done()
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
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				// Client disconnected — detach cleanly; the deferred
				// SIGHUP above only reaches the `sh -c` wrapper, the
				// tmux/tclaude session itself lives on.
				_ = ptmx.Close()
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
