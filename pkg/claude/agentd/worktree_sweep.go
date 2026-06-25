package agentd

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// worktree_sweep.go is the REPO-WIDE worktree janitor — distinct from
// worktree_cleanup.go, which removes the single worktree a deleted /
// retired agent was working in. Where that one is per-agent, this one
// answers "tidy up all the stale worktrees in the repo(s) this group
// works in": the leftovers from retired/deleted agents and hand-made
// feature branches that accumulate over a long-running project.
//
// Two dashboard-only endpoints (cookie + Origin pin, human-only — agents
// have no path here):
//
//	GET  /api/groups/{name}/worktrees   — discover candidate worktrees,
//	                                       classified + smart-ticked.
//	POST /api/worktrees/cleanup         — remove a human-picked, explicit
//	                                       list of worktree paths.
//
// Discovery scope. The group's default_cwd and every member's recorded
// history dir are resolved to git repo roots; ALL linked worktrees of
// those repos are listed (`git worktree list` is repo-global, so one
// scan per distinct repo covers every sibling worktree). The group is
// just how the human picks which repo(s) to sweep.
//
// Safety model — the same explicit-list discipline the retire-preview
// modal uses. Discovery never deletes; it returns the candidate set with
// a smart-default `checked` flag (orphans on, risky ones off). The human
// edits that selection and the browser POSTs the EXACT ticked path list;
// the daemon re-validates every path at execute time (never the "orphan"
// label the snapshot rendered) and refuses the main repo and any worktree
// a still-LIVE agent occupies. Removal is force (the human confirmed),
// but the dirty/agent badges + un-ticked defaults keep the destructive
// cases off by default.

// The repo-scan git seam — production shells out via the worktree
// package; flow tests swap fakes (the package-level inspectWorktreeFn /
// removeWorktreeFn from worktree_cleanup.go cover the rest). All are
// vars so a test can route them at a simulated repo.
var (
	listWorktreesInFn = worktree.ListWorktreesIn
	repoRootForPathFn = worktree.RepoRootForPath
	worktreeDirtyFn   = worktree.IsDirtyIn
	mainRepoForPathFn = worktree.MainRepoForPath
	pruneWorktreesFn  = worktree.PruneWorktreesIn
)

// sweepAgent is an agent bound to a worktree — its resolved CurrentDir
// roots there. A worktree with any bound agent is never an "orphan":
// removing it would break that conversation's cwd-scoped resume (a live
// agent loses its cwd outright; an offline one can no longer be resumed).
type sweepAgent struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Online bool   `json:"online"`
}

// sweepWorktree is one candidate row in the discovery response.
type sweepWorktree struct {
	Path     string       `json:"path"`
	Name     string       `json:"name"`      // base dir name, for a terse label
	Branch   string       `json:"branch"`    // "" when detached
	RepoRoot string       `json:"repo_root"` // the repo this worktree belongs to
	IsMain   bool         `json:"is_main"`
	Dirty    bool         `json:"dirty"`    // uncommitted changes (removal would lose work)
	Agents   []sweepAgent `json:"agents"`   // agents bound here (any group)
	Category string       `json:"category"` // main | live | agent | orphan
	Checked  bool         `json:"checked"`  // smart-default tick
	Reason   string       `json:"reason"`   // why this default / what the row is
}

// categoryRank orders the list so the safe-to-remove orphans float to
// the top and the never-removed main repo sinks to the bottom.
func categoryRank(cat string) int {
	switch cat {
	case "orphan":
		return 0
	case "agent":
		return 1
	case "live":
		return 2
	default: // main
		return 3
	}
}

