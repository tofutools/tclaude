package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// awb_ready_cleanup.go is the housekeeping half of AWB ready polling: what
// happens to the actor and the git footprint a pickup created once the daemon
// has closed its issue.
//
// The poller's monitored closure already proves the work landed — a merged pull
// request, or a recorded commit that reached main — and it only closes once the
// spawned agent has settled. At that point the pickup's whole footprint is
// finished work: an idle pane holding a context nobody will read again, a
// linked worktree, and a feature branch. Leaving them behind made every
// completed issue cost the operator a manual retire plus a `git worktree
// remove`, and a long-running process accumulated both.
//
// Two rules bound the sweep, and both are about not destroying something the
// operator still needs:
//
//   - The agent is retired only while it is still settled. The settle check ran
//     before the closure; it is taken again here because a human may have typed
//     into that pane in between, and an agent mid-turn must not be demoted out
//     from under itself.
//   - The branch is deleted only when the sweep can PROVE it reached main.
//     Everything else about a retire is recoverable — a retired conversation is
//     reinstatable, a worktree is a checkout of commits that live in the
//     repository — but a deleted branch whose commits are unmerged and unpushed
//     is gone. So an unprovable branch is kept, and the worktree directory goes
//     without it.

// awbReadyRetireActor is the `retired_by` attribution for an automatic sweep.
// A system literal rather than a conv-id: no agent asked for this retire, the
// polling process did, and the audit trail should not imply a peer did it.
const awbReadyRetireActor = "system:awb-ready-poller"

// liveAWBReadyBranchMergedFn is the git seam for the branch-merged proof, so
// flow tests can exercise both verdicts without building a merged history.
var liveAWBReadyBranchMergedFn = liveAWBReadyBranchMerged

// cleanupAfterClose retires the agent this dispatch spawned and removes the git
// footprint the pickup created. Best-effort by construction: every failure is
// logged and audited, none is returned, because the issue is already closed and
// a tick that failed here would either re-close it or hold the process on
// finished work.
//
// pr carries the merge verdict for the monitored pull request, empty under
// commit monitoring. It is what lets a squash- or rebase-merged branch still be
// recognised as merged; see awbReadyBranchMerged.
func (w awbReadyWorker) cleanupAfterClose(ctx context.Context, dispatch *db.AWBReadyDispatch, pr awbReadyPRState) {
	agentID := strings.TrimSpace(dispatch.AgentID)
	if agentID == "" {
		return
	}
	a, err := db.GetAgent(agentID)
	if err != nil {
		slog.Warn("awb ready polling: could not load the spawned agent for cleanup",
			"process", w.process, "issue", dispatch.IssueID, "agent_id", agentID, "error", err)
		return
	}
	if a == nil || a.CurrentConvID == "" {
		return
	}
	convID := a.CurrentConvID
	// A pane the operator picked back up is not this sweep's business. The
	// closure's own settle check is a tick old by now, so ask again.
	settled, err := liveAWBReadyAgentSettled(agentID)
	if err != nil {
		slog.Warn("awb ready polling: could not confirm the spawned agent settled before cleanup",
			"process", w.process, "issue", dispatch.IssueID, "agent_id", agentID, "error", err)
		return
	}
	if !settled {
		slog.Info("awb ready polling: agent is working again, leaving it and its worktree in place",
			"process", w.process, "issue", dispatch.IssueID, "agent_id", agentID, "conv", convID)
		w.auditDetail("agent.cleanup.skipped", dispatch.IssueID, http.StatusConflict,
			"agent_id="+agentID+" reason=agent_working")
		return
	}

	// Resolve the worktree BEFORE anything is demoted or stopped: the
	// shared-with-a-surviving-agent check reads sibling sessions, and the
	// branch-merged proof has to be taken while the branch still exists.
	var wt agentWorktreeView
	if w.config.Worktree {
		wt = resolveRetireWorktree(convID)
		if wt.Removable() {
			merged, why := w.branchMerged(ctx, wt.Branch, pr)
			wt.KeepBranch = !merged
			slog.Info("awb ready polling: resolved the branch cleanup verdict",
				"process", w.process, "issue", dispatch.IssueID, "branch", retireBranchLabel(wt.Branch),
				"merged", merged, "why", why)
		}
	}

	if a.Active() {
		if _, _, err := retireAgentConv(convID, awbReadyRetireActor,
			"AWB issue "+dispatch.IssueID+" closed automatically"); err != nil {
			slog.Warn("awb ready polling: could not retire the spawned agent",
				"process", w.process, "issue", dispatch.IssueID, "agent_id", agentID,
				"conv", convID, "error", err)
			w.auditDetail("agent.retire", dispatch.IssueID, http.StatusInternalServerError,
				"agent_id="+agentID+" error="+err.Error())
			return
		}
	}
	// Shutdown is unconditional: an idle pane on a closed issue is exactly what
	// the operator no longer wants occupying a window, and it is a soft exit —
	// finishRetiredConv never kills a process that is still doing something.
	td := finishRetiredConv(convID, true /* shutdown */, w.config.Worktree, wt, "")
	detail := "agent_id=" + agentID + " conv=" + short8(convID)
	if td.Worktree != nil {
		detail += " worktree=" + td.Worktree.Action
	}
	slog.Info("awb ready polling: retired the spawned agent after closing its issue",
		"process", w.process, "workspace", w.workspace, "issue", dispatch.IssueID,
		"agent_id", agentID, "conv", convID, "notes", strings.Join(td.Notes, "; "))
	w.auditDetail("agent.retire", dispatch.IssueID, http.StatusOK, detail)
}

