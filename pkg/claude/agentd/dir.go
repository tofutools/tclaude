package agentd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
)

// --- /v1/whoami/dir and /v1/agent/{selector}/dir ---
//
// These report (and act on) an agent's directories:
//
//   - start_dir    — where Claude Code was launched (sessions.cwd)
//   - current_dir  — the most-recent dir the agent edited files in,
//     recorded by the PostToolUse hook into agent_workdir
//   - worktree_dir — the git working-tree root containing current_dir
//     (a linked-worktree root, or the main repo root); falls back to
//     start_dir when current_dir isn't in a git repo
//
// GET returns all three. POST opens a terminal window in one of them —
// the daemon runs outside the agent's sandbox, so it can spawn the
// window the agent itself cannot. GET is ungated: reporting a path is
// harmless. POST is gated: an agent may open a terminal only for
// itself; spawning a window targeting another agent is human-only
// (the human is the one whose desktop the window lands on).

// dirResp is the wire shape for GET .../dir.
type dirResp struct {
	// AgentID is the resolved agent's stable actor key — the canonical ID
	// the CLI/dashboard leads with; ConvID is the live generation behind
	// it (kept as the snapshot/hover). "" when the conv is not a known
	// agent.
	AgentID       string `json:"agent_id,omitempty"`
	ConvID        string `json:"conv_id"`
	StartDir      string `json:"start_dir"`                // Claude Code launch dir
	StartBranch   string `json:"start_branch,omitempty"`   // git branch of start_dir
	CurrentDir    string `json:"current_dir"`              // most-recent dir the agent edited in
	WorktreeDir   string `json:"worktree_dir"`             // git working-tree root of current_dir
	CurrentBranch string `json:"current_branch,omitempty"` // git branch of worktree_dir
	Source        string `json:"source"`                   // "hook" (tracked) | "fallback" (== start_dir)
	CallerConv    string `json:"caller_conv,omitempty"`    // set when a different agent asked
	// CallerAgentID is the asking agent's stable actor key — companion to
	// CallerConv, set when a different agent asked.
	CallerAgentID string `json:"caller_agent_id,omitempty"`
}

// dirOpenResp is the wire shape for POST .../dir.
type dirOpenResp struct {
	// AgentID is the resolved agent's stable actor key — the canonical ID
	// the CLI/dashboard leads with; ConvID is the live generation behind
	// it. "" when the conv is not a known agent.
	AgentID    string `json:"agent_id,omitempty"`
	ConvID     string `json:"conv_id"`
	Dir        string `json:"dir"`
	Which      string `json:"which"`
	Opened     bool   `json:"opened"`
	CallerConv string `json:"caller_conv,omitempty"`
	// CallerAgentID is the asking agent's stable actor key — companion to
	// CallerConv, set when a different agent asked.
	CallerAgentID string `json:"caller_agent_id,omitempty"`
}

// dirRepairResp is the wire shape for POST /v1/whoami/dir/repair.
type dirRepairResp struct {
	ConvID   string `json:"conv_id"`
	Dir      string `json:"dir"`
	Repaired bool   `json:"repaired"`
}

// recordedStartupDir returns the immutable physical launch directory when the
// session has trusted resume provenance. Keeping the physical path matters when
// sessions.cwd used a symlink that was later removed or retargeted. Legacy rows
// have no provenance, so they retain the recorded lexical cwd as a best-effort
// fallback.
func recordedStartupDir(sess *db.SessionRow) string {
	if sess == nil {
		return ""
	}
	if provenance, err := resumeprovenance.Decode(sess.ResumeProvenance); err == nil {
		return strings.TrimSpace(provenance.Cwd.Path)
	}
	return strings.TrimSpace(sess.Cwd)
}

// resolveDirs adapts agent.ResolveLocation to this endpoint's wire
// vocabulary, returning the three directories tclaude tracks:
//
//   - current_dir falls back to start_dir when the PostToolUse hook
//     hasn't recorded an edit yet (fresh agent, or one that's only
//     read files / run commands). source distinguishes the two cases.
//   - worktree_dir is the git working-tree root containing current_dir,
//     falling back to start_dir when current_dir isn't in a git repo.
//
// The git resolution itself happens once, in the hook, at edit time —
// resolveDirs is a pure read of stored state (see agent.ResolveLocation).
func resolveDirs(convID string) (startDir, currentDir, worktreeDir, source string) {
	loc := agent.ResolveLocation(convID)
	source = "fallback"
	if loc.Tracked {
		source = "hook"
	}
	return loc.StartupDir, loc.EditDir, loc.CurrentDir, source
}

