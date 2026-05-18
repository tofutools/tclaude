package agentd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
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
	ConvID        string `json:"conv_id"`
	StartDir      string `json:"start_dir"`               // Claude Code launch dir
	StartBranch   string `json:"start_branch,omitempty"`  // git branch of start_dir
	CurrentDir    string `json:"current_dir"`             // most-recent dir the agent edited in
	WorktreeDir   string `json:"worktree_dir"`            // git working-tree root of current_dir
	CurrentBranch string `json:"current_branch,omitempty"` // git branch of worktree_dir
	Source        string `json:"source"`                  // "hook" (tracked) | "fallback" (== start_dir)
	CallerConv    string `json:"caller_conv,omitempty"`   // set when a different agent asked
}

// dirOpenResp is the wire shape for POST .../dir.
type dirOpenResp struct {
	ConvID     string `json:"conv_id"`
	Dir        string `json:"dir"`
	Which      string `json:"which"`
	Opened     bool   `json:"opened"`
	CallerConv string `json:"caller_conv,omitempty"`
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
		if !isHuman && callerConv != convID {
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
	resp := dirOpenResp{ConvID: convID, Dir: dir, Which: which, Opened: true}
	if caller != "" && caller != convID {
		resp.CallerConv = caller
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
	if err := openTerminal(openShellCmd(dir)); err != nil {
		http.Error(w, "open terminal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"dir": dir, "which": which})
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
func openAttachCmd(label string) string {
	return clcommon.DetectAbsoluteCmd("session", "attach") + " " + shellSingleQuote(label)
}