// dashboardGroupWorktrees answers GET /api/groups/{name}/worktrees: the
// candidate set for the worktree-cleanup modal. Pure read — it lists and
// classifies, never removes. Always 200 on a reachable daemon; an empty
// `worktrees` (no default_cwd, no git repo) is a normal result the modal
// renders as "nothing to clean up".
func dashboardGroupWorktrees(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	// 1. Candidate dirs: the group's default start dir plus every
	//    member's recorded history dir (where it has been editing).
	var dirs []string
	if d := strings.TrimSpace(g.DefaultCwd); d != "" {
		dirs = append(dirs, d)
	}
	members, _ := db.ListAgentGroupMembers(g.ID)
	for _, m := range members {
		if d := agent.ResolveLocation(m.ConvID).CurrentDir; d != "" {
			dirs = append(dirs, d)
		}
	}

	// 2. Resolve each candidate dir to its git repo root, deduped.
	repoRoots := map[string]bool{}
	for _, d := range dirs {
		if root, err := repoRootForPathFn(d); err == nil && root != "" {
			repoRoots[root] = true
		}
	}

	// 3. List every worktree of each repo, deduped by path. `git worktree
	//    list` is repo-global, so once a repo is listed (its sibling
	//    worktree paths land in wtByPath) a later candidate root that is
	//    one of those paths is skipped — N agents in N worktrees of the
	//    same repo cost one scan, not N.
	wtByPath := map[string]worktree.WorktreeInfo{}
	for root := range repoRoots {
		if _, done := wtByPath[root]; done {
			continue
		}
		wts, err := listWorktreesInFn(root)
		if err != nil {
			continue
		}
		for _, wt := range wts {
			wtByPath[wt.Path] = wt
		}
	}

	// 4. Map worktree roots → the agents working there, across ALL
	//    sessions (an agent in another group still pins its worktree).
	rootConvs := worktreeRootConvs()

	// 5. Classify each worktree.
	out := make([]sweepWorktree, 0, len(wtByPath))
	for path, wt := range wtByPath {
		row := sweepWorktree{
			Path:     path,
			Name:     filepath.Base(path),
			Branch:   wt.Branch,
			RepoRoot: repoRootOf(wt, repoRoots),
			IsMain:   wt.IsMain,
		}
		// Resolve the bound agents (title + liveness) for this worktree.
		var anyOnline bool
		for _, cid := range rootConvs[path] {
			online := isConvOnline(cid)
			anyOnline = anyOnline || online
			row.Agents = append(row.Agents, sweepAgent{ConvID: cid, Title: agent.FreshTitle(cid), Online: online})
		}
		switch {
		case wt.IsMain:
			row.Category, row.Checked, row.Reason = "main", false, "main repo — never removed"
		case anyOnline:
			row.Category, row.Checked, row.Reason = "live", false,
				"in use by a running agent ("+agentNames(row.Agents)+")"
		case len(row.Agents) > 0:
			row.Category, row.Checked, row.Reason = "agent", false,
				"belongs to agent "+agentNames(row.Agents)+" — deleting breaks its resume"
		default:
			// Orphan: no agent maps here. Dirtiness only matters for the
			// rows we'd otherwise tick — skip the git status call on main /
			// live / agent worktrees we won't remove anyway.
			row.Dirty = worktreeDirtyFn(path)
			if row.Dirty {
				row.Category, row.Checked, row.Reason = "orphan", false,
					"orphan with uncommitted changes — review before deleting"
			} else {
				row.Category, row.Checked, row.Reason = "orphan", true, "orphan — safe to remove"
			}
		}
		out = append(out, row)
	}

	// Orphans first, main last; stable tiebreak on path.
	sort.Slice(out, func(i, j int) bool {
		ri, rj := categoryRank(out[i].Category), categoryRank(out[j].Category)
		if ri != rj {
			return ri < rj
		}
		return out[i].Path < out[j].Path
	})

	roots := make([]string, 0, len(repoRoots))
	for r := range repoRoots {
		roots = append(roots, r)
	}
	sort.Strings(roots)

	writeJSON(w, http.StatusOK, map[string]any{
		"group":      g.Name,
		"repo_roots": roots,
		"worktrees":  out,
	})
}

// repoRootOf returns the repo a worktree belongs to for display. The
// main worktree IS its repo root; for a linked worktree we can't cheaply
// know the main from the porcelain row alone, so fall back to the first
// resolved repo root (the common case is a single repo). Best-effort
// labelling only — never used for a removal decision.
func repoRootOf(wt worktree.WorktreeInfo, repoRoots map[string]bool) string {
	if wt.IsMain {
		return wt.Path
	}
	for r := range repoRoots {
		return r
	}
	return ""
}