// pickDir selects one of the three resolved dirs by name. which is
// already normalised (lower-cased, "" → "current"). validWhich must
// have passed first.
func pickDir(which, start, current, worktree string) string {
	switch which {
	case "start":
		return start
	case "worktree":
		return worktree
	default: // "current"
		return current
	}
}

// normaliseWhich lower-cases and defaults the which selector, and
// reports whether it's one tclaude understands.
func normaliseWhich(raw string) (which string, ok bool) {
	which = strings.ToLower(strings.TrimSpace(raw))
	if which == "" {
		which = "current"
	}
	return which, which == "start" || which == "current" || which == "worktree"
}

// handleWhoamiDir serves the calling agent's own directory info.
//
//	GET  /v1/whoami/dir → dirResp
//	POST /v1/whoami/dir → open a terminal (body: {"which":"start|current|worktree"})
func handleWhoamiDir(w http.ResponseWriter, r *http.Request) {
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeDirInfo(w, convID, "")
	case http.MethodPost:
		openDirTerminal(w, r, convID, "")
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleWhoamiDirRepair recreates exactly the calling agent's immutable
// startup directory. The request accepts no path: sessions.cwd is the trusted
// selector, so this is not an agentd-mediated arbitrary mkdir primitive and
// does not require granting the sandbox write access to the parent directory.
//
// It intentionally creates only directories. Reconstructing Git worktree
// metadata, branches, or later directories an agent moved into is outside this
// command's contract; the purpose is to unbrick the agent's original root so
// its existing sandbox mounts become reachable again.
func handleWhoamiDirRepair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfDirRepair)
	if !ok {
		return
	}
	sess, err := db.FindSessionByConvID(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "repair_failed",
			"load recorded startup directory: "+err.Error())
		return
	}
	if sess == nil || strings.TrimSpace(sess.Cwd) == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"no recorded startup directory for "+short8(convID))
		return
	}
	dir := recordedStartupDir(sess)
	if !filepath.IsAbs(dir) {
		writeError(w, http.StatusConflict, "invalid_startup_dir",
			"recorded startup directory is not absolute; refusing host-side repair")
		return
	}
	missing, err := launchDirMissing(dir)
	if err != nil {
		writeError(w, http.StatusConflict, "path_conflict", err.Error())
		return
	}
	// Always traverse the full path through descriptor-held parents, even for
	// an existing directory. That both refuses symlink substitution and closes
	// the os.Stat/os.MkdirAll check-use race for this default-granted host-side
	// mkdir capability.
	if err := mkdirAllNoFollow(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "repair_failed",
			"recreate recorded startup directory: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dirRepairResp{ConvID: convID, Dir: dir, Repaired: missing})
}

