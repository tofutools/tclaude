package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// subRepoScanDepth caps how deep dashboardListWorktrees walks a
// non-repo directory looking for nested git repos. Two levels covers
// the common `monorepo/category/repo` layout; four absorbs a couple
// of extra grouping dirs without turning the scan into a full tree
// crawl.
const subRepoScanDepth = 4

const dashboardWorktreeTrackingRetries = 10

type dashboardWorktreeProgress struct {
	Attempt int `json:"attempt"`
	Max     int `json:"max"`
}

var (
	dashboardWorktreeProgressMu sync.Mutex
	dashboardWorktreeProgresses = map[string]dashboardWorktreeProgress{}
)

// dashboardListWorktrees answers GET /api/worktrees?repo=<path>.
//
// A missing or non-repo `repo` is NOT an error — the picker simply
// shows "not a git repo" and the spawn proceeds with the raw cwd. So
// this always 200s on a reachable daemon; the `is_repo` flag tells the
// client which branch of the UI to render.
//
// When `repo` isn't itself a git repo, the response also carries a
// `sub_repos` list of nested git repos found beneath it. That's the
// "virtual monorepo" case: the launch dir holds shared docs plus
// several independent repos, and the picker lets the human drill into
// one of them to worktree.
func dashboardListWorktrees(w http.ResponseWriter, r *http.Request) {
	repo := expandTilde(strings.TrimSpace(r.URL.Query().Get("repo")))
	if repo == "" {
		writeJSON(w, http.StatusOK, map[string]any{"is_repo": false})
		return
	}
	root, err := worktree.RepoRootForPath(repo)
	if err != nil {
		resp := map[string]any{"is_repo": false, "reason": err.Error()}
		if subs := worktree.FindSubRepos(repo, subRepoScanDepth); len(subs) > 0 {
			resp["sub_repos"] = subs
		}
		writeJSON(w, http.StatusOK, resp)
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
		// false for a freshly-init'd repo (unborn HEAD): there's no
		// commit to base a worktree on, so the picker hides the base
		// dropdown and "+ create" cuts an orphan branch instead.
		"has_commits": worktree.HasCommitsIn(root),
	})
}

// dashboardCreateWorktree answers POST /api/worktrees. Body:
// {repo, branch, from_branch?, path?}. Creates the worktree (a new
// branch off from_branch, or a checkout of an existing branch) and
// returns its absolute path so the caller can spawn into it.
func dashboardCreateWorktree(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo        string `json:"repo"`
		Branch      string `json:"branch"`
		FromBranch  string `json:"from_branch"`
		Path        string `json:"path"`
		FetchLatest bool   `json:"fetch_latest"`
		ProgressID  string `json:"progress_id"`
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
	root, err := worktree.RepoRootForPath(repo)
	if err != nil {
		writeError(w, http.StatusBadRequest, "worktree", err.Error())
		return
	}
	base := strings.TrimSpace(body.FromBranch)
	if body.FetchLatest && worktree.HasCommitsIn(root) && !worktree.BranchExistsIn(root, body.Branch) {
		if base == "" {
			base, err = worktree.DefaultBranchIn(root)
			if err != nil {
				writeError(w, http.StatusBadRequest, "worktree_fetch", "determine worktree base: "+err.Error())
				return
			}
		}
		base, err = fetchLatestWorktreeBase(r.Context(), root, base)
		if err != nil {
			writeError(w, http.StatusBadGateway, "worktree_fetch", "fetch latest worktree base: "+err.Error())
			return
		}
	}
	progressID := strings.TrimSpace(body.ProgressID)
	if progressID != "" && !validWorktreeProgressID(progressID) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid worktree progress id")
		return
	}
	defer clearDashboardWorktreeProgress(progressID)
	path, retries, fallback, err := createDashboardWorktree(r.Context(), root, body.Branch, base,
		expandTilde(strings.TrimSpace(body.Path)), waitOneSecond, func(attempt int) {
			setDashboardWorktreeProgress(progressID, attempt)
		})
	if err != nil {
		writeError(w, http.StatusBadRequest, "worktree", err.Error())
		return
	}
	if fallback {
		postWorktreeTrackingFallbackNotice(root, strings.TrimSpace(body.Branch), base)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":              path,
		"branch":            strings.TrimSpace(body.Branch),
		"tracking_retries":  retries,
		"tracking_fallback": fallback,
	})
}

func dashboardWorktreeProgressAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if !validWorktreeProgressID(id) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid worktree progress id")
		return
	}
	dashboardWorktreeProgressMu.Lock()
	progress, ok := dashboardWorktreeProgresses[id]
	dashboardWorktreeProgressMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"retrying": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"retrying": true,
		"attempt":  progress.Attempt,
		"max":      progress.Max,
	})
}

func validWorktreeProgressID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func setDashboardWorktreeProgress(id string, attempt int) {
	if id == "" {
		return
	}
	dashboardWorktreeProgressMu.Lock()
	dashboardWorktreeProgresses[id] = dashboardWorktreeProgress{
		Attempt: attempt,
		Max:     dashboardWorktreeTrackingRetries,
	}
	dashboardWorktreeProgressMu.Unlock()
}

func clearDashboardWorktreeProgress(id string) {
	if id == "" {
		return
	}
	dashboardWorktreeProgressMu.Lock()
	delete(dashboardWorktreeProgresses, id)
	dashboardWorktreeProgressMu.Unlock()
}

func waitOneSecond(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// createDashboardWorktree preserves Git's ordinary automatic tracking setup,
// but treats its shared-config lock as transient. `git worktree add -b` creates
// the branch before attempting that config write, so retries complete the
// missing upstream directly and then check out the already-created branch.
// After ten one-second waits, the checkout still completes without tracking;
// a config lock must not make agent spawning unavailable indefinitely.
func createDashboardWorktree(
	ctx context.Context,
	repoRoot, branch, base, path string,
	wait func(context.Context) error,
	onRetry func(attempt int),
) (createdPath string, retries int, fallback bool, err error) {
	createdPath, err = worktree.AddWorktreeIn(repoRoot, branch, base, path)
	if err == nil || !worktree.IsUpstreamConfigLockError(err) {
		return createdPath, 0, false, err
	}

	for retries = 1; retries <= dashboardWorktreeTrackingRetries; retries++ {
		if onRetry != nil {
			onRetry(retries)
		}
		if waitErr := wait(ctx); waitErr != nil {
			return "", retries - 1, false, waitErr
		}
		if worktree.BranchExistsIn(repoRoot, branch) {
			err = worktree.SetBranchUpstreamIn(repoRoot, branch, base)
			if err == nil {
				createdPath, err = worktree.AddWorktreeIn(repoRoot, branch, base, path)
				return createdPath, retries, false, err
			}
		} else {
			createdPath, err = worktree.AddWorktreeIn(repoRoot, branch, base, path)
			if err == nil {
				return createdPath, retries, false, nil
			}
		}
		if !worktree.IsConfigLockError(err) {
			return "", retries, false, err
		}
	}

	// Git normally leaves the branch behind on the upstream-config failure. If
	// this version did not, explicitly suppress automatic tracking on the final
	// attempt so the still-locked config is not touched again.
	if worktree.BranchExistsIn(repoRoot, branch) {
		createdPath, err = worktree.AddWorktreeIn(repoRoot, branch, base, path)
	} else {
		createdPath, err = worktree.AddWorktreeInWithoutTracking(repoRoot, branch, base, path)
	}
	return createdPath, dashboardWorktreeTrackingRetries, err == nil, err
}

func postWorktreeTrackingFallbackNotice(repoRoot, branch, upstream string) {
	subject := "Worktree branch created without upstream tracking"
	body := fmt.Sprintf(
		"Git's repository config stayed locked for 10 seconds while creating branch %q in %s. "+
			"The worktree was created so the agent could still spawn, but the branch does not have complete upstream tracking. "+
			"After the lock clears, run: git -C %s branch --set-upstream-to=%s %s",
		branch, repoRoot, clcommon.ShellQuoteArg(repoRoot), clcommon.ShellQuoteArg(upstream),
		clcommon.ShellQuoteArg(branch))
	if _, err := db.InsertHumanMessage(&db.HumanMessage{
		FromTitle: "worktree spawn",
		Subject:   subject,
		Body:      body,
	}); err != nil {
		slog.Warn("worktree spawn: failed to post tracking fallback notice", "error", err)
		return
	}
	dispatchHumanMessageNotification("", "worktree spawn", "", subject, body)
}