// worktreeRootConvs maps each git worktree root to the conv-ids of
// agents working there, across ALL sessions (every distinct conv once).
// The dir→root resolution is cached so a host with many agents in one
// worktree pays one git inspection per distinct dir. Liveness/title are
// resolved later, only for the worktrees that survive into the candidate
// set, to keep the per-session cost cheap here.
func worktreeRootConvs() map[string][]string {
	rootConvs := map[string][]string{}
	sessions, err := db.ListSessions()
	if err != nil {
		return rootConvs
	}
	seenConv := map[string]bool{}
	dirRoot := map[string]string{}
	for _, s := range sessions {
		if s.ConvID == "" || seenConv[s.ConvID] {
			continue
		}
		seenConv[s.ConvID] = true
		dir := agent.ResolveLocation(s.ConvID).CurrentDir
		if dir == "" {
			continue
		}
		root, cached := dirRoot[dir]
		if !cached {
			root = inspectWorktreeFn(dir).Root
			dirRoot[dir] = root
		}
		if root != "" {
			rootConvs[root] = append(rootConvs[root], s.ConvID)
		}
	}
	return rootConvs
}

// liveAgentWorktreeRoots is the execute-time safety set: git worktree
// roots in use by an ONLINE agent. A worktree in this set is never
// removed by the sweep — yanking the directory out from under a running
// process is exactly what the cleanup must not do. Liveness is checked
// first (cheap) so an offline-heavy roster skips the location resolve.
func liveAgentWorktreeRoots() map[string]bool {
	roots := map[string]bool{}
	sessions, err := db.ListSessions()
	if err != nil {
		return roots
	}
	seenConv := map[string]bool{}
	dirRoot := map[string]string{}
	for _, s := range sessions {
		if s.ConvID == "" || seenConv[s.ConvID] {
			continue
		}
		seenConv[s.ConvID] = true
		if !isConvOnline(s.ConvID) {
			continue
		}
		dir := agent.ResolveLocation(s.ConvID).CurrentDir
		if dir == "" {
			continue
		}
		root, cached := dirRoot[dir]
		if !cached {
			root = inspectWorktreeFn(dir).Root
			dirRoot[dir] = root
		}
		if root != "" {
			roots[root] = true
		}
	}
	return roots
}

// agentNames renders a short, comma-joined label of an agent set for a
// reason string. Falls back to a short conv-id when a title is unknown.
func agentNames(agents []sweepAgent) string {
	if len(agents) == 0 {
		return ""
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		switch {
		case a.Title != "" && a.Title != agent.UnknownTitle:
			names = append(names, a.Title)
		case len(a.ConvID) >= 8:
			names = append(names, a.ConvID[:8])
		default:
			names = append(names, a.ConvID)
		}
	}
	return strings.Join(names, ", ")
}

// worktreeCleanupOutcome is the per-path result of a sweep, rendered
// back into the modal so the human sees exactly what happened, skips and
// failures included.
type worktreeCleanupOutcome struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	Result string `json:"result"`           // removed | removed_with_branch | skipped | failed
	Detail string `json:"detail,omitempty"` // human-readable reason
}

// worktreeCleanupResponse is the wire shape returned by POST
// /api/worktrees/cleanup. Outcomes is always non-nil so the dashboard
// can .map() over it unconditionally.
type worktreeCleanupResponse struct {
	Outcomes []worktreeCleanupOutcome `json:"outcomes"`
	Removed  int                      `json:"removed"`
	Branches int                      `json:"branches"`
	Skipped  int                      `json:"skipped"`
	Failed   int                      `json:"failed"`
}