// handleAgentDir serves another agent's directory info — the
// manager-pattern / human form, reached via /v1/agent/{selector}/dir.
// Read-only and ungated, mirroring /v1/lookup.
func handleAgentDir(w http.ResponseWriter, r *http.Request, convID string) {
	caller := peerFromContext(r.Context()).ConvID
	switch r.Method {
	case http.MethodGet:
		writeDirInfo(w, convID, caller)
	case http.MethodPost:
		// Opening a terminal lands a window on the human's desktop. An
		// agent may do that for itself, but spawning windows targeting
		// *other* agents is human-only. The human operator always passes;
		// an agent passes only when it IS the target; unidentified /
		// unconfirmed callers are refused fail-closed.
		callerConv, isHuman, ok := authedCaller(w, r)
		if !ok {
			return
		}
		// "Only for itself" is matched on the stable actor (JOH-323): a
		// caller naming any of its own generations is still itself, so the
		// gate compares agents, not the exact conv generation. sameActor
		// only ever widens "self" to the same agent's generations — it
		// never lets one agent open a terminal targeting a different agent
		// (distinct agent_ids), so the human-only cross-agent rule holds.
		if !isHuman && !sameActor(callerConv, convID) {
			writeError(w, http.StatusForbidden, "permission",
				"an agent may open a terminal only for itself; cross-agent terminal spawn is human-only")
			return
		}
		openDirTerminal(w, r, convID, caller)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

func writeDirInfo(w http.ResponseWriter, convID, caller string) {
	loc := agent.ResolveLocation(convID)
	if loc.StartupDir == "" && loc.EditDir == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"no known directory for "+short8(convID)+
				" — no session row yet (is the agent running under tclaude?)")
		return
	}
	source := "fallback"
	if loc.Tracked {
		source = "hook"
	}
	resp := dirResp{
		AgentID:       peerAgentID(convID),
		ConvID:        convID,
		StartDir:      loc.StartupDir,
		StartBranch:   loc.StartupBranch,
		CurrentDir:    loc.EditDir,
		WorktreeDir:   loc.CurrentDir,
		CurrentBranch: loc.CurrentBranch,
		Source:        source,
	}
	if caller != "" && caller != convID {
		resp.CallerConv = caller
		resp.CallerAgentID = peerAgentID(caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// openDirTerminal spawns a terminal window in one of the agent's
// directories. The daemon is outside the agent's sandbox, so it can
// do what the agent can't.
func openDirTerminal(w http.ResponseWriter, r *http.Request, convID, caller string) {
	var req struct {
		Which string `json:"which"`
	}
	// The body is optional — an empty body (io.EOF) means "use the
	// default". Any other decode error is malformed JSON: reject it
	// rather than silently opening the default directory.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_arg", "malformed JSON body: "+err.Error())
			return
		}
	}
	which, ok := normaliseWhich(req.Which)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			`which must be "start", "current", or "worktree"`)
		return
	}
	start, current, worktree, _ := resolveDirs(convID)
	dir := pickDir(which, start, current, worktree)
	if dir == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"no known "+which+" directory for "+short8(convID))
		return
	}
	if err := openTerminal(openShellCmd(dir)); err != nil {
		writeError(w, http.StatusInternalServerError, "open_failed",
			"could not open a terminal: "+err.Error())
		return
	}
	resp := dirOpenResp{AgentID: peerAgentID(convID), ConvID: convID, Dir: dir, Which: which, Opened: true}
	if caller != "" && caller != convID {
		resp.CallerConv = caller
		resp.CallerAgentID = peerAgentID(caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDashboardTermAPI is the cookie-authenticated dashboard twin of
// POST .../dir: it opens a terminal in one of an agent's directories.
// Wired into the loopback mux from registerDashboardEditRoutes.
//
//	POST /api/term/{conv}   body: {"which":"start|current|worktree"}
//
// Same threat model as the rest of /api/* — the dashboard cookie +
// Origin pin is the human-consent layer (see dashboard_edit.go).
func handleDashboardTermAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/term/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/term/{conv}", http.StatusNotFound)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/term/{conv}/"+parts[1], http.StatusNotFound)
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
	var body struct {
		Which string `json:"which"`
		// Web forces the in-browser PTY terminal even when a native GUI
		// window could be popped — the dashboard's dedicated "web term"
		// button sets it. The plain "term" button leaves it false and
		// keeps the native-first / browser-fallback behaviour below.
		Web bool `json:"web"`
	}
	// Optional body; empty (io.EOF) is fine, malformed JSON is a 400.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			http.Error(w, "malformed JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	which, ok := normaliseWhich(body.Which)
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
	// The "web term" button (body.Web) wants the in-browser terminal
	// unconditionally, so skip the native attempt entirely. Otherwise try a
	// native window first and fall back to the browser only if one can't be
	// popped (no display, no terminal emulator installed, …) instead of
	// failing outright.
	useBrowser := body.Web
	if !useBrowser {
		if err := openTerminal(openShellCmd(dir)); err != nil {
			useBrowser = true
		}
	}
	if useBrowser {
		// The dashboard terminal shell opens a WebSocket at ws and streams a PTY attached to
		// an ad hoc tmux session at dir (handleDashboardTermWS).
		writeJSON(w, http.StatusOK, map[string]string{
			"dir": dir, "which": which, "mode": "browser",
			"ws": "/api/term-ws/" + url.PathEscape(res.ConvID) + "?which=" + url.QueryEscape(which),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"dir": dir, "which": which, "mode": "native"})
}

// handleDashboardOpenWindowAPI opens a fresh terminal window ATTACHED to
// an agent's live tclaude session — the explicit way to get a console for
// a headless/detached agent, independent of the focus.raise_only setting
// (which, when on, makes plain focus a no-op for a windowless agent).
// Reuses the same openTerminal + openAttachCmd shape as spawn auto-focus.
// Wired in from registerDashboardEditRoutes.
//
//	POST /api/open-window/{conv}
//
// Same threat model as the rest of /api/* — the dashboard cookie + Origin
// pin is the human-consent layer (see dashboard_edit.go).
func handleDashboardOpenWindowAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/open-window/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/open-window/{conv}", http.StatusNotFound)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/open-window/{conv}/"+parts[1], http.StatusNotFound)
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
	var body struct {
		// Web forces the in-browser PTY terminal even when a native GUI
		// window could be popped — the dashboard's dedicated "web window"
		// button sets it. The plain "open window" button sends no body,
		// leaving it false and keeping the native-first / browser-fallback
		// behaviour below.
		Web bool `json:"web"`
	}
	// Optional body; empty (io.EOF) is fine, malformed JSON is a 400.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			http.Error(w, "malformed JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	sess := pickAliveSession(res.ConvID)
	if sess == nil {
		http.Error(w, "no live tmux session for "+short8(res.ConvID), http.StatusNotFound)
		return
	}
	// The "web window" button (body.Web) wants the in-browser terminal
	// unconditionally, so skip the native attempt entirely. Otherwise try a
	// native window first and fall back to the browser only if one can't be
	// popped, instead of failing outright.
	useBrowser := body.Web
	if !useBrowser {
		if err := openTerminal(openAttachCmd(sess.ID)); err != nil {
			useBrowser = true
		}
	}
	if useBrowser {
		// In-browser terminal attached to the same live session
		// (handleDashboardOpenWindowWS).
		writeJSON(w, http.StatusOK, map[string]string{
			"conv_id": res.ConvID, "label": sess.ID, "mode": "browser",
			"ws": "/api/open-window-ws/" + url.PathEscape(res.ConvID),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID, "label": sess.ID, "mode": "native"})
}

