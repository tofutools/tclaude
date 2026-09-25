package agentd

import (
	"context"
	"errors"
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
// request, or a recorded commit that reached origin/main — or the agent closed
// its issue under monitor_close. Cleanup waits until the spawned agent has
// settled. At that point the pickup's whole footprint is
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
//     from under itself. The re-check is passed to the retire as a guard rather
//     than evaluated here, so "still idle" is read microseconds before the
//     demotion commits instead of before a git fetch that can take seconds.
//   - The branch is deleted only when the sweep can PROVE it reached main, and
//     only while it still points at the commit that proof was about. Everything
//     else about a retire is recoverable — a retired conversation is
//     reinstatable, a worktree is a checkout of commits that live in the
//     repository — but a deleted branch whose commits are unmerged and unpushed
//     is gone. So an unprovable branch is kept, the worktree directory goes
//     without it, and even a proven branch is deleted through a
//     compare-and-swap, because the removal can run long after the proof.

// awbReadyRetireActor is the `retired_by` attribution for an automatic sweep.
// A system literal rather than a conv-id: no agent asked for this retire, the
// polling process did, and the audit trail should not imply a peer did it.
const awbReadyRetireActor = "system:awb-ready-poller"

// awbReadyBranchTipFn is the git seam for resolving a branch tip, so tests can
// drive the merge proof without building a real history. The ancestry half of
// that proof reuses liveAWBReadyCommitOnMainFn, which is already a seam.
var awbReadyBranchTipFn = awbReadyBranchTip

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
	// A cheap early out for the overwhelmingly common "went back to work" case,
	// so the sweep does not pay for a worktree probe and a git fetch it is about
	// to throw away. It is NOT the guard that makes the retire safe — that is
	// awbReadyStillSettled below, evaluated at the commit itself.
	if settled, err := liveAWBReadyAgentSettled(agentID); err != nil {
		slog.Warn("awb ready polling: could not confirm the spawned agent settled before cleanup",
			"process", w.process, "issue", dispatch.IssueID, "agent_id", agentID, "error", err)
		return
	} else if !settled {
		w.reportCleanupSkipped(dispatch.IssueID, agentID, convID)
		return
	}

	// Resolve the worktree BEFORE anything is demoted or stopped: the
	// shared-with-a-surviving-agent check reads sibling sessions, and the
	// branch-merged proof has to be taken while the branch still exists.
	var wt agentWorktreeView
	if w.config.Worktree {
		wt = resolveRetireWorktree(convID)
		if wt.Removable() {
			tip, why := w.branchMergedAt(ctx, wt.Branch, pr)
			// The tip travels with the verdict rather than being re-read at the
			// removal: it is what licenses the delete, so the delete is
			// conditional on it and a branch that moved since survives.
			wt.BranchTip, wt.KeepBranch = tip, tip == ""
			slog.Info("awb ready polling: resolved the branch cleanup verdict",
				"process", w.process, "issue", dispatch.IssueID, "branch", retireBranchLabel(wt.Branch),
				"merged", tip != "", "why", why)
		}
	}

	if a.Active() {
		_, _, err := retireAgentConvGuarded(convID, awbReadyRetireActor,
			"AWB issue "+dispatch.IssueID+" closed automatically", false,
			func() error { return awbReadyStillSettled(agentID) })
		if errors.Is(err, errAWBReadyAgentBusy) {
			w.reportCleanupSkipped(dispatch.IssueID, agentID, convID)
			return
		}
		if err != nil {
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

// errAWBReadyAgentBusy aborts the guarded retire when the actor is no longer
// settled at the commit boundary. Distinct from a real failure: nothing went
// wrong, the sweep simply has no licence to act any more.
var errAWBReadyAgentBusy = errors.New("agent is no longer settled")

// awbReadyStillSettled is the retire guard. A read error is treated as busy, not
// as settled: the sweep is housekeeping and must fail toward leaving the actor
// alone.
func awbReadyStillSettled(agentID string) error {
	settled, err := liveAWBReadyAgentSettled(agentID)
	if err != nil {
		return fmt.Errorf("confirm the spawned agent is still settled: %w", err)
	}
	if !settled {
		return errAWBReadyAgentBusy
	}
	return nil
}

func (w awbReadyWorker) reportCleanupSkipped(issueID, agentID, convID string) {
	slog.Info("awb ready polling: agent is working again, leaving it and its worktree in place",
		"process", w.process, "issue", issueID, "agent_id", agentID, "conv", convID)
	w.auditDetail("agent.cleanup.skipped", issueID, http.StatusConflict,
		"agent_id="+agentID+" reason=agent_working")
}

// branchMergedAt decides whether the automatic cleanup may delete branch, and
// returns the commit that licenses the deletion ("" for "it may not") together
// with the reason for the log line that records it.
//
// Both proofs are about the branch's CURRENT tip, because that is the commit
// deletion would destroy. Either of them is enough:
//
//   - GitHub merged that commit. The monitored pull request merged, its head
//     branch is this branch, its head commit is this tip, and it merged into a
//     trunk. This is the only proof that survives a squash or a rebase merge,
//     where the commits on main are new objects and no local ancestry ever
//     connects them back. Pinning it to the tip is what keeps it from covering
//     work committed on the branch after the merge.
//   - Local ancestry contains that commit. The tip is already reachable from
//     origin/main — the ordinary merge-commit or
//     fast-forward case, and the only proof available under commit monitoring.
//
// Anything else — a detached HEAD, a branch that no longer resolves, a git
// error, a pull request that merged a different branch or an older commit — is
// "not proven", and the branch is kept.
func (w awbReadyWorker) branchMergedAt(ctx context.Context, branch string, pr awbReadyPRState) (string, string) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", "no branch is checked out (detached HEAD)"
	}
	tip, err := awbReadyBranchTipFn(ctx, w.config.Cwd, branch)
	if err != nil {
		return "", "could not resolve the branch tip: " + err.Error()
	}
	if tip == "" {
		return "", "the branch no longer exists"
	}
	if pr.Merged && strings.EqualFold(pr.HeadRef, branch) &&
		strings.EqualFold(pr.HeadOID, tip) && isProtectedBranchName(pr.BaseRef) {
		return tip, "the merged pull request merged this exact commit into " + pr.BaseRef
	}
	// The same check the closure itself was decided by, so the branch verdict is
	// taken against an isolated, hardened fetch of origin/main. With no origin,
	// this proof is unavailable and the branch is kept.
	reached, _, err := liveAWBReadyCommitOnMainFn(ctx, w.config.Cwd, tip)
	if err != nil {
		return "", "could not check whether the branch reached main: " + err.Error()
	}
	if reached {
		return tip, "the branch tip is already contained in main"
	}
	return "", "the branch tip has not reached main"
}

// awbReadyBranchTip resolves branch to a commit id in the repository at cwd, or
// to "" when no such branch exists.
//
// A missing branch is not an error: there is nothing left to delete, and the
// caller's "keep it" path is already the right answer for that.
func awbReadyBranchTip(ctx context.Context, cwd, branch string) (string, error) {
	if fault := validateBranchName(branch); fault != nil {
		return "", fmt.Errorf("invalid branch name: %s", fault.Msg)
	}
	probeCtx, cancel := context.WithTimeout(ctx, gitProxyNetworkTimeout)
	defer cancel()
	// remoteScoped: this session only runs a local rev-parse, so it must not
	// require the operator's remote allow-list. The ancestry check the caller
	// makes next opens its own session and applies that gate itself.
	s, fault := newGitProxySessionBase(probeCtx, true)
	if fault != nil {
		return "", fmt.Errorf("prepare hardened git session: %s", fault.Msg)
	}
	s.repoRoot = cwd
	// The refs/heads/ prefix is also the option-injection guard: a value under
	// it can never be read as a flag, whatever validateBranchName let through.
	res, err := s.git(probeCtx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve branch %s: %w", branch, err)
	}
	if res.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(res.Stdout), nil
}
