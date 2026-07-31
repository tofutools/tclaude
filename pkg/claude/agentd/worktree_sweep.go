package agentd

import (
	"encoding/json"
	"fmt"
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
// Three dashboard-only endpoints (cookie + Origin pin, human-only — agents
// have no path here):
//
//	GET  /api/groups/{name}/worktrees   — discover candidate worktrees,
//	                                       classified + smart-ticked.
//	GET  /api/worktrees/cleanup         — discover candidates across every
//	                                       group's repos.
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
	listWorktreesInFn   = worktree.ListWorktreesIn
	repoRootForPathFn   = worktree.RepoRootForPath
	worktreeDirtyFn     = worktree.IsDirtyIn
	prunableWorktreesFn = worktree.PrunableWorktreesIn
	pruneWorktreesFn    = worktree.PruneWorktreesIn
)

// sweepAgent is an agent bound to a worktree — either its immutable startup
// directory or its tracked current directory roots there. A worktree with any
// bound agent is never an "orphan": removing it would break that
// conversation's cwd-scoped resume (a live agent loses its launch directory
// outright; an offline one can no longer be resumed).
type sweepAgent struct {
	// AgentID is the bound agent's stable actor key — the canonical ID the
	// dashboard/CLI leads with; ConvID is the live generation behind it
	// (kept as the snapshot/hover). "" when the conv is not a known agent.
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Online  bool   `json:"online"`
	Retired bool   `json:"retired"` // enrollment retired_at set — a demoted, cleanup-bound conv
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

// sweepPrunableRepo is one bookkeeping-only cleanup choice. Git's dry-run
// often cannot recover a useful checkout path or branch for these broken
// administrative entries, so the UI presents one aggregate row per repo and
// keeps the reason counts available behind a disclosure.
type sweepPrunableRepo struct {
	RepoRoot string             `json:"repo_root"`
	Count    int                `json:"count"`
	Reasons  []sweepPruneReason `json:"reasons"`
	Checked  bool               `json:"checked"`
}

type sweepPruneReason struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// registeredSweepWorktree is Git's authoritative record for a worktree plus
// the surviving main checkout used to mutate it. It remains usable when Path
// itself no longer exists.
type registeredSweepWorktree struct {
	Info     worktree.WorktreeInfo
	RepoRoot string
}

// categoryRank orders the list so the safe-to-remove rows (orphans, then
// retired-agent leftovers) float to the top and the never-removed main
// repo sinks to the bottom.
func categoryRank(cat string) int {
	switch cat {
	case "orphan":
		return 0
	case "retired":
		return 1
	case "agent":
		return 2
	case "live":
		return 3
	default: // main
		return 4
	}
}

// groupWorktreeDirs returns the group's default start dir plus every
// member's recorded history dir (where it has been editing). Discovery
// resolves these to repos and deduplicates them, so callers may freely
// concatenate this result across groups.
func groupWorktreeDirs(g *db.AgentGroup) []string {
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
	return dirs
}

// dashboardGroupWorktrees answers GET /api/groups/{name}/worktrees: the
// candidate set for the worktree-cleanup modal. Pure read — it lists and
// classifies, never removes. Always 200 on a reachable daemon; an empty
// `worktrees` (no default_cwd, no git repo) is a normal result the modal
// renders as "nothing to clean up".
func dashboardGroupWorktrees(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	roots, out := discoverSweepWorktrees(groupWorktreeDirs(g))
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":          "group",
		"group":          g.Name,
		"repo_roots":     roots,
		"worktrees":      out,
		"prunable_repos": discoverPrunableRepos(roots),
	})
}

// dashboardAllGroupWorktrees answers GET /api/worktrees/cleanup. It combines
// every group's discovery dirs before scanning, so groups that share a repo do
// not duplicate rows or git calls. Ungrouped agents are intentionally outside
// this scope: this is the all-GROUPS counterpart of the per-group command.
func dashboardAllGroupWorktrees(w http.ResponseWriter) {
	names, dirs, err := allGroupWorktreeDirs()
	if err != nil {
		http.Error(w, "list groups: "+err.Error(), http.StatusInternalServerError)
		return
	}
	roots, out := discoverSweepWorktrees(dirs)
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":          "all",
		"groups":         names,
		"repo_roots":     roots,
		"worktrees":      out,
		"prunable_repos": discoverPrunableRepos(roots),
	})
}