// branchMerged decides whether the automatic cleanup may delete branch, and
// returns the reason for the decision for the log line that records it.
//
// Two proofs are accepted, because one of them is unavailable in the case this
// team hits most often:
//
//   - GitHub says so. The monitored pull request merged, its head branch IS
//     this branch, and it merged into a trunk. This is the only proof that
//     survives a squash or a rebase merge, where the commits on main are new
//     objects and no local ancestry ever connects them to the branch tip.
//   - Local ancestry says so. The branch tip is already contained in the main
//     branch the process monitors — the ordinary merge-commit or fast-forward
//     case, and the only proof available under commit monitoring.
//
// Anything else — a detached HEAD, a branch the poller cannot resolve, a git
// error, a pull request that merged a DIFFERENT branch — is "not proven", and
// the branch is kept.
func (w awbReadyWorker) branchMerged(ctx context.Context, branch string, pr awbReadyPRState) (bool, string) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false, "no branch is checked out (detached HEAD)"
	}
	if pr.Merged && strings.EqualFold(pr.HeadRef, branch) && isProtectedBranchName(pr.BaseRef) {
		return true, "the merged pull request merged this branch into " + pr.BaseRef
	}
	merged, err := liveAWBReadyBranchMergedFn(ctx, w.config.Cwd, branch)
	if err != nil {
		return false, "could not check whether the branch reached main: " + err.Error()
	}
	if merged {
		return true, "the branch tip is already contained in main"
	}
	return false, "the branch tip has not reached main"
}

// liveAWBReadyBranchMerged reports whether branch's tip is already contained in
// the main branch this process monitors.
//
// It resolves the branch tip locally and then reuses the poller's existing
// commit-on-main check, so the branch verdict is decided against exactly the
// same main the closure was: an isolated, hardened fetch of `origin/main` when
// an allow-listed origin exists, and the local `main` when none is configured.
//
// A branch that no longer resolves is reported as not merged rather than as an
// error: there is nothing left to delete, and the caller's "keep it" path is
// already the right behaviour for that.
func liveAWBReadyBranchMerged(ctx context.Context, cwd, branch string) (bool, error) {
	if fault := validateBranchName(branch); fault != nil {
		return false, fmt.Errorf("invalid branch name: %s", fault.Msg)
	}
	checkCtx, cancel := context.WithTimeout(ctx, gitProxyNetworkTimeout)
	defer cancel()
	// remoteScoped: this session only runs a local rev-parse, so it must not
	// require the operator's remote allow-list. The ancestry check below opens
	// its own session and applies that gate itself.
	s, fault := newGitProxySessionBase(checkCtx, true)
	if fault != nil {
		return false, fmt.Errorf("prepare hardened git session: %s", fault.Msg)
	}
	s.repoRoot = cwd
	// The refs/heads/ prefix is also the option-injection guard: a value under
	// it can never be read as a flag, whatever validateBranchName let through.
	res, err := s.git(checkCtx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	tip := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || tip == "" {
		return false, nil
	}
	reached, _, err := liveAWBReadyCommitOnMainFn(ctx, cwd, tip)
	return reached, err
}
