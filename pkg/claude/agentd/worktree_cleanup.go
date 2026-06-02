package agentd

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// retireWorktreeExitGrace bounds how long the deferred retire cleanup
// waits for a soft-stopped agent's pane to actually exit before it
// removes the worktree. CC's /exit can take a few seconds (it finishes
// the current turn, flushes, then closes); 60s is generous headroom.
// An agent that outlives the window keeps its worktree — reported, not
// force-removed out from under a still-live process. A var, not a const,
// so flow tests can shrink it to exercise the grace-timeout branch.
var retireWorktreeExitGrace = 60 * time.Second

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
	// removeWorktreeBranchFn is the retire-time variant: it removes the
	// worktree AND deletes its local branch (main/master always kept).
	// Delete keeps the branch (removeWorktreeFn); retire cleans up the
	// agent's whole git footprint.
	removeWorktreeBranchFn = worktree.RemoveLinkedWorktreeAndBranch
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

// applyRetireWorktreeCleanup is the retire-flow sibling of
// applyWorktreeCleanup: it removes wt's worktree AND force-deletes its
// local branch (main/master are always kept — worktree.go's
// protected-branch guard). Retiring an agent that owns a throwaway
// feature branch should leave no git footprint behind, where a plain
// delete keeps the branch.
//
// Same safety rules and never-errors contract as applyWorktreeCleanup:
// a removal failure is reported in the returned note, never propagated,
// so it can't undo the retire that already happened. The caller decides
// WHEN to run this — for a live agent it is deferred until the pane
// exits (see scheduleRetireWorktreeCleanup), since the agent's cwd is
// the worktree being removed.
//
// ok reports whether the cleanup succeeded (or had nothing to do): it is
// false ONLY when the git removal actually errored. The deferred path
// uses it to decide whether to bother the human — a successful delete is
// silent, a failure is surfaced.
func applyRetireWorktreeCleanup(wt agentWorktreeView, requested bool) (note string, ok bool) {
	if !requested {
		return "", true
	}
	switch {
	case wt.Kind == "none":
		return "", true // no worktree — nothing to report
	case wt.Kind == "main":
		return "worktree kept (main repo)", true
	case wt.Shared:
		return "worktree kept (shared with another agent)", true
	}
	removed, branchDeleted, err := removeWorktreeBranchFn(wt.Path, wt.Branch, true)
	switch {
	case err != nil:
		return "worktree removal failed: " + err.Error(), false
	case removed && branchDeleted:
		return "worktree + branch " + wt.Branch + " removed", true
	case removed && wt.Branch != "" && !isProtectedBranchName(wt.Branch):
		// Removed the dir but the branch survived (already gone, or git
		// refused) — say so rather than implying a clean sweep.
		return "worktree removed (branch " + wt.Branch + " kept)", true
	case removed:
		return "worktree removed", true
	default:
		return "worktree already gone", true
	}
}

// isProtectedBranchName mirrors worktree.isProtectedBranch for the
// note-phrasing above (that helper is unexported). main/master are
// never deleted, so a "branch kept" note for them would be misleading.
func isProtectedBranchName(branch string) bool {
	switch strings.ToLower(strings.TrimSpace(branch)) {
	case "main", "master":
		return true
	}
	return false
}