// discoverPrunableRepos runs Git's authoritative prune dry-run for every
// discovered main repo. Only affected repos become rows. Counts are a live
// preview, not inventory: execute-time cleanup takes another before snapshot.
func discoverPrunableRepos(roots []string) []sweepPrunableRepo {
	out := make([]sweepPrunableRepo, 0, len(roots))
	for _, root := range roots {
		entries, err := prunableWorktreesFn(root)
		if err != nil || len(entries) == 0 {
			continue
		}
		counts := map[string]int{}
		for _, entry := range entries {
			counts[entry.Reason]++
		}
		reasons := make([]sweepPruneReason, 0, len(counts))
		for reason, count := range counts {
			reasons = append(reasons, sweepPruneReason{Reason: reason, Count: count})
		}
		sort.Slice(reasons, func(i, j int) bool {
			if reasons[i].Count != reasons[j].Count {
				return reasons[i].Count > reasons[j].Count
			}
			return reasons[i].Reason < reasons[j].Reason
		})
		out = append(out, sweepPrunableRepo{
			RepoRoot: root,
			Count:    len(entries),
			Reasons:  reasons,
			Checked:  true,
		})
	}
	return out
}

func allGroupWorktreeDirs() (names, dirs []string, err error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, nil, err
	}
	names = make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
		dirs = append(dirs, groupWorktreeDirs(g)...)
	}
	return names, dirs, nil
}

// registeredWorktreesForCleanup scans every repository for which tclaude has
// a surviving anchor: group directories plus recorded session locations. This
// lets per-agent delete/retire dialogs recognize a Git registration even when
// that agent's own worktree directory is gone.
func registeredWorktreesForCleanup() map[string]registeredSweepWorktree {
	_, dirs, _ := allGroupWorktreeDirs()
	sessions, _ := db.ListSessions()
	seenConv := map[string]bool{}
	for _, sess := range sessions {
		if sess.Cwd != "" {
			dirs = append(dirs, sess.Cwd)
		}
		if physical, err := recordedStartupDir(sess); err == nil && physical != "" {
			dirs = append(dirs, physical)
		}
		if sess.ConvID == "" || seenConv[sess.ConvID] {
			continue
		}
		seenConv[sess.ConvID] = true
		loc := agent.ResolveLocation(sess.ConvID)
		dirs = append(dirs, loc.StartupDir, loc.CurrentDir)
	}
	_, registered := scanSweepWorktrees(dirs)
	return registered
}