// openTerminal is the seam for spawning a terminal window. Production
// uses terminal.OpenWithCommand (platform-specific); flow tests swap
// in a recorder so they can assert which dir would have been opened
// without spawning a real window. Mirrors the clcommon.Default /
// agentd.Spawn boundary handles.
var openTerminal = terminal.OpenWithCommand

// openShellCmd builds the payload terminal.OpenWithCommand runs to
// land the human in an interactive shell at dir. The shape depends on
// how the resolved terminal will deliver the payload — see
// openShellCmdFor.
func openShellCmd(dir string) string {
	return openShellCmdFor(dir, terminal.ResolvedTerminal())
}

// openShellCmdFor is openShellCmd factored to take the terminal ID so
// it can be unit-tested without resolving a real terminal.
//
// Two shapes:
//
//   - `cd '<dir>'` — for AppleScript-driven terminals (iTerm2,
//     Terminal.app). Those launchers open a window with the user's
//     default profile, which is already an interactive login shell
//     with profile + rc loaded, and then keystroke the command into
//     it. The shell stays open regardless of what we type, so the
//     `exec ${SHELL}` keepalive would just round-trip back to the
//     same shell for nothing.
//
//   - `cd '<dir>' && exec sh -c 'exec "${SHELL:-bash}"'` — for every
//     other launcher. Those deliver the payload through `sh -c
//     '<cmd>'` (Linux + WSL) or `$SHELL -l -c '<cmd>'` (macOS CLI
//     terminals via loginShellArgv). Without the trailing exec, that
//     wrapping shell finishes after `cd` and the window closes; the
//     exec replaces it with the user's interactive shell.
//
// The `${SHELL:-bash}` expansion is wrapped in `sh -c '…'` so the
// outer shell never parses it directly: fish has no POSIX
// `${VAR:-default}` and errors on it. The single-quoted body is
// opaque to fish/bash/zsh/sh; only sh — chosen for its uniform
// parameter-expansion — evaluates it.
func openShellCmdFor(dir, terminalID string) string {
	cd := "cd " + shellSingleQuote(dir)
	if isAppleScriptTerminal(terminalID) {
		return cd
	}
	return cd + ` && exec sh -c 'exec "${SHELL:-bash}"'`
}