// retireWorktreePlan is the synchronous descriptor the retire handler
// returns for a requested worktree cleanup. Because the removal must
// wait until the agent's pane exits (its cwd is the worktree), the HTTP
// response often reports a *plan* ("scheduled"), not a finished result.
type retireWorktreePlan struct {
	// Action is one of: "none" (no worktree), "kept" (main/shared/still-
	// running), "removed" (done inline — agent was already offline), or
	// "scheduled" (removal deferred until the soft-stopped pane exits).
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// scheduleRetireWorktreeCleanup arranges the worktree+branch removal the
// retire ?delete_worktree option asked for, honouring the hard rule
// that it must run AFTER the agent's process exits — the agent's cwd is
// the very worktree being removed.
//
//   - No removable worktree (none / main / shared) → reported kept now.
//   - Agent already offline → removed inline, synchronously.
//   - Agent soft-stopped this retire (shutdownRequested) → deferred to a
//     background goroutine that waits up to retireWorktreeExitGrace for
//     the pane to die, then removes. An agent that never exits keeps its
//     worktree (logged).
//   - Agent still live and shutdown was declined → kept; we never yank a
//     worktree out from under a running agent.
//
// wt must be resolved by the caller BEFORE any shutdown is issued (the
// shared-worktree check reads sibling sessions, and the agent's recorded
// location must still be intact).
func scheduleRetireWorktreeCleanup(convID string, wt agentWorktreeView, shutdownRequested bool) retireWorktreePlan {
	switch {
	case wt.Kind == "none" || wt.Path == "":
		return retireWorktreePlan{Action: "none", Detail: "no worktree"}
	case wt.Kind == "main":
		return retireWorktreePlan{Action: "kept", Detail: "worktree kept (main repo)"}
	case wt.Shared:
		return retireWorktreePlan{Action: "kept", Detail: "worktree kept (shared with another agent)"}
	}

	// Already offline → safe to remove right now, inline. The outcome
	// (success or failure) rides back in the HTTP response and toast, so
	// the inline path needs no separate human-message notice.
	if pickAliveSession(convID) == nil {
		note, _ := applyRetireWorktreeCleanup(wt, true)
		return retireWorktreePlan{Action: "removed", Detail: note}
	}

	// Still alive but the human declined shutdown — leave the worktree
	// alone; removing it under a running agent is exactly what the
	// "after the agent exits" rule forbids.
	if !shutdownRequested {
		return retireWorktreePlan{Action: "kept",
			Detail: "worktree kept — session still running (retire without shutdown)"}
	}

	// Shutdown was requested: a /exit is in flight. Wait for the pane to
	// die, then remove. Background so the HTTP response returns now. The
	// HTTP response (and toast) already fired with the optimistic
	// "will be removed after the agent exits", so the human is told again
	// ONLY when that promise is NOT kept — the git removal failed, or the
	// agent never exited so the worktree is still there. A clean delete
	// matches the toast and stays silent (no Messages-tab noise).
	title := agent.FreshTitle(convID)
	goBackground(func() {
		if waitForConvOffline(convID, retireWorktreeExitGrace) {
			note, ok := applyRetireWorktreeCleanup(wt, true)
			slog.Info("retire: worktree cleanup after exit", "conv", convID, "detail", note, "ok", ok)
			if !ok {
				// Only a real removal failure reaches the human.
				postRetireWorktreeNotice(title, "Retire worktree cleanup failed", note)
			}
		} else {
			note := "worktree kept — agent did not exit within " + retireWorktreeExitGrace.String() +
				"; remove " + wt.Path + " manually"
			slog.Warn("retire: worktree kept — agent did not exit within grace",
				"conv", convID, "path", wt.Path, "grace", retireWorktreeExitGrace)
			postRetireWorktreeNotice(title, "Retire worktree kept", note)
		}
	})
	return retireWorktreePlan{Action: "scheduled",
		Detail: "worktree + branch will be removed after the agent exits"}
}

// postRetireWorktreeNotice records a FAILED deferred retire worktree
// cleanup in the dashboard Messages tab. The deferred path fires its
// optimistic toast ("will be removed after the agent exits") long before
// the removal runs, so when it does NOT succeed — the git removal
// errored, or the agent never exited and the worktree is still there —
// the human needs a signal that the promise wasn't kept; the daemon log
// alone is invisible. A successful delete matches the toast and is never
// posted here. Best-effort: a failed insert is logged, never bubbled.
func postRetireWorktreeNotice(agentTitle, subject, detail string) {
	body := detail
	if agentTitle != "" && agentTitle != agent.UnknownTitle {
		body = agentTitle + ": " + detail
	}
	if _, err := db.InsertHumanMessage(&db.HumanMessage{
		FromTitle: "retire cleanup",
		Subject:   subject,
		Body:      body,
	}); err != nil {
		slog.Warn("retire: failed to post worktree cleanup notice", "error", err)
	}
}

// resolveRetireWorktree resolves the worktree view the retire cleanup
// acts on, including the shared-with-another-agent check. Split out so
// the handler can resolve it BEFORE issuing the shutdown that the
// deferred removal then waits on.
func resolveRetireWorktree(convID string) agentWorktreeView {
	wt := inspectAgentWorktree(convID)
	wt.Shared = wt.Path != "" &&
		otherAgentWorktreeRoots(map[string]bool{convID: true})[wt.Path]
	return wt
}