// discoverSweepWorktrees resolves candidate directories to repos, lists each
// distinct repo once, and classifies its linked worktrees. The returned roots
// are the repos actually scanned (main worktree paths), not every member
// worktree that happened to serve as a discovery anchor.
func discoverSweepWorktrees(dirs []string) ([]string, []sweepWorktree) {
	roots, registered := scanSweepWorktrees(dirs)
	wtByPath := make(map[string]worktree.WorktreeInfo, len(registered))
	repoByPath := make(map[string]string, len(registered))
	for path, reg := range registered {
		wtByPath[path] = reg.Info
		repoByPath[path] = reg.RepoRoot
	}

	// Map worktree roots → the agents working there, across ALL
	// sessions (an agent in another group still pins its worktree).
	rootConvs := worktreeRootConvs(registered)

	// Classify each worktree.
	out := make([]sweepWorktree, 0, len(wtByPath))
	for path, wt := range wtByPath {
		row := sweepWorktree{
			Path:     path,
			Name:     filepath.Base(path),
			Branch:   wt.Branch,
			RepoRoot: repoByPath[path],
			IsMain:   wt.IsMain,
		}
		// Resolve the bound agents (title + liveness + retired state) for
		// this worktree.
		var anyOnline bool
		agentIndex := map[string]int{}
		for _, cid := range rootConvs[path] {
			bound := resolveSweepAgent(cid)
			key := bound.AgentID
			if key == "" {
				key = "conv:" + bound.ConvID
			}
			if i, seen := agentIndex[key]; seen {
				// Several conversation generations of one stable actor can
				// claim the same root. Preserve liveness from ANY generation,
				// even though the wire renders the actor only once.
				row.Agents[i].Online = row.Agents[i].Online || bound.Online
				anyOnline = anyOnline || bound.Online
				continue
			}
			agentIndex[key] = len(row.Agents)
			anyOnline = anyOnline || bound.Online
			row.Agents = append(row.Agents, bound)
		}
		switch {
		case wt.IsMain:
			row.Category, row.Checked, row.Reason = "main", false, "main repo — never removed"
		case anyOnline:
			row.Category, row.Checked, row.Reason = "live", false,
				"in use by a running agent ("+agentNames(row.Agents)+")"
		case len(row.Agents) > 0 && allRetiredAgents(row.Agents):
			row.Dirty = worktreeDirtyFn(path)
			if row.Dirty {
				row.Category, row.Checked, row.Reason = "retired", false,
					"retired agent "+agentNames(row.Agents)+" with uncommitted changes — review before deleting"
			} else {
				row.Category, row.Checked, row.Reason = "retired", true,
					"retired agent "+agentNames(row.Agents)+" — safe to remove (reinstate-resume loses this dir)"
			}
		case len(row.Agents) > 0:
			row.Category, row.Checked, row.Reason = "agent", false,
				"belongs to agent "+agentNames(row.Agents)+" — deleting breaks its resume"
		default:
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
	return roots, out
}

// scanSweepWorktrees resolves candidate directories to repositories and reads
// Git's registered worktree set. The registration is authoritative even when
// a linked worktree's directory no longer exists.
func scanSweepWorktrees(dirs []string) ([]string, map[string]registeredSweepWorktree) {
	// 1. Resolve each candidate dir to its git worktree root, deduped.
	repoRoots := map[string]bool{}
	for _, d := range dirs {
		if root, err := repoRootForPathFn(d); err == nil && root != "" {
			repoRoots[root] = true
		}
	}

	// 2. List every worktree of each repo, deduped by path. `git worktree
	//    list` is repo-global, so once a repo is listed (its sibling
	//    worktree paths land in wtByPath) a later candidate root that is
	//    one of those paths is skipped — N agents in N worktrees of the
	//    same repo cost one scan, not N.
	registered := map[string]registeredSweepWorktree{}
	scannedRepos := map[string]bool{}
	for root := range repoRoots {
		if _, done := registered[root]; done {
			continue
		}
		wts, err := listWorktreesInFn(root)
		if err != nil {
			continue
		}
		mainRoot := root
		for _, wt := range wts {
			if wt.IsMain {
				mainRoot = wt.Path
				break
			}
		}
		scannedRepos[mainRoot] = true
		for _, wt := range wts {
			registered[wt.Path] = registeredSweepWorktree{
				Info: wt, RepoRoot: mainRoot,
			}
		}
	}

	roots := make([]string, 0, len(scannedRepos))
	for r := range scannedRepos {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	return roots, registered
}

// registeredWorktreeForDir resolves dir against Git's registered worktree
// roots without requiring dir to exist. Stored agent locations can name a
// subdirectory of the registered root, so exact string equality is
// insufficient once filesystem/Git inspection is unavailable.
func registeredWorktreeForDir(
	registered map[string]registeredSweepWorktree,
	dir string,
) (registeredSweepWorktree, bool) {
	dir = cleanClaimDir(dir)
	var best registeredSweepWorktree
	bestLen := -1
	for _, reg := range registered {
		root := cleanClaimDir(reg.Info.Path)
		if !dirContains(root, dir) || len(root) <= bestLen {
			continue
		}
		best, bestLen = reg, len(root)
	}
	return best, bestLen >= 0
}

// worktreeRootConvs maps each git worktree root to the conv-ids of agents
// bound there, across ALL sessions. Both the immutable startup directory and
// the tracked current directory are claims: an edit in another repo must not
// make cleanup forget the worktree the agent was launched in. The dir→root
// resolution is cached so a host with many agents in one worktree pays one git
// inspection per distinct dir. Liveness/title are resolved later, only for the
// worktrees that survive into the candidate set, to keep the per-session cost
// cheap here.
func worktreeRootConvs(registered map[string]registeredSweepWorktree) map[string][]string {
	rootConvs := map[string][]string{}
	sessions, err := db.ListSessions()
	if err != nil {
		return rootConvs
	}
	seenConv := map[string]bool{}
	seenRootConv := map[string]map[string]bool{}
	dirRoot := map[string]string{}
	addClaim := func(convID, dir string) {
		if dir == "" {
			return
		}
		root, cached := dirRoot[dir]
		if !cached {
			root = inspectWorktreeFn(dir).Root
			if root == "" {
				if reg, ok := registeredWorktreeForDir(registered, dir); ok {
					root = reg.Info.Path
				}
			}
			dirRoot[dir] = root
		}
		if root == "" {
			return
		}
		if seenRootConv[root] == nil {
			seenRootConv[root] = map[string]bool{}
		}
		if !seenRootConv[root][convID] {
			rootConvs[root] = append(rootConvs[root], convID)
			seenRootConv[root][convID] = true
		}
	}
	for _, s := range sessions {
		if s.ConvID == "" {
			continue
		}
		// Every launch row is a claim. Multiple live sessions can share one
		// conv-id while having different startup CWDs, so conv-level
		// deduplication must happen only after their immutable dirs are read.
		addClaim(s.ConvID, s.Cwd)
		if physical, err := recordedStartupDir(s); err == nil {
			addClaim(s.ConvID, physical)
		}
		if seenConv[s.ConvID] {
			continue
		}
		seenConv[s.ConvID] = true
		loc := agent.ResolveLocation(s.ConvID)
		for _, dir := range []string{loc.StartupDir, loc.CurrentDir} {
			addClaim(s.ConvID, dir)
		}
	}
	return rootConvs
}

// liveAgentWorktreeRoots is the execute-time safety set: git worktree
// roots claimed by an ONLINE agent. The immutable startup root remains claimed
// even when PostToolUse has tracked an edit elsewhere. A worktree in this set
// is never removed by the sweep — yanking the launch directory out from under
// a running process is exactly what the cleanup must not do. Liveness is
// checked first (cheap) so an offline-heavy roster skips the location resolve.
func liveAgentWorktreeRoots() map[string]bool {
	roots := map[string]bool{}
	sessions, err := db.ListSessions()
	if err != nil {
		return roots
	}
	seenLocation := map[string]bool{}
	dirRoot := map[string]string{}
	online := map[string]bool{}
	isOnline := func(convID string) bool {
		if convID == "" {
			return false
		}
		if v, ok := online[convID]; ok {
			return v
		}
		v := isConvOnline(convID)
		online[convID] = v
		return v
	}
	addRoot := func(dir string) {
		if dir == "" {
			return
		}
		// Keep the lexical claim as well as the inspected root. When the
		// directory is already missing, Git inspection cannot resolve it,
		// but an online pane recorded at that exact worktree path must still
		// block registration/branch cleanup.
		if claim := cleanClaimDir(dir); claim != "" {
			roots[claim] = true
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
	for _, s := range sessions {
		if s.ConvID == "" {
			continue
		}
		// A startup worktree belongs to the stable actor, not only to the
		// conversation generation that happened to launch there. If that
		// actor has rotated, liveness is the UNION of the recorded generation
		// and its current head: a predecessor pane can outlive a best-effort
		// stop, while a live successor keeps the actor's historical launch
		// roots claimed.
		liveConv := s.ConvID
		if a, err := db.GetAgentByConv(s.ConvID); err == nil && a != nil && a.CurrentConvID != "" {
			liveConv = a.CurrentConvID
		}
		if !isOnline(s.ConvID) && !isOnline(liveConv) {
			continue
		}
		// Read every session row's launch cwd before deduplicating the shared
		// conversation-level Location. Resume provenance supplies the physical
		// startup root when the lexical launch path was a symlink.
		addRoot(s.Cwd)
		if physical, err := recordedStartupDir(s); err == nil {
			addRoot(physical)
		}
		if !seenLocation[s.ConvID] {
			seenLocation[s.ConvID] = true
			loc := agent.ResolveLocation(s.ConvID)
			addRoot(loc.StartupDir)
			addRoot(loc.CurrentDir)
		}
	}
	return roots
}

// worktreeClaimedByAny reports whether any live root/directory claim falls
// inside path. Claims include both successfully inspected worktree roots and
// lexical startup/current dirs, so a missing worktree remains protected when
// the recorded CWD names one of its subdirectories.
func worktreeClaimedByAny(path string, claims map[string]bool) bool {
	for claim := range claims {
		if dirContains(path, claim) {
			return true
		}
	}
	return false
}

// resolveSweepAgent resolves a conversation-generation claim through the
// stable actor identity. The startup worktree belongs to that actor until the
// actor itself is retired; rotating current_conv_id does not make the previous
// generation's launch directory a cleanup candidate. The wire row leads with
// agent_id and the actor's current generation so duplicate historical session
// rows collapse into one claimant. Liveness is the union of the bound
// generation and the live head: a predecessor pane can survive a rotation.
//
// A plain, non-agent conversation remains conv-keyed and protected as before.
// Read failures fail safe: the claim is treated as a non-retired conversation.
func resolveSweepAgent(convID string) sweepAgent {
	a, err := db.GetAgentByConv(convID)
	if err == nil && a != nil {
		current := a.CurrentConvID
		if current == "" {
			current = convID
		}
		return sweepAgent{
			AgentID: a.AgentID,
			ConvID:  current,
			Title:   agent.FreshTitle(current),
			Online:  isConvOnline(convID) || (current != convID && isConvOnline(current)),
			Retired: !a.Active(),
		}
	}
	return sweepAgent{
		ConvID: convID,
		Title:  agent.FreshTitle(convID),
		Online: isConvOnline(convID),
	}
}

// allRetiredAgents reports whether every bound agent is retired (and there
// is at least one). A single still-active bound agent keeps the worktree
// out of the "retired" cleanup bucket — its resume must stay protected.
func allRetiredAgents(agents []sweepAgent) bool {
	if len(agents) == 0 {
		return false
	}
	for _, a := range agents {
		if !a.Retired {
			return false
		}
	}
	return true
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

// worktreePruneOutcome reports verified bookkeeping cleanup for one repo.
// Result is pruned, partial, failed, or skipped. Remaining is authoritative:
// Git can exit 0 after failing every deletion, so exit status alone never
// produces a successful outcome.
type worktreePruneOutcome struct {
	RepoRoot  string `json:"repo_root"`
	Before    int    `json:"before"`
	Cleared   int    `json:"cleared"`
	Remaining int    `json:"remaining"`
	Result    string `json:"result"`
	Detail    string `json:"detail,omitempty"`
}

// worktreeCleanupResponse is the wire shape returned by POST
// /api/worktrees/cleanup. Outcomes is always non-nil so the dashboard
// can .map() over it unconditionally.
type worktreeCleanupResponse struct {
	Outcomes       []worktreeCleanupOutcome `json:"outcomes"`
	PruneOutcomes  []worktreePruneOutcome   `json:"prune_outcomes"`
	Removed        int                      `json:"removed"`
	Branches       int                      `json:"branches"`
	Skipped        int                      `json:"skipped"`
	Failed         int                      `json:"failed"`
	Pruned         int                      `json:"pruned"`
	PruneRemaining int                      `json:"prune_remaining"`
	PruneFailed    int                      `json:"prune_failed"`
	PruneSkipped   int                      `json:"prune_skipped"`
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
	if r.Method == http.MethodGet {
		dashboardAllGroupWorktrees(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Paths          []string `json:"paths"`
		PruneRoots     []string `json:"prune_roots"`
		DeleteBranches bool     `json:"delete_branches"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Re-read Git's registered worktree set at the destructive boundary.
	// This is the authoritative fallback when a selected worktree directory
	// has disappeared since discovery: inspecting that path necessarily says
	// "none", while the surviving main checkout can still remove its stale
	// registration (and branch, when requested).
	_, dirs, err := allGroupWorktreeDirs()
	if err != nil {
		http.Error(w, "list groups: "+err.Error(), http.StatusInternalServerError)
		return
	}
	roots, registered := scanSweepWorktrees(dirs)
	liveRoots := liveAgentWorktreeRoots()
	resp := worktreeCleanupResponse{
		Outcomes:      []worktreeCleanupOutcome{},
		PruneOutcomes: []worktreePruneOutcome{},
	}
	seen := map[string]bool{}
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
			reg, ok := registered[path]
			switch {
			case !ok:
				out.Result, out.Detail = "skipped", "already removed"
				resp.Skipped++
			case reg.Info.IsMain:
				out.Branch = reg.Info.Branch
				out.Result, out.Detail = "skipped", "main repo — never removed"
				resp.Skipped++
			case worktreeClaimedByAny(path, liveRoots):
				out.Branch = reg.Info.Branch
				out.Result, out.Detail = "skipped", "in use by a running agent — stop it first"
				resp.Skipped++
			default:
				out = removeOneRegisteredWorktree(
					reg, path, body.DeleteBranches,
				)
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
		case st.Kind == "main":
			out.Result, out.Detail = "skipped", "main repo — never removed"
			resp.Skipped++
		case worktreeClaimedByAny(st.Root, liveRoots):
			// Re-check against live state: an agent may have started here
			// between discovery and submit. Never yank a running agent's cwd.
			out.Result, out.Detail = "skipped", "in use by a running agent — stop it first"
			resp.Skipped++
		default:
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

	// Stale-record cleanup is an independent, explicit action. Restrict roots
	// to the group-derived repo set re-read above, then verify the live state
	// before and after prune. In particular, do not trust prune's exit code:
	// Git returns 0 when sandbox bind mounts make every deletion fail.
	allowedRoots := make(map[string]string, len(roots))
	for _, root := range roots {
		allowedRoots[cleanClaimDir(root)] = root
	}
	seenRoots := map[string]bool{}
	for _, raw := range body.PruneRoots {
		requested := cleanClaimDir(raw)
		if requested == "" || seenRoots[requested] {
			continue
		}
		seenRoots[requested] = true
		root, allowed := allowedRoots[requested]
		if !allowed {
			resp.PruneOutcomes = append(resp.PruneOutcomes, worktreePruneOutcome{
				RepoRoot: strings.TrimSpace(raw),
				Result:   "skipped",
				Detail:   "repo is not in the current group cleanup scope",
			})
			resp.PruneSkipped++
			continue
		}
		out := pruneOneRepo(root)
		resp.PruneOutcomes = append(resp.PruneOutcomes, out)
		resp.Pruned += out.Cleared
		resp.PruneRemaining += out.Remaining
		switch out.Result {
		case "partial", "failed":
			resp.PruneFailed++
		case "skipped":
			resp.PruneSkipped++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func pruneOneRepo(root string) worktreePruneOutcome {
	out := worktreePruneOutcome{RepoRoot: root}
	before, err := prunableWorktreesFn(root)
	if err != nil {
		out.Result = "failed"
		out.Detail = "could not verify stale Git records before prune: " + err.Error()
		return out
	}
	out.Before = len(before)
	if out.Before == 0 {
		out.Result = "skipped"
		out.Detail = "already pruned — the live pre-check found no stale Git records"
		return out
	}

	stderr, pruneErr := pruneWorktreesFn(root)
	after, afterErr := prunableWorktreesFn(root)
	if afterErr != nil {
		out.Result = "failed"
		out.Detail = "prune ran, but post-state verification failed: " + afterErr.Error()
		if pruneErr != nil {
			out.Detail += "; prune command: " + pruneErr.Error()
		}
		if stderr != "" {
			out.Detail += "; Git reported: " + strings.Join(strings.Fields(stderr), " ")
		}
		return out
	}
	out.Remaining = len(after)
	out.Cleared = out.Before - out.Remaining
	if out.Cleared < 0 {
		out.Cleared = 0
	}
	if out.Remaining > 0 {
		out.Result = "failed"
		if out.Cleared > 0 {
			out.Result = "partial"
		}
		out.Detail = fmt.Sprintf(
			"%d of %d stale Git records cleared; %d remain. Git could not remove every administrative entry. An active agent sandbox may be holding bind mounts on .git/worktrees entries; stop the affected sandboxed agents and retry.",
			out.Cleared, out.Before, out.Remaining,
		)
	} else {
		out.Result = "pruned"
		out.Detail = fmt.Sprintf("%d stale Git records cleared (bookkeeping only; no checkout or branch removed)", out.Cleared)
	}
	if pruneErr != nil {
		out.Detail += " Prune command reported: " + pruneErr.Error() + "."
	}
	if stderr != "" {
		out.Detail += " Git reported: " + strings.Join(strings.Fields(stderr), " ")
	}
	return out
}

// removeOneRegisteredWorktree is the missing-directory sibling of
// removeOneWorktree. Git's registration, read through a surviving main
// checkout, supplies both the linked-worktree identity and its branch.
func removeOneRegisteredWorktree(
	reg registeredSweepWorktree,
	path string,
	deleteBranch bool,
) worktreeCleanupOutcome {
	removed, branchDeleted, branch, err := removeRegisteredWorktreeFn(
		reg.RepoRoot, path, deleteBranch, true,
	)
	out := worktreeCleanupOutcome{Path: path, Branch: branch}
	switch {
	case err != nil:
		out.Result, out.Detail = "failed", err.Error()
	case removed && branchDeleted:
		out.Result, out.Detail = "removed_with_branch",
			"stale worktree + branch "+branch+" removed"
	case removed && deleteBranch && branch != "" && !isProtectedBranchName(branch):
		out.Result, out.Detail = "removed",
			"stale worktree removed (branch "+branch+" kept — already gone or protected)"
	case removed:
		out.Result, out.Detail = "removed", "stale worktree registration removed"
	default:
		out.Result, out.Detail = "skipped", "already removed"
	}
	return out
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