// handleDashboardWorktreeCleanup answers POST /api/worktrees/cleanup.
// Body:
//
//	{
//	  "paths":           ["/abs/worktree", ...], // the human-edited list
//	  "delete_branches": true                    // also force-delete each branch?
//	}
//
// Not group-scoped — the paths are absolute and self-identifying. Every
// path is re-validated against live git + session state (never the
// snapshot's stale label): the main repo and any worktree a still-LIVE
// agent occupies are skipped, not removed. Everything else the human
// ticked is force-removed; with delete_branches the local branch goes
// too (main/master always protected by the worktree package). Idempotent
// — a path whose worktree is already gone reports "already removed".
func handleDashboardWorktreeCleanup(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Paths          []string `json:"paths"`
		DeleteBranches bool     `json:"delete_branches"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	liveRoots := liveAgentWorktreeRoots()
	resp := worktreeCleanupResponse{Outcomes: []worktreeCleanupOutcome{}}
	seen := map[string]bool{}
	// Main repos of the worktrees we touch — pruned once at the end to
	// clear any DANGLING worktree links (entries whose dir was deleted
	// out-of-band). Resolved BEFORE removal, while the worktree dir still
	// exists to anchor the git call. `git worktree remove` already cleans
	// the link for the worktrees it removes; this mops up the rest.
	pruneRepos := map[string]bool{}
	for _, raw := range body.Paths {
		path := strings.TrimSpace(raw)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		st := inspectWorktreeFn(path)
		out := worktreeCleanupOutcome{Path: path, Branch: st.Branch}
		switch {
		case st.Kind == "none":
			out.Result, out.Detail = "skipped", "not a git worktree"
			resp.Skipped++
		case st.Kind == "main":
			out.Result, out.Detail = "skipped", "main repo — never removed"
			resp.Skipped++
		case liveRoots[path] || (st.Root != "" && liveRoots[st.Root]):
			// Re-check against live state: an agent may have started here
			// between discovery and submit. Never yank a running agent's cwd.
			out.Result, out.Detail = "skipped", "in use by a running agent — stop it first"
			resp.Skipped++
		default:
			if main := mainRepoForPathFn(path); main != "" {
				pruneRepos[main] = true
			}
			out = removeOneWorktree(path, st.Branch, body.DeleteBranches)
			switch out.Result {
			case "removed":
				resp.Removed++
			case "removed_with_branch":
				resp.Removed++
				resp.Branches++
			case "skipped":
				resp.Skipped++
			default:
				resp.Failed++
			}
		}
		resp.Outcomes = append(resp.Outcomes, out)
	}
	// Finishing tidy-up: prune dangling worktree registrations in every
	// repo we touched. Best-effort — a prune failure never affects the
	// per-path outcomes already reported.
	for repo := range pruneRepos {
		_ = pruneWorktreesFn(repo)
	}
	writeJSON(w, http.StatusOK, resp)
}

// removeOneWorktree force-removes one linked worktree and, when
// deleteBranch is set, force-deletes its branch too (main/master kept by
// the worktree package). Returns the outcome row; never errors — a git
// failure is reported in Result/Detail.
func removeOneWorktree(path, branch string, deleteBranch bool) worktreeCleanupOutcome {
	out := worktreeCleanupOutcome{Path: path, Branch: branch}
	if deleteBranch {
		removed, branchDeleted, err := removeWorktreeBranchFn(path, branch, true)
		switch {
		case err != nil:
			out.Result, out.Detail = "failed", err.Error()
		case removed && branchDeleted:
			out.Result, out.Detail = "removed_with_branch", "worktree + branch "+branch+" removed"
		case removed && branch != "" && !isProtectedBranchName(branch):
			out.Result, out.Detail = "removed", "worktree removed (branch "+branch+" kept — already gone or protected)"
		case removed:
			out.Result, out.Detail = "removed", "worktree removed"
		default:
			out.Result, out.Detail = "skipped", "already removed"
		}
		return out
	}
	removed, err := removeWorktreeFn(path, true)
	switch {
	case err != nil:
		out.Result, out.Detail = "failed", err.Error()
	case removed:
		out.Result, out.Detail = "removed", "worktree removed (branch kept)"
	default:
		out.Result, out.Detail = "skipped", "already removed"
	}
	return out
}
