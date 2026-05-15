package agentd

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// worktrees.go backs the optional worktree picker in the dashboard's
// spawn and clone modals. The picker is a convenience layer over
// `tclaude worktree`: it never spawns anything itself, it just
// resolves a git worktree directory the caller then passes as the
// spawn/clone `cwd`. Dashboard-only (cookie auth) — agents reach
// worktrees through the `tclaude worktree` CLI, not this endpoint.

// worktreeView is one row in the GET /api/worktrees response.
type worktreeView struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	IsMain bool   `json:"is_main"`
}

// handleDashboardWorktreesAPI dispatches:
//
//	GET  /api/worktrees?repo=<path>   → worktrees + branches of the repo containing <path>
//	POST /api/worktrees               → create a worktree, return its path
func handleDashboardWorktreesAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		dashboardListWorktrees(w, r)
	case http.MethodPost:
		dashboardCreateWorktree(w, r)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// dashboardListWorktrees answers GET /api/worktrees?repo=<path>.
//
// A missing or non-repo `repo` is NOT an error — the picker simply
// shows "not a git repo" and the spawn proceeds with the raw cwd. So
// this always 200s on a reachable daemon; the `is_repo` flag tells the
// client which branch of the UI to render.
func dashboardListWorktrees(w http.ResponseWriter, r *http.Request) {
	repo := expandTilde(strings.TrimSpace(r.URL.Query().Get("repo")))
	if repo == "" {
		writeJSON(w, http.StatusOK, map[string]any{"is_repo": false})
		return
	}
	root, err := worktree.RepoRootForPath(repo)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"is_repo": false, "reason": err.Error()})
		return
	}
	wts, err := worktree.ListWorktreesIn(root)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "worktree", err.Error())
		return
	}
	views := make([]worktreeView, 0, len(wts))
	for _, wt := range wts {
		views = append(views, worktreeView{Path: wt.Path, Branch: wt.Branch, IsMain: wt.IsMain})
	}
	defBranch, _ := worktree.DefaultBranchIn(root)
	writeJSON(w, http.StatusOK, map[string]any{
		"is_repo":        true,
		"repo_root":      root,
		"default_branch": defBranch,
		"worktrees":      views,
		"branches":       worktree.BranchesIn(root),
	})
}

// dashboardCreateWorktree answers POST /api/worktrees. Body:
// {repo, branch, from_branch?, path?}. Creates the worktree (a new
// branch off from_branch, or a checkout of an existing branch) and
// returns its absolute path so the caller can spawn into it.
func dashboardCreateWorktree(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo       string `json:"repo"`
		Branch     string `json:"branch"`
		FromBranch string `json:"from_branch"`
		Path       string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	repo := expandTilde(strings.TrimSpace(body.Repo))
	if repo == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "repo is required")
		return
	}
	if strings.TrimSpace(body.Branch) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "branch is required")
		return
	}
	path, err := worktree.AddWorktreeIn(repo, body.Branch, body.FromBranch,
		expandTilde(strings.TrimSpace(body.Path)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "worktree", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":   path,
		"branch": strings.TrimSpace(body.Branch),
	})
}