// isAppleScriptTerminal reports whether the terminal with id is driven
// via AppleScript (iTerm2 / Terminal.app on macOS). Those launchers
// keystroke into a shell that's already interactive, so the cd-only
// payload from openShellCmdFor is sufficient.
func isAppleScriptTerminal(id string) bool {
	return id == terminal.IDITerm2 || id == terminal.IDTerminalApp
}

// shellSingleQuote wraps s so it survives as a single shell word — the
// dir comes from our own DB, but it can legitimately contain spaces.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// openAttachCmd builds the `sh -c` payload that attaches a terminal to
// a tclaude session by its label. Unlike openShellCmd — a bare shell in
// a directory — this lands the human inside the agent's live Claude
// Code TUI. The attach always goes through the `tclaude` wrapper (never
// raw `tmux attach`) so the reattached session keeps its tclaude
// features: status bar, window-title stamping, focus/notify wiring.
//
// Used by the spawn flow's "auto focus" option — a freshly-spawned
// agent runs detached with no window of its own, so this is what gives
// it one. The absolute tclaude path is used because the daemon launches
// the terminal outside the human's login shell, where PATH may be
// minimal (same reasoning as openShellCmd's exec-the-login-shell).
//
// The `exec ` prefix replaces the wrapping shell with tclaude, so when
// a "hide" detaches the tmux client and tclaude exits, no shell is left
// holding the tab open. This matches the openShellCmd pattern: there
// the trailing `exec sh -c '...'` performs the same role for the
// interactive case. Without the prefix the iTerm2 / Terminal.app
// AppleScript drivers — which type the command into a default-profile
// interactive shell — would return to that shell's prompt after tclaude
// exits, leaving an orphaned tab. Linux / WSL / Ghostty / etc. already
// wrap the command in `sh -c`, so the prefix only changes whether sh
// terminates by exec-replacement (tab closes immediately) or by normal
// exit (same outcome — the tab still closed) — no behavioural drift.
// Skipped on Windows: cmd has no `exec` builtin and would try to spawn
// `exec.exe`, and the `/k` wrapper already keeps the window open by
// design after the command exits.
func openAttachCmd(label string) string {
	return attachCmd(label, false)
}

// openAttachCmdForce is openAttachCmd with `--force`, i.e. the attach maps to
// tmux `attach-session -d`: it atomically detaches any client already on the
// session and then attaches, instead of the unforced attach's bail ("already
// attached in another terminal", attaching nothing).
//
// The web-window open path (handleDashboardOpenWindowWS) uses this. Opening a
// web window is an explicit "give me a console on this agent HERE" gesture, and
// a browser pane can't meaningfully fall back to the unforced attach's
// TryFocusAttachedSession (raising a native OS window is pointless when the
// human is in the browser). Without --force the pane would attach nothing and
// exit, and runPTYOverWS's teardown detach would then drop the OLD window — the
// bug this fixes. `attach-session -d` detaches the old client as an atomic
// precondition of attaching the new one, so no separate detach/confirm step is
// needed.
//
// This is fully clean when the displaced client is a native terminal. When it
// is another web window on the same session, the caller comment in
// handleDashboardOpenWindowWS explains the residual whole-session-teardown race
// that a per-client (#{client_tty}) detach would close.
func openAttachCmdForce(label string) string {
	return attachCmd(label, true)
}

// attachCmd is the shared core of openAttachCmd / openAttachCmdForce. force adds
// `--force` (tmux `attach-session -d`). See openAttachCmd's doc for the `exec `
// prefix rationale (tab-close on hide) and the absolute-path reasoning.
func attachCmd(label string, force bool) string {
	cmd := clcommon.DetectAbsoluteCmd("session", "attach")
	if force {
		cmd += " --force"
	}
	cmd += " " + shellSingleQuote(label)
	if runtime.GOOS == "windows" {
		return cmd
	}
	return "exec " + cmd
}
