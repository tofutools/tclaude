package agentd

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
// window the agent itself cannot. Read + open are both ungated:
// reporting a path is harmless, and opening a terminal is something
// the human asked for (it's their machine). Identity is still
// resolved for the audit log.

// dirResp is the wire shape for GET .../dir.
type dirResp struct {
	ConvID      string `json:"conv_id"`
	StartDir    string `json:"start_dir"`             // Claude Code launch dir
	CurrentDir  string `json:"current_dir"`           // most-recent dir the agent edited in
	WorktreeDir string `json:"worktree_dir"`          // git working-tree root of current_dir
	Source      string `json:"source"`                // "hook" (tracked) | "fallback" (== start_dir)
	CallerConv  string `json:"caller_conv,omitempty"` // set when a different agent asked
}

// dirOpenResp is the wire shape for POST .../dir.
type dirOpenResp struct {
	ConvID     string `json:"conv_id"`
	Dir        string `json:"dir"`
	Which      string `json:"which"`
	Opened     bool   `json:"opened"`
	CallerConv string `json:"caller_conv,omitempty"`
}

// gitToplevelOf is the seam for resolving a directory's git
// working-tree root. Production shells out to git; flow tests swap in
// a stub so they don't need a real repo on disk.
var gitToplevelOf = realGitToplevel

// realGitToplevel returns the git working-tree root containing dir —
// the linked-worktree root when dir is inside a linked worktree, the
// main repo root otherwise. Returns ("", false) when dir isn't in a
// git repo, or git isn't on PATH.
func realGitToplevel(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", false
	}
	return top, true
}

// resolveDirs returns the three directories tclaude tracks for a conv.
//
//   - current_dir falls back to start_dir when the PostToolUse hook
//     hasn't recorded an edit yet (fresh agent, or one that's only
//     read files / run commands). source distinguishes the two cases.
//   - worktree_dir is the git working-tree root containing current_dir,
//     falling back to start_dir when current_dir isn't in a git repo.
func resolveDirs(convID string) (startDir, currentDir, worktreeDir, source string) {
	if sess, err := db.FindSessionByConvID(convID); err == nil && sess != nil {
		startDir = sess.Cwd
	}
	if startDir == "" {
		if row := agent.FreshConvRowResolved(convID); row != nil {
			startDir = row.ProjectPath
		}
	}
	currentDir, source = startDir, "fallback"
	if w, err := db.GetAgentWorkdir(convID); err == nil && w.Dir != "" {
		currentDir, source = w.Dir, "hook"
	}
	worktreeDir = startDir
	if top, ok := gitToplevelOf(currentDir); ok {
		worktreeDir = top
	}
	return startDir, currentDir, worktreeDir, source
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
		openDirTerminal(w, r, convID, caller)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

func writeDirInfo(w http.ResponseWriter, convID, caller string) {
	start, current, worktree, source := resolveDirs(convID)
	if start == "" && current == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"no known directory for "+short8(convID)+
				" — no session row yet (is the agent running under tclaude?)")
		return
	}
	resp := dirResp{
		ConvID: convID, StartDir: start, CurrentDir: current,
		WorktreeDir: worktree, Source: source,
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
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
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
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
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

// openShellCmd builds the `sh -c` payload terminal.OpenWithCommand
// runs: cd into dir, then exec an interactive shell so the window
// stays open for the human instead of closing when a command exits.
func openShellCmd(dir string) string {
	return "cd " + shellSingleQuote(dir) + ` && exec "${SHELL:-bash}"`
}

// shellSingleQuote wraps s so it survives as a single shell word — the
// dir comes from our own DB, but it can legitimately contain spaces.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
