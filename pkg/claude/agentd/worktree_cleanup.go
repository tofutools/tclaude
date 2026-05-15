package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// worktree_cleanup.go is the worktree-removal side of agent deletion.
// When the human deletes an agent — via the per-row delete button or
// the 🧹 cleanup modal — they may opt to also remove the git worktree
// that agent was working in. These helpers resolve that worktree and
// decide whether it's safe to remove.
//
// Safety rules:
//   - The repo's MAIN worktree is never removed (that's the human's
//     actual checkout).
//   - A worktree another, surviving agent is still working in is never
//     removed (it's "shared").
//   - A worktree whose directory is already gone is a silent no-op.
//
// inspectWorktreeFn / removeWorktreeFn are the git seam — production
// shells out via the worktree package; flow tests swap fakes with
// SetWorktreeFnsForTest so they need no real git repos.
var (
	inspectWorktreeFn = worktree.InspectWorktree
	removeWorktreeFn  = worktree.RemoveLinkedWorktree
)

// agentWorktreeView is the cleanup-oriented view of the git worktree
// an agent has been working in.
type agentWorktreeView struct {
	Path   string `json:"path"`             // worktree root; "" when none
	Branch string `json:"branch,omitempty"` // branch checked out there
	Kind   string `json:"kind"`             // "none" | "main" | "linked"
	Shared bool   `json:"shared"`           // a surviving agent also works here
}

// Removable reports whether cleanup may delete this worktree: it must
// be a linked worktree (never the main repo) that no surviving agent
// is working in.
func (v agentWorktreeView) Removable() bool {
	return v.Kind == "linked" && v.Path != "" && !v.Shared
}

// inspectAgentWorktree resolves the git worktree an agent has been
// working in. It reads the agent's stored Location (no git — the hook
// already resolved the worktree root at edit time) for the directory,
// then classifies that directory's worktree. Shared is left false;
// callers fill it via otherAgentWorktreeRoots with the right
// exclusion set.
func inspectAgentWorktree(convID string) agentWorktreeView {
	dir := agent.ResolveLocation(convID).CurrentDir
	if dir == "" {
		return agentWorktreeView{Kind: "none"}
	}
	st := inspectWorktreeFn(dir)
	return agentWorktreeView{Path: st.Root, Branch: st.Branch, Kind: st.Kind}
}

// otherAgentWorktreeRoots returns the set of git worktree roots in use
// by agents OTHER than those in `excluding`. A worktree in this set is
// "shared" — some agent that survives the current cleanup still
// depends on it, so cleanup must leave it alone.
//
// The directory→root resolution is cached so a host with many agents
// in the same worktree pays one git inspection per distinct dir, not
// one per agent.
func otherAgentWorktreeRoots(excluding map[string]bool) map[string]bool {
	roots := map[string]bool{}
	sessions, err := db.ListSessions()
	if err != nil {
		return roots
	}
	seenConv := map[string]bool{}
	dirRoot := map[string]string{}
	for _, s := range sessions {
		if s.ConvID == "" || excluding[s.ConvID] || seenConv[s.ConvID] {
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
			roots[root] = true
		}
	}
	return roots
}

// dashboardAgentWorktree answers GET /api/agents/{conv}/worktree —
// the delete-agent modal reads this to decide whether to show, and
// enable, its "delete worktree" checkbox. shared/removable are
// computed against every OTHER agent, so a worktree another agent
// still works in comes back removable=false.
func dashboardAgentWorktree(w http.ResponseWriter, convSelector string) {
	convID := convSelector
	if res, _, err := agent.ResolveSelector(convSelector); err == nil {
		convID = res.ConvID
	} else if !looksLikeConvID(convSelector) {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	wt := inspectAgentWorktree(convID)
	wt.Shared = wt.Path != "" &&
		otherAgentWorktreeRoots(map[string]bool{convID: true})[wt.Path]
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":      wt.Kind,
		"path":      wt.Path,
		"branch":    wt.Branch,
		"shared":    wt.Shared,
		"removable": wt.Removable(),
	})
}

// applyWorktreeCleanup removes wt's worktree when the human asked for
// it (requested) and it is safe to do so. It returns a short note for
// the deletion's outcome detail and never returns an error — a
// worktree-removal failure is reported in the note, never propagated,
// so it can't undo or block the agent deletion that already happened.
func applyWorktreeCleanup(wt agentWorktreeView, requested bool) string {
	if !requested {
		return ""
	}
	switch {
	case wt.Kind == "none":
		return "" // no worktree — nothing to report
	case wt.Kind == "main":
		return "worktree kept (main repo)"
	case wt.Shared:
		return "worktree kept (shared with another agent)"
	}
	removed, err := removeWorktreeFn(wt.Path, true)
	switch {
	case err != nil:
		return "worktree removal failed: " + err.Error()
	case removed:
		return "worktree removed"
	default:
		return "worktree already gone"
	}
}
