package agentd

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// memberOpResult is the per-member outcome of a bulk lifecycle op
// (stop / resume). The CLI prints these as a summary table so the
// human can see which members succeeded, which were no-ops, and
// which failed.
type memberOpResult struct {
	// AgentID is the member's stable actor key — the canonical ID the CLI
	// leads with in the result table; ConvID is the live generation behind it.
	AgentID  string   `json:"agent_id,omitempty"`
	ConvID   string   `json:"conv_id"`
	Title    string   `json:"title,omitempty"`
	Action   string   `json:"action"`           // "soft_stopped", "killed", "killed_no_soft_exit", "resumed", "skipped:already_online", "skipped:no_conv_id", "error"
	Detail   string   `json:"detail,omitempty"` // human-readable note (e.g. error message)
	TmuxSes  string   `json:"tmux_session,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// Worktree is the optional worktree+branch cleanup outcome attached by
	// a bulk retire that requested it (delete_worktree). nil on every other
	// bulk op (stop/resume) and on a retire that did not ask for cleanup,
	// so the field is omitted from those responses entirely.
	Worktree *retireWorktreePlan `json:"worktree,omitempty"`
}

type groupOpResp struct {
	Group   string           `json:"group"`
	Action  string           `json:"action"`
	Members []memberOpResult `json:"members"`
	// RhythmsReenabled is the number of group-target cron jobs a resume
	// re-enabled — exactly the rhythms a prior emptying retire auto-disabled
	// (JOH-345). Omitted when zero / for a stop.
	RhythmsReenabled int `json:"rhythms_reenabled,omitempty"`
}

const daemonSoftExitReason = "soft_exit"

// handleGroupStop ends every member's running tmux session.
//
// Modes:
//   - soft (default): inject `/exit` via tmux send-keys, mirroring the
//     /rename pattern. Lets CC clean up its own state. The actual tmux
//     session usually goes away on CC's next iteration.
//   - force (?force=1): tmux kill-session -t <name>. Last resort —
//     drops any unsubmitted input the agent hadn't sent yet.
//
// Members that aren't currently online are reported as
// `skipped:already_offline` and skipped — stop is idempotent.
func handleGroupStop(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	affected := make([]string, 0, len(members))
	selected := make(map[string]bool, len(members))
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "snapshot live group members: "+err.Error())
		return
	}
	for _, member := range members {
		online, _ := convLiveStatus(member.ConvID, alive)
		if online {
			affected = append(affected, member.ConvID)
			selected[member.ConvID] = true
		}
	}
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersStop, g,
		ActionContext{affectedConvs: affected}); !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	action := db.AgentExitActionStop
	if force {
		action = db.AgentExitActionForceStop
	}
	requestEventID := auditRequestEventID(r)
	// Every member is stopped CONCURRENTLY and each stop WAITS for its pane
	// process to actually die (the full ladder: the exit command plus its
	// double-tap re-injections, then kill-pane, SIGTERM, SIGKILL). Sequentially
	// that would be the sum of every member's exit latency — a group of ten
	// agents that each take their time closing would hold the request for a
	// minute-plus and report "soft_stopped" for panes still running. Bounded
	// fan-out makes the wall-clock the SLOWEST member instead of the sum, which
	// is what makes waiting affordable in the first place.
	out := groupOpResp{Group: g.Name, Action: "stop", Members: []memberOpResult{}}
	out.Members = mapAgentsConcurrently(members, batchAgentOpConcurrency,
		func(_ int, m *db.AgentGroupMember) (memberOpResult, bool) {
			if m.ConvID != "" && !selected[m.ConvID] {
				return memberOpResult{ConvID: m.ConvID, AgentID: peerAgentID(m.ConvID),
					Title: agent.FreshTitle(m.ConvID), Action: "skipped:already_offline"}, true
			}
			res, _ := stopOneConvAndWait(m.ConvID, force, action, requestEventID, 0)
			res.AgentID = peerAgentID(m.ConvID)
			res.Title = agent.FreshTitle(m.ConvID)
			return res, true
		})
	if out.Members == nil {
		out.Members = []memberOpResult{}
	}
	writeJSON(w, http.StatusOK, out)
}

// stopOneConv soft-stops (or force-kills with `force=true`) the live
// tmux session for convID. Returns the per-conv result. Shared between
// the bulk groups.members.stop loop and the single-conv agent.stop endpoint.
//
// Result shape mirrors the existing memberOpResult so the bulk
// summary table renders the same regardless of how the call was
// initiated. Idempotent: convs already offline come back as
// `skipped:already_offline`.
func stopOneConv(convID string, force bool) memberOpResult {
	action := db.AgentExitActionStop
	if force {
		action = db.AgentExitActionForceStop
	}
	return stopOneConvWithIntent(convID, force, action, "")
}

// stopOneConvWithIntent adds audit-only lifecycle attribution to a stop. The
// intent is armed immediately before the tmux mutation and cleared whenever
// that mutation fails. Persistence is deliberately best-effort: an audit I/O
// failure is logged but never changes the established stop result.
func stopOneConvWithIntent(convID string, force bool, lifecycleAction, relatedEventID string) memberOpResult {
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	return stopOneConvWithIntentUnderLaunchLock(convID, force, lifecycleAction, relatedEventID)
}

// stopWaitPolicy says whether a stop returns as soon as the exit command is
// delivered (the default, stopNoWait) or stays until the pane process is
// actually gone.
//
// A delivered /exit is not a stopped agent. Every caller that goes on to touch
// what the agent still holds — retire deleting its agent-owned directories or
// its worktree, a bulk stop reporting to the human what it achieved, a restart
// about to relaunch into the same identity — needs the second contract, or it
// races the pane it just asked to close. Waiting is also what makes a bulk
// stop's answer true: without it the response says "soft_stopped" for agents
// that are still very much running.
//
// deadline is how long the delivered exit (and its bounded double-tap
// re-injections) has to work before the kill ladder starts. It is taken
// literally, zero included: a caller that wants "no grace, escalate now"
// (the power buttons' grace_ms=0) gets exactly that.
type stopWaitPolicy struct {
	wait     bool
	deadline time.Duration
}

// stopNoWait is the fire-and-forget contract: deliver the exit, arm the
// out-of-band escalation (scheduleSoftExitEscalation), return. The pane still
// dies — the same ladder runs, just in the background — the caller simply does
// not learn when.
//
// It is correct ONLY where "stopped" is the entire request and nothing
// downstream depends on the process being gone. That is exactly three callers
// today, and the list should stay short:
//
//   - POST /v1/agent/{selector}/stop      — the CLI's `tclaude agent stop`
//   - POST /api/agents/{id}/stop          — without {"wait":true}; the shutdown
//     dialog opts INTO waiting so its spinner spans the real stop (OpenCode's
//     control-API exit dispatch returns in milliseconds), and its retry /
//     force-kill affordances give a stuck ladder somewhere to land
//   - the non-force agent delete, which deliberately refuses with 409 and asks
//     the caller to retry rather than waiting out the escalation window
//
// The CLI stop would otherwise hold a human's request for the length of the
// escalation window to report something the human can already see in the pane.
// Anything that goes on to touch what the agent holds — a retire deleting its
// directories or worktree, a delete unlinking its .jsonl, a restart relaunching
// into the same identity, a bulk op reporting what it achieved — must use
// stopWaitForExit / stopOneConvAndWait instead.
var stopNoWait = stopWaitPolicy{}

// stopWaitForExit blocks the stop until the pane process is gone or the kill
// ladder is exhausted. deadline 0 → softExitEscalationDeadline.
func stopWaitForExit(deadline time.Duration) stopWaitPolicy {
	return stopWaitPolicy{wait: true, deadline: deadline}
}

// stopOneConvWithIntentUnderLaunchLock is stopOneConvWithIntent after the
// caller has acquired the conversation launch lock. Compound lifecycle
// operations use it to keep stop → posture mutation → resume indivisible from
// other daemon wake/stop requests.
func stopOneConvWithIntentUnderLaunchLock(convID string, force bool, lifecycleAction, relatedEventID string) memberOpResult {
	res, _ := stopOneConvUnderLaunchLock(convID, force, lifecycleAction, relatedEventID, stopNoWait)
	return res
}

// stopOneConvAndWait soft-stops convID and does not return until its pane
// process has actually gone away — the full ladder run inline: the delivered
// exit command plus its bounded re-injections get `deadline` to work, then
// kill-pane, SIGTERM and SIGKILL each get their turn.
//
// This is the primitive for every caller that must not proceed while the agent
// is still running: the retire tiers (which then delete the agent's
// directories and worktree) and the bulk stop surfaces. The returned
// softExitOutcome says whether it closed on its own, needed the ladder, or is
// STILL alive — the last of which a caller must treat as "the agent never
// released anything".
//
// deadline 0 means "the standard window" (softExitEscalationDeadline); a
// caller that needs an explicit zero-grace stop drives
// stopOneConvUnderLaunchLock with stopWaitForExit(0) itself.
func stopOneConvAndWait(convID string, force bool, lifecycleAction, relatedEventID string, deadline time.Duration) (memberOpResult, softExitOutcome) {
	if deadline <= 0 {
		deadline = softExitEscalationDeadline
	}
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	return stopOneConvUnderLaunchLock(convID, force, lifecycleAction, relatedEventID, stopWaitForExit(deadline))
}

// errAgentStillRunning reports that a stop ran the whole escalation ladder (or
// could not act on the pane at all) and the process is STILL alive.
var errAgentStillRunning = errors.New("agent is still running after the full stop escalation")

// stopBeforePurge is the stop every DESTRUCTIVE path must use before it unlinks
// what the agent holds — its agent-owned directories, its worktree, its .jsonl,
// its rows.
//
// Retire does not need this: its cleanups defer themselves until the pane is
// observed offline and keep whatever they cannot safely remove. A purge has no
// such luxury — it acts immediately and cannot be undone — so it needs the
// strong contract: soft exit first (with the double-tap re-injections), then
// kill-pane, SIGTERM, SIGKILL, and a definite answer about whether the process
// is gone.
//
// A softExitStuck or softExitUnattempted outcome is a REFUSAL, not a warning.
// Deleting a running agent's files leaves a live orphan writing into paths that
// no longer exist and rows that no longer exist — strictly worse than the
// delete not happening. Callers surface the error; the human retries or
// investigates. An already-offline conv returns immediately with no error,
// which is the overwhelmingly common case for a delete.
func stopBeforePurge(convID, relatedEventID string) (memberOpResult, error) {
	res, outcome := stopOneConvAndWait(
		convID, false /* soft exit first */, db.AgentExitActionForceStop, relatedEventID, 0)
	switch outcome {
	case softExitStuck, softExitUnattempted:
		detail := res.Detail
		if detail == "" {
			detail = res.Action
		}
		return res, fmt.Errorf("%w: %s", errAgentStillRunning, detail)
	}
	return res, nil
}

// stopOneConvUnderLaunchLock is the one stop implementation both contracts go
// through; waitPolicy picks which. The softExitOutcome is only meaningful when
// waitPolicy.wait is set (it is softExitClosed otherwise — nothing was
// waited for, so nothing is known).
func stopOneConvUnderLaunchLock(convID string, force bool, lifecycleAction, relatedEventID string, waitPolicy stopWaitPolicy) (memberOpResult, softExitOutcome) {
	recoveryReason := lifecycleAction
	if recoveryReason == "" {
		recoveryReason = db.AgentExitActionStop
	}
	if cancelled, err := db.CancelAgentRecoveryForConv(convID, recoveryReason, time.Now()); err != nil {
		slog.Warn("stop: cancel automatic recovery failed", "conv", short8(convID), "error", err)
	} else if cancelled {
		if recovery, _ := db.AgentRecoveryForConv(convID); recovery != nil {
			_ = db.RecordAgentRecoveryAudit(*recovery, db.AuditVerbAgentRecoveryCancelled, recoveryReason, time.Now())
		}
	}
	res := memberOpResult{ConvID: convID}
	sess := pickAliveSession(convID)
	if sess == nil {
		res.Action = "skipped:already_offline"
		return res, softExitClosed
	}
	res.TmuxSes = sess.TmuxSession
	target, targetErr := captureLifecycleTarget(sess)
	if targetErr != nil {
		logLifecycleStopFailure("capture", sess.TmuxSession, sess.ID, targetErr)
		res.Action = "error"
		res.Detail = "capture selected pane: " + targetErr.Error()
		return res, softExitUnattempted
	}
	if err := refreshStoppedSessionResumeProvenance(sess); err != nil {
		// Administrative stop must remain available even when the target cwd is
		// unhealthy. The helper clears stale provenance before returning whenever
		// the DB is writable, so a later non-human resume fails closed.
		res.Detail = "resume provenance unavailable; human recovery will be required: " + err.Error()
		slog.Error("stop: resume provenance capture failed; continuing stop with provenance invalidated",
			"session", sess.ID, "conv", convID, "error", err)
	}
	if force {
		intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
		if lifecycleAction != "" && intentSet == nil {
			res.Action, res.Detail = "error", "selected launch intent became stale"
			return res, softExitUnattempted
		}
		if err := killLifecycleTarget(target); err != nil {
			clearFailedExitIntent(intentSet)
			res.Action = "error"
			res.Detail = "kill-session: " + err.Error()
		} else {
			res.Action = "killed"
		}
		// A successful kill-pane is tmux letting go of the pane, not proof the
		// harness process died — it can be wedged in uninterruptible work or
		// held open by a child. A waiting caller gets the signal half of the
		// ladder (SIGTERM, then SIGKILL) until the process group is really gone.
		return res, finishStopWait(target, waitPolicy, lifecycleAction, relatedEventID, "force-stop", daemonEscalatedKillReason, &res)
	}
	// Soft stop: inject the harness's exit command (CC's `/exit`). The
	// harness closes the conversation cleanly and the tmux session goes
	// away when it exits. The command is sourced from the harness's
	// Lifecycle so a non-CC pane is never typed `/exit` if that's not its
	// exit command.
	//
	// An API-connected Copilot agent stays on this path, unlike its rename,
	// compaction and message delivery (TCL-1058). The RPCs that look like the
	// answer are not one, measured against Copilot CLI 1.0.78: `session.
	// shutdown` and `sessions.close` both succeed and leave the copilot process
	// running with its session still foregrounded, and `runtime.shutdown`
	// refuses with "Runtime shutdown is not available for this server". They end
	// a session, not the CLI. Routing soft exit through them would report a
	// delivered exit for a pane that never dies — which every stop, retire and
	// dashboard surface then reads as an agent that finished its work. Ending
	// the CLI through the pane really does work, so the pane path stays —
	// though for Copilot the pane input is ctrl-c presses rather than a typed
	// /exit (see sendSoftExitToTarget).
	h := harnessForConv(convID)
	if h.SupportsSoftExit() {
		exitCmd := h.Life.SoftExitCommand()
		fallbackExitReason := ""
		if h.Name == harness.OpenCodeName && openCodeControlInputBlocked(sess.Status) {
			res.Action = "error"
			res.Detail = "OpenCode TUI is " + sess.Status + "; retry soft stop when idle or force kill"
			return res, softExitUnattempted
		}
		intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
		if lifecycleAction != "" && intentSet == nil {
			res.Action, res.Detail = "error", "selected launch intent became stale"
			return res, softExitUnattempted
		}
		delivered := false
		if h.Name == harness.OpenCodeName {
			delivered = injectOpenCodeSoftExitTarget(target, "soft-exit", intentSet)
		} else {
			delivered = injectSoftExitTarget(target, exitCmd, h.Life.SoftExitPrefixKeys(), "soft-exit", intentSet)
		}
		if delivered {
			fallbackExitReason = daemonSoftExitReason
			if h.Name == harness.CodexName {
				// Codex has no SessionEnd hook; record daemon-owned /quit
				// separately from an unclassified user pane close.
				if err := db.SetSessionExitReason(sess.ID, daemonSoftExitReason); err != nil {
					slog.Warn("failed to record daemon soft-exit reason",
						"session", sess.ID, "conv", convID, "error", err)
				}
			}
			res.Action = "soft_stopped"
		} else {
			clearFailedExitIntent(intentSet)
			res.Action = "error"
			switch {
			case h.Name == harness.OpenCodeName:
				res.Detail = "managed OpenCode TUI exit dispatch failed"
			case len(h.SignalExitKeys()) > 0:
				// Every keystroke-free harness (Copilot, Claude Code, Codex)
				// reports the signal path it actually took, not a typed
				// exitCmd it never sent.
				res.Detail = "managed " + h.Name + " signal exit dispatch failed"
			default:
				res.Detail = "send-keys " + exitCmd + " failed"
			}
		}
		// Whether the exit was delivered or the injection itself failed, the
		// pane must end: this is a stop. Nothing stronger used to follow a soft
		// exit that never took, which is how a retired agent's pane outlived
		// its own directory-cleanup grace (TCL-1001). The ladder converges well
		// inside that grace.
		if stopIntendsPaneClosure(lifecycleAction) {
			if waitPolicy.wait {
				return res, finishStopWait(target, waitPolicy, lifecycleAction, relatedEventID, "soft-exit", fallbackExitReason, &res)
			}
			scheduleSoftExitEscalation(target, lifecycleAction, relatedEventID, "soft-exit", fallbackExitReason)
		} else if waitPolicy.wait {
			// The caller asked to wait, but this action does not entitle us to
			// kill (stopIntendsPaneClosure is what gates that). Wait for the pane
			// anyway — silently returning "closed" without a single probe would
			// hand the caller a guarantee nothing checked — just never escalate.
			if waitForLifecycleTargetGone(target, waitPolicy.deadline) {
				// Verified gone: the retry watchdog has nothing left to do.
				// (A pane still alive keeps it armed — this stop may not
				// escalate, but the re-injections are still meaningful.)
				target.markSoftExitSettled()
				return res, softExitClosed
			}
			res.Detail = joinDetail(res.Detail, "pane still alive; no escalation for this stop")
			return res, softExitStuck
		}
		return res, softExitClosed
	}
	// No soft-exit command for this harness → hard kill so the pane never
	// lingers because we couldn't type a graceful exit.
	intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
	if lifecycleAction != "" && intentSet == nil {
		res.Action, res.Detail = "error", "selected launch intent became stale"
		return res, softExitUnattempted
	}
	if err := killLifecycleTarget(target); err != nil {
		clearFailedExitIntent(intentSet)
		res.Action = "error"
		res.Detail = "kill-session (harness has no soft-exit): " + err.Error()
	} else {
		res.Action = "killed_no_soft_exit"
	}
	return res, finishStopWait(target, waitPolicy, lifecycleAction, relatedEventID, "no-soft-exit", daemonEscalatedKillReason, &res)
}

// finishStopWait runs the inline half of a waiting stop and annotates the
// result with what the wait cost. A non-waiting policy is a no-op, so every
// stop branch can end with the same call.
//
// The Detail notes matter to the operator: a bulk retire whose members all
// came back "escalated to kill" is a harness that stopped honouring its own
// exit command, and a "still alive" member is one whose directories and
// worktree were deliberately left in place.
func finishStopWait(target *lifecycleTarget, waitPolicy stopWaitPolicy, lifecycleAction, relatedEventID, reason, fallbackExitReason string, res *memberOpResult) softExitOutcome {
	if !waitPolicy.wait {
		return softExitClosed
	}
	deadline := waitPolicy.deadline
	if reason != "soft-exit" {
		// The kill already happened; only the signal ladder is left, so there
		// is no delivered exit command to wait out first.
		deadline = softExitEscalationSignalGrace
	}
	outcome := awaitLifecycleTargetExit(target, deadline, lifecycleAction, relatedEventID, reason)
	// Whatever the outcome, the stop is resolved: the pane exit was verified,
	// or the ladder ran to its end. Release the background retry watchdog now
	// rather than letting it sleep out its delay to rediscover this.
	target.markSoftExitSettled()
	switch outcome {
	case softExitClosed:
		if err := reconcileStoppedLifecycleTarget(target, lifecycleAction, relatedEventID, fallbackExitReason); err != nil {
			res.Action = "error"
			res.Detail = joinDetail(res.Detail, "session stopped but recording exited state failed: "+err.Error())
		}
	case softExitEscalated:
		if err := reconcileStoppedLifecycleTarget(target, lifecycleAction, relatedEventID, daemonEscalatedKillReason); err != nil {
			res.Action = "error"
			res.Detail = joinDetail(res.Detail, "session stopped but recording exited state failed: "+err.Error())
		}
		if reason == "soft-exit" {
			res.Detail = joinDetail(res.Detail, "pane did not exit; escalated to kill")
		}
	case softExitStuck:
		res.Detail = joinDetail(res.Detail, "pane process still alive after kill escalation")
	}
	return outcome
}

type lifecycleTarget struct {
	sessionID           string
	convID              string
	tmuxSession         string
	generation          string
	paneID              string
	panePID             int
	paneGenerationBound bool

	// softExitSettled closes (via markSoftExitSettled) once a stop path has
	// verified this target's pane is gone or run the escalation ladder to its
	// end. The background soft-exit retry watchdog selects on it between
	// attempts so it stands down the moment the stop resolves, instead of
	// sleeping out its full delay only to rediscover a session that is long
	// gone. Never closed directly — always through markSoftExitSettled.
	softExitSettled     chan struct{}
	softExitSettledOnce sync.Once
}

// markSoftExitSettled tells this target's background soft-exit retry watchdog
// that the stop already resolved — the pane's exit was verified, or the
// escalation ladder ran to its end (after which re-typing an exit command is
// pointless either way). Idempotent, and safe when no watchdog was ever
// scheduled.
func (t *lifecycleTarget) markSoftExitSettled() {
	if t == nil || t.softExitSettled == nil {
		return
	}
	t.softExitSettledOnce.Do(func() { close(t.softExitSettled) })
}

type paneProbeState int

const (
	paneProbeLive paneProbeState = iota
	paneProbeDead
	paneProbeUnknown
)

type lifecyclePaneProbe struct {
	state      paneProbeState
	paneID     string
	panePID    int
	generation string
}

func captureLifecycleTarget(sess *db.SessionRow) (*lifecycleTarget, error) {
	identity, err := db.GetSessionExitLaunchIdentity(sess.ID)
	if err != nil {
		return nil, err
	}
	p, err := probeLifecyclePane(sess.TmuxSession)
	if err != nil {
		return nil, err
	}
	if p.state != paneProbeLive {
		return nil, fmt.Errorf("pane is not live")
	}
	if identity.Generation != "" && p.generation != "" && p.generation != identity.Generation {
		return nil, fmt.Errorf("pane generation mismatch")
	}
	return &lifecycleTarget{sessionID: sess.ID, convID: sess.ConvID, tmuxSession: sess.TmuxSession, generation: identity.Generation, paneID: p.paneID, panePID: p.panePID, paneGenerationBound: p.generation != "", softExitSettled: make(chan struct{})}, nil
}

// probeLifecyclePane reads the pane's identity and liveness in one tmux call.
//
// Bounded by tmuxCommandTimeout, like every other tmux subprocess the daemon
// runs on a latency-sensitive path. It matters more here than most: a stop that
// WAITS calls this in a loop from inside an HTTP handler while holding the
// conversation's launch lock, so a tmux client that connects and never returns
// would park the request forever and block every later wake/stop/retire for
// that conv. The loop's own deadline bounds the polling, not the subprocess
// inside it — only this does that.
func probeLifecyclePane(tmuxSession string) (lifecyclePaneProbe, error) {
	format := "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}"
	out, err := tmuxOutputWithTimeout(
		"display-message", "-p", "-t", clcommon.ExactTarget(tmuxSession)+":", format)
	if err != nil {
		return lifecyclePaneProbe{state: paneProbeUnknown}, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) != 7 || parts[0] != tmuxSession || !validLifecyclePaneID(parts[1]) {
		return lifecyclePaneProbe{state: paneProbeUnknown}, fmt.Errorf("malformed pane probe")
	}
	pid, pidErr := strconv.Atoi(parts[2])
	if pidErr != nil || pid <= 0 {
		return lifecyclePaneProbe{state: paneProbeUnknown}, fmt.Errorf("malformed pane pid")
	}
	if parts[3] == "1" {
		return lifecyclePaneProbe{state: paneProbeDead, paneID: parts[1], panePID: pid, generation: parts[6]}, nil
	}
	return lifecyclePaneProbe{state: paneProbeLive, paneID: parts[1], panePID: pid, generation: parts[6]}, nil
}

func validLifecyclePaneID(v string) bool { return strings.HasPrefix(v, "%") && len(v) > 1 }

func (t *lifecycleTarget) revalidate() (lifecyclePaneProbe, error) {
	p, err := probeLifecyclePane(t.tmuxSession)
	if err != nil {
		return p, err
	}
	if p.state != paneProbeLive || !lifecycleProbeMatchesTarget(p, t) {
		return p, fmt.Errorf("selected pane identity changed")
	}
	return p, nil
}

func killLifecycleTarget(t *lifecycleTarget) error {
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := t.revalidate(); err != nil {
		logLifecycleStopFailure("revalidate", t.paneID, t.sessionID, err)
		return err
	}
	// Bounded: the escalation ladder runs this from inside a waiting stop's
	// request goroutine, under the launch lock.
	if err := runTmuxCommand("kill-pane", "-t", t.paneID); err != nil {
		logLifecycleStopFailure("kill", t.paneID, t.sessionID, err)
		return err
	}
	return nil
}

func logLifecycleStopFailure(action, target, session string, err error) {
	slog.Warn("stop: managed lifecycle action failed",
		"target", auditClip(target, 120),
		"session", auditClip(session, 120),
		"action", auditClip(action, 32),
		"error", auditClip(err.Error(), 240))
}

func setExitIntentBestEffort(sess *db.SessionRow, action, relatedEventID string) *db.SessionExitIntentRef {
	if sess == nil || action == "" {
		return nil
	}
	ref, err := db.SetSessionExitIntent(sess.ID, action, relatedEventID, time.Now())
	if err != nil {
		slog.Warn("exit audit: record lifecycle intent failed",
			"session", sess.ID, "action", action, "error", err)
		return nil
	}
	if cancelled, cancelErr := db.CancelAgentRecoveryForConv(sess.ConvID, action, time.Now()); cancelErr != nil {
		slog.Warn("lifecycle: cancel automatic recovery failed", "conv", short8(sess.ConvID), "action", action, "error", cancelErr)
	} else if cancelled {
		if recovery, _ := db.AgentRecoveryForConv(sess.ConvID); recovery != nil {
			_ = db.RecordAgentRecoveryAudit(*recovery, db.AuditVerbAgentRecoveryCancelled, action, time.Now())
		}
	}
	return &ref
}

func setExitIntentTargetBestEffort(target *lifecycleTarget, action, relatedEventID string) *db.SessionExitIntentRef {
	if target == nil || action == "" {
		return nil
	}
	ref, err := db.SetSessionExitIntentIfTarget(target.sessionID, target.tmuxSession, target.generation, action, relatedEventID, time.Now())
	if err != nil {
		slog.Warn("exit audit: selected lifecycle intent CAS failed", "session", target.sessionID, "error", err)
		return nil
	}
	if cancelled, cancelErr := db.CancelAgentRecoveryForConv(target.convID, action, time.Now()); cancelErr != nil {
		slog.Warn("lifecycle: cancel automatic recovery failed", "conv", short8(target.convID), "action", action, "error", cancelErr)
	} else if cancelled {
		if recovery, _ := db.AgentRecoveryForConv(target.convID); recovery != nil {
			_ = db.RecordAgentRecoveryAudit(*recovery, db.AuditVerbAgentRecoveryCancelled, action, time.Now())
		}
	}
	return &ref
}

// injectOpenCodeSoftExitTarget asks the managed server to dispatch app.exit to
// the attached TUI's command registry. This is the semantic equivalent of
// /exit without prompt-state or keybinding risk. The selected session ID and
// pane identity remain bound across the API send and bounded retry.
func injectOpenCodeSoftExitTarget(
	target *lifecycleTarget,
	reason string,
	intentRef *db.SessionExitIntentRef,
) bool {
	if target == nil {
		return false
	}
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := target.revalidate(); err != nil {
		logLifecycleStopFailure("revalidate", target.paneID, target.sessionID, err)
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
		return false
	}
	if err := sendOpenCodeTUICommand(
		target.convID, target.sessionID, openCodeTUIExit,
	); err != nil {
		logLifecycleStopFailure("OpenCode TUI API", target.paneID, target.sessionID, err)
		return false
	}
	if afterSoftExitTargetSendForTest != nil {
		afterSoftExitTargetSendForTest()
	}
	probe, _ := probeLifecyclePane(target.tmuxSession)
	if probe.state == paneProbeUnknown {
		if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
			return true
		}
		scheduleUnknownIntentCleanup(target, intentRef)
		return true
	}
	if probe.state == paneProbeDead || !lifecycleProbeMatchesTarget(probe, target) {
		return true
	}
	scheduleOpenCodeSoftExitRetryTarget(target, reason, intentRef)
	return true
}

func scheduleOpenCodeSoftExitRetryTarget(
	target *lifecycleTarget,
	reason string,
	intentRef *db.SessionExitIntentRef,
) {
	goBackground(func() {
		for attempt := 2; attempt <= softExitMaxAttempts; attempt++ {
			select {
			case <-target.softExitSettled:
				// Mirrors scheduleSoftExitRetryTarget: the stop's own wait
				// already verified the outcome, so stand down silently
				// (attempt logging is a Copilot forensic, not an OpenCode one).
				return
			case <-time.After(softExitRetryDelay):
			}
			if beforeSoftExitTargetRetryProbeForTest != nil {
				beforeSoftExitTargetRetryProbeForTest(attempt)
			}
			probe, err := probeLifecyclePane(target.tmuxSession)
			if err != nil || probe.state == paneProbeUnknown {
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			if probe.state == paneProbeDead || !lifecycleProbeMatchesTarget(probe, target) {
				return
			}
			sess := aliveSessionForConv(target.convID)
			if sess == nil || sess.ID != target.sessionID {
				return
			}
			if openCodeControlInputBlocked(sess.Status) {
				slog.Info("OpenCode soft-exit retry deferred while TUI is not idle",
					"conv_id", target.convID, "tmux_session", target.tmuxSession,
					"status", sess.Status, "attempt", attempt, "reason", reason)
				if attempt == softExitMaxAttempts {
					scheduleUnknownIntentCleanup(target, intentRef)
				}
				continue
			}
			if err := sendOpenCodeTUICommand(
				target.convID, target.sessionID, openCodeTUIExit,
			); err != nil {
				slog.Warn("OpenCode soft-exit retry API dispatch failed",
					"error", err, "conv_id", target.convID,
					"tmux_session", target.tmuxSession, "attempt", attempt,
					"reason", reason)
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			if attempt == softExitMaxAttempts {
				scheduleUnknownIntentCleanup(target, intentRef)
			}
		}
	})
}

func clearFailedExitIntent(intentRef *db.SessionExitIntentRef) {
	if intentRef == nil {
		return
	}
	if _, err := db.ClearSessionExitIntentIfCurrent(*intentRef); err != nil {
		slog.Warn("exit audit: clear failed lifecycle intent failed",
			"session", intentRef.SessionID, "error", err)
	}
}

// logSoftExitPaneState records what the pane is showing around one soft-exit
// injection attempt. Two captures bracket each attempt: "pre-send" is taken
// BEFORE that attempt's prefix keys run — Copilot's C-c clears a half-typed
// input line, so the escalation-time capture alone can never show whether an
// earlier attempt's exit command was sitting unsubmitted in the input box —
// and "post-send" is taken right after the settled submit, where a discarded
// command shows an idle prompt, a stuck one shows the text still in the box,
// and an accepted one shows the pane tearing down. Together they let a single
// occurrence of an ignored soft exit say which of those it was, instead of the
// operator reconstructing it from timing (the intermittent Copilot retire
// escalations that motivated this).
//
// Copilot-only, like every soft-exit screen capture (see
// softExitPaneScreenTail): the intermittent ignored-exit failure is a Copilot
// TUI behaviour, and other harnesses should not pay the capture nor leak
// their screens into logs for a bug they do not have.
func logSoftExitPaneState(target *lifecycleTarget, reason, phase string, attempt int) {
	if harnessForConv(target.convID).Name != harness.CopilotName {
		return
	}
	slog.Info("soft-exit: pane state",
		"phase", phase, "attempt", attempt,
		"session", target.sessionID, "conv", short8(target.convID),
		"tmux_session", target.tmuxSession, "pane_id", target.paneID,
		"reason", reason,
		"pane_screen", capturePaneScreenTail(target.paneID))
}

// logSoftExitBatchStart records that one soft-exit attempt (a "batch" — the
// full signal-key sequence, or the typed exit command) is about to be sent
// into the pane, and which attempt of the bounded ladder it is. Unlike the
// Copilot-only screen captures (logSoftExitPaneState) this is cheap and runs
// for every harness: when several agents stop in parallel these lines are what
// lets an operator line up per-pane batch timings against how long each stop
// actually took.
func logSoftExitBatchStart(target *lifecycleTarget, reason string, attempt int, signalKeys []string) {
	mode := "typed"
	if len(signalKeys) > 0 {
		mode = "signal"
	}
	slog.Info("soft-exit: sending exit batch",
		"conv", short8(target.convID), "session", target.sessionID,
		"tmux_session", target.tmuxSession, "pane_id", target.paneID,
		"attempt", attempt, "max_attempts", softExitMaxAttempts,
		"mode", mode, "keys", len(signalKeys), "reason", reason)
}

// sendSoftExitToTarget delivers one soft-exit attempt to the pane. A harness
// with a keystroke-free signal exit (harness.Lifecycle.SignalExitKeys non-empty:
// Copilot, Claude Code, Codex) gets those keys sent as signals — a typed slash
// command is silently dropped both mid-turn and whenever a TUI's keypress
// reader wedges, while ctrl-c handling survives both states; every other
// harness gets its exit command typed and submitted.
//
// signalKeys is resolved ONCE per stop by the caller and threaded through, like
// exitCmd and prefixKeys already are: re-resolving per attempt would let a conv
// whose rows vanish mid-stop fall back to the default harness on a retry and
// type "/exit" into the very pane this branch exists to never type into.
func sendSoftExitToTarget(target *lifecycleTarget, signalKeys []string, exitCmd string, prefixKeys []string) error {
	if len(signalKeys) > 0 {
		return injectSignalExitSerializedBy(target.tmuxSession+":0.0", target.paneID, signalKeys)
	}
	return injectSoftExitTextSerializedBy(target.tmuxSession+":0.0", target.paneID, exitCmd, prefixKeys)
}

// softExitPaneScreenTail is capturePaneScreenTail gated to Copilot panes —
// the one harness whose soft-exit failures need screen-state forensics.
// Returns "" for every other harness so their escalation logs stay
// screen-free.
func softExitPaneScreenTail(target *lifecycleTarget) string {
	if harnessForConv(target.convID).Name != harness.CopilotName {
		return ""
	}
	return capturePaneScreenTail(target.paneID)
}

func injectSoftExitTarget(target *lifecycleTarget, exitCmd string, prefixKeys []string, reason string, intentRef *db.SessionExitIntentRef) bool {
	if target == nil {
		return false
	}
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := target.revalidate(); err != nil {
		logLifecycleStopFailure("revalidate", target.paneID, target.sessionID, err)
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
		return false
	}
	// Resolved once for the whole stop, retries included — see
	// sendSoftExitToTarget for why per-attempt re-resolution is unsafe.
	h := harnessForConv(target.convID)
	signalKeys := h.SignalExitKeys()
	copilot := h.Name == harness.CopilotName
	logSoftExitPaneState(target, reason, "pre-send", 1)
	logSoftExitBatchStart(target, reason, 1, signalKeys)
	if err := sendSoftExitToTarget(target, signalKeys, exitCmd, prefixKeys); err != nil {
		logLifecycleStopFailure("send", target.paneID, target.sessionID, err)
		return false
	}
	logSoftExitPaneState(target, reason, "post-send", 1)
	if afterSoftExitTargetSendForTest != nil {
		afterSoftExitTargetSendForTest()
	}
	probe, _ := probeLifecyclePane(target.tmuxSession)
	if probe.state == paneProbeUnknown {
		if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
			return true // confirmed session disappearance; reaper owns attribution
		}
		scheduleUnknownIntentCleanup(target, intentRef)
		return true // preserve intent; bounded cleanup is owned by retry watchdog
	}
	if probe.state == paneProbeDead {
		return true
	}
	if !lifecycleProbeMatchesTarget(probe, target) {
		// The command was already delivered; a post-send identity change means
		// the predecessor transitioned, so preserve intent for callback/reaper
		// attribution and never retry against a successor.
		return true
	}
	scheduleSoftExitRetryTarget(target, signalKeys, copilot, exitCmd, prefixKeys, reason, intentRef)
	return true
}

func lifecycleProbeMatchesTarget(probe lifecyclePaneProbe, target *lifecycleTarget) bool {
	generationMatches := probe.generation == ""
	if target.paneGenerationBound {
		generationMatches = target.generation != "" && probe.generation == target.generation
	}
	return probe.paneID == target.paneID &&
		(target.panePID <= 0 || probe.panePID == target.panePID) && generationMatches
}

// lifecycleSessionAlive reports whether tmux still lists tmuxSession, and
// whether the answer is known at all. Bounded for the same reason as
// probeLifecyclePane: the ladder's signal rungs poll it from a request
// goroutine, and an unbounded call there is an unbounded request.
//
// A list-sessions failure that says NO SERVER EXISTS is a known all-offline
// answer, not an unreadable probe. When the pane being stopped was the last
// live session, its death takes the whole tmux server down with it (tmux's
// default exit-empty on), and from that moment every pane probe and every
// list-sessions errors — treating that as "unknown" left
// waitForLifecycleTargetGone polling blind for the entire escalation deadline
// and turned the fastest possible exit into the slowest observed stop (a
// retire that stalled the full 10 s after the pane had died within 1 s).
// liveTmuxSessionsWithTimeout already reads a dead server as the normal
// all-offline state; this is the same call, made distinguishable from a
// transient failure so the unknown contract stays intact for real faults.
func lifecycleSessionAlive(tmuxSession string) (alive, known bool) {
	out, err := tmuxOutputWithTimeout("list-sessions", "-F", "#{session_name}")
	if err != nil {
		if tmuxServerNotRunning(err) {
			return false, true
		}
		return false, false
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == tmuxSession {
			return true, true
		}
	}
	return false, true
}

// tmuxServerNotRunning reports whether a failed tmux invocation failed because
// no tmux server exists at all — no socket, or a socket nothing is serving.
// Message-matched because that is the only channel tmux reports it on, and the
// spelling differs across versions: "error connecting to <socket> (No such
// file or directory)" (observed on 3.x when the socket is gone) and "no server
// running on <socket>" (when the socket file remains). A timeout is explicitly
// NOT a dead server — a wedged server still owns live panes — and any other
// failure keeps the unknown contract.
func tmuxServerNotRunning(err error) bool {
	if err == nil || errors.Is(err, errTmuxCommandTimeout) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting to")
}

func scheduleUnknownIntentCleanup(target *lifecycleTarget, intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		time.Sleep(unknownIntentCleanupDelay)
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
	})
}

// scheduleUnknownIntentCleanupCurrent is the current-CAS twin of
// scheduleUnknownIntentCleanup for the pid-keyed retry path, whose intent
// ref carries no selected-target binding to key the target CAS on.
func scheduleUnknownIntentCleanupCurrent(intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		time.Sleep(unknownIntentCleanupDelay)
		clearFailedExitIntent(intentRef)
	})
}

var beforeSoftExitTargetRevalidateForTest func()
var beforeSoftExitTargetRetryProbeForTest func(int)
var afterSoftExitTargetSendForTest func()

func clearFailedExitIntentTarget(ref *db.SessionExitIntentRef, tmuxSession string) {
	if ref == nil {
		return
	}
	if _, err := db.ClearSessionExitIntentIfTarget(*ref, tmuxSession); err != nil {
		slog.Warn("exit audit: clear selected lifecycle intent failed", "session", ref.SessionID, "error", err)
	}
}

func scheduleSoftExitRetryTarget(target *lifecycleTarget, signalKeys []string, copilot bool, exitCmd string, prefixKeys []string, reason string, intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		// Attempt-timeline logging is Copilot-only, like the screen captures
		// (logSoftExitPaneState): the ignored-soft-exit forensics they exist
		// for is a Copilot failure mode, and every other harness would just
		// log more for nothing. The signal-exit send path is picked by the
		// caller-resolved signalKeys (sendSoftExitToTarget).
		logAttempts := copilot
		for attempt := 2; attempt <= softExitMaxAttempts; attempt++ {
			select {
			case <-target.softExitSettled:
				// The stop's own wait already verified the outcome; no reason
				// to wake later just to rediscover it (or to keep an operator
				// guessing whether the watchdog is still pending).
				if logAttempts {
					slog.Info("soft-exit retry: watchdog cancelled; stop already settled",
						"session", target.sessionID, "conv", short8(target.convID),
						"attempt", attempt, "reason", reason)
				}
				return
			case <-time.After(softExitRetryDelay):
			}
			if beforeSoftExitTargetRetryProbeForTest != nil {
				beforeSoftExitTargetRetryProbeForTest(attempt)
			}
			probe, err := probeLifecyclePane(target.tmuxSession)
			if err != nil || probe.state == paneProbeUnknown {
				// The first /exit was already delivered (the stop reported
				// soft_stopped), so this is not a stop failure to log: the
				// watchdog mirrors the synchronous unknown branch. A confirmed
				// disappearance is the delivered exit landing — the reaper
				// owns attribution — and anything else keeps the intent
				// through the bounded observer window. An instant clear here
				// would erase a delivered exit's attribution on a transient
				// probe failure.
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					if logAttempts {
						slog.Info("soft-exit retry: session gone before attempt",
							"session", target.sessionID, "conv", short8(target.convID),
							"attempt", attempt, "reason", reason)
					}
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			if probe.state == paneProbeDead {
				if logAttempts {
					slog.Info("soft-exit retry: pane closed before attempt",
						"session", target.sessionID, "conv", short8(target.convID),
						"attempt", attempt, "reason", reason)
				}
				return
			}
			if !lifecycleProbeMatchesTarget(probe, target) {
				// Delivered plus a post-send identity change means the
				// predecessor transitioned; preserve intent for
				// callback/reaper attribution and never retry against a
				// successor (mirrors injectSoftExitTarget).
				if logAttempts {
					slog.Info("soft-exit retry: pane identity changed before attempt",
						"session", target.sessionID, "conv", short8(target.convID),
						"attempt", attempt, "reason", reason)
				}
				return
			}
			logSoftExitPaneState(target, reason, "pre-send", attempt)
			logSoftExitBatchStart(target, reason, attempt, signalKeys)
			if err := sendSoftExitToTarget(target, signalKeys, exitCmd, prefixKeys); err != nil {
				logLifecycleStopFailure("send", target.paneID, target.sessionID, err)
				// The first /exit was already delivered; a failed RE-send must
				// not erase that delivery's attribution. Mirror the unknown
				// branch: a confirmed disappearance is the delivered exit
				// landing (the send often fails precisely because the pane
				// just died) — the reaper owns it — and anything else keeps
				// the intent through the bounded observer window instead of
				// clearing instantly.
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			logSoftExitPaneState(target, reason, "post-send", attempt)
			if attempt == softExitMaxAttempts {
				// Delivery succeeded; retain attribution through the observer window.
				scheduleUnknownIntentCleanup(target, intentRef)
			}
		}
	})
}

func refreshStoppedSessionResumeProvenance(sess *db.SessionRow) error {
	if sess == nil {
		return errors.New("missing live session row")
	}
	physicalCwd, err := livePaneCwd(sess.TmuxSession)
	if err == nil {
		var captured resumeprovenance.Provenance
		captured, err = resumeprovenance.Capture(physicalCwd)
		if err == nil {
			var encoded string
			encoded, err = resumeprovenance.Encode(captured)
			if err == nil {
				if persistErr := db.SetSessionResumeProvenance(sess.ID, encoded); persistErr == nil {
					return nil
				} else {
					err = fmt.Errorf("persist captured provenance: %w", persistErr)
				}
			}
		}
	}
	// Never leave an older apparently valid value after a failed controlled
	// stop capture. This is a single-column replacement, not a hook UPSERT (whose
	// empty-value semantics intentionally preserve existing metadata).
	if clearErr := db.SetSessionResumeProvenance(sess.ID, ""); clearErr != nil {
		return fmt.Errorf("%v; additionally failed to invalidate stale provenance: %w", err, clearErr)
	}
	return err
}

// injectSoftExit injects a harness soft-exit command (Claude Code's
// /exit, Codex's /quit) into convID's live pane and arms a background
// retry. It returns whether the FIRST injection's send-keys succeeded —
// the soft_stopped/error contract callers (stopOneConv, reincarnate)
// already rely on.
//
// Why the retry: a single /exit can be silently lost when the pane's
// input buffer wasn't empty. send-keys appends the command to whatever
// junk was already sitting there (a half-typed line, a stray paste), so
// the trailing Enter submits "<junk>/exit" as one ordinary prompt instead
// of an exit — and the pane keeps running. That submit DOES clear the
// buffer, though, so a second /exit a few seconds later lands on a clean
// input box and takes. scheduleSoftExitRetry re-injects while the SAME
// pane process is still alive.
func injectSoftExit(convID, exitCmd, reason string, intentRef *db.SessionExitIntentRef) bool {
	sess := aliveSessionForConv(convID)
	if sess == nil {
		return false
	}
	if sess.Harness == harness.OpenCodeName {
		target, err := captureLifecycleTarget(sess)
		if err != nil {
			logLifecycleStopFailure("capture OpenCode soft exit", sess.TmuxSession, sess.ID, err)
			return false
		}
		// OpenCode's first send and every bounded retry must use the managed
		// TUI API. Falling through to scheduleSoftExitRetry would reintroduce
		// tmux keystrokes when a reincarnating attach pane lingers.
		return injectOpenCodeSoftExitTarget(target, reason, intentRef)
	}
	// Capture before injection: a responsive pane can exit synchronously after
	// Enter, but that successful exit still owns the lifecycle intent and must
	// remain correlatable by callback/reaper.
	panePID := livePanePID(sess.TmuxSession)
	// Same harness contract as the selected-target path: a soft exit typed at
	// a busy TUI can be silently discarded, so the prefix keys go in front of
	// it here too (nil for every harness that needs none).
	prefixKeys := harnessForConv(convID).Life.SoftExitPrefixKeys()
	softExitTarget := sess.TmuxSession + ":0.0"
	if err := injectSoftExitTextSerializedBy(softExitTarget, softExitTarget, exitCmd, prefixKeys); err != nil {
		slog.Warn("soft-exit inject failed", "error", err,
			"tmux_session", sess.TmuxSession, "conv_id", convID, "reason", reason)
		return false
	}
	slog.Info("soft-exit injected via send-keys",
		"conv_id", convID, "line", exitCmd, "reason", reason,
		"tmux_session", sess.TmuxSession)
	// Capture the pane's live OS pid so the retry can tell THIS process apart
	// from a later one that reused the same tmux name (a resume re-derives the
	// name from the conv-id — see scheduleSoftExitRetry). 0 = couldn't read
	// it; skip the retry rather than risk re-injecting blind.
	if panePID > 0 {
		scheduleSoftExitRetry(convID, sess.TmuxSession, panePID, exitCmd, prefixKeys, reason, intentRef)
	} else if alive, known := lifecycleSessionAlive(sess.TmuxSession); known && !alive {
		// Confirmed session disappearance: the delivered /exit is landing and
		// the reaper owns attribution.
	} else {
		// An unreadable pid is not a stop failure — the /exit above WAS
		// delivered (a responsive pane can even exit synchronously after
		// Enter, taking its pid with it). Instantly clearing here would erase
		// that delivered exit's attribution, so mirror the retry engines'
		// unknown treatment: retain the intent through the bounded observer
		// window instead.
		scheduleUnknownIntentCleanupCurrent(intentRef)
	}
	return true
}

// softExitRetryDelay is how long the background soft-exit retry waits
// before each re-check of a pane it asked to /exit. A package var so flow
// tests can shrink it (SetSoftExitRetryDelayForTest); production keeps it
// short — long enough for a pane that's honouring the exit to close before
// we bother re-injecting, but tight enough that a batch whose press missed
// the harness's re-press window (Claude Code's is ~0.8 s under load) gets
// its next chance quickly instead of riding most of the way to the 10 s
// escalation deadline.
var softExitRetryDelay = 1500 * time.Millisecond

// Unknown cleanup must remain available for the reaper to observe exits when
// hooks and immediate probes are unavailable.
var unknownIntentCleanupDelay = 65 * time.Second

// softExitMaxAttempts bounds the TOTAL number of soft-exit injections per
// stop (the initial one + retries). The first retry recovers an /exit
// lost to input-buffer junk (see injectSoftExit); the remaining margin
// covers an unlucky pane that was mid-render or whose signal-exit presses
// missed the harness's re-press window. Sized so the batches keep coming for
// the whole softExitEscalationDeadline (a batch is ~1.3 s of key spacing plus
// softExitRetryDelay between batches, so attempts start roughly every 2.8 s),
// and capped so a pane that simply will not exit isn't injected forever —
// the retry engines stand down as soon as the pane dies or the stop settles,
// and the escalation ladder owns the force-kill fallback.
const softExitMaxAttempts = 5

// scheduleSoftExitRetry backgrounds the re-injection of exitCmd into the
// pane that injectSoftExit first targeted. It re-injects ONLY while that
// pane is still the SAME live process — keyed on the tmux pane's OS pid
// (panePID), captured at the first injection.
//
// The pid is the load-bearing guard: a resume re-derives the tmux session
// name from the conv-id (sessionResumeArgs → session new -r, no --label →
// name = conv-id[:8]), so a stop → exit → resume cycle can land a brand
// new agent process under the very same tmux name within the retry window.
// Matching on the name alone would then type /exit at that innocent,
// freshly-resumed pane and drop its input. tmux assigns a fresh pane pid
// to every new process, so a changed (or unreadable → 0) pid means "not my
// pane anymore — stop." Re-injection goes straight to the captured target
// (no conv re-resolution) so the pane we validated is the pane we type at.
//
// Runs through goBackground so it outlives the HTTP handler that asked for
// the stop and flow tests can drain it with WaitForBackgroundForTest.
func scheduleSoftExitRetry(convID, tmuxSession string, panePID int, exitCmd string, prefixKeys []string, reason string, intentRef *db.SessionExitIntentRef) {
	target := tmuxSession + ":0.0"
	goBackground(func() {
		for attempt := 2; attempt <= softExitMaxAttempts; attempt++ {
			time.Sleep(softExitRetryDelay)
			if livePanePID(tmuxSession) != panePID {
				return // exited, force-killed, or a different process now owns the name
			}
			slog.Info("soft-exit retry: pane still alive, re-injecting exit",
				"conv_id", convID,
				"tmux_session", tmuxSession,
				"pane_pid", panePID,
				"attempt", attempt,
				"max_attempts", softExitMaxAttempts,
				"reason", reason)
			if err := injectSoftExitTextSerializedBy(target, target, exitCmd, prefixKeys); err != nil {
				slog.Warn("soft-exit retry inject failed",
					"error", err, "tmux_session", tmuxSession, "reason", reason)
				// The first /exit was already delivered; mirror the
				// selected-pane watchdog's re-send-failure treatment: a
				// vanished session leaves attribution to the reaper, anything
				// else retains the intent through the bounded observer window
				// instead of instantly erasing a delivered exit's attribution.
				if alive, known := lifecycleSessionAlive(tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanupCurrent(intentRef)
				return
			}
		}
		if livePanePID(tmuxSession) == panePID {
			// The final re-send was delivered and there is no settle delay
			// before this check, so a pane honoring that /exit is often still
			// alive here. Mirror the target engine's final-attempt treatment:
			// retain attribution through the bounded observer window rather
			// than erasing it moments before the exit lands; a genuinely
			// wedged pane reaches the same cleared end state, just bounded.
			scheduleUnknownIntentCleanupCurrent(intentRef)
		}
	})
}

// livePanePID returns the OS pid tmux reports for tmuxSession's active
// pane, or 0 when the session is gone or the query fails. Unlike the
// sessions-table pid column — written only on the pane's first hook tick,
// so stale right after a resume — tmux knows a pane's pid the instant it
// is created, making this the reliable "is this still the same process?"
// signal the soft-exit retry needs to avoid re-injecting into a resumed
// pane that reused the tmux name.
func livePanePID(tmuxSession string) int {
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", clcommon.ExactTarget(tmuxSession)+":", "#{pane_dead}|#{pane_pid}").Output()
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) != 2 || parts[0] == "1" {
		return 0
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return pid
}

// livePaneCwd returns tmux's view of the live pane process's physical working
// directory. Unlike sessions.cwd, this follows the cwd inode the predecessor
// is actually running in, so retargeting a symlink used at the original launch
// cannot redirect an inherited clone or reincarnation.
func livePaneCwd(tmuxSession string) (string, error) {
	return clcommon.LivePaneCwd(tmuxSession)
}

// handleGroupResume starts a tclaude session for every member that
// has a known conv-id but no live tmux session. Spawns the
// subprocess detached (`tclaude session new -r <conv> -d --global`)
// so each member gets a fresh tmux pane attached to its existing conv.
//
// Members already online are reported as `skipped:already_online`
// — resume is idempotent. The "ensure my team is up" reconciliation
// the TODO design described.
func handleGroupResume(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	affected := make([]string, 0, len(members))
	selected := make(map[string]bool, len(members))
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "snapshot live group members: "+err.Error())
		return
	}
	for _, member := range members {
		online, _ := convLiveStatus(member.ConvID, alive)
		if member.ConvID != "" && !online {
			affected = append(affected, member.ConvID)
			selected[member.ConvID] = true
		}
	}
	caller, ok := requireGroupPermission(w, r, PermGroupsMembersResume, g,
		ActionContext{affectedConvs: affected})
	if !ok {
		return
	}
	authTarget := caller
	requestTrustRoot := caller == "" || hasHumanApprovalContinuation(r, PermGroupsMembersResume, authTarget)
	// Resume every member CONCURRENTLY: each one is a DB/filesystem probe plus
	// a spawned `tclaude session new` subprocess, so a sequential loop cost the
	// SUM of every member's launch. The bound is the same one the dashboard's
	// Power On button already uses (powerOnConcurrency) — a group resume and a
	// scope=group power-on start the identical work, and an unbounded burst of
	// harness launches is what that bound exists to prevent.
	out := groupOpResp{Group: g.Name, Action: "resume", Members: []memberOpResult{}}
	out.Members = mapAgentsConcurrently(members, powerOnConcurrency,
		func(_ int, m *db.AgentGroupMember) (memberOpResult, bool) {
			if m.ConvID != "" && !selected[m.ConvID] {
				return memberOpResult{ConvID: m.ConvID, AgentID: peerAgentID(m.ConvID),
					Title: agent.FreshTitle(m.ConvID), Action: "skipped:already_online"}, true
			}
			res := resumeOneConvLocked(m.ConvID, false, requestTrustRoot)
			confirmResumedConvOnline(m.ConvID, &res)
			res.AgentID = peerAgentID(m.ConvID)
			res.Title = agent.FreshTitle(m.ConvID)
			return res, true
		})
	if out.Members == nil {
		out.Members = []memberOpResult{}
	}
	// The recovery-approval retry stays SEQUENTIAL and after the fan-out: it
	// pops a human prompt per member and can write the response itself, neither
	// of which is safe from a worker goroutine. Only members that actually hit
	// the provenance gate reach it, so the common case never pays for it.
	if !requestTrustRoot && parseAskHumanHeader(r) > 0 {
		for i := range out.Members {
			if out.Members[i].Action != "error:resume_provenance" {
				continue
			}
			convID := out.Members[i].ConvID
			if !requestResumeRecoveryApproval(w, r, PermGroupsMembersResume, authTarget, convID) {
				return
			}
			res := resumeOneConvLocked(convID, false, true)
			res.AgentID = peerAgentID(convID)
			res.Title = agent.FreshTitle(convID)
			// Same verification the fan-out applies — a member rescued through
			// the popup must not be the one path that still reports a resume
			// that never came up.
			confirmResumedConvOnline(convID, &res)
			out.Members[i] = res
		}
	}
	// Re-enable exactly the rhythms a prior emptying retire auto-disabled
	// (JOH-345) — but ONLY once the group has live members again. Retire REMOVES
	// membership, so a resume on a still-empty dormant group can't repopulate it;
	// re-enabling there would just re-create the "firing to nobody" state the
	// auto-disable existed to prevent. Gate on live members so the rhythms come
	// back exactly when the force does (a member re-added / re-spawned before the
	// resume), and stay disabled when the group is still empty. Best-effort
	// tidy-up: a failure is logged and swallowed — the resume itself succeeded.
	if live, err := groupHasLiveMembers(g.ID); err != nil {
		slog.Warn("resume: could not check group liveness for rhythm re-enable", "group", g.Name, "err", err)
	} else if live {
		if n, err := db.ReenableGroupRetiredCronJobs(g.ID); err != nil {
			slog.Warn("resume: could not re-enable group rhythms", "group", g.Name, "err", err)
		} else {
			out.RhythmsReenabled = n
			if n > 0 {
				slog.Info("resume re-enabled group rhythms", "group", g.Name, "reenabled", n)
			}
		}
		// Standing orders auto-pause with the group's rhythms and must come
		// back with them. Left out, a group would resume with orders the
		// operator was told had been paused, delivered to whoever is enrolled
		// next.
		hookHarnesses := standingOrderHookHarnessesForGroupBestEffort(g.ID)
		if n, err := db.ReenableGroupRetiredStandingOrders(g.ID); err != nil {
			slog.Warn("resume: could not re-enable group standing orders", "group", g.Name, "err", err)
		} else if n > 0 {
			slog.Info("resume re-enabled group standing orders", "group", g.Name, "reenabled", n)
			if warning := reconcileStandingOrderHookHarnesses(hookHarnesses); warning != "" {
				w.Header().Set("X-Tclaude-Hook-Warning", warning)
				slog.Warn("resume: standing-order hook reconciliation failed",
					"group", g.Name, "warning", warning)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// confirmResumedConvOnline downgrades a "resumed" result to an error when the
// agent never actually appears as a live tmux session.
//
// A resume launches `tclaude session new -r <conv>` DETACHED, so a successful
// spawn means the child started, not that the agent is up. Everything that can
// go wrong after that — a harness that exits during startup, a sandbox that
// refuses, a cwd that vanished between the pre-check and the launch — was
// previously invisible: the member table said "resumed" for an agent that is
// not running. Reporting the truth is the whole point of a bulk resume the
// operator is watching.
//
// A no-op for every non-"resumed" action, so the skip/error results the resume
// already resolved are passed through untouched.
func confirmResumedConvOnline(convID string, res *memberOpResult) {
	if res.Action != "resumed" {
		return
	}
	if confirmConvOnline(convID, powerOnOnlineGrace) {
		return
	}
	res.Action = "error:not_online"
	res.Detail = joinDetail(res.Detail,
		"resume was launched but the agent did not come online within "+powerOnOnlineGrace.String())
}

// resumeOneConv spawns a detached `tclaude session new -r <conv>`
// for convID if it isn't already online. Returns the per-conv
// result. Shared between the bulk groups.members.resume loop and the
// single-conv agent.resume endpoint.
//
// Idempotent: convs already online come back as
// `skipped:already_online`. Empty conv-ids (placeholder members
// with no conv yet) come back as `skipped:no_conv_id` since we
// have no .jsonl to resume from — those are template-based
// spawns, deferred to a future "groups create --team" pass.
// resolveConvLaunchMetadata resolves how to (re)launch convID without requiring
// process history. Durable conversation facts win; an enrolled agent contributes
// its durable model/effort intent. Legacy session, conv_index, and harness-native
// fallbacks remain for unmanaged/pre-v145 conversations and standalone export.
//
// Shared by resumeOneConv and the clone-based export (JOH-266): the export needs
// the original's cwd to spawn the summary-writer clone into, and a clone works
// offline (it resumes from the .jsonl), so it must not depend on a live session.
func resolveConvLaunchMetadata(convID string) (cwd, effort, model, harnessName string, ok bool) {
	if conversation, err := db.ConversationResumeProfileForConv(convID); err == nil && conversation != nil {
		cwd, harnessName = conversation.Cwd, conversation.Harness
		if agentProfile, aerr := db.AgentRelaunchProfileForConv(convID); aerr == nil && agentProfile != nil {
			if cfg, cerr := durableRelaunchConfigForConv(convID); cerr == nil {
				effort, model = cfg.Effort, cfg.Model
			}
		}
		return cwd, effort, model, harnessName, true
	}
	if rows, _ := db.FindSessionsByConvID(convID); len(rows) > 0 {
		effort, model = inheritedLaunchFlags(rows[0].ID)
		// Relaunch under the harness the conv was last running on — a Codex
		// conv must relaunch as `--harness codex` so session-new resolves its
		// rollout id (resolveResumeConv, JOH-155) instead of looking in
		// ~/.claude/projects. An untagged/claude row leaves it "" (flag omitted).
		return rows[0].Cwd, effort, model, rows[0].Harness, true
	}
	if row, err := db.GetConvIndex(convID); err == nil && row != nil {
		cwd = row.ProjectPath
		if cwd == "" {
			cwd = row.ProjectDir
		}
		return cwd, "", "", row.Harness, true
	}
	if ref, ok := resolveResumeConvFromHarnessStores(convID); ok {
		return ref.ProjectPath, "", "", ref.Harness, true
	}
	return "", "", "", "", false
}

// resumeOneConv resumes convID without recreating a deleted launch dir:
// a resume into a vanished cwd surfaces as `error:missing_cwd` (Detail =
// the path) so the caller can decide whether to recreate it. Thin wrapper
// over resumeOneConvRecreate — the default for the bulk groups.members.resume /
// power-on loops, which must not silently recreate directories en masse.
func resumeOneConv(convID string) memberOpResult {
	return resumeOneConvRecreate(convID, false)
}

// resumeOneConvRecreate is resumeOneConv with an explicit opt-in for the
// deleted-launch-dir case. When recreateMissingDir is true and the recorded
// cwd no longer exists, it recreates that directory empty before relaunching
// — the "recreate the local dir so the agent can start" path the dashboard's
// wake button and `tclaude agent resume --recreate-dir` drive after the human
// confirms. When false, a missing cwd short-circuits to `error:missing_cwd`
// instead of spawning a child that would wedge at startup.
func resumeOneConvRecreate(convID string, recreateMissingDir bool) memberOpResult {
	return resumeOneConvLocked(convID, recreateMissingDir, false)
}

func resumeOneConvWithTrustRoot(convID string, recreateMissingDir bool) memberOpResult {
	return resumeOneConvLocked(convID, recreateMissingDir, true)
}

var resumeLaunchLocks sync.Map         // map[stable actor or unowned conv]*sync.Mutex
var recoveryLaunchCommitLocks sync.Map // map[convID]*sync.Mutex

func resumeLaunchLock(convID string) *sync.Mutex {
	key := strings.TrimSpace(convID)
	// Reincarnation changes the current conversation but not the actor whose
	// process lifecycle and generated policy are being serialized. Resolving
	// every known generation to the stable actor keeps delayed predecessor
	// reaping on the same lock as current-generation reinstate/resume.
	if agentID, err := db.AgentIDForConv(key); err == nil && agentID != "" {
		key = "agent:" + agentID
	}
	lock, _ := resumeLaunchLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func recoveryLaunchCommitLock(convID string) *sync.Mutex {
	lock, _ := recoveryLaunchCommitLocks.LoadOrStore(convID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

type effectiveSandboxChangedError struct{ err error }

func (e *effectiveSandboxChangedError) Error() string { return e.err.Error() }
func (e *effectiveSandboxChangedError) Unwrap() error { return e.err }

func writeEffectiveSandboxLoadError(w http.ResponseWriter, err error) {
	var changed *effectiveSandboxChangedError
	if errors.As(err, &changed) {
		writeError(w, http.StatusConflict, "sandbox_profile_changed", changed.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "io", "load effective sandbox snapshot: "+err.Error())
}

// sandboxWriteProofDir returns the concrete directory that controls whether a
// frozen write path can materialize. Existing roots prove themselves; missing
// roots prove their nearest existing ancestor. Thus an agent cannot arrange
// for an unproved path to appear between the challenge and harness launch.
func sandboxWriteProofDir(path string) (string, error) {
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("sandbox profile write proof path %q is not a directory", path)
			}
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}

func resumeOneConvLocked(convID string, recreateMissingDir, trustRoot bool) memberOpResult {
	// Flag the whole attempt — including time queued on the launch lock — so
	// dashboards render the multi-second wake as "waking" rather than a dead
	// offline dot. See waking.go.
	defer markConvWaking(convID)()
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	return resumeOneConvClaimedUnderLaunchLock(convID, recreateMissingDir, trustRoot)
}

func resumeOneConvClaimedUnderLaunchLock(convID string, recreateMissingDir, trustRoot bool) memberOpResult {
	if isConvOnline(convID) {
		return resumeOneConvUnderLaunchLock(convID, recreateMissingDir, trustRoot, nil)
	}
	now := time.Now()
	manualClaim, err := db.BeginManualAgentRecovery(convID, now)
	if err != nil {
		slog.Warn("resume: claim pending automatic recovery failed", "conv", short8(convID), "error", err)
	} else if manualClaim != nil {
		_ = db.RecordAgentRecoveryAudit(*manualClaim, db.AuditVerbAgentRecoveryManual, "manual_resume", now)
	}
	res := resumeOneConvUnderLaunchLock(convID, recreateMissingDir, trustRoot, manualClaim)
	if manualClaim != nil && res.Action != "resumed" && res.Action != "skipped:already_online" {
		if cancelled, cancelErr := db.CancelAgentRecoveryGeneration(*manualClaim, "manual_resume_failed", time.Now()); cancelErr != nil {
			slog.Warn("resume: cancel failed manual recovery claim", "conv", short8(convID), "error", cancelErr)
		} else if cancelled {
			if recovery, _ := db.AgentRecoveryForConv(convID); recovery != nil {
				_ = db.RecordAgentRecoveryAudit(*recovery, db.AuditVerbAgentRecoveryCancelled, "manual_resume_failed", time.Now())
			}
		}
	}
	return res
}

// resumeOneConvWithCodexRollbackLocked proves and changes one stopped
// generation under the same lock that excludes concurrent stop/resume. It
// never signals a PID: the eligible runtime is already terminal, and its
// retained numeric PID is not process identity after exit.
var setAgentCodexAppServerSelectionForConv = db.SetAgentCodexAppServerSelectionForConv
var unregisterCodexNativePermissionProfile = session.UnregisterCodexNativePermissionProfile
var restoreCodexNativePermissionProfile = session.RestoreCodexNativePermissionProfile

func resumeOneConvWithCodexRollbackLocked(convID string, recreateMissingDir, trustRoot bool) memberOpResult {
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	res := memberOpResult{ConvID: convID}
	if isConvOnline(convID) {
		res.Action = "skipped:already_online"
		res.Detail = "Codex drive unchanged; compatibility rollback requires the intended generation to be stopped"
		return res
	}
	profile, err := db.AgentRelaunchProfileForConv(convID)
	if err != nil || profile == nil {
		res.Action = "error:codex_drive_rollback"
		res.Detail = "load durable Codex drive: " + firstNonEmpty(errorString(err), "missing stable-agent relaunch profile")
		return res
	}
	if profile.CodexAppServer == nil || !*profile.CodexAppServer {
		return resumeOneConvClaimedUnderLaunchLock(convID, recreateMissingDir, trustRoot)
	}
	runtime, err := db.GetCodexAppServerRuntimeByConvID(convID)
	if err != nil || runtime == nil ||
		(runtime.State != db.CodexAppServerDead && runtime.State != db.CodexAppServerUnavailable) {
		res.Action = "error:codex_drive_rollback"
		res.Detail = "Codex drive unchanged; no terminal app-server runtime proves the intended stopped generation"
		return res
	}
	launch, err := db.LoadSession(runtime.LaunchID)
	if err != nil || launch == nil || launch.ConvID != convID || launch.Harness != harness.CodexName ||
		launch.Status != session.StatusExited || launch.CreatedAt.Before(runtime.CreatedAt) {
		res.Action = "error:codex_drive_rollback"
		res.Detail = "Codex drive unchanged; recorded runtime does not match the intended stopped launch generation"
		return res
	}
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		res.Action = "error:codex_drive_rollback"
		res.Detail = "Codex drive unchanged; list conversation launch generations: " + err.Error()
		return res
	}
	for _, candidate := range candidates {
		if candidate.CreatedAt.After(launch.CreatedAt) {
			res.Action = "error:codex_drive_rollback"
			res.Detail = "Codex drive unchanged; a newer conversation launch supersedes the recorded app-server generation"
			return res
		}
	}
	nativeProfile, err := db.GetCodexNativePermissionProfile(runtime.Generation)
	if err != nil {
		res.Action = "error:codex_drive_rollback"
		res.Detail = "load native Codex permission profile for compatibility rollback: " + err.Error()
		return res
	}
	if err := unregisterCodexNativePermissionProfile(runtime.Generation); err != nil {
		res.Action = "error:codex_drive_rollback"
		res.Detail = "Codex drive unchanged; native permission-profile cleanup is pending: " + err.Error()
		return res
	}
	source := "explicit --send-keys compatibility rollback"
	if err := setAgentCodexAppServerSelectionForConv(convID, false, source); err != nil {
		res.Action = "error:codex_drive_rollback"
		res.Detail = "persist Codex compatibility rollback: " + err.Error()
		if nativeProfile != nil {
			if restoreErr := restoreCodexNativePermissionProfile(runtime.Generation,
				nativeProfile.ProfileName, nativeProfile.ProfileTOML); restoreErr != nil {
				res.Detail += "; native permission profile restore will retry at restart: " + restoreErr.Error()
			}
		}
		return res
	}
	removeCodexAppServerGeneration(runtime.SocketPath)
	return resumeOneConvClaimedUnderLaunchLock(convID, recreateMissingDir, trustRoot)
}
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// resumeOneConvUnderLaunchLock is the production resume path after its
// per-conversation launch mutex has been acquired. Automatic recovery calls it
// directly so it does not cancel its own durable lease; every manual wrapper
// above cancels a pending automatic attempt first under the same mutex.
func resumeOneConvUnderLaunchLock(convID string, recreateMissingDir, trustRoot bool, recoveryClaim *db.AgentRecovery) memberOpResult {
	res := memberOpResult{ConvID: convID}
	if isConvOnline(convID) {
		res.Action = "skipped:already_online"
		return res
	}
	if state, err := db.AgentState(convID); err != nil {
		res.Action = "error"
		res.Detail = "agent-state lookup: " + err.Error()
		return res
	} else if state == db.AgentStateRetired {
		res.Action = "skipped:not_active_agent"
		res.Detail = "state: " + state
		return res
	}
	if convID == "" {
		res.Action = "skipped:no_conv_id"
		res.Detail = "placeholder member (no conv yet) — Phase B will support template-based fresh spawn"
		return res
	}
	// Resume authority is durable agent + conversation state. A human trust root
	// may establish a missing conversation profile from the real harness store;
	// a predecessor session row is never required or consulted here.
	conversationProfile, profileErr := db.ConversationResumeProfileForConv(convID)
	if profileErr != nil {
		res.Action = "error"
		res.Detail = "load durable conversation resume profile: " + profileErr.Error()
		return res
	}
	if conversationProfile == nil {
		if !trustRoot {
			res.Action = "error:resume_provenance"
			res.Detail = "no durable conversation resume profile for this agent; a direct human resume or --ask-human approval is required to recover it from the real harness conversation"
			return res
		}
		_, recoverErr := recoverMissingConversationResumeProfile(convID, recreateMissingDir)
		if recoverErr != nil {
			var missing *missingResumeAnchorCwdError
			if errors.As(recoverErr, &missing) {
				res.Action = "error:missing_cwd"
				res.Detail = missing.path
				return res
			}
			res.Action = "error"
			res.Detail = recoverErr.Error()
			return res
		}
	}
	launchConfig, configErr := durableRelaunchConfigForConv(convID)
	if configErr != nil {
		res.Action = "error:resume_profile"
		res.Detail = configErr.Error()
		return res
	}
	expected, provenanceErr := resumeprovenance.Decode(launchConfig.ResumeProvenance)
	cwd := launchConfig.Cwd
	if provenanceErr == nil {
		// Never follow the old launch spelling again. It may contain a symlink
		// that now targets a different directory; the durable physical path is
		// the only unattended resume candidate.
		cwd = expected.Cwd.Path
	}
	if cwd == "" || !filepath.IsAbs(cwd) {
		res.Action = "error:resume_provenance"
		res.Detail = "resume provenance unusable: no absolute launch directory is available; ask the human to recreate this agent"
		return res
	}
	// The recorded launch dir may have been deleted since the agent last ran.
	// Spawning `session new -r` into a non-existent cwd leaves the child
	// wedged at startup with no clear error, so detect it here. Without an
	// explicit recreate opt-in, report `error:missing_cwd` (Detail = the path)
	// so the caller can offer to recreate the dir empty; with the opt-in,
	// MkdirAll the empty dir and continue so the agent can start.
	missingCwd, dirErr := launchDirMissing(cwd)
	if dirErr != nil {
		res.Action = "error"
		res.Detail = dirErr.Error()
		return res
	}
	if missingCwd {
		if !recreateMissingDir {
			res.Action = "error:missing_cwd"
			res.Detail = cwd
			return res
		}
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			res.Action = "error"
			res.Detail = "failed to recreate launch directory " + cwd + ": " + err.Error()
			return res
		}
		slog.Info("resume: recreated missing launch directory before relaunch",
			"conv", short8(convID), "cwd", cwd)
	}
	observed, observeErr := resumeprovenance.Capture(cwd)
	if provenanceErr == nil && observeErr == nil {
		provenanceErr = resumeprovenance.Compare(expected, observed)
	}
	if provenanceErr == nil && observeErr != nil {
		provenanceErr = observeErr
	}
	if provenanceErr != nil {
		if !trustRoot {
			res.Action = "error:resume_provenance"
			res.Detail = "resume provenance verification failed: " + provenanceErr.Error() +
				"; a direct human resume or --ask-human approval is required to trust the current target identity"
			return res
		}
		// A direct human or an actually approved one-shot recovery is the only
		// trust root allowed to bless the current physical identity. Persist it
		// before launch so the recovery is explicit and durable.
		if observeErr != nil {
			res.Action = "error"
			res.Detail = "human recovery could not capture current resume provenance: " + observeErr.Error()
			return res
		}
		encoded, err := resumeprovenance.Encode(observed)
		if err != nil {
			res.Action = "error"
			res.Detail = "human recovery could not encode current resume provenance: " + err.Error()
			return res
		}
		if err := db.SetConversationResumeProvenance(convID, encoded); err != nil {
			res.Action = "error"
			res.Detail = "human recovery could not persist current resume provenance: " + err.Error()
			return res
		}
		expected = observed
		slog.Info("resume: human trust root recovered target provenance",
			"conv", short8(convID), "cwd", expected.Cwd.Path)
	}
	// Re-arm Remote Access if the conv's own persisted best-known state was on
	// (JOH-261). Read BEFORE relaunch: resume keeps the conv-id but mints a NEW
	// session row defaulting remote_control=0, so the freshest row reads OFF the
	// moment the new pane reports in — the armed flag lives on the old/dead row,
	// which is still the most-recent until then.
	remoteControl := launchConfig.RemoteControl
	sshLaunchKey := generateSpawnLabel()
	resumePolicy, snapshotErr := resolveResumeSandboxPolicy(
		convID, launchConfig.SSHWorkaround, sshLaunchKey)
	if snapshotErr != nil {
		res.Action = "error"
		res.Detail = "sandbox_profile_changed: " + snapshotErr.Error()
		return res
	}
	if resumePolicy != nil && resumePolicy.Snapshot != nil {
		refreshed, refreshErr := finalizeCodexSSHWorkaroundForRelaunch(
			*resumePolicy.Snapshot, launchConfig.SSHWorkaround)
		if refreshErr != nil {
			res.Action = "error"
			res.Detail = "prepare Codex SSH workaround: " + refreshErr.Error()
			if cleanupErr := cleanupUncommittedResumeSandboxPolicy(resumePolicy); cleanupErr != nil {
				res.Detail += "; remove unused agent-owned directories: " + cleanupErr.Error()
			}
			return res
		}
		resumePolicy.Snapshot = &refreshed
	}
	var effectiveSandbox *sandboxpolicy.Snapshot
	if resumePolicy != nil {
		effectiveSandbox = resumePolicy.Snapshot
	}
	if effectiveSandbox != nil {
		validated, err := sandboxpolicy.RevalidateSnapshot(*effectiveSandbox)
		if err != nil {
			res.Action = "error"
			res.Detail = "sandbox_profile_changed: " + err.Error()
			return res
		}
		effectiveSandbox = &validated
	}
	stableEffectiveSandbox := effectiveSandbox
	// Relaunch never re-engages the experimental guardian (auto-review is an
	// explicit fresh-spawn opt-in, not persisted per-conv), so AutoReview stays false.
	// Preserve the mode this conversation was launched under; the harness
	// default would silently drop an enforced `sandbox on` posture on resume.
	relaunchSandbox := launchConfig.Sandbox
	harnessName := launchConfig.Harness
	relaunchSandboxImplementation := launchConfig.activeSandboxImplementation()
	if launchConfig.TemporaryHarnessBuiltinMode {
		effectiveSandbox = temporarySandboxLaunchSnapshot(harnessName, stableEffectiveSandbox)
	}
	if session.CodexNativeRegistryApplicable(launchConfig.CodexAppServer, harnessName,
		relaunchSandbox, relaunchSandboxImplementation) {
		if err := codexNativeRegistryReadiness(); err != nil {
			res.Action = "error:" + codexNativeRegistryErrorCode(err)
			res.Detail = err.Error()
			return res
		}
	}
	// The harness's own sandbox configuration is re-verified on every relaunch,
	// never replayed from the recorded posture. For a harness tclaude can
	// switch off at launch the recorded mode IS the posture; for one configured
	// out of band it is only a record of what was true at spawn time, and an
	// operator can have enabled the harness's own wall since. Replaying the
	// record would resume an agent under two stacked boundaries while still
	// claiming one.
	if fail := sandboxImplementationPostureFailure(
		harnessName, relaunchSandboxImplementation); fail != nil {
		res.Action = "error"
		res.Detail = "sandbox_posture_changed: " + fail.Msg
		return res
	}
	if fail := sandboxProfileCapabilityFailure(
		harnessName,
		relaunchSandbox,
		effectiveSandbox,
		relaunchSandboxImplementation,
	); fail != nil {
		res.Action = "error"
		res.Detail = "sandbox_profile_changed: " + fail.Msg
		return res
	}
	// Re-asked on every relaunch rather than trusted from the spawn that
	// admitted it. The recorded drive is durable but the PROFILE is not: an
	// operator can close network access between two launches, and resuming an
	// API-driven agent into a private network namespace would bring back a pane
	// whose channel can never connect. Refusing names the reason while the agent
	// is still recoverable by widening the profile or dropping the drive.
	if fail := copilotAPILoopbackFailure(
		launchConfig.CopilotAPI, effectiveSandbox, relaunchSandboxImplementation,
	); fail != nil {
		res.Action = "error"
		res.Detail = "sandbox_profile_changed: " + fail.Msg
		return res
	}
	if _, fail := planSandboxProfileAccessForLaunch(
		harnessName, relaunchSandbox, effectiveSandbox, relaunchSandboxImplementation,
		session.ModelTransportLaunchContext{
			Model: launchConfig.Model,
			Cwd:   cwd,
		},
		false,
	); fail != nil {
		res.Action = "error"
		res.Detail = "sandbox_profile_changed: " + fail.Msg
		return res
	}
	if effectiveSandbox != nil {
		for _, notice := range effectiveSandbox.Effective.AccessNotices {
			res.Warnings = append(res.Warnings, notice.Detail)
		}
	}
	// Derive repository grants only from the verified durable identity. Calling
	// git rev-parse here would follow a mutable .git file a second time and could
	// turn a post-verification retarget into new write authority.
	codexGitCommonDirPinned := spawnUsesPinnedGitCommonDir(
		harnessName, relaunchSandbox, relaunchSandboxImplementation)
	codexGitCommonDir := ""
	gitDir := ""
	var gitWriteDirs []string
	if codexGitCommonDirPinned && expected.RepositoryState == resumeprovenance.RepositoryGit {
		codexGitCommonDir = expected.Repository.CommonDir.Path
		gitDir = expected.Repository.Dir.Path
		home, err := os.UserHomeDir()
		if err != nil {
			res.Action = "error"
			res.Detail = "resolve home for verified repository grants: " + err.Error()
			return res
		}
		gitWriteDirs = harness.GitWorktreeWriteDirsForIdentity(codexGitCommonDir, gitDir, home)
		// Exact metadata dirs are redundant descendants of the ordinary grant
		// roots, but carrying them makes the child guard bind both provenance
		// identities instead of merely their writable ancestor.
		gitWriteDirs = appendUniqueDirs(gitWriteDirs, codexGitCommonDir, gitDir)
	}

	// Close provenance-check→session-new races with daemon-owned markers. The
	// child checks cwd relative to the inode tmux actually entered and checks
	// every extra root by canonical pathname immediately before exec. Profile
	// write roots participate only when concrete for this launch; missing
	// read/write rules stay inactive in session new.
	rawPins := appendUniqueDirs([]string{cwd}, gitWriteDirs...)
	if effectiveSandbox != nil {
		for _, grant := range effectiveSandbox.Effective.Filesystem {
			if grant.Access != sandboxpolicy.AccessWrite {
				continue
			}
			info, err := os.Lstat(grant.Path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				res.Action = "error"
				res.Detail = fmt.Sprintf("sandbox_profile_changed: write root %s is no longer a canonical directory", grant.Path)
				return res
			}
			rawPins = appendUniqueDirs(rawPins, grant.Path)
		}
	}
	pinMapping, pinToken, pinDirs, cleanupPins, pinErr := pinInheritedLaunchDirs(rawPins)
	if pinErr != nil {
		res.Action = "error"
		res.Detail = "pin verified resume directories: " + pinErr.Error()
		return res
	}
	defer cleanupPins()
	if resolved := pinMapping[cwd]; resolved != "" {
		cwd = resolved
	}
	for i, dir := range gitWriteDirs {
		if resolved := pinMapping[dir]; resolved != "" {
			gitWriteDirs[i] = resolved
		}
	}
	if resolved := pinMapping[codexGitCommonDir]; resolved != "" {
		codexGitCommonDir = resolved
	}
	// Re-observe after the markers exist. If a pathname was replaced between
	// provenance verification and pin creation, this comparison catches it;
	// if it changes later, the child-side marker guard catches it.
	postPin, err := resumeprovenance.Capture(cwd)
	if err != nil {
		res.Action = "error"
		res.Detail = "resume identity changed while pinning launch: " + err.Error()
		return res
	}
	if err := resumeprovenance.Compare(expected, postPin); err != nil {
		res.Action = "error"
		res.Detail = "resume identity changed while pinning launch: " + err.Error()
		return res
	}
	if fail := reassertDirWriteProof(pinDirs); fail != nil {
		res.Action = "error"
		res.Detail = fail.Msg
		return res
	}
	var grantFail *spawnFailure
	gitWriteDirs, grantFail = canonicalizeRepositoryWriteDirs(gitWriteDirs, pinDirs, pinToken)
	if grantFail != nil {
		res.Action = "error"
		res.Detail = grantFail.Msg
		return res
	}
	var persistedAgentID string
	if effectiveSandbox != nil && !launchConfig.TemporaryHarnessBuiltinMode {
		agentID, err := db.AgentIDForConv(convID)
		if err != nil {
			res.Action = "error"
			res.Detail = "record refreshed sandbox snapshot: " + err.Error()
			return res
		}
		if agentID != "" {
			if err := db.SetAgentEffectiveSandboxConfig(agentID, effectiveSandbox); err != nil {
				res.Action = "error"
				res.Detail = "record refreshed sandbox snapshot: " + err.Error()
				return res
			}
			persistedAgentID = agentID
		}
	}
	approval, autoReview := launchConfig.Approval, launchConfig.AutoReview
	if recoveryClaim != nil {
		commitLock := recoveryLaunchCommitLock(convID)
		commitLock.Lock()
		defer commitLock.Unlock()
		current, err := db.AgentRecoveryClaimCurrent(*recoveryClaim)
		if err != nil {
			res.Action = "error"
			res.Detail = "revalidate recovery claim before spawn: " + err.Error()
			return res
		}
		if !current {
			res.Action = "skipped:recovery_cancelled"
			return res
		}
	}
	var fastModeAtLaunch *bool
	if harnessName == harness.CodexName {
		fastModeAtLaunch = codexFastModeAtLaunch(launchConfig.FastMode, launchConfig.CodexStateRoot)
	}
	if err := SpawnDetachedTclaudeResume(clcommon.SpawnArgs{
		EffectiveSandbox:           effectiveSandbox,
		AgentID:                    persistedAgentID,
		ConvID:                     convID,
		Cwd:                        cwd,
		CwdWriteProof:              pinToken,
		CodexGitCommonDir:          codexGitCommonDir,
		CodexGitCommonDirPinned:    codexGitCommonDirPinned,
		GitWorktreeWriteDirs:       gitWriteDirs,
		GitWorktreeWriteDirsPinned: true,
		Effort:                     launchConfig.Effort,
		Model:                      launchConfig.Model,
		Harness:                    harnessName,
		Sandbox:                    relaunchSandbox,
		SandboxImplementation:      relaunchSandboxImplementation,
		SandboxChosenBy:            launchConfig.HarnessBuiltinModeSource,
		Approval:                   approval,
		AutoReview:                 autoReview,
		AskUserQuestionTimeout:     launchConfig.AskUserQuestionTimeout,
		ToolGovernance:             launchConfig.ToolGovernance,
		RemoteControl:              remoteControl,
		AutoMemory:                 launchConfig.AutoMemory,
		ContextFeatures:            launchConfig.ContextFeatures,
		AutoCompactWindow:          launchConfig.AutoCompactWindow,
		ContextWindowMax:           launchConfig.ContextWindowMax,
		CopilotAPI:                 launchConfig.CopilotAPI,
		CodexAppServer:             launchConfig.CodexAppServer,
		CodexStateRoot:             launchConfig.CodexStateRoot,
		FastMode:                   launchConfig.FastMode,
	}); err != nil {
		res.Action = "error"
		res.Detail = "spawn: " + err.Error()
		if !launchConfig.TemporaryHarnessBuiltinMode && resumePolicy != nil && resumePolicy.Previous != nil && effectiveSandbox != nil {
			if _, cleanupErr := removeSupersededMaterializedAgentDirectories(*effectiveSandbox, *resumePolicy.Previous); cleanupErr != nil {
				res.Detail += "; remove unused agent-owned directories: " + cleanupErr.Error()
			}
		}
		if persistedAgentID != "" {
			var previous *sandboxpolicy.Snapshot
			if resumePolicy != nil {
				previous = resumePolicy.Previous
			}
			if restoreErr := db.SetAgentEffectiveSandboxConfig(persistedAgentID, previous); restoreErr != nil {
				res.Detail += "; restore previous sandbox snapshot: " + restoreErr.Error()
			}
		}
	} else if launchConfig.CodexAppServer && !awaitCodexAppServerReady(convID) {
		failedTmux := ""
		if failedSession := pickAliveSession(convID); failedSession != nil {
			failedTmux = failedSession.TmuxSession
		}
		stopFailedCodexAppServerLaunch(convID, "", failedTmux)
		res.Action = "error"
		res.Detail = "the explicitly selected Codex app-server did not become ready; the failed resumed pane was stopped"
		if !launchConfig.TemporaryHarnessBuiltinMode && resumePolicy != nil && resumePolicy.Previous != nil && effectiveSandbox != nil {
			if _, cleanupErr := removeSupersededMaterializedAgentDirectories(*effectiveSandbox, *resumePolicy.Previous); cleanupErr != nil {
				res.Detail += "; remove unused agent-owned directories: " + cleanupErr.Error()
			}
		}
		if persistedAgentID != "" {
			var previous *sandboxpolicy.Snapshot
			if resumePolicy != nil {
				previous = resumePolicy.Previous
			}
			if restoreErr := db.SetAgentEffectiveSandboxConfig(persistedAgentID, previous); restoreErr != nil {
				res.Detail += "; restore previous sandbox snapshot: " + restoreErr.Error()
			}
		}
	} else {
		res.Action = "resumed"
		if harnessName == harness.CodexName {
			persistCodexFastModeAtLaunch(convID, fastModeAtLaunch)
		}
		if !launchConfig.TemporaryHarnessBuiltinMode && resumePolicy != nil && resumePolicy.Previous != nil && effectiveSandbox != nil {
			if _, cleanupErr := removeSupersededMaterializedAgentDirectories(*resumePolicy.Previous, *effectiveSandbox); cleanupErr != nil {
				res.Detail = "resumed; remove superseded agent-owned directories: " + cleanupErr.Error()
			}
		}
		// Tag the fresh row's best-known state ON once it comes online. The
		// --remote-control launch flag (threaded above) already re-armed CC;
		// this only re-records tclaude's best-known state. Backgrounded so the
		// bulk groups-resume loop isn't serialised on the online-wait.
		if remoteControl {
			goBackground(func() { armRemoteControlAfterResume(convID) })
		}
	}
	return res
}

type missingResumeAnchorCwdError struct{ path string }

func (e *missingResumeAnchorCwdError) Error() string { return "missing launch directory: " + e.path }

// recoverMissingConversationResumeProfile is the compatibility bridge for
// managed agents that predate durable conversation profiles. It is called at a
// real human trust boundary. The harness conversation must still exist; a
// stale conv_index row alone is not enough. Once resolved, recovery captures
// the same physical cwd/repository identity as an ordinary trusted launch and
// persists it on the conversation before returning to the normal path.
func recoverMissingConversationResumeProfile(convID string, recreateMissingDir bool) (*db.ConversationResumeProfile, error) {
	cwd, harnessName, err := resolveMissingSessionResumeTarget(convID)
	if err != nil {
		return nil, err
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("no trustworthy recovery target for agent %s: harness conversation has no absolute launch directory", short8(convID))
	}
	missing, err := launchDirMissing(cwd)
	if err != nil {
		return nil, fmt.Errorf("inspect recovered launch directory: %w", err)
	}
	if missing {
		if !recreateMissingDir {
			return nil, &missingResumeAnchorCwdError{path: cwd}
		}
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			return nil, fmt.Errorf("failed to recreate launch directory %s: %w", cwd, err)
		}
	}
	observed, err := resumeprovenance.Capture(cwd)
	if err != nil {
		return nil, fmt.Errorf("human recovery could not capture current resume provenance: %w", err)
	}
	encoded, err := resumeprovenance.Encode(observed)
	if err != nil {
		return nil, fmt.Errorf("human recovery could not encode current resume provenance: %w", err)
	}
	empty := ""
	// Recovery has no recorded approval input to reproduce, so it records none:
	// a blank posture re-resolves under current config at every relaunch (see
	// harness.ReconstructApprovalPolicy). Writing a value here would be
	// reconstruction inventing an input — and the value it used to write for
	// Codex (`untrusted`) is exactly the one the rule forbids, since it both
	// prompts on a detached pane and is denied the in-sandbox lineage bit the
	// agent needs to delegate. TCL-990.
	approval := ""
	no := false
	sshWorkaround := harnessName == harness.CodexName
	zero := int64(0)
	legacy := db.AgentRelaunchProfile{
		Version:            db.RelaunchProfileVersion,
		HarnessBuiltinMode: &empty, ApprovalPolicy: &approval,
		ApprovalAutoReview: &no, ModelID: &empty, Effort: &empty,
		ContextWindowSize: &zero, AskUserQuestionTimeout: &empty,
		RemoteControl: &no, AutoMemory: &no, AutoCompactWindow: &empty,
		SSHWorkaround: &sshWorkaround,
	}
	profile := db.ConversationResumeProfile{
		Version: db.RelaunchProfileVersion, Harness: harnessName, Cwd: observed.Cwd.Path, ResumeProvenance: encoded,
		FallbackRelaunch: &legacy,
	}
	if err := db.SetConversationResumeProfile(convID, profile); err != nil {
		return nil, fmt.Errorf("human recovery could not persist a conversation resume profile: %w", err)
	}
	// TCL-636's compatibility path historically materialized a blank legacy
	// session anchor, whose launch readers reconstructed the same explicit
	// baseline below. Persist that baseline on the stable agent now so the human
	// recovery remains one-shot and later session pruning cannot erase it.
	if agentProfile, err := db.AgentRelaunchProfileForConv(convID); err != nil {
		return nil, fmt.Errorf("human recovery could not inspect agent relaunch profile: %w", err)
	} else if agentProfile == nil {
		agentID, err := db.AgentIDForConv(convID)
		if err != nil {
			return nil, fmt.Errorf("human recovery could not resolve stable agent: %w", err)
		}
		if agentID != "" {
			if err := db.SetAgentRelaunchProfile(agentID, legacy); err != nil {
				return nil, fmt.Errorf("human recovery could not persist agent relaunch profile: %w", err)
			}
		}
	}
	slog.Info("resume: human trust root recovered missing conversation profile",
		"conv", short8(convID), "harness", harnessName, "cwd", observed.Cwd.Path)
	return &profile, nil
}

// resolveMissingSessionResumeTarget first honors the harness tag and cwd cached
// in conv_index, then falls back to each harness's native resolver
// (needed for Codex rollouts that were never indexed). Every candidate is
// checked through ConvStore.Exists so a stale cache row cannot be blessed.
func resolveMissingSessionResumeTarget(convID string) (string, string, error) {
	var lookupErrors []string
	attempted := map[string]bool{}
	if row, err := db.GetConvIndex(convID); err == nil && row != nil {
		fallbackCwd := strings.TrimSpace(row.ProjectPath)
		if fallbackCwd == "" {
			fallbackCwd = strings.TrimSpace(row.ProjectDir)
		}
		if h, resolveErr := harness.Resolve(strings.TrimSpace(row.Harness)); resolveErr != nil {
			lookupErrors = append(lookupErrors, resolveErr.Error())
		} else if !h.SupportsConvs() {
			lookupErrors = append(lookupErrors, fmt.Sprintf("harness %q has no conversation store", h.Name))
		} else {
			attempted[h.Name] = true
			cwd, found, probeErr := verifiedMissingSessionResumeTarget(h, convID, fallbackCwd)
			if probeErr != nil {
				lookupErrors = append(lookupErrors, fmt.Sprintf("%s conversation lookup: %v", h.Name, probeErr))
			} else if found {
				return cwd, h.Name, nil
			}
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Defer cache read failures to the native-store probes below and retain
		// the diagnostic if none can resolve the conversation.
		lookupErrors = append(lookupErrors, "conversation index lookup: "+err.Error())
	}

	for _, name := range harness.Names() {
		h, ok := harness.Get(name)
		if !ok || !h.SupportsConvs() || attempted[name] {
			continue
		}
		cwd, found, err := verifiedMissingSessionResumeTarget(h, convID, "")
		if err != nil {
			lookupErrors = append(lookupErrors, fmt.Sprintf("%s conversation lookup: %v", name, err))
			continue
		}
		if found {
			return cwd, h.Name, nil
		}
	}
	if len(lookupErrors) > 0 {
		return "", "", fmt.Errorf("no trustworthy recovery target for agent %s; conversation lookup failed: %s",
			short8(convID), strings.Join(lookupErrors, "; "))
	}
	return "", "", fmt.Errorf("no trustworthy recovery target for agent %s; the harness conversation no longer exists", short8(convID))
}

func verifiedMissingSessionResumeTarget(h *harness.Harness, convID, fallbackCwd string) (string, bool, error) {
	ref, err := h.Convs.Resolve(convID, "", true)
	if err != nil {
		return "", false, err
	}
	if ref == nil || ref.ConvID != convID {
		return "", false, nil
	}
	// Claude's resolver is conv_index-backed and can see rows tagged for
	// another harness. Do not let that cross-harness cache view preempt the
	// actual owner's native resolver.
	if owner := strings.TrimSpace(ref.Harness); owner != "" && owner != h.Name {
		return "", false, nil
	}
	cwd := strings.TrimSpace(ref.ProjectPath)
	if cwd == "" {
		cwd = strings.TrimSpace(fallbackCwd)
	}
	exists, err := h.Convs.Exists(convID, cwd)
	if err != nil {
		return "", false, err
	}
	return cwd, exists, nil
}

func resolveResumeConvFromHarnessStores(convID string) (*harness.ConvRef, bool) {
	for _, name := range harness.Names() {
		h, ok := harness.Get(name)
		if !ok || !h.SupportsConvs() {
			continue
		}
		ref, err := h.Convs.Resolve(convID, "", true)
		if err != nil {
			slog.Warn("resume: harness conversation lookup failed",
				"conv", convID, "harness", name, "error", err)
			continue
		}
		if ref != nil {
			return ref, true
		}
	}
	return nil, false
}

// groupRetireResp is the response shape of the bulk groups.members.retire
// endpoint. It mirrors groupOpResp (so the CLI renders the per-member
// table identically to stop/resume) but carries an extra Warnings list
// — retire can leave a group ownerless when it demotes an owner, and
// the human needs to hear about that.
type groupRetireResp struct {
	Group   string           `json:"group"`
	Action  string           `json:"action"`
	Members []memberOpResult `json:"members"`
	// RhythmsDisabled is the number of group-target cron jobs (template-seeded
	// rhythms) auto-disabled because this retire left the group with no live
	// members (JOH-345). Omitted when zero — a partial retire, or one that left
	// live members behind, disables nothing.
	RhythmsDisabled int      `json:"rhythms_disabled,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// groupHasLiveMembers reports whether groupID still has at least one member
// backed by a live (current-generation, active) agent conv. Placeholder members
// (no conv yet) and retired/superseded convs do not count. Used to decide
// whether a retire has emptied the group of live recipients.
func groupHasLiveMembers(groupID int64) (bool, error) {
	members, err := db.ListAgentGroupMembers(groupID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.ConvID == "" {
			continue
		}
		live, err := db.IsLiveAgentConv(m.ConvID)
		if err != nil {
			return false, err
		}
		if live {
			return true, nil
		}
	}
	return false, nil
}

// disableGroupRhythmsIfEmptied disables the group's template-seeded rhythm cron
// jobs when a retire has left it with no live members (JOH-345). Non-destructive
// (the jobs stay visible + reversible in the Cron tab, marked 'group-retired'),
// and a later `groups resume` re-enables exactly these. Returns the number
// disabled (0 when live members remain). Best-effort tidy-up: a failure is
// logged and swallowed — the retire itself already succeeded, and a stray rhythm
// firing at a dormant group merely no-ops (fireCronGroupJob resolves an empty
// roster gracefully).
func disableGroupRhythmsIfEmptied(g *db.AgentGroup) int {
	live, err := groupHasLiveMembers(g.ID)
	if err != nil {
		slog.Warn("retire: could not check group liveness for rhythm cleanup",
			"group", g.Name, "err", err)
		return 0
	}
	if live {
		return 0
	}
	// Standing orders share the rhythms' auto-pause semantics, so an emptied
	// group must pause both. Only rows tclaude itself paused carry the marker,
	// so a hand-disabled order is untouched and a later resume restores exactly
	// what this paused.
	hookHarnesses := standingOrderHookHarnessesForGroupBestEffort(g.ID)
	if so, err := db.DisableGroupTargetStandingOrdersForRetire(g.ID); err != nil {
		slog.Warn("retire: could not disable group standing orders",
			"group", g.Name, "err", err)
	} else if so > 0 {
		slog.Info("retire emptied group — disabled its standing orders",
			"group", g.Name, "disabled", so)
		if warning := reconcileStandingOrderHookHarnesses(hookHarnesses); warning != "" {
			slog.Warn("retire: standing-order hook reconciliation failed",
				"group", g.Name, "warning", warning)
		}
	}
	n, err := db.DisableGroupTargetCronJobsForRetire(g.ID)
	if err != nil {
		slog.Warn("retire: could not disable group rhythms",
			"group", g.Name, "err", err)
		return 0
	}
	if n > 0 {
		slog.Info("retire emptied group — disabled its rhythms",
			"group", g.Name, "disabled", n)
	}
	return n
}

// handleGroupRetire retires the active-agent members of the group in
// one shot — the bulk parallel of `agent retire`, completing the
// groups.members.stop / groups.members.resume lifecycle family (which until now had no
// retire sibling). It is the SO_PEERCRED /v1 surface; the cookie-authed
// dashboard route (dashboardGroupRetire) shares the same core.
//
// "Retire" demotes an agent to a plain conversation: retireAgentConv
// drops every group membership (this group and any others the member
// belongs to), revokes every permission and sudo grant, and flips the
// enrollment bit. The conversation itself — .jsonl, history, conv_index
// row — is left completely intact and reinstatable; this is the
// non-destructive bulk cleanup, never `agent delete`. Unless
// ?shutdown=0, a retired member's running tmux pane is also soft-exited
// (stopOneConv, soft only — never a force-kill), since a retired
// agent's idle process is almost never wanted.
//
// ?status= optionally restricts the cohort to members of a given live
// status (e.g. status=idle, status=offline, or a comma list) — the
// "retire idle agents in <group>" palette command. Absent / "all" =
// every member, the legacy behaviour. See parseRetireStatusFilter.
//
// ?delete_worktree=1 additionally removes each retired member's git
// worktree and force-deletes its branch — the bulk parallel of the
// single-agent retire option. It defaults OFF (the failsafe in
// retireShouldDeleteWorktree); the same safety rules apply per member
// (the main repo and worktrees shared with a surviving agent are kept,
// removal waits until the member's pane exits).
//
// Permission: groups.members.retire (not in the global defaults — retiring
// agents is a sensitive cleanup the human normally drives; the slug
// delegates it to a trusted coordinator). Gated with
// requireGroupPermission, like the other bulk group endpoints
// (stop/resume/spawn): owning THIS group raises the slug by default
// (the owner-state bypass), so an owner can run its own team's
// lifecycle without an explicit grant. The bypass fills only the
// permUndecided gap — an explicit deny override is always
// authoritative and suppresses it.
func handleGroupRetire(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	filter, ferr := parseRetireStatusFilter(r.URL.Query().Get("status"))
	if ferr != nil {
		writeError(w, http.StatusBadRequest, "status", ferr.Error())
		return
	}
	var selected map[string]struct{}
	var affected []string
	if filter != nil {
		affected = []string{}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		alive, _ := session.LiveTmuxSessions()
		selected = map[string]struct{}{}
		for _, member := range members {
			online, status := convLiveStatus(member.ConvID, alive)
			if filter.matches(online, status) {
				selected[member.ConvID] = struct{}{}
				affected = append(affected, member.ConvID)
			}
		}
	}
	ctx := ActionContext{}
	if filter != nil {
		ctx.affectedConvs = affected
	}
	caller, ok := requireGroupPermission(w, r, PermGroupsMembersRetire, g, ctx)
	if !ok {
		return
	}
	out, err := bulkRetireGroupMembers(g, caller,
		strings.TrimSpace(r.URL.Query().Get("reason")),
		retireShouldShutdown(r), retireShouldDeleteWorktree(r), nil, selected,
		auditRequestEventID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// If this retire left the group with no live members, disable its
	// template-seeded rhythms so they stop firing to nobody (JOH-345).
	out.RhythmsDisabled = disableGroupRhythmsIfEmptied(g)
	writeJSON(w, http.StatusOK, out)
}

// bulkRetireGroupConcurrency bounds how many members the bulk retire
// works on at once. Retire is I/O-bound per member (a .jsonl title read,
// the SQLite demotion writes, a tmux soft-exit), so a handful of workers
// overlaps that latency without stampeding tmux or the single SQLite
// writer (WAL serialises writes; busy_timeout absorbs contention).
const bulkRetireGroupConcurrency = 8

// bulkRetireGroupMembers is the shared core behind both retire surfaces:
// the SO_PEERCRED /v1/groups/{name}/retire endpoint (agent callers,
// slug-gated via handleGroupRetire) and the cookie-authed
// /api/groups/{name}/retire dashboard route (the human, via
// dashboardGroupRetire). It retires every member of g that passes the
// status filter and returns the per-member table plus any
// ownerless-group warnings.
//
// caller is the requester's own conv ("" for the human): it is always
// skipped (skipped:self), since the brief is "retire OTHER agents in the
// group" and an agent demoting itself mid-request would revoke its own
// grants and /exit its own pane out from under the request it is
// serving.
//
// Cohort selection is one of two mutually-exclusive mechanisms:
//   - selected != nil — an EXPLICIT set of conv-ids: retire exactly the
//     members whose conv-id is in the set, regardless of their current
//     live status. This is the dashboard preview path — the human ticked
//     a list, and the BE must retire precisely that list and nothing it
//     re-derived (so an agent that flips status between preview and
//     submit is still retired iff it was on the previewed list). A member
//     not in the set is omitted from the response; a conv in the set that
//     is not (or no longer) a member of g is simply never reached, so it
//     is silently ignored — the membership table is authoritative, the
//     set only narrows it.
//   - selected == nil — the status FILTER path: filter==nil retires every
//     member (the legacy behaviour); a non-nil filter restricts the
//     cohort to members whose live status matches, re-resolved server-side
//     from live tmux. Non-matching members are omitted from the response.
//
// When selected is non-nil the status filter is ignored entirely (the
// human's explicit pick wins — there is nothing to re-resolve).
//
// deleteWorktree (the batch parallel of the single-agent retire option)
// additionally removes each retired member's git worktree and force-
// deletes its branch. It is per-member and reuses the single-agent
// machinery (resolveRetireWorktree before the shutdown, then
// scheduleRetireWorktreeCleanup), so the same safety rules hold: the main
// repo and worktrees a SURVIVING agent still works in are kept, and the
// removal waits until the member's pane exits (its cwd is the worktree).
// A worktree shared by two members BOTH retired in this batch is
// conservatively kept: every member's view is resolved from the same
// pre-mutation cohort snapshot, while all co-sharers are still active. The
// safe failure mode — never a yank from under a sibling whose pane is still
// draining. The per-member outcome rides back in memberOpResult.Worktree.
//
// Per-member outcomes (memberOpResult.Action):
//   - retired                  — demoted (Detail summarises what changed)
//   - skipped:self             — the caller's own conv; never self-retire
//   - skipped:no_conv_id       — a placeholder member with no conv yet
//   - skipped:not_active_agent — already retired / never an agent
//   - error                    — the retire failed (Detail has the cause)
//
// The per-member work runs in parallel, bounded by
// bulkRetireGroupConcurrency. Each worker writes its result and the
// owner-groups it touched into its own pre-sized slot, so there is no
// contended shared state; the ownerless set is merged sequentially once
// every worker has settled — checked once at the end so a bulk retire
// that demotes a member-owner warns about the now-ownerless group,
// matching the single-agent cleanup path.
func bulkRetireGroupMembers(g *db.AgentGroup, caller, reason string, shutdown, deleteWorktree bool, filter retireStatusFilter, selected map[string]struct{}, relatedEventID string) (groupRetireResp, error) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return groupRetireResp{}, err
	}
	by := enrollmentActor(caller)

	// Normalize an explicit selection to canonical conv-ids. The group preview
	// deliberately submits canonical conv_id values, but a selector may also be
	// an agt_ id, a live conv-id, or a UUID-shaped reference to a dangling
	// agent. resolveCleanupConv maps agt_/conv to the conv-id the member
	// universe (m.ConvID) is keyed on, and KEEPS a raw UUID-shaped fallback
	// so a dangling agent — actor row broken/unresolvable — stays retirable
	// by its conv-id (the recovery escape hatch D2's cold review pinned,
	// PR #628). An entry that resolves to nothing AND isn't UUID-shaped is
	// dropped: it can match no member, and the explicit set only ever
	// NARROWS the authoritative membership table (never widens it). Runs
	// only on the dashboard's explicit-selection path; the /v1 status-filter
	// path passes selected==nil and is untouched.
	if selected != nil {
		canon := make(map[string]struct{}, len(selected))
		for sel := range selected {
			if convID, ok := resolveCleanupConv(sel); ok {
				canon[convID] = struct{}{}
			}
		}
		selected = canon
	}

	// The status filter needs live tmux state; fetch it once
	// (snapshot-shaped) and share the read-only map across workers.
	// Skipped entirely when no filter is active OR an explicit selection
	// is supplied (the explicit path never consults live status), so the
	// legacy "retire everyone" path and the preview path keep their cost.
	var alive map[string]struct{}
	if filter != nil && selected == nil {
		alive, _ = session.LiveTmuxSessions()
	}

	// Snapshot every member's worktree view before any worker demotes or stops
	// anyone. The workers run concurrently; resolving inside a worker would
	// make a co-shared worktree's safety decision depend on whether its sibling
	// had already retired/exited. Pre-resolution keeps the whole batch on one
	// stable pre-mutation view and preserves the conservative co-share rule.
	retireWorktrees := map[string]agentWorktreeView{}
	if deleteWorktree {
		claimSnapshot := captureAgentWorktreeClaims()
		for _, m := range members {
			if m.ConvID != "" {
				retireWorktrees[m.ConvID] = claimSnapshot.resolve(
					m.ConvID, map[string]bool{m.ConvID: true})
			}
		}
	}

	results := make([]*memberOpResult, len(members))
	ownerGroupsPer := make([][]int64, len(members))

	forEachAgentConcurrently(members, bulkRetireGroupConcurrency, func(i int, m *db.AgentGroupMember) {
		res := memberOpResult{AgentID: peerAgentID(m.ConvID), ConvID: m.ConvID, Title: agent.FreshTitle(m.ConvID)}
		switch {
		case m.ConvID == "":
			res.Action = "skipped:no_conv_id"
			res.Detail = "placeholder member (no conv yet)"
		case caller != "" && sameActor(m.ConvID, caller):
			// Match on the stable actor (JOH-323): the caller never
			// retires itself, including a predecessor generation of
			// itself that still sits in the roster.
			res.Action = "skipped:self"
			res.Detail = "the caller never retires itself"
		default:
			switch {
			case selected != nil:
				if _, ok := selected[m.ConvID]; !ok {
					return // not in the explicit selection — omit
				}
			case filter != nil:
				online, status := convLiveStatus(m.ConvID, alive)
				if !filter.matches(online, status) {
					return // filtered out — omit from the response
				}
			}
			res, ownerGroupsPer[i] = retireGroupMember(
				m.ConvID, by, reason, shutdown, deleteWorktree, retireWorktrees[m.ConvID], res, relatedEventID)
		}
		results[i] = &res
	})

	out := groupRetireResp{Group: g.Name, Action: "retire", Members: []memberOpResult{}}
	ownerless := map[int64]bool{}
	for i := range members {
		if results[i] != nil {
			out.Members = append(out.Members, *results[i])
		}
		for _, gid := range ownerGroupsPer[i] {
			ownerless[gid] = true
		}
	}
	out.Warnings = warnOwnerlessGroups(ownerless)
	return out, nil
}

// retireGroupMember retires one member as part of the bulk retire. It
// enforces the "active agent only" guard (a no-op on a conv that was
// never an agent or is already retired comes back as
// skipped:not_active_agent), runs the shared retireAgentConv demotion,
// and — when shutdown is requested — soft-exits the member's pane.
// Returns the populated result plus the ids of any groups whose owner
// roster the demotion touched (for the caller's ownerless-warning
// merge); res arrives pre-seeded with ConvID + Title so the table stays
// consistent across every branch.
//
// When deleteWorktree is set the member's git worktree+branch is also
// cleaned up, reusing the single-agent retire machinery: the worktree is
// resolved for the whole cohort BEFORE any worker starts — defensive ordering
// that keeps shared-worktree decisions stable across concurrent demotion and
// shutdown. scheduleRetireWorktreeCleanup then runs it — inline when the
// member is already offline, deferred to a waiter when a /exit is in flight,
// kept when no shutdown was asked for. The per-member plan rides back on
// res.Worktree, and its one-line note is folded into Detail so the CLI/table
// row says what happened.
func retireGroupMember(convID, by, reason string, shutdown, deleteWorktree bool, wt agentWorktreeView, res memberOpResult, relatedEventID string) (memberOpResult, []int64) {
	// Gate on the LIVE generation (current conv of an active actor), not just
	// "active": retire acts on the actor, so a superseded predecessor handle
	// would demote the live agent. Members always come through as the current
	// generation, so this is a defensive guard for the invariant.
	live, serr := db.IsLiveAgentConv(convID)
	if serr != nil {
		res.Action = "error"
		res.Detail = "agent-state lookup: " + serr.Error()
		return res, nil
	}
	if !live {
		state, _ := db.AgentState(convID)
		res.Action = "skipped:not_active_agent"
		res.Detail = "state: " + state
		return res, nil
	}
	outcome, ownerGroups, rerr := retireAgentConv(convID, by, reason)
	if rerr != nil {
		res.Action = "error"
		res.Detail = rerr.Error()
		return res, nil
	}
	res.Action = "retired"
	res.Detail = summarizeRetireOutcome(outcome)

	td := finishRetiredConv(convID, shutdown, deleteWorktree, wt, relatedEventID)
	res.TmuxSes = td.Stop.TmuxSes
	res.Worktree = td.Worktree
	for _, note := range td.Notes {
		res.Detail = joinDetail(res.Detail, note)
	}
	return res, ownerGroups
}

// retireTeardown is what the post-demotion half of a retire did: the stop
// result (zero-valued when no shutdown was asked for), the worktree plan (nil
// when ?delete_worktree was off) and the human-readable notes each step
// produced, in order.
type retireTeardown struct {
	Stop     memberOpResult
	Worktree *retireWorktreePlan
	Notes    []string
}

// finishRetiredConv is THE post-demotion half of a retire, shared by all three
// retire surfaces: the single-agent /v1/agent/{selector}/retire endpoint, the
// bulk group retire (retireGroupMember) and the dashboard's fleet cleanup tier.
// Each of those used to inline its own slightly-different copy of the same
// three steps, which is how they drifted apart on which stop primitive they
// used and which notes they surfaced.
//
// The order is load-bearing:
//
//  1. Stop the pane, and WAIT for the process to actually be gone. Waiting is
//     the default for every stop that has work behind it: the response then
//     means what it says, and steps 2 and 3 get to do their job inline instead
//     of promising it. The bulk surfaces make this affordable by running their
//     members concurrently, so the cost is the slowest agent rather than the
//     sum.
//  2. Agent-owned directory cleanup.
//  3. Worktree + branch cleanup, from the view the CALLER resolved before any
//     demotion or shutdown (wt) — resolving it here would read a world the
//     stop above already changed.
//
// Both cleanups keep their own deferred fallback for the case the wait cannot
// resolve: a pane that survives the whole escalation ladder (softExitStuck) is
// still alive when they run, so they schedule themselves behind a background
// exit-waiter and report to the human via the Messages tab if that promise
// cannot be kept. That is why retire does NOT use stopBeforePurge's refusal —
// unlike a purge, it has somewhere safe to land.
//
// The demotion itself (retireAgentConv) stays with the caller: only it knows
// the precondition to enforce (live-generation guard, require_offline) and the
// actor to attribute the change to.
func finishRetiredConv(convID string, shutdown, deleteWorktree bool, wt agentWorktreeView, relatedEventID string) retireTeardown {
	var td retireTeardown
	if shutdown {
		td.Stop, _ = stopOneConvAndWait(convID, false /* soft exit */, db.AgentExitActionRetire, relatedEventID, 0)
		switch td.Stop.Action {
		case "soft_stopped":
			// Harness-agnostic wording on purpose: the group-retire copy of this
			// used to say "/exit sent", which is simply untrue for a Codex pane
			// (/quit) or an OpenCode one (a managed TUI call, no keystroke).
			note := "session soft-stopped"
			if td.Stop.Detail != "" {
				note += " (" + td.Stop.Detail + ")"
			}
			td.Notes = append(td.Notes, note)
		case "error":
			td.Notes = append(td.Notes, "session shutdown failed: "+td.Stop.Detail)
		}
	}
	cleanupAgentDirectoriesAfterRetire(convID, shutdown)
	cleanupRetiredCodexNativeProfiles(convID)
	if deleteWorktree {
		plan := scheduleRetireWorktreeCleanup(convID, wt, shutdown)
		td.Worktree = &plan
		// Every plan gets its note, "none" ("no worktree") included: the note
		// only ever appears when the caller explicitly asked for
		// delete_worktree, and there "this one had nothing to delete" is an
		// answer, not noise. Keeps the group-retire member table byte-identical
		// to what it produced before the three copies were merged.
		if plan.Detail != "" {
			td.Notes = append(td.Notes, plan.Detail)
		}
	}
	return td
}

// retireStatusFilter is the optional ?status= filter for bulk retire.
// nil = match every member (the legacy "retire everyone" behaviour). A
// non-nil set restricts the retire to members whose live status
// normalizes to one of its tokens:
//
//   - "offline"  → no live tmux session (the pane is dead)
//   - "idle"     → online, last hook status == idle
//   - "working"  → online, working
//   - "awaiting" → online, awaiting_permission OR awaiting_input
//   - "error"    → online, error
//
// The dashboard palette uses "idle" and "offline"; the rest fall out of
// the same normalization for free and are reachable via the CLI
// --status flag.
type retireStatusFilter map[string]bool

// validRetireStatuses is the closed vocabulary of ?status= tokens — the
// outputs of normalizeMemberStatus. Kept in sync with that switch: an
// unknown token is rejected rather than silently matching nobody.
var validRetireStatuses = map[string]bool{
	"offline": true, "idle": true, "working": true, "awaiting": true, "error": true,
}

// parseRetireStatusFilter reads the ?status= query value into a filter.
// Empty / absent / "all" yield a nil filter (match everything). Tokens
// are comma-separated, lower-cased and trimmed. An unknown token is an
// error, not a silent no-op: without this a typo (?status=offlien) would
// match nobody and return 200 with an empty member list, indistinguish-
// able from "the group has no offline agents". Callers surface it as 400.
func parseRetireStatusFilter(raw string) (retireStatusFilter, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "all" {
		return nil, nil
	}
	set := retireStatusFilter{}
	for tok := range strings.SplitSeq(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if tok == "all" {
			return nil, nil // "all" anywhere in the list = no filter
		}
		if !validRetireStatuses[tok] {
			return nil, fmt.Errorf("unknown status %q (valid: all, offline, idle, working, awaiting, error)", tok)
		}
		set[tok] = true
	}
	if len(set) == 0 {
		return nil, nil
	}
	return set, nil
}

// matches reports whether a member with the given liveness + hook status
// passes the filter. A nil filter matches everything.
func (f retireStatusFilter) matches(online bool, status string) bool {
	if f == nil {
		return true
	}
	return f[normalizeMemberStatus(online, status)]
}

// normalizeMemberStatus folds a member's (online, hook-status) pair into
// the single token the retire filter keys on — the SAME mapping the
// dashboard snapshot renders, so a "retire idle agents" palette command
// retires exactly the rows the human sees marked idle. An offline member
// (no live session) is "offline" regardless of its frozen hook status;
// an online member reports its hook status, with the two awaiting_*
// variants collapsed to "awaiting".
func normalizeMemberStatus(online bool, status string) string {
	if !online {
		return "offline"
	}
	switch status {
	case session.StatusAwaitingPermission, session.StatusAwaitingInput:
		return "awaiting"
	default:
		return status
	}
}

// convLiveStatus resolves a conv's (online, hook-status) from the
// pre-fetched alive set — the snapshot-shaped twin of isConvOnlineIn /
// stateForConvIn used by the retire status filter. online is true when
// any of the conv's session rows names a live tmux session; status is
// that live row's hook status (empty for an offline conv).
func convLiveStatus(convID string, alive map[string]struct{}) (bool, string) {
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false, ""
	}
	for _, r := range rows {
		if r.TmuxSession == "" {
			continue
		}
		if _, ok := alive[r.TmuxSession]; ok {
			return true, r.Status
		}
	}
	return false, ""
}

// summarizeRetireOutcome renders the parts of a retireConvOutcome the
// bulk table cares about into a compact, human-readable Detail cell:
// how many groups the member left and how many grants were revoked. An
// outcome that changed nothing beyond the enrollment bit yields "".
func summarizeRetireOutcome(o retireConvOutcome) string {
	var parts []string
	if n := len(o.GroupsLeft); n > 0 {
		parts = append(parts, fmt.Sprintf("left %d group(s)", n))
	}
	if revoked := o.PermsRevoked + o.SudoRevoked; revoked > 0 {
		parts = append(parts, fmt.Sprintf("revoked %d grant(s)", revoked))
	}
	return strings.Join(parts, ", ")
}

// joinDetail appends extra to a Detail string with ", " glue, treating
// an empty base as "no prefix".
func joinDetail(base, extra string) string {
	if base == "" {
		return extra
	}
	return base + ", " + extra
}

// handleAgentStop stops a single conv's tmux session. Sibling of
// the bulk groups.members.stop. Auth: agent.stop slug OR caller is owner of
// a group containing target. Routed via /v1/agent/{selector}/stop;
// `?force=1` switches to tmux kill-session.
func handleAgentStop(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentStop, targetConv)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	action := db.AgentExitActionStop
	if force {
		action = db.AgentExitActionForceStop
	}
	res := stopOneConvWithIntent(targetConv, force, action, auditRequestEventID(r))
	resp := map[string]any{
		"conv_id":      res.ConvID,
		"action":       res.Action,
		"tmux_session": res.TmuxSes,
	}
	if res.Detail != "" {
		resp["detail"] = res.Detail
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	status := http.StatusOK
	if res.Action == "error" {
		status = http.StatusInternalServerError
		setAuditDetail(r, res.Detail)
		// The standard {"error": ...} envelope rides along with the
		// lifecycle result fields so DaemonError.Msg carries the bounded
		// failure detail — without it the CLI can only print the bare
		// "agentd returned 500" status line.
		resp["error"] = "stop failed: " + res.Detail
		resp["code"] = "stop_failed"
	}
	writeJSON(w, status, resp)
}

// handleAgentDelete permanently removes an agent: every row in every
// agent / conv / session table that references the conv-id, plus the
// .jsonl file and the ~/.claude/session-env/<conv-id> token. Sibling
// of stop / resume but DESTRUCTIVE — there is no undo. Auth:
// agent.delete slug OR caller is owner of a group containing target.
// Default-grant policy explicitly excludes agent.delete (humans
// only, unless someone explicitly grants).
//
// Refuses when the target's tmux session is alive — the human must
// stop it first via `tclaude agent stop`. `?force=1` kills the tmux
// session inline before deleting (mirrors the stop endpoint's force
// switch). Refusing-by-default avoids racing the live agent's writes
// to its own .jsonl while we're tearing it down.
//
// Returns the per-table deletion counts so the human can see scope.
func handleAgentDelete(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentDelete, targetConv)
	if !ok {
		return
	}
	// Self-delete prevention. An agent shouldn't be able to wipe its
	// own conv mid-turn — the daemon's own request context is keyed
	// off the caller's conv-id, and the cleanup goroutine would race
	// the response write. Humans (caller == "") can always proceed.
	//
	// Match on the stable actor (JOH-323): DeleteAgentAllGenerations below
	// sweeps EVERY generation of the actor, so deleting any generation of
	// oneself wipes the live request conv too and hits the same race. The
	// selector already resolves a predecessor forward to the head, so today
	// targetConv == caller for a self-delete; sameActor only ever widens
	// this guard to the same actor's generations — a genuinely different
	// agent still differs and a peer/owner delete is unaffected.
	if caller != "" && sameActor(caller, targetConv) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"cannot delete self via this endpoint; use `tclaude conv rm` from a human shell or have a peer/owner do it")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	// ?force=1 goes on to purge immediately, so it must WAIT for the pane
	// process to actually be gone — the teardown below unlinks the .jsonl and
	// every row the agent might still be writing to. The non-force path
	// deliberately does NOT wait: its contract is to refuse (409) and let the
	// caller retry, so blocking it for the escalation window would buy nothing.
	var stopRes memberOpResult
	if force {
		var stopErr error
		stopRes, stopErr = stopBeforePurge(targetConv, auditRequestEventID(r))
		if stopErr != nil {
			// The ladder is exhausted and the agent is still up. Purging now
			// would leave a live orphan writing into rows and a .jsonl that no
			// longer exist.
			writeError(w, http.StatusConflict, "alive", stopErr.Error())
			return
		}
		// Deliberately NOT gated on stopRes.Action here. The force path is
		// soft-first now, and a soft exit whose injection failed reports
		// Action="error" and then falls through to the ladder — which routinely
		// goes on to kill the pane successfully. stopBeforePurge returning nil
		// IS the authority that the process is gone; failing the delete on the
		// injection's Action would 500 on an agent we just killed and leave its
		// rows behind.
	} else {
		stopRes = stopOneConv(targetConv, force)
		if stopRes.Action == "error" {
			writeError(w, http.StatusInternalServerError, "stop", stopRes.Detail)
			return
		}
	}
	// If the conv is alive but force wasn't passed, stopOneConv
	// returned `soft_stopped` (sent /exit) — the tmux pane may still
	// be in the process of dying. Refuse without ?force=1 to avoid
	// racing the live agent's writes during teardown.
	if !force && stopRes.Action == "soft_stopped" {
		writeError(w, http.StatusConflict, "alive",
			"target had a live tmux session; sent /exit. Re-run with ?force=1 to delete now, or wait for the pane to exit and retry.")
		return
	}

	// Comprehensive cleanup: DB purge + filesystem + sync tombstone +
	// session-env. Single source of truth shared with the dashboard
	// `DELETE /api/agents/...` path and `tclaude conv rm`. Actor-aware
	// (JOH-26 PR3d): when targetConv is an agent's head generation, this
	// also sweeps every predecessor generation's rows + .jsonl, so a
	// multi-generation actor's delete leaves nothing orphaned. The selector
	// resolves a predecessor forward to the head before it reaches here, so
	// `targetConv` is the head in the agent-delete case.
	if _, err := removeAgentDirectoriesForConv(targetConv); err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"delete agent-owned directories: "+err.Error())
		return
	}
	counts, swept, err := conv.DeleteAgentAllGenerations(targetConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"delete failed: "+err.Error())
		return
	}

	resp := map[string]any{
		"conv_id":   targetConv,
		"action":    "deleted",
		"db_counts": counts,
	}
	// Surface the full generation set reaped when more than the named conv
	// went (a multi-generation actor) — otherwise it's just [targetConv].
	if len(swept) > 1 {
		resp["generations"] = swept
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	if stopRes.Action != "skipped:already_offline" {
		resp["pre_stop"] = stopRes.Action
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAgentResume resumes a single conv into a fresh detached
// tmux session. Sibling of the bulk groups.members.resume. Auth:
// agent.resume slug OR caller is owner of a group containing
// target. Routed via /v1/agent/{selector}/resume.
func handleAgentResume(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentResume, targetConv)
	if !ok {
		return
	}
	trustRoot := caller == "" || hasHumanApprovalContinuation(r, PermAgentResume, targetConv)
	sendKeys := r.URL.Query().Get("send_keys") == "1"
	if sendKeys && !trustRoot {
		if parseAskHumanHeader(r) <= 0 || !requestCodexDriveRollbackApproval(w, r, PermAgentResume, targetConv, targetConv) {
			if parseAskHumanHeader(r) <= 0 {
				writeError(w, http.StatusForbidden, "codex_drive_change_restricted",
					"moving a Codex agent from its selected app-server drive to send-keys requires a direct human resume or actual --ask-human approval")
			}
			return
		}
		trustRoot = true
	}
	// Recreating a missing path is a daemon-side mkdir with the human's
	// filesystem authority. Keep that opt-in human-only; an agent cannot prove
	// write access inside a directory that does not exist.
	recreate := r.URL.Query().Get("recreate") == "1"
	if recreate && !trustRoot {
		if parseAskHumanHeader(r) <= 0 || !requestResumeRecoveryApproval(w, r, PermAgentResume, targetConv, targetConv) {
			if parseAskHumanHeader(r) <= 0 {
				writeError(w, http.StatusForbidden, "recreate_dir_restricted",
					"agent-initiated resume may not recreate a missing launch directory without actual human approval; ask the human to run resume --recreate-dir or retry with --ask-human")
			}
			return
		}
		trustRoot = true
	}
	// ?recreate=1 opts into recreating a deleted launch dir empty before the
	// relaunch (the CLI's `--recreate-dir`, the dashboard's confirm-and-retry).
	// Absent it, a vanished cwd comes back as `error:missing_cwd` so the caller
	// can decide.
	var res memberOpResult
	if sendKeys {
		res = resumeOneConvWithCodexRollbackLocked(targetConv, recreate, trustRoot)
	} else {
		res = resumeOneConvLocked(targetConv, recreate, trustRoot)
	}
	if res.Action == "error:resume_provenance" && !trustRoot && parseAskHumanHeader(r) > 0 {
		if !requestResumeRecoveryApproval(w, r, PermAgentResume, targetConv, targetConv) {
			return
		}
		res = resumeOneConvLocked(targetConv, recreate, true)
	}
	resp := map[string]any{
		"conv_id": res.ConvID,
		"action":  res.Action,
	}
	if res.Detail != "" {
		resp["detail"] = res.Detail
	}
	if len(res.Warnings) > 0 {
		resp["warnings"] = res.Warnings
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// requestResumeRecoveryApproval is used only after ordinary authorization has
// succeeded but durable target integrity cannot be established. It creates a
// real, audited access request; approval marks this exact in-flight operation
// as a human trust root, while deny/timeout returns before provenance or the
// stopped target is changed.
func requestResumeRecoveryApproval(w http.ResponseWriter, r *http.Request, perm, authTarget, targetConv string) bool {
	timeout := parseAskHumanHeader(r)
	if timeout <= 0 {
		return false
	}
	if popupBaseURL == "" {
		writeError(w, http.StatusForbidden, "permission",
			"no popup base URL configured; resume provenance recovery cannot be approved")
		return false
	}
	p := peerFromContext(r.Context())
	if classify(p) != classAgent {
		return false
	}
	callerTitle := ""
	if row := agent.FreshConvRowResolved(p.ConvID); row != nil {
		callerTitle = agent.DisplayTitle(row)
	}
	targetTitle := ""
	if row := agent.FreshConvRowResolved(targetConv); row != nil {
		targetTitle = agent.DisplayTitle(row)
	}
	targetGroup, _, _ := extractApprovalTargets(r, "")
	req := &approvalRequest{
		id:              newApprovalID(),
		perm:            perm,
		convID:          p.ConvID,
		convTitle:       callerTitle,
		method:          r.Method,
		path:            r.URL.Path,
		rawQuery:        r.URL.RawQuery,
		bodyPreview:     "Recapture and trust the stopped target's current physical working-directory and Git identity for this resume.",
		bodyLabel:       "Resume provenance recovery",
		targetGroup:     targetGroup,
		targetConvID:    targetConv,
		targetConvTitle: targetTitle,
		createdAt:       time.Now(),
		timeout:         timeout,
		decision:        make(chan approvalOutcome, 1),
		extend:          make(chan time.Duration, 1),
	}
	if requestHumanApproval(req, popupBaseURL) {
		markHumanApprovalContinuation(r, perm, authTarget)
		return true
	}
	writeError(w, http.StatusForbidden, "permission",
		fmt.Sprintf("human declined or timed out after %s while recovering resume provenance for target %s",
			timeout, short8(targetConv)))
	return false
}

func requestCodexDriveRollbackApproval(w http.ResponseWriter, r *http.Request, perm, authTarget, targetConv string) bool {
	timeout := parseAskHumanHeader(r)
	if timeout <= 0 {
		return false
	}
	if popupBaseURL == "" {
		writeError(w, http.StatusForbidden, "permission",
			"no popup base URL configured; Codex drive rollback cannot be approved")
		return false
	}
	p := peerFromContext(r.Context())
	if classify(p) != classAgent {
		return false
	}
	callerTitle := ""
	if row := agent.FreshConvRowResolved(p.ConvID); row != nil {
		callerTitle = agent.DisplayTitle(row)
	}
	targetTitle := ""
	if row := agent.FreshConvRowResolved(targetConv); row != nil {
		targetTitle = agent.DisplayTitle(row)
	}
	targetGroup, _, _ := extractApprovalTargets(r, "")
	req := &approvalRequest{
		id: newApprovalID(), perm: perm, convID: p.ConvID, convTitle: callerTitle,
		method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery,
		bodyPreview: "Durably change the stopped target from the Codex app-server drive to tmux send-keys, tear down its recorded app-server runtime, and resume it. Future relaunches will remain on send-keys until explicitly changed again.",
		bodyLabel:   "Codex drive rollback (app-server → send-keys)", targetGroup: targetGroup,
		targetConvID: targetConv, targetConvTitle: targetTitle, createdAt: time.Now(), timeout: timeout,
		decision: make(chan approvalOutcome, 1), extend: make(chan time.Duration, 1),
	}
	if requestHumanApproval(req, popupBaseURL) {
		markHumanApprovalContinuation(r, perm, authTarget)
		return true
	}
	writeError(w, http.StatusForbidden, "permission",
		fmt.Sprintf("human declined or timed out after %s while approving Codex drive rollback for target %s",
			timeout, short8(targetConv)))
	return false
}

// pickAliveSession returns the most-recent session row for convID
// whose tmux session is still alive. Same selector as queued mail delivery.
func pickAliveSession(convID string) *db.SessionRow {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil
	}
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			return c
		}
	}
	return nil
}

// armRemoteControlOnNewRow tags a freshly-relaunched session row's best-known
// remote-control state ON, out-of-band (db.SetSessionRemoteControl) — the same
// discipline executeSpawn uses after a --remote-control spawn (JOH-258): a
// targeted UPDATE the hook callback's SaveSession UPSERT never writes, so a
// status tick can't clobber it. label is the NEW row's tclaude session id; the
// --remote-control launch flag already armed Claude Code's Remote Access, so a
// write failure here is only a best-known-state drift the human can re-toggle —
// logged, never fatal, never a broken relaunch. See JOH-261.
func armRemoteControlOnNewRow(label string) {
	if err := db.SetSessionRemoteControl(label, true); err != nil {
		slog.Warn("relaunch: failed to arm remote-control on new session row",
			"label", label, "error", err)
	}
}

// armRemoteControlAfterResume waits for a resumed pane's FRESH session row to
// come online, then tags its best-known remote-control state ON. Resume mints a
// new session row (new label) for the SAME conv-id, so its remote_control
// defaults to 0 even when the source was armed; without this re-tag the
// dashboard indicator + the toggle's direction logic would read OFF after every
// resume, even though the --remote-control launch flag already re-armed CC's
// Remote Access.
//
// Unlike reincarnate / clone — whose handlers already poll for the new row
// synchronously, so they tag inline — resume is fire-and-forget with no known
// label, so this runs in the background (goBackground) and the bulk
// groups-resume loop is never serialised on each member's online-wait.
//
// pickAliveSession is unambiguous here: resumeOneConv only relaunches a conv
// that is OFFLINE (it gates on !isConvOnline), so the resumed pane is the only
// ALIVE row for the conv-id — the dead predecessor row is skipped. See JOH-261.
func armRemoteControlAfterResume(convID string) {
	deadline := time.Now().Add(reincarnateSpawnTimeout)
	for time.Now().Before(deadline) {
		if s := pickAliveSession(convID); s != nil {
			armRemoteControlOnNewRow(s.ID)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	slog.Warn("resume: remote-control re-arm timed out; resumed pane never came online",
		"conv", convID)
}

// handleGroupSpawn starts a fresh CC session and registers it in
// the group as soon as its conv-id materialises.
//
// Flow:
//  1. Pick a unique label (used as the tclaude session ID + tmux
//     session name).
//  2. Fork-exec `tclaude session new -d --global --label <label>`
//     fully detached. The wrapper exits in milliseconds; the actual
//     CC process is parented to the long-running tmux server, so
//     CC's process-ownership checks see no Claude ancestor in the
//     daemon's chain.
//  3. Poll the sessions table for that label until conv-id appears
//     (CC's first hook callback writes it). 30s default timeout.
//  4. Add the conv to the group with the supplied role/descr; the
//     `name` (when set) becomes the new agent's conversation title
//     via the post-spawn /rename injection.
//
// normalizeSpawnPermissionOverrides validates the birth-time permission
// overrides off a SpawnRequest and returns the canonical slug→override map to
// apply at enrollment. Each slug must be registered and each effect
// must be "grant" or "deny"; a "default"/"" effect is a no-op and is dropped
// (the agent inherits the global default for that slug), so an editor that
// posts every slug — most at Default — collapses to just the real overrides.
// An unknown slug or an unrecognised effect returns a non-empty human-readable
// error string (the caller maps it to a 400); the map is nil for no overrides.
//
// This is also the scope boundary for every birth-time grant: an optional
// scope is parsed, checked against the dimensions its slug declares, and
// stored CANONICALIZED, so nothing below this line has to re-validate a scope
// and the stored bytes compare equal to a gate-side canonical form. A deny
// carries no scope by design (a deny is unconditional).
func normalizeSpawnPermissionOverrides(in map[string]db.PermissionOverride) (map[string]db.PermissionOverride, string) {
	if len(in) == 0 {
		return nil, ""
	}
	out := make(map[string]db.PermissionOverride, len(in))
	for slug, override := range in {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		effect := strings.TrimSpace(override.Effect)
		switch effect {
		case "", "default":
			continue // no override — inherits the global default
		case db.PermEffectGrant, db.PermEffectDeny:
			if !IsKnownPermSlug(slug) {
				return nil, fmt.Sprintf("unknown permission slug %q. Known slugs: %s.",
					slug, strings.Join(knownSlugs(), ", "))
			}
		default:
			return nil, fmt.Sprintf("permission override for %q must be \"grant\", \"deny\", or \"default\"; got %q",
				slug, override.Effect)
		}
		scope := strings.TrimSpace(override.Scope)
		if scope != "" && effect == db.PermEffectDeny {
			return nil, fmt.Sprintf("permission override for %q is a deny and cannot carry a scope", slug)
		}
		canonical, err := canonicalPermissionScopeForSlug(slug, scope)
		if err != nil {
			return nil, fmt.Sprintf("permission override for %q: %v", slug, err)
		}
		out[slug] = db.PermissionOverride{Effect: effect, Scope: canonical}
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// Permission: groups.members.spawn (default human-only — this lets an agent
// run arbitrary CC instances on the human's machine, blast radius
// matches `agent.spawn` in the design doc).
// spawnAuditResolution is the complete, user-facing shape that reached the
// shared spawn core. It deliberately excludes write-proof tokens and other
// daemon-only capabilities, while retaining every launch/enrollment choice,
// the profile rows that participated in resolution, and the resolved
// provenance echo. Profile briefings are omitted for the same reason the raw
// request briefing is redacted: audit detail is not a prompt/content store.
// The raw request and HTTP response are captured alongside it by the audit
// middleware.
func spawnAuditProfileSnapshot(p *db.SpawnProfile) any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"id":                            p.ID,
		"name":                          p.Name,
		"aliases":                       append([]string(nil), p.Aliases...),
		"disabled":                      p.Disabled,
		"disabled_reason":               p.DisabledReason,
		"operator_only":                 p.OperatorOnly,
		"harness":                       p.Harness,
		"model":                         p.Model,
		"effort":                        p.Effort,
		"sandbox":                       p.Sandbox,
		"sandbox_implementation":        p.SandboxImplementation,
		"approval":                      p.Approval,
		"tools":                         p.ToolGovernance,
		"ask_user_question_timeout":     p.AskUserQuestionTimeout,
		"auto_compact_window":           p.AutoCompactWindow,
		"context_window_max":            p.ContextWindowMax,
		"auto_review":                   p.AutoReview,
		"trust_dir":                     p.TrustDir,
		"remote_control":                p.RemoteControl,
		"auto_memory":                   p.AutoMemory,
		"ssh_workaround":                p.SSHWorkaround,
		"context_features":              p.ContextFeatures,
		"agent_name":                    p.AgentName,
		"role":                          p.Role,
		"role_ref":                      p.RoleRef,
		"role_refs":                     p.RoleRefs,
		"descr":                         p.Descr,
		"initial_message":               redactedAuditText(p.InitialMessage),
		"startup_context":               redactedAuditText(p.StartupContext),
		"sync_worktree":                 p.SyncWorktree,
		"fetch_latest_worktree":         p.FetchLatestWorktree,
		"auto_focus":                    p.AutoFocus,
		"include_group_default_context": p.IncludeGroupDefaultContext,
		"is_owner":                      p.IsOwner,
		"permission_overrides":          p.PermissionOverrides,
		"created_at":                    p.CreatedAt,
		"updated_at":                    p.UpdatedAt,
	}
}

func spawnAuditResolution(p spawnParams, launch *agent.ResolvedLaunch, requestedProfile string, profiles map[string]*db.SpawnProfile) map[string]any {
	// The audit row records the override's full shape, scope included: an
	// unscoped one still renders as the bare "grant"/"deny" it always did.
	permissions := make(map[string]db.PermissionOverride, len(p.PermissionOverrides))
	for slug, override := range p.PermissionOverrides {
		permissions[slug] = override
	}
	features := make(map[string]string, len(p.ContextFeatures))
	for feature, state := range p.ContextFeatures {
		features[feature] = state
	}
	attachments := append([]string(nil), p.Attachments...)
	profileSnapshots := make(map[string]any, len(profiles))
	for name, profile := range profiles {
		profileSnapshots[name] = spawnAuditProfileSnapshot(profile)
	}
	return map[string]any{
		"launch":          launch,
		"profile_request": requestedProfile,
		"profiles":        profileSnapshots,
		"params": map[string]any{
			"name":                      p.Name,
			"role":                      p.Role,
			"descr":                     p.Descr,
			"task_ref_url":              p.TaskURL,
			"task_ref_label":            p.TaskLabel,
			"attachments":               attachments,
			"cwd":                       p.Cwd,
			"worktree_path":             p.WorktreePath,
			"worktree_branch":           p.WorktreeBranch,
			"auto_focus":                p.AutoFocus,
			"harness":                   p.Harness,
			"model":                     p.Model,
			"effort":                    p.Effort,
			"ssh_workaround":            p.SSHWorkaround,
			"sandbox":                   p.HarnessBuiltinMode,
			"sandbox_source":            p.HarnessBuiltinModeSource,
			"sandbox_implementation":    p.SandboxImplementation,
			"allow_unenforced_sandbox":  p.AllowUnenforcedSandbox,
			"approval":                  p.ApprovalPolicy,
			"tools":                     p.ToolGovernance,
			"auto_review":               p.AutoReview,
			"trust_dir":                 p.TrustDir,
			"remote_control":            p.RemoteControl,
			"auto_memory":               p.AutoMemory,
			"context_features":          features,
			"auto_compact_window":       p.AutoCompactWindow,
			"context_window_max":        p.ContextWindowMax,
			"ask_user_question_timeout": p.AskUserQuestionTimeout,
			"reply_to_conv":             p.ReplyToConv,
			"spawned_by_conv":           p.SpawnedByConv,
			"include_group_context":     p.GroupContext != "",
			"profile_context":           redactedAuditText(p.ProfileContext),
			"is_owner":                  p.IsOwner,
			"permission_overrides":      permissions,
			"timeout":                   p.Timeout.String(),
			"async":                     p.Async,
		},
	}
}

// decodeSpawnBody decodes the spawn request, returns its original JSON, and
// puts the raw bytes BACK on
// r.Body, so a later ask-human popup still previews the request the agent
// actually sent (snapshotRequestBody has the same restore contract, for the
// same reason). Decoding has to happen before the permission gate — the gate
// needs the request's profile — and the gate is what may open the popup, so
// the body cannot simply be consumed here.
//
// On a malformed body it writes the 400 and returns false. That 400 now
// precedes the groups.members.spawn refusal for an unauthorized caller; it discloses
// nothing beyond "your JSON did not parse".
//
// "Was there a body" is decided from the bytes actually READ, not from
// ContentLength. A chunked request carries ContentLength -1, so testing it for
// <= 0 would hand the handler a zero-valued SpawnRequest and spawn with
// defaults — silently dropping the caller's name, profile and permission
// overrides, and leaving the spawn_profile gate to judge a profile the request
// never got to state. Only ContentLength == 0 is a declared empty body, and
// even that falls through to the same trimmed-length test.
func decodeSpawnBody(w http.ResponseWriter, r *http.Request, body *agent.SpawnRequest) (string, bool) {
	if r.Body == nil {
		return `{}`, true
	}
	// The popup's restore bound is the sibling limit and the natural ceiling: a
	// body this path cannot buffer is one snapshotRequestBody could not preview
	// either. Without it, ReadAll on an unbounded chunked stream is a trivial
	// pre-authorization memory sink — this runs BEFORE the groups.members.spawn gate.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxApprovalRestoreBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		return `{}`, true
	}
	if err := json.Unmarshal(raw, body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return "", false
	}
	return string(raw), true
}

// resolvedSpawnProfileNameForScope answers "which named spawn profile will
// this request launch with", for the spawn_profile scope dimension only. It
// mirrors the profile precedence the full resolution below applies —
// request profile → group default → global default — and returns the profile's
// CANONICAL name, so a grant scoped to a profile still matches a request that
// reached it through an alias.
//
// It returns "" whenever no named profile resolves, including for a request
// naming one that does not exist. A scoped grant then finds the dimension
// undescribed and fails closed, so a scoped caller sees 403 rather than the
// ordinary invalid_profile 400 the unscoped caller still gets further down.
// That ordering is deliberate: the gate must not leak which profile names
// exist to a caller that is not allowed to spawn with them anyway.
func resolvedSpawnProfileNameForScope(g *db.AgentGroup, requested string) string {
	if name := strings.TrimSpace(requested); name != "" {
		prof, err := db.ResolveSpawnProfile(name)
		if err != nil || prof == nil {
			return ""
		}
		return prof.Name
	}
	for _, prof := range []*db.SpawnProfile{groupDefaultProfile(g), globalDefaultProfile()} {
		if prof != nil {
			return prof.Name
		}
	}
	return ""
}

func handleGroupSpawn(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	// requireGroupPermission also hands back the caller's conv-id: a real
	// agent (e.g. a PO orchestrating workers) resolves to its conv-id,
	// the human resolves to "". It is the default reply-to target for
	// the startup briefing assembled further down. Owners of g pass
	// without an explicit groups.members.spawn grant (owner-state default); the
	// spawn guardrails below still bind them (member cap, rate limit) and
	// already treat an owner as allowed for the group restriction.
	//
	// agent.SpawnRequest is the single shared request shape — the same
	// type `tclaude agent spawn`, `tclaude --join-group`, and the
	// dashboard's spawn modal marshal — so the wire contract can't drift
	// between the CLI and the dashboard. See its doc comment for the
	// per-field semantics.
	//
	// It is decoded BEFORE the gate because groups.members.spawn may be scoped by
	// spawn_profile, and the gate can only evaluate that against the profile
	// this spawn will actually launch with. decodeSpawnBody restores r.Body so
	// the ask-human popup still previews the full request.
	var body agent.SpawnRequest
	requestedSpawnConfigJSON, decoded := decodeSpawnBody(w, r, &body)
	if !decoded {
		return
	}
	// Preserve the caller's decoded wire parameters before profile/default
	// resolution, generated-name assignment, normalization, or permission
	// attenuation mutates body below. Resolved and running launch state have
	// their own durable records; this is the requested side of the comparison.
	// The spawn_profile the gate judges is the RESOLVED one — request profile,
	// else the group default, else the global default — never the raw request
	// field. "" (no profile resolves, or a named one that does not exist)
	// leaves the dimension undescribed, which a scoped grant cannot satisfy:
	// an inline launch shape is not a named profile and must not pass a
	// profile-pinned grant. An unscoped grant is unaffected, so this reaches
	// the existing 400 for a bad profile name exactly as before.
	spawnerConvID, ok := requireGroupPermission(w, r, PermGroupsMembersSpawn, g,
		ActionContext{Group: g.Name, SpawnProfile: resolvedSpawnProfileNameForScope(g, body.Profile)})
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	if !routeMembershipMutationAllowed(w, g) {
		return
	}
	if body.AllowUnenforcedSandbox && !isDashboardSpawnPeer(r) {
		writeError(w, http.StatusForbidden, "unenforced_sandbox_override_restricted",
			"only the human operator may allow an unenforced sandbox through the dashboard spawn dialog")
		return
	}
	if fail := rejectBreakGlassSpawn(body.BreakGlassAcknowledged); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	body.SandboxProfile = strings.TrimSpace(body.SandboxProfile)
	if body.OmitSandboxProfiles && body.SandboxProfile != "" {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile",
			"sandbox_profile and omit_sandbox_profiles are mutually exclusive")
		return
	}
	if (body.SandboxProfile != "" || body.OmitSandboxProfiles) &&
		classify(peerFromContext(r.Context())) != classHuman {
		writeError(w, http.StatusForbidden, "sandbox_profile_restricted",
			"only the human operator may select or omit sandbox profiles; agents may only inherit existing policy")
		return
	}
	if !validateSpawnHarnessConfig(w, r, body.HarnessConfig) {
		return
	}

	// Spawn guardrails — runaway-prevention for an agent that the human
	// granted `groups.members.spawn`. The group's hard member cap (binds the human
	// too) and — for agent callers only (spawnerConvID != "") — the group
	// restriction run here, before any subprocess is launched, so a rejected
	// spawn costs nothing. The third guardrail, the per-caller rate limit,
	// is claimed after the validation gates below (claimSpawnRateSlot) so a
	// refused request — including the dir write-proof challenge round-trip —
	// never burns a slot. See spawn_guardrails.go.
	if !checkSpawnGuardrails(w, g, spawnerConvID) {
		return
	}

	// The initial message is delivered to the new agent's inbox as an
	// agent_messages row — not typed into its tmux pane — so newlines
	// survive verbatim and a multi-line task brief arrives intact. We
	// only cap the length and reject NUL / escape / other non-text
	// control characters that would corrupt an `inbox read` render.
	body.InitialMessage = strings.TrimSpace(body.InitialMessage)
	if !isValidInitialMessage(body.InitialMessage) {
		writeError(w, http.StatusBadRequest, "invalid_initial_message",
			fmt.Sprintf("initial_message must be at most %d characters; newlines and tabs "+
				"are allowed (it is delivered to the agent's inbox, not typed into "+
				"its pane), but other control characters are not", agent.MaxInitialMessageBytes))
		return
	}

	// Reject an invalid agent name at the boundary rather than silently
	// dropping it downstream (executeSpawn only applies a name that clears
	// isValidRenameTitle). An empty name stays valid — the agent gets an
	// auto-generated label; a non-empty one must be a safe token. The CLI
	// (agent.isValidSpawnName) and dashboard mirror this, but this is the
	// authoritative gate for the user-facing spawn surfaces: `tclaude agent
	// spawn`, `--join-group`, and the dashboard modal all POST through here.
	// (The group-template instantiator builds names as group+template and
	// calls executeSpawn directly, bypassing this gate; it falls back to the
	// downstream isValidRenameTitle silent-drop — see handleTemplateInstantiate.)
	body.Name = strings.TrimSpace(body.Name)
	// Auto-normalize an invalid name to the safe branch-token charset when
	// config's agent.spawn_name_normalize is on (the default), so any name a
	// human types "just works" — "code reviewer!" lands as "code-reviewer"
	// rather than 400ing. The CLI and dashboard normalize client-side too, so
	// this is usually a no-op here (NormalizeSpawnName is idempotent); it is
	// the authoritative backstop for a raw POST. Read config live so a Config
	// tab toggle takes effect without a daemon restart. Disabled (explicit
	// false) keeps the strict reject below.
	if !isValidSpawnName(body.Name) {
		if cfg, _ := config.Load(); cfg.SpawnNameNormalizeEnabled() {
			body.Name = agent.NormalizeSpawnName(body.Name)
		}
	}
	if !isValidSpawnName(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid_name",
			fmt.Sprintf("name must be 1-%d characters from [A-Za-z0-9_-] (letters, "+
				"digits, underscore, dash); spaces, punctuation, and unicode are not "+
				"allowed (the name doubles as a git worktree branch name and becomes "+
				"the conversation title)", agent.MaxSpawnNameLen))
		return
	}

	// Attachment paths (uploaded files / pasted screenshots from the dashboard's
	// /api/spawn-attachments endpoint) are folded into the startup briefing as an
	// "Attached files" section. Clean + bound them the same way as the initial
	// message — they share its inbox render and inline-launch path.
	attachments, attErr := sanitizeSpawnAttachments(body.Attachments)
	if attErr != "" {
		writeError(w, http.StatusBadRequest, "invalid_attachments", attErr)
		return
	}

	// Birth-time access controls: make the new agent a group owner and/or seed
	// its permanent per-slug permission overrides, the same grants the
	// Edit-agent modal applies to a live agent — but applied at enrollment so
	// the agent's first turn already has them. SHAPE is validated here, at the
	// boundary, before any subprocess launches: every override slug must be
	// registered and every effect in {grant,deny} ("default"/"" carries no
	// override and is dropped).
	//
	// The PRIVILEGE gate runs further down instead, once the profile tier stack
	// has had its say: a spawn that merely NAMES a profile inherits that
	// profile's owner flag and overrides, so gating the raw request body would
	// check a value the launch does not use. See the birth-time access block
	// after the launch fields resolve.
	permOverrides, povErr := normalizeSpawnPermissionOverrides(body.PermissionOverrides)
	if povErr != "" {
		writeError(w, http.StatusBadRequest, "invalid_permission_overrides", povErr)
		return
	}

	// Resolve the startup briefing's sender. Default: the spawn
	// requester (an agent → its conv-id; a human → ""). An explicit
	// reply_to selector overrides it — the knob a coordinator uses to
	// route a worker's replies to a third agent rather than itself.
	replyToConv := spawnerConvID
	if rt := strings.TrimSpace(body.ReplyTo); rt != "" {
		res, _, rtErr := agent.ResolveSelector(rt)
		if rtErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_reply_to",
				fmt.Sprintf("reply_to %q: %v", rt, rtErr))
			return
		}
		replyToConv = res.ConvID
	}

	timeout := 30 * time.Second
	if body.TimeoutSeconds > 0 {
		timeout = time.Duration(body.TimeoutSeconds) * time.Second
		if timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
	}

	// When the request leaves cwd blank, fall back to the group's
	// default_cwd (the "group default start dir" set via the
	// dashboard or `groups set-default-dir`). This makes the default
	// reach every spawn path — CLI, API, dashboard — not just the
	// dashboard's client-side prefill. An empty default_cwd leaves
	// cwd blank, so resolveSpawnCwd keeps its prior behaviour of
	// inheriting the daemon's own cwd.
	if body.Cwd == "" {
		body.Cwd = g.DefaultCwd
	}

	// Resolve the harness independently through the complete chain. Other launch
	// fields never pin it: explicit request > named CLI profile > group default
	// profile > global default profile > Claude. Field candidates are validated
	// against this resolved harness below.
	var namedProfile *db.SpawnProfile
	namedProfileHandle := ""
	if name := strings.TrimSpace(body.Profile); name != "" {
		var profileErr error
		namedProfileHandle = name
		namedProfile, profileErr = db.ResolveSpawnProfile(name)
		if profileErr != nil || namedProfile == nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", fmt.Sprintf("spawn profile %q does not exist", name))
			return
		}
		if fail := profileSpawnFailure(namedProfile, spawnerConvID); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}
	groupProfile := groupDefaultProfile(g)
	globalProfile := globalDefaultProfile()
	for _, prof := range []*db.SpawnProfile{groupProfile, globalProfile} {
		if fail := profileSpawnFailure(prof, spawnerConvID); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}
	namedProfileSource := profileSource(namedProfile, agent.ProvCLIProfileSource)
	if namedProfile != nil && namedProfileHandle != namedProfile.Name {
		namedProfileSource = fmt.Sprintf(`profile %q via alias %q`, namedProfile.Name, namedProfileHandle)
	}
	profileTiers := []launchProfileTier{
		{profile: namedProfile, source: namedProfileSource},
		{profile: groupProfile, source: profileSource(groupProfile, agent.ProvGroupProfileSource),
			defaultTier: true},
		{profile: globalProfile, source: profileSource(globalProfile, agent.ProvGlobalProfileSource),
			defaultTier: true},
	}
	harnessSource := agent.ProvExplicit
	if strings.TrimSpace(body.Harness) == "" {
		harnessSource = agent.ProvHarnessDefault
		for _, tier := range profileTiers {
			if tier.profile != nil {
				body.Harness = harnessOrDefault(tier.profile.Harness)
				harnessSource = tier.source
				break
			}
		}
	}
	if strings.TrimSpace(body.Harness) == "" {
		body.Harness = harness.DefaultName
	}

	// Validate the requested cwd before doing any work. Expands "~",
	// makes the path absolute, and confirms it exists as a directory.
	// Catching a bad cwd here turns what used to be a silent 30s
	// conv-id-poll timeout into an immediate, actionable error.
	cwd, cwdErr := resolveSpawnCwd(body.Cwd)
	if cwdErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", cwdErr.Error())
		return
	}

	// Validate the optional worktree dir the same way — it must exist
	// (the dashboard creates it just before spawning). Caught here so
	// a stale path becomes an immediate 400 rather than a welcome
	// message pointing the agent at a directory that isn't there.
	var worktreePath string
	if strings.TrimSpace(body.WorktreePath) != "" {
		wt, wtErr := resolveSpawnCwd(body.WorktreePath)
		if wtErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_worktree", wtErr.Error())
			return
		}
		worktreePath = wt
	}
	worktreeBranch := strings.TrimSpace(body.WorktreeBranch)

	// Resolve the requested harness (default Claude Code). An unknown
	// name is a 400 here rather than a silent failure once the forked
	// session exits. The chosen harness's ModelCatalog then validates
	// effort/model below, so a Codex spawn is checked against Codex's
	// rules (rejects Claude Code slugs, accepts effort levels) instead of
	// Claude Code's.
	h, harnessErr := resolveSpawnHarness(body.Harness)
	if harnessErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_harness", harnessErr.Error())
		return
	}
	// Cross-harness spawn policy is evaluated only after the complete profile
	// stack has resolved the target vendor. That closes the indirect path where
	// an agent omits --harness but a group/global default profile flips it.
	if fail := spawnHarnessPolicyFailure(g, spawnerConvID, h.Name); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	validateModel := func(raw string) (string, error) {
		value, err := h.Models.ValidateModel(raw)
		if err == nil {
			return value, nil
		}
		other := "codex"
		if h.Name == "codex" {
			other = harness.DefaultName
		}
		return "", fmt.Errorf("model %q is not valid for %s; pass --harness %s or a matching --model: %w",
			strings.TrimSpace(raw), h.Name, other, err)
	}
	var fieldFail *spawnFailure
	var modelSource, modelNote, effortSource, effortNote string
	body.Model, modelSource, modelNote, fieldFail = resolveStringLaunchField(
		modelField, body.Model, h.Name, profileTiers, func(p *db.SpawnProfile) string { return p.Model }, validateModel)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	var fastMode, fastModeSet bool
	var fastModeNote, fastModeSource string
	requestedFastMode := strings.TrimSpace(body.FastMode)
	if requestedFastMode != "" {
		normalizedFastMode, fastErr := harness.ResolveFastModeFlag(h, requestedFastMode)
		if fastErr != nil {
			fieldFail = &spawnFailure{Status: http.StatusBadRequest, Kind: "invalid_request", Msg: fastErr.Error()}
		} else {
			requestedFastMode = normalizedFastMode
			fastModeSource = "explicit"
			switch requestedFastMode {
			case harness.FastModeOn:
				fastMode, fastModeSet = true, true
			case harness.FastModeOff:
				fastModeSet = true
			}
		}
	} else {
		fastMode, fastModeSet, fastModeSource, fastModeNote, fieldFail = resolveBoolLaunchField(
			"fast_mode", false, false, h.Name, profileTiers,
			func(p *db.SpawnProfile) *bool { return p.FastMode },
			func(v bool) (bool, error) {
				_, err := harness.ResolveFastMode(h, &v)
				return v, err
			})
	}
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	body.Effort, effortSource, effortNote, fieldFail = resolveStringLaunchField(
		effortField, body.Effort, h.Name, profileTiers, func(p *db.SpawnProfile) string { return p.Effort }, h.Models.ValidateEffort)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	var sandboxNote, approvalNote, toolsNote, askTimeoutNote string
	// The tier that chose the sandbox is kept, not discarded: it is the only
	// party that can say a global/group default profile forced the containment,
	// and the badge would otherwise credit "this launch" — i.e. the operator.
	var sandboxSource string
	body.HarnessBuiltinMode, sandboxSource, sandboxNote, fieldFail = resolveStringLaunchField(
		"sandbox", body.HarnessBuiltinMode, h.Name, profileTiers, func(p *db.SpawnProfile) string { return p.Sandbox },
		func(raw string) (string, error) { return harness.ValidateHarnessBuiltinMode(h, raw) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	body.ApprovalPolicy, _, approvalNote, fieldFail = resolveStringLaunchField(
		"approval", body.ApprovalPolicy, h.Name, profileTiers, func(p *db.SpawnProfile) string { return p.Approval },
		func(raw string) (string, error) { return harness.ValidateApprovalPolicy(h, raw) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	body.ToolGovernance, _, toolsNote, fieldFail = resolveStringLaunchField(
		"tools", body.ToolGovernance, h.Name, profileTiers,
		func(p *db.SpawnProfile) string { return p.ToolGovernance },
		func(raw string) (string, error) { return harness.ValidateToolGovernance(h, raw) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	body.AskUserQuestionTimeout, _, askTimeoutNote, fieldFail = resolveStringLaunchField(
		"ask_user_question_timeout", body.AskUserQuestionTimeout, h.Name, profileTiers,
		func(p *db.SpawnProfile) string { return p.AskUserQuestionTimeout },
		func(raw string) (string, error) { return harness.ResolveAskTimeoutMode(h, raw) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	// The auto-compaction window rides the same tier stack as effort. The
	// validator both gates it on the harness and NORMALIZES the spelling, so a
	// profile saved as "450k" and a spawn body carrying "450000" resolve to the
	// same canonical value before anything downstream compares them.
	var autoCompactWindowNote string
	body.AutoCompactWindow, _, autoCompactWindowNote, fieldFail = resolveStringLaunchField(
		"auto_compact_window", body.AutoCompactWindow, h.Name, profileTiers,
		func(p *db.SpawnProfile) string { return p.AutoCompactWindow },
		func(raw string) (string, error) { return harness.ResolveAutoCompactWindow(h, raw) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	var contextWindowMaxNote string
	var contextWindowMaxSource string
	body.ContextWindowMax, contextWindowMaxSource, contextWindowMaxNote, fieldFail = resolveIntLaunchField(
		contextWindowMaxField, body.ContextWindowMax, h.Name, profileTiers,
		func(p *db.SpawnProfile) int64 { return p.ContextWindowMax },
		func(value int64) (int64, error) { return harness.ResolveCopilotContextWindow(h, value) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	// The sandbox IMPLEMENTATION rides the same tier stack, but only its HARNESS
	// applicability is validated inside the validator. That placement is the
	// whole design: a lower-tier profile pinned to a different harness is
	// ambient configuration, so it is skipped, disclosed, and falls through —
	// while an explicit request, or a profile that claims this harness, fails
	// loudly. Whether the HOST can run the layer is a separate gate below,
	// because tier fallthrough would turn "no bwrap on this box" into a silent
	// downgrade to harness-builtin. See sandbox_implementation.go.
	var sandboxImplNote string
	var sandboxImplSource string
	body.SandboxImplementation, sandboxImplSource, sandboxImplNote, fieldFail = resolveStringLaunchField(
		sandboxImplementationField, body.SandboxImplementation, h.Name, profileTiers,
		func(p *db.SpawnProfile) string { return p.SandboxImplementation },
		func(raw string) (string, error) { return validateSandboxImplementationForHarness(h, raw) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	// Host gate, on the RESOLVED value and whichever tier supplied it. Never
	// falls through; refuses naming the missing capability. Probed live, so an
	// operator who just installed bwrap is not refused by a stale answer.
	if fail := sandboxImplementationHostFailure(h.Name, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	// Posture gate, beside the host gate and on the same terms: a harness whose
	// own OS sandbox tclaude cannot switch off has to have its configuration
	// verified at every launch, or the recorded single-boundary claim outlives
	// the configuration it was made about.
	if fail := sandboxImplementationPostureFailure(h.Name, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	var autoReviewSet, trustDirSet, sshWorkaroundSet bool
	var autoReviewNote, trustDirNote, autoMemoryNote, sshWorkaroundNote, contextFeaturesNote string
	body.AutoReview, autoReviewSet, _, autoReviewNote, fieldFail = resolveBoolLaunchField(
		"auto_review", body.AutoReview, body.AutoReviewSpecified(), h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.AutoReview }, func(v bool) (bool, error) { return harness.ResolveAutoReview(h, v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	sshWorkaround, sshWorkaroundSet, sshWorkaroundSource, sshWorkaroundNote, fieldFail := resolveBoolLaunchField(
		"ssh_workaround", body.SSHWorkaround != nil && *body.SSHWorkaround, body.SSHWorkaround != nil, h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.SSHWorkaround },
		func(v bool) (bool, error) { return harness.ResolveSSHWorkaround(h, &v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	if !sshWorkaroundSet {
		sshWorkaround, _ = harness.ResolveSSHWorkaround(h, nil)
	}
	body.TrustDir, trustDirSet, _, trustDirNote, fieldFail = resolveBoolLaunchField(
		"trust_dir", body.TrustDir, body.TrustDirSpecified(), h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.TrustDir }, func(v bool) (bool, error) { return harness.ResolveTrustDir(h, v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	// auto_memory rides the same tier stack, but note the default it falls back
	// to: unset everywhere resolves to FALSE, and false here means tclaude
	// injects CLAUDE_CODE_DISABLE_AUTO_MEMORY=1. That is deliberate — Claude
	// Code's per-project memory store is shared by every agent on the repo, so
	// leaving it on cross-pollutes their notes. Only an explicit opt-in (spawn
	// body or a matching profile) turns it back on.
	var autoMemory bool
	autoMemory, _, _, autoMemoryNote, fieldFail = resolveBoolLaunchField(
		"auto_memory", body.AutoMemory != nil && *body.AutoMemory, body.AutoMemory != nil, h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.AutoMemory },
		func(v bool) (bool, error) { return harness.ResolveAutoMemory(h, &v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	// The Copilot drive rides the same tier stack, with the same fallback the
	// send-keys path has always had: unset everywhere is FALSE. Only an explicit
	// opt-in (spawn body or a matching Copilot profile) selects the API drive, so
	// a launch nobody steered is byte-for-byte the launch it was before this
	// field existed. See TCL-1053.
	var copilotAPI, copilotAPISet bool
	var copilotAPINote, copilotAPISource string
	copilotAPI, copilotAPISet, copilotAPISource, copilotAPINote, fieldFail = resolveBoolLaunchField(
		"copilot_api", body.CopilotAPI != nil && *body.CopilotAPI, body.CopilotAPI != nil, h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.CopilotAPI },
		func(v bool) (bool, error) { return harness.ResolveCopilotAPI(h, &v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	var codexAppServer, codexAppServerSet bool
	var codexAppServerNote, codexAppServerSource string
	codexAppServer, codexAppServerSet, codexAppServerSource, codexAppServerNote, fieldFail = resolveBoolLaunchField(
		"codex_app_server", body.CodexAppServer != nil && *body.CodexAppServer,
		body.CodexAppServer != nil, h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.CodexAppServer },
		func(v bool) (bool, error) { return harness.ResolveCodexAppServer(h, &v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	// Group-context inclusion rides the same tier stack, minus the harness gate:
	// it decides what the new agent is TOLD, not how its harness runs, so a
	// profile authored for another vendor still speaks (harnessAgnosticLaunchField).
	// Resolving it here rather than only client-side is what lets a caller that
	// merely NAMES a profile honour its toggle — the agentd TUI does exactly that,
	// while `tclaude agent spawn` and the dashboard both send an explicit flag.
	// Unset at every tier keeps the long-standing default: include.
	includeGroupContext, includeGroupContextSet, includeGroupContextSource, _, fieldFail := resolveBoolLaunchField(
		includeGroupContextField, body.IncludeGroupContext != nil && *body.IncludeGroupContext,
		body.IncludeGroupContext != nil, h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.IncludeGroupDefaultContext },
		func(v bool) (bool, error) { return v, nil })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	if !includeGroupContextSet {
		includeGroupContext = true
	}
	// Disclose a tier nobody typed at this launch. A DEFAULT profile silently
	// withholding the group's shared guidance is exactly the action-at-a-distance
	// the launch echo exists to surface: the operator sees WHICH tier decided,
	// not just an agent that mysteriously arrived unbriefed. The resolver's own
	// note channel stays silent for this field — it only speaks on the harness
	// mismatch this field skips — so the disclosure is built here.
	includeGroupContextNote := ""
	if includeGroupContextSet && includeGroupContextSource != agent.ProvExplicit {
		includeGroupContextNote = fmt.Sprintf("%s include_group_context = %v",
			includeGroupContextSource, includeGroupContext)
	}
	// Identity, the auto-focus toggle and the birth-time access controls ride
	// the same tier stack, and for the same reason group-context inclusion does:
	// a caller that merely NAMES a profile must get what that profile says. The
	// agentd TUI's spawn form is exactly such a caller — it has boxes for the
	// name and the brief only, so role, descr, auto-focus, the owner flag and
	// the permission overrides reached the daemon unspoken and were dropped.
	//
	// PRESENCE on the wire, not a non-empty value, is what counts as the caller
	// speaking (see resolveIdentityLaunchField). That is what keeps a dashboard
	// operator who clears a profile-prefilled Role from having it restored.
	var identityNotes []string
	var profileNameNote string
	body.Name, _, profileNameNote = resolveIdentityLaunchField(
		nameField, body.Name, body.NameSpecified(), profileTiers,
		func(p *db.SpawnProfile) string { return p.AgentName },
		func(raw string) (string, bool) {
			// A profile's agent_name is held to looser rules than a spawn name
			// (it may carry spaces and punctuation), so put it through the same
			// normalize-then-validate an explicit name took at the boundary. One
			// that still cannot be a spawn name is skipped and disclosed rather
			// than failing a launch nobody typed it into.
			if !isValidSpawnName(raw) {
				if cfg, _ := config.Load(); cfg.SpawnNameNormalizeEnabled() {
					raw = agent.NormalizeSpawnName(raw)
				}
			}
			return raw, raw != "" && isValidSpawnName(raw)
		})
	if profileNameNote != "" {
		identityNotes = append(identityNotes, profileNameNote)
	}
	roleRefs, roleRefsSource := resolveRoleRefsLaunchField(body, profileTiers)
	selectedRoles := make([]*db.Role, 0, len(roleRefs))
	for _, roleRef := range roleRefs {
		selectedRole, roleErr := db.GetRole(roleRef)
		if roleErr != nil {
			writeError(w, http.StatusInternalServerError, "io", roleErr.Error())
			return
		}
		if selectedRole == nil {
			writeError(w, http.StatusBadRequest, "invalid_role",
				fmt.Sprintf("role %q does not name a role in the role library", roleRef))
			return
		}
		selectedRoles = append(selectedRoles, selectedRole)
	}
	body.RoleRefs = make([]string, 0, len(selectedRoles))
	for _, selectedRole := range selectedRoles {
		body.RoleRefs = append(body.RoleRefs, selectedRole.Name)
	}
	body.RoleRef = ""
	if len(body.RoleRefs) > 0 {
		body.RoleRef = body.RoleRefs[0]
	}
	var roleSource string
	body.Role, roleSource, _ = resolveIdentityLaunchField(
		roleField, body.Role, body.RoleSpecified(), profileTiers,
		func(p *db.SpawnProfile) string { return p.Role }, nil)
	if body.Role == "" && roleSource == "" && len(selectedRoles) > 0 {
		body.Role = selectedRoles[0].Name
	}
	body.Descr, _, _ = resolveIdentityLaunchField(
		descrField, body.Descr, body.DescrSpecified(), profileTiers,
		func(p *db.SpawnProfile) string { return p.Descr }, nil)
	// A profile's initial_message is a replaceable task default: it fills the
	// brief only when the caller sent none. Durable per-profile guidance is
	// startup_context, resolved separately above and never overridden by a task.
	body.InitialMessage, _, _ = resolveIdentityLaunchField(
		initialMessageField, body.InitialMessage, body.InitialMessageSpecified(), profileTiers,
		func(p *db.SpawnProfile) string { return p.InitialMessage }, nil)
	// Name an otherwise unnamed group spawn before building spawnParams and its
	// durable/audit snapshots. executeSpawn retains the same fallback for
	// non-HTTP adapters, but the shared HTTP path must record the name it
	// actually launches rather than an empty pre-derivation value.
	if strings.TrimSpace(body.Name) == "" {
		body.Name = derivedGroupSpawnName(g.Name, time.Now(), randomLabelToken())
	}
	// No disclosure note for auto_focus: the resolver's note channel only speaks
	// on the harness mismatch this field skips, and a terminal window opening is
	// its own announcement.
	body.AutoFocus, _, _, _, fieldFail = resolveBoolLaunchField(
		autoFocusField, body.AutoFocus, body.AutoFocusSpecified(), h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.AutoFocus },
		func(v bool) (bool, error) { return v, nil })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	isOwner, _, isOwnerSource, _, fieldFail := resolveBoolLaunchField(
		isOwnerField, body.IsOwner, body.IsOwnerSpecified(), h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.IsOwner },
		func(v bool) (bool, error) { return v, nil })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	overridesSource := ""
	permOverrides, overridesSource = resolveOverridesLaunchField(
		permOverrides, body.PermissionOverridesSpecified(), profileTiers)
	if overridesSource != "" && overridesSource != agent.ProvExplicit {
		// The profile write path validated this map, but a slug can be retired
		// between the write and the launch. Re-run the boundary check rather
		// than trusting stored state, and drop-with-disclosure instead of
		// failing a spawn over a profile field nobody typed here.
		normalized, novErr := normalizeSpawnPermissionOverrides(permOverrides)
		if novErr != "" {
			identityNotes = append(identityNotes, fmt.Sprintf(
				"%s %s ignored (%s)", overridesSource, permissionOverridesField, novErr))
			permOverrides, overridesSource = nil, ""
		} else {
			permOverrides = normalized
		}
	}
	// Role grants form the lowest access tier. A spawn profile or explicit
	// per-spawn override can still narrow or replace an individual slug.
	if len(selectedRoles) > 0 {
		merged := make(map[string]db.PermissionOverride, len(permOverrides))
		for _, selectedRole := range selectedRoles {
			for _, grant := range selectedRole.Permissions {
				candidate := db.PermissionOverride{Effect: db.PermEffectGrant, Scope: grant.Scope}
				if existing, ok := merged[grant.Slug]; ok && existing != candidate {
					writeError(w, http.StatusBadRequest, "role_scope_conflict",
						fmt.Sprintf("roles grant %s with incompatible scopes; make the scopes identical or keep that grant on one role", grant.Slug))
					return
				}
				merged[grant.Slug] = candidate
			}
		}
		for slug, override := range permOverrides {
			merged[slug] = override
		}
		// Preserve the strongest intent source for the privilege gate below. An
		// explicitly selected role must not become an ambient/default grant just
		// because a lower-intent default profile also contributes overrides.
		if len(permOverrides) == 0 ||
			(!launchTierIsDefault(profileTiers, roleRefsSource) && launchTierIsDefault(profileTiers, overridesSource)) {
			overridesSource = roleRefsSource
		}
		permOverrides = merged
	}
	// Birth-time access privilege gate, on the RESOLVED values. A human
	// (dashboard) caller always passes; an agent caller must hold the SAME slug
	// the dedicated post-spawn endpoints require — groups.owners.manage to mint an owner
	// (handleGroupOwnersAdd) and permissions.grant to set per-slug overrides
	// (handlePermissionsGrant). Group ownership is deliberately NOT sufficient:
	// owner-state confers only the owner-implied lifecycle slugs
	// (groups.members.spawn/stop/…), NOT groups.owners.manage or permissions.grant — so keying on
	// ownership would let an owner mint a child holding permissions.grant and
	// escalate globally. resolvePermission (no owner bypass) is the same
	// evaluation those endpoints run.
	//
	// Direct intent — an explicit request field, or a profile the caller NAMED
	// — is refused loudly, exactly as it was before the tier stack reached these
	// two fields. A group or global DEFAULT profile is ambient configuration
	// nobody typed at this launch, so an unauthorized caller has it skipped and
	// disclosed instead: an operator's house default must not start refusing
	// every spawn its own agents make.
	if spawnerConvID != "" {
		if isOwner && resolvePermission(spawnerConvID, PermGroupsOwnersManage) != permAllow {
			if !launchTierIsDefault(profileTiers, isOwnerSource) {
				writeError(w, http.StatusForbidden, "forbidden",
					"making the spawned agent a group owner requires the "+PermGroupsOwnersManage+" permission")
				return
			}
			identityNotes = append(identityNotes, fmt.Sprintf(
				"%s is_owner ignored (caller lacks %s)", isOwnerSource, PermGroupsOwnersManage))
			isOwner, isOwnerSource = false, ""
		}
		if len(permOverrides) > 0 && resolvePermission(spawnerConvID, PermPermissionsGrant) != permAllow {
			if !launchTierIsDefault(profileTiers, overridesSource) {
				writeError(w, http.StatusForbidden, "forbidden",
					"setting the spawned agent's permission overrides requires the "+PermPermissionsGrant+" permission")
				return
			}
			identityNotes = append(identityNotes, fmt.Sprintf(
				"%s %s ignored (caller lacks %s)", overridesSource, permissionOverridesField, PermPermissionsGrant))
			permOverrides, overridesSource = nil, ""
		}
		// Attenuation-only delegation: holding permissions.grant lets the
		// spawner mint overrides, but never WIDER ones than it holds itself.
		// Without this an agent whose own grant is pinned to one profile or one
		// group could mint a child holding the same slug unscoped and act
		// through it. On the RESOLVED map, so a profile tier cannot smuggle in
		// a grant the request never named. See permission_attenuation.go.
		//
		// Same direct-intent split the gate above draws: an explicit request
		// field or a profile the caller NAMED is refused loudly, while an
		// operator's house DEFAULT profile is skipped and disclosed. Dropping
		// the overrides only ever NARROWS what the child is born with, and a
		// default nobody typed at this launch must not start refusing every
		// spawn a scoped agent makes.
		// The child has no stable id yet. Its lineage edge is mandatory during
		// enrollment, before these overrides are written, so it is a descendant
		// by construction rather than by a lookup that cannot succeed yet.
		conferee := grantConferee{descendantByConstruction: true}
		if err := checkGrantAttenuation(spawnerConvID, conferee, conferredGrantsFromOverrides(permOverrides)); err != nil {
			if !launchTierIsDefault(profileTiers, overridesSource) {
				writeError(w, http.StatusForbidden, "scope_not_attenuated", err.Error())
				return
			}
			identityNotes = append(identityNotes, fmt.Sprintf(
				"%s %s ignored (%v)", overridesSource, permissionOverridesField, err))
			permOverrides, overridesSource = nil, ""
		}
	}
	// Disclose the two access decisions whenever a tier other than the request
	// made them. An agent that silently comes up owning its group, or holding
	// grants nobody asked for at this launch, is exactly the action-at-a-distance
	// the launch echo exists to surface.
	if isOwnerSource != "" && isOwnerSource != agent.ProvExplicit && isOwner {
		identityNotes = append(identityNotes,
			fmt.Sprintf("%s is_owner = true", isOwnerSource))
	}
	if overridesSource != "" && overridesSource != agent.ProvExplicit && len(permOverrides) > 0 {
		identityNotes = append(identityNotes, fmt.Sprintf(
			"%s %s (%d)", overridesSource, permissionOverridesField, len(permOverrides)))
	}
	// Fold both back onto the working body for subsequent resolution. The
	// original request was captured before this mutation; resolved launch state
	// is persisted separately.
	body.IsOwner, body.PermissionOverrides = isOwner, permOverrides
	// Startup-context trims ride the tier stack whole rather than merging — see
	// resolveContextFeaturesLaunchField. Unset everywhere means "trim nothing",
	// so an agent's startup context only shrinks when someone asked for it.
	requestedFeatures := map[string]string{}
	if body.ContextFeatures != nil {
		requestedFeatures = *body.ContextFeatures
	}
	contextFeatures, cfNote, cfFail := resolveContextFeaturesLaunchField(
		requestedFeatures, body.ContextFeatures != nil, h, profileTiers)
	if cfFail != nil {
		writeError(w, cfFail.Status, cfFail.Kind, cfFail.Msg)
		return
	}
	contextFeaturesNote = cfNote
	// Profile guidance resolves as one whole text block from the highest
	// compatible tier. It has no per-spawn override: unlike initial_message this
	// is policy attached to the selected model/profile, not a task default.
	profileContext, profileContextNote := resolveProfileStartupContext(h.Name, profileTiers)
	for _, selectedRole := range selectedRoles {
		profileContext = appendRoleBlock(profileContext, selectedRole.Brief)
	}
	contextWindowMaxValue := ""
	if body.ContextWindowMax > 0 {
		contextWindowMaxValue = strconv.FormatInt(body.ContextWindowMax, 10)
	}
	copilotAPIValue := ""
	if copilotAPI {
		copilotAPIValue = "api"
	}
	codexAppServerValue := ""
	if h.Name == harness.CodexName {
		codexAppServerValue = "send-keys"
		if codexAppServer {
			codexAppServerValue = "app-server"
		}
	}
	fastModeValue := ""
	if fastModeSet {
		fastModeValue = harness.FastModeOff
		if fastMode {
			fastModeValue = harness.FastModeOn
		}
	}
	resolvedLaunch := &agent.ResolvedLaunch{
		Harness: agent.ResolvedField{Value: h.Name, Source: harnessSource},
		Model:   agent.ResolvedField{Value: body.Model, Source: modelSource, Note: modelNote},
		Effort:  agent.ResolvedField{Value: body.Effort, Source: effortSource, Note: effortNote},
		ContextWindowMax: agent.ResolvedField{
			Value: contextWindowMaxValue, Source: contextWindowMaxSource,
		},
		// Named for what it selects, not for the flag that selects it: "api" reads
		// correctly whether or not send-keys is still the other option.
		CopilotAPI:     agent.ResolvedField{Value: copilotAPIValue, Source: copilotAPISource},
		CodexAppServer: agent.ResolvedField{Value: codexAppServerValue, Source: codexAppServerSource},
		FastMode:       agent.ResolvedField{Value: fastModeValue, Source: fastModeSource},
	}
	resolvedLaunch.SandboxImpl = agent.ResolvedField{
		Value: body.SandboxImplementation, Source: sandboxImplSource, Note: sandboxImplNote}
	// The note rides the echoed field, like Harness/Model/Effort. It ALSO enters
	// Notes only when the value is blank, because the echo suppresses the
	// "Sandbox impl:" line for a default-off launch — and a disclosure with no
	// line to sit on would otherwise vanish. Adding it unconditionally would
	// print it twice whenever a later tier did supply a value.
	if body.SandboxImplementation == "" && sandboxImplNote != "" {
		resolvedLaunch.Notes = append(resolvedLaunch.Notes, sandboxImplNote)
	}
	for _, note := range append([]string{sandboxNote, approvalNote, toolsNote, askTimeoutNote, autoCompactWindowNote, contextWindowMaxNote, copilotAPINote, codexAppServerNote, fastModeNote, autoReviewNote, trustDirNote, autoMemoryNote, sshWorkaroundNote, contextFeaturesNote, profileContextNote, includeGroupContextNote}, identityNotes...) {
		if note != "" {
			resolvedLaunch.Notes = append(resolvedLaunch.Notes, note)
		}
	}

	// resolveStringLaunchField already validated and normalized both values.
	effort, model := body.Effort, body.Model

	// Resolve the sandbox mode for the chosen harness: a Codex agent gets its
	// secure default (the managed tclaude-agent profile) when unset, a Claude
	// agent gets its inherit default (normalized to "" — no `--settings`
	// override), and an explicit mode is validated per-harness. Then the
	// cwd-safety guard: a writable Codex sandbox confines writes to the cwd
	// subtree, so a cwd at/above $HOME would expose ~/.tclaude / ~/.codex /
	// ~/.claude — refuse here with a clean 400 rather than after the forked
	// session times out. (Claude's `on` block protects those dirs via settings,
	// so this Codex-specific guard doesn't apply to it.)
	harnessBuiltinMode, sbErr := harness.ResolveHarnessBuiltinMode(h, body.HarnessBuiltinMode)
	if sbErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox", sbErr.Error())
		return
	}
	harnessBuiltinMode, fieldFail = resolveSandboxImplementationMode(
		h, harnessBuiltinMode, body.SandboxImplementation)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	if h.UsesAuthoritativeServer() &&
		body.SandboxImplementation == string(sandboxpolicy.ImplementationTclaudeLayer) {
		resolvedLaunch.Notes = append(resolvedLaunch.Notes,
			clcommon.OpenCodeStatePrivateNote)
	}
	if harnessBuiltinMode != harness.SandboxManagedProfile {
		if sshWorkaround {
			resolvedLaunch.Notes = append(resolvedLaunch.Notes,
				"SSH workaround disabled because it applies only to the Codex tclaude-agent managed sandbox")
		}
		sshWorkaround = false
	}
	// Persist the resolved posture in the audit request as an explicit boolean,
	// including the default-on case and an operator's opt-out.
	body.SSHWorkaround = &sshWorkaround
	// body.ApprovalPolicy is already profile-merged above (resolveStringLaunchField
	// overlays the profile tiers without defaulting), so an empty value here means
	// NOTHING chose a posture — neither an explicit flag nor a spawn profile. Only
	// then may the harness default be narrowed to what this caller can mint; an
	// explicit or profile-set posture must fail loudly in the guard below.
	approvalUnset := strings.TrimSpace(body.ApprovalPolicy) == ""
	approvalPolicy, apErr := harness.ResolveApprovalPolicy(h, body.ApprovalPolicy)
	if apErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_approval", apErr.Error())
		return
	}
	if approvalUnset {
		defaultApprovalPolicy := approvalPolicy
		approvalPolicy = narrowDefaultApprovalToCaller(spawnerConvID, h.Name, approvalPolicy)
		if approvalPolicy != defaultApprovalPolicy {
			resolvedLaunch.Notes = append(resolvedLaunch.Notes,
				callerNarrowedApprovalNote(approvalPolicy, defaultApprovalPolicy))
		}
	}
	toolGovernance, tgErr := harness.ResolveToolGovernance(h, body.ToolGovernance)
	if tgErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_tools", tgErr.Error())
		return
	}
	// Both axes are final here (profile overlay, harness default, caller
	// narrowing), which is the only point where "autonomous but unconfined" is
	// a true statement about the launch that is about to happen. Warn, don't
	// refuse: forcing sandbox `on` would override the operator's settings.json
	// on the one axis tclaude deliberately leaves to them (TCL-586). The same
	// call also surfaces OpenCode's toothless access-control "sandbox".
	resolvedLaunch.Warnings = append(resolvedLaunch.Warnings,
		harness.SpawnSandboxWarnings(h, approvalPolicy, harnessBuiltinMode, cwd,
			spawnUsesTclaudeLayer(body.SandboxImplementation))...)
	resolvedLaunch.Info = append(resolvedLaunch.Info,
		harness.SpawnSandboxInfo(h, harnessBuiltinMode)...)
	autoReview, arErr := harness.ResolveAutoReview(h, body.AutoReview)
	if arErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_auto_review", arErr.Error())
		return
	}
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(harnessBuiltinMode, cwd, home) {
		writeError(w, http.StatusBadRequest, "invalid_cwd", fmt.Sprintf(
			"refusing to spawn a %s agent in %q under sandbox %q: it would expose "+
				"~/.tclaude / ~/.codex / ~/.claude to the agent's writes; spawn in a "+
				"project subdirectory or set sandbox %q to opt out",
			h.Name, cwd, harnessBuiltinMode, harness.SandboxDangerFull))
		return
	}

	// Sandbox lineage — the child may not launch with a looser sandbox MODE
	// than the caller currently has (spawn_sandbox_guard.go). Checked HERE,
	// before claimSpawnRateSlot below, so a refused escalation costs the
	// caller no rate slot. executeSpawn re-runs the same (idempotent) check
	// for the other spawn callers (templates/waves/process adapters) that
	// don't pass through this HTTP boundary.
	//
	// It judges the LAUNCH mode, not the harness-native one every gate above
	// judges: a tclaude-layer child records the single-wall posture, and the
	// guard has to reason about the posture that will actually be persisted and
	// later read back when this child spawns children of its own (TCL-989).
	childLaunchSandbox, fieldFail := resolveLaunchHarnessBuiltinMode(
		h, harnessBuiltinMode, body.SandboxImplementation)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	if fail := spawnSandboxLineageFailure(
		spawnerConvID, h.Name, childLaunchSandbox, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := spawnApprovalLineageFailure(spawnerConvID, h.Name, approvalPolicy, autoReview); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	// danger-full-access is Codex's explicit raw no-sandbox launch. It cannot
	// carry the managed permission profile that represents tclaude filesystem
	// policy, so omit every sandbox-profile tier (global, group, and explicit)
	// instead of resolving a policy that must fail capability validation later.
	// The dashboard mirrors this by forcing its selector to the visible "none"
	// state; this server-side rule also covers CLI callers and older tabs.
	effectiveSandbox := sandboxpolicy.OmittedProfilesSnapshot()
	var policyErr error
	if !sandboxProfilesDisabled(h.Name, harnessBuiltinMode, body.SandboxImplementation) &&
		!body.OmitSandboxProfiles {
		effectiveSandbox, policyErr = db.ResolveEffectiveSandboxSnapshot(g.ID, body.SandboxProfile)
	}
	if errors.Is(policyErr, db.ErrSandboxProfileNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile",
			fmt.Sprintf("sandbox profile %q does not exist", body.SandboxProfile))
		return
	}
	if policyErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", policyErr.Error())
		return
	}
	if applied, fail := applySpawnHarnessConfig(effectiveSandbox, body.HarnessConfig); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	} else {
		effectiveSandbox = applied
	}
	if spawnerConvID != "" {
		parentSnapshot, err := db.AgentEffectiveSandboxConfigForConv(spawnerConvID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "load parent sandbox snapshot: "+err.Error())
			return
		}
		if parentSnapshot == nil {
			if sandboxpolicy.HasCapabilities(effectiveSandbox) {
				writeError(w, http.StatusForbidden, "sandbox_profile_restricted",
					"this parent predates effective sandbox snapshots and may not inherit custom capabilities; relaunch it under current policy or ask the human to spawn the child")
				return
			}
		} else {
			validatedParent, err := ensureAgentDirectoriesForRelaunch(*parentSnapshot)
			if err != nil {
				writeError(w, http.StatusConflict, "sandbox_profile_changed", err.Error())
				return
			}
			if err := sandboxpolicy.RequireContained(validatedParent, effectiveSandbox); err != nil {
				writeError(w, http.StatusForbidden, "sandbox_profile_restricted", err.Error())
				return
			}
		}
	}
	if fail := sandboxProfileCapabilityFailure(
		h.Name, harnessBuiltinMode, &effectiveSandbox, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := copilotAPILoopbackFailure(
		copilotAPI, &effectiveSandbox, body.SandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if _, fail := planSandboxProfileAccessForLaunch(
		h.Name, harnessBuiltinMode, &effectiveSandbox, body.SandboxImplementation,
		session.ModelTransportLaunchContext{
			Model: body.Model,
			Cwd:   cwd,
		},
		body.AllowUnenforcedSandbox,
	); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	for _, notice := range effectiveSandbox.Effective.AccessNotices {
		resolvedLaunch.Warnings = append(resolvedLaunch.Warnings, notice.Detail)
	}
	if effectiveSandbox.ProfilesOmitted && sshWorkaround {
		sshWorkaround = false
		body.SSHWorkaround = &sshWorkaround
		resolvedLaunch.Notes = append(resolvedLaunch.Notes,
			"SSH workaround disabled because sandbox profiles were explicitly omitted")
	}
	resolvedLaunch.SandboxPolicy = agent.SummarizeSandboxPolicy(effectiveSandbox)

	// Dir write-proof — the launch-directory half of the spawn sandbox guard
	// (spawn_dir_proof.go). The lineage guard above caps the child's sandbox
	// MODE; this caps its anchor: an agent caller must prove its own sandbox
	// can write in every directory the child would get write access to (the
	// launch cwd, plus the designated worktree when one is passed), otherwise
	// spawning a child there would be a write-permission escape. The gate
	// challenges (403 write_proof_required) or verifies; on success it pins
	// cwd/worktree to the symlink-resolved paths the proof was verified in, so
	// a link swapped after verification cannot retarget the grant. Humans,
	// fully-open callers, and no-cwd-write children (Codex read-only) pass
	// untouched — requireDirWriteProof/childSandboxGrantsDirWrite decide.
	var proofDirs []string
	var proofToken string
	var codexGitCommonDir string
	codexGitCommonDirPinned := spawnUsesPinnedGitCommonDir(
		h.Name, harnessBuiltinMode, body.SandboxImplementation)
	var gitWorktreeWriteDirs []string
	if codexGitCommonDirPinned {
		var gerr error
		codexGitCommonDir, gerr = spawnGitCommonDir(
			h.Name, harnessBuiltinMode, body.SandboxImplementation, cwd)
		if gerr != nil {
			writeError(w, http.StatusInternalServerError, "io", gerr.Error())
			return
		}
		if home, herr := os.UserHomeDir(); herr == nil {
			gitWorktreeWriteDirs = harness.GitWorktreeWriteDirs(cwd, codexGitCommonDir, home)
		}
	}
	autoTrustSiblingWorktree, trustLayoutErr := defaultSiblingWorktreeTrust(h.Name, cwd, codexGitCommonDir)
	if trustLayoutErr != nil {
		writeError(w, http.StatusInternalServerError, "io", trustLayoutErr.Error())
		return
	}
	// A child with no cwd write (Codex read-only) normally needs no proof — it
	// receives no write access to anchor. But the caller-owned trust exemption
	// (spawn_dir_trust.go) justifies itself with a write capability: pre-trust
	// is safe there BECAUSE the caller could already write the harness config
	// that dir carries. So whenever that exemption is what would permit the
	// request, demand the proof regardless of the child's own sandbox — else a
	// read-only caller, which by construction can write nothing, would seed the
	// human's trust store off an unproven claim. The sibling-worktree exemption
	// is excluded: it is forced on precisely so a detached child cannot stall on
	// the trust dialog, and it carries its own proof pairing when the child
	// takes repository grants.
	if spawnerConvID != "" && (childSandboxGrantsDirWrite(
		h.Name, harnessBuiltinMode, body.SandboxImplementation) ||
		(body.TrustDir && !autoTrustSiblingWorktree)) {
		dirs := []string{cwd}
		dirs = appendUniqueDirs(dirs, worktreePath)
		dirs = appendUniqueDirs(dirs, gitWorktreeWriteDirs...)
		for _, grant := range effectiveSandbox.Effective.Filesystem {
			if grant.Access == sandboxpolicy.AccessWrite {
				proofDir, proofErr := sandboxWriteProofDir(grant.Path)
				if proofErr != nil {
					writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", proofErr.Error())
					return
				}
				dirs = appendUniqueDirs(dirs, proofDir)
			}
		}
		resolved, ok := requireDirWriteProof(w, r, spawnerConvID, body.WriteProofToken, dirs)
		if !ok {
			return
		}
		if resolved != nil {
			proofToken = strings.TrimSpace(body.WriteProofToken)
			if v := resolved[cwd]; v != "" {
				cwd = v
			}
			if worktreePath != "" {
				if v := resolved[worktreePath]; v != "" {
					worktreePath = v
				}
			}
			if codexGitCommonDir != "" {
				if v := resolved[codexGitCommonDir]; v != "" {
					codexGitCommonDir = v
				}
			}
			for i, dir := range gitWorktreeWriteDirs {
				if v := resolved[dir]; v != "" {
					gitWorktreeWriteDirs[i] = v
				}
			}
			// Carry the verified, symlink-resolved dirs to executeSpawn, which
			// re-asserts they are still canonical immediately before the fork —
			// closing the window between verification here and the child's
			// launch (a swap after verification is caught, not launched into).
			proofDirs = make([]string, 0, len(dirs))
			for _, raw := range dirs {
				resolvedDir := raw
				if v := resolved[raw]; v != "" {
					resolvedDir = v
				}
				proofDirs = appendUniqueDirs(proofDirs, resolvedDir)
			}
		}
	}

	// Resolve the AskUserQuestion idle-timeout for the chosen harness: a
	// Claude-Code-only settings.json override (never|60s|5m|10m) delivered via
	// `--settings`, so an explicit value for a harness with no AskUserQuestion
	// dialog (Codex) is a 400 here rather than a flag silently dropped. There is
	// no forced default (inherit/blank → "" = no override) — enabling
	// auto-continue for an unattended agent is an explicit per-agent / profile
	// opt-in, already overlaid from the group default profile above.
	askTimeout, atErr := harness.ResolveAskTimeoutMode(h, body.AskUserQuestionTimeout)
	if atErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_ask_user_question_timeout", atErr.Error())
		return
	}

	// Same gate for the auto-compaction window, already normalized and overlaid
	// from the profile tiers above. Re-resolving here keeps this boundary
	// self-sufficient rather than trusting the earlier pass, exactly as the
	// ask-timeout re-resolve does.
	autoCompactWindow, acwErr := harness.ResolveAutoCompactWindow(h, body.AutoCompactWindow)
	if acwErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_auto_compact_window", acwErr.Error())
		return
	}

	// Gate the opt-in dir-trust request: it means something only for a harness
	// with a trust dialog tclaude can pre-seed (Claude Code, Codex) and, unlike
	// sandbox/approval, edits a config tclaude does not own — so requesting it
	// for a harness with no trust dialog is a 400 here rather than a flag
	// silently dropped. Off by default except for a verified tclaude-style
	// sibling worktree, which must be trusted before a detached child starts.
	// See JOH-205 inc4 / JOH-369.
	trustDir, tdErr := harness.ResolveTrustDir(h, body.TrustDir)
	if tdErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_trust_dir", tdErr.Error())
		return
	}
	if autoTrustSiblingWorktree {
		trustDir = true
	}
	if spawnerConvID != "" && trustDir && !autoTrustSiblingWorktree {
		callerOwned, ownErr := callerOwnedDirTrustProved(spawnerConvID, cwd, proofToken, proofDirs)
		if ownErr != nil {
			writeError(w, http.StatusInternalServerError, "io", ownErr.Error())
			return
		}
		if !callerOwned {
			writeError(w, http.StatusForbidden, "trust_dir_restricted", trustDirRestrictedMessage)
			return
		}
	}

	// Gate the explicit "start with remote control" opt-in: it is a Claude Code
	// feature (the --remote-control launch flag), so an EXPLICIT request for a
	// harness with no built-in Remote Access (Codex) is a 400 here rather than a
	// flag silently dropped. body.RemoteControl is tri-state (*bool): only a
	// non-nil request is validated here (the dashboard form always sends one for a
	// Remote-Access-capable harness; the CLI sets &true on opt-in). nil = caller
	// said nothing → the policy stack below fills it. See JOH-258.
	if body.RemoteControl != nil {
		if _, rcErr := harness.ResolveRemoteControl(h, *body.RemoteControl); rcErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_remote_control", rcErr.Error())
			return
		}
	}
	// Layer the spawn-time policy stack (JOH-262, revised): an explicit per-spawn
	// value (the dashboard form / CLI flag) is AUTHORITATIVE — it overrides BOTH
	// the group's remote-control policy AND the group default profile's default,
	// so whatever the spawn form shows decides the spawn state. With it
	// unspecified (nil), the group policy wins, then the profile default, then off.
	// A policy-DERIVED force-on is then clamped to off for a harness with no Remote
	// Access — a group/profile default must not fail a Codex spawn (an EXPLICIT
	// opt-in for Codex already 400'd above). See resolveRemoteControlIntent.
	// Profile bools participate only when their profile harness matches the
	// resolved harness. This gate applies even to false: unlike string catalogs,
	// false validates everywhere and would otherwise shadow a matching lower
	// tier's true.
	//
	// Every tier participates, the NAMED profile included. The CLI still does not
	// fold remote_control in client-side — it cannot see the group's
	// remote-control policy, which must win — but the daemon can and does, so a
	// caller that merely names a profile gets its default under that policy
	// rather than losing it. That is what makes the toggle real for the agentd
	// TUI, which names a profile and sends no flag of its own.
	var profileRemoteControl *bool
	for _, tier := range profileTiers {
		prof := tier.profile
		if prof == nil || prof.RemoteControl == nil {
			continue
		}
		if !profileMatchesHarness(prof, h.Name) {
			resolvedLaunch.Notes = append(resolvedLaunch.Notes,
				fmt.Sprintf("%s remote_control ignored (not valid for %s)", tier.source, h.Name))
			continue
		}
		if _, err := harness.ResolveRemoteControl(h, *prof.RemoteControl); err == nil {
			profileRemoteControl = prof.RemoteControl
			break
		} else {
			writeError(w, http.StatusBadRequest, "invalid_remote_control", fmt.Sprintf("profile %q: %v", prof.Name, err))
			return
		}
	}
	remoteControl := resolveRemoteControlIntent(g.RemoteControl, profileRemoteControl, body.RemoteControl)
	if remoteControl && !h.CanRemoteControl() {
		remoteControl = false
	}

	// Validate an explicit task-reference link at the spawn boundary so a
	// bad URL fails the request with a clear 400, rather than being
	// silently dropped when enrollSpawnedConv tries to persist it. Empty
	// means "no link". The scheme check is the same http(s) guard the
	// standalone task endpoints apply — keeps `javascript:`/`data:` URLs
	// out of the dashboard's Task-column href.
	if u := strings.TrimSpace(body.TaskURL); u != "" {
		if err := validateTaskRefURL(u); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_task_url", err.Error())
			return
		}
		if err := validateTaskRefLabel(strings.TrimSpace(body.TaskLabel)); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_task_label", err.Error())
			return
		}
	}

	// Rate limit (guardrail 3) — claimed only now, after every validation
	// gate: a request refused above (bad arg, sandbox lineage, missing dir
	// write-proof / its challenge round-trip) costs no slot, while anything
	// past this point counts even if the spawn itself then fails — the
	// intended runaway-prevention behaviour.
	if !claimSpawnRateSlot(w, spawnerConvID) {
		return
	}

	// Hand the validated request to the shared spawn core. executeSpawn
	// owns the label → subprocess → conv-id poll → membership →
	// post-init sequence; the group-template instantiator drives the
	// same function in a loop. handleGroupSpawn keeps only the HTTP
	// shape — decode + validate above, error/JSON mapping below.
	p := spawnParams{
		EffectiveSandbox:           &effectiveSandbox,
		Name:                       body.Name,
		Role:                       body.Role,
		Descr:                      body.Descr,
		TaskURL:                    strings.TrimSpace(body.TaskURL),
		TaskLabel:                  strings.TrimSpace(body.TaskLabel),
		InitialMessage:             body.InitialMessage,
		ProfileContext:             profileContext,
		Attachments:                attachments,
		Cwd:                        cwd,
		WorktreePath:               worktreePath,
		WorktreeBranch:             worktreeBranch,
		DirWriteProofDirs:          proofDirs,
		DirWriteProofToken:         proofToken,
		CwdWriteProofToken:         proofToken,
		CleanupDirWriteProof:       true,
		CodexGitCommonDir:          codexGitCommonDir,
		CodexGitCommonDirPinned:    codexGitCommonDirPinned,
		GitWorktreeWriteDirs:       gitWorktreeWriteDirs,
		GitWorktreeWriteDirsPinned: codexGitCommonDirPinned,
		AutoFocus:                  body.AutoFocus,
		AutoFocusWeb:               body.AutoFocusWeb,
		Effort:                     effort,
		Model:                      model,
		Harness:                    h.Name,
		// This boundary resolves a tier applyDefaultProfile cannot see — the CLI's
		// named --profile — so seeding its attributions is what keeps the launch's
		// own echo (and the drive-acquisition log built from it) naming `profile
		// "x"` rather than the "explicit" artifact a re-resolution would produce.
		HarnessSource:               harnessSource,
		ModelSource:                 modelSource,
		EffortSource:                effortSource,
		ContextWindowMaxSource:      contextWindowMaxSource,
		FastModeSource:              fastModeSource,
		SandboxImplementationSource: sandboxImplSource,
		SSHWorkaround:               sshWorkaround,
		SSHWorkaroundSet:            true,
		SSHWorkaroundSource:         sshWorkaroundSource,
		HarnessBuiltinMode:          harnessBuiltinMode,
		HarnessBuiltinModeSource:    sandboxSource,
		SandboxImplementation:       body.SandboxImplementation,
		AllowUnenforcedSandbox:      body.AllowUnenforcedSandbox,
		AskUserQuestionTimeout:      askTimeout,
		ApprovalPolicy:              approvalPolicy,
		ToolGovernance:              toolGovernance,
		AutoReview:                  autoReview,
		AutoReviewSet:               autoReviewSet,
		TrustDir:                    trustDir,
		TrustDirSet:                 trustDirSet,
		RemoteControl:               remoteControl,
		AutoMemory:                  autoMemory,
		ContextFeatures:             contextFeatures,
		AutoCompactWindow:           autoCompactWindow,
		ContextWindowMax:            body.ContextWindowMax,
		CopilotAPI:                  copilotAPI,
		CopilotAPISet:               copilotAPISet,
		CopilotAPISource:            copilotAPISource,
		CodexAppServer:              codexAppServer,
		CodexAppServerSet:           codexAppServerSet,
		CodexAppServerSource:        codexAppServerSource,
		FastMode:                    fastMode,
		FastModeSet:                 fastModeSet,
		ReplyToConv:                 replyToConv,
		SpawnedByConv:               spawnerConvID,
		IsOwner:                     isOwner,
		PermissionOverrides:         permOverrides,
		Timeout:                     timeout,
		// Verbatim decoded request captured at the HTTP boundary, before the
		// working body was resolved or normalized. enrollSpawnedConv persists it
		// as the durable "what did the caller request" record.
		SpawnConfigJSON: requestedSpawnConfigJSON,
		// The HTTP spawn endpoint (dashboard + `tclaude agent spawn`) is
		// non-blocking: a spawn whose conv-id does not materialise within the
		// inline grace becomes a PENDING agent rather than hanging the request
		// — the JOH-205 spawn-freeze fix. The group-template instantiator
		// builds its own params and leaves this false, so it stays synchronous
		// (it needs the conv-id for owner/permission grants).
		Async: true,
	}
	// An omitted include_group_context flag means opt-in — every spawn
	// path inherits the group context by default, the same way it
	// inherits default_cwd; the dashboard sends false explicitly to opt
	// out, and a profile's toggle spoke for it above.
	if includeGroupContext {
		p.GroupContext = g.DefaultContext
	}
	setAuditSpawnResolved(r, spawnAuditResolution(p, resolvedLaunch, namedProfileHandle, map[string]*db.SpawnProfile{
		"explicit":       namedProfile,
		"group_default":  groupProfile,
		"global_default": globalProfile,
	}))

	if beforeExecuteSpawnForTest != nil {
		beforeExecuteSpawnForTest()
	}
	outcome, fail := executeSpawn(g, p)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	// executeSpawn intentionally re-reads default profiles as a safety net for
	// non-HTTP callers. If state changed after the handler snapshot, report the
	// values that actually reached the spawner and label the late fill honestly.
	//
	// Compared against executeSpawn's OWN echo rather than three loose strings,
	// so this covers every echoed field: before TCL-1097 a late fill of the
	// Copilot drive, the sandbox implementation, fast mode or the context cap
	// changed the launch and left this response describing the snapshot.
	// The relabel prefers the tier the final resolution NAMES and falls back to
	// the anonymous launch-default label. A difference is not always a race: for
	// fast_mode an explicit "inherit" clears the Set bit, so the overlay re-reads
	// the profile tiers the handler deliberately suppressed and a group default
	// legitimately wins here (that override is TCL-1109 and is not this echo's
	// doing). Whatever the cause, an operator needs the profile's NAME to go and
	// change it — "default profile (applied at launch)" names no tier at all, and
	// is kept only for a final source that names nothing usable either.
	launched := outcome.Resolved
	if launched == nil {
		launched = &agent.ResolvedLaunch{}
	}
	for field, final := range map[*agent.ResolvedField]agent.ResolvedField{
		&resolvedLaunch.Harness:          launched.Harness,
		&resolvedLaunch.Model:            launched.Model,
		&resolvedLaunch.Effort:           launched.Effort,
		&resolvedLaunch.ContextWindowMax: launched.ContextWindowMax,
		&resolvedLaunch.CopilotAPI:       launched.CopilotAPI,
		&resolvedLaunch.CodexAppServer:   launched.CodexAppServer,
		&resolvedLaunch.FastMode:         launched.FastMode,
		&resolvedLaunch.SandboxImpl:      launched.SandboxImpl,
	} {
		if field.Value != final.Value {
			field.Value = final.Value
			field.Source = agent.ProvLaunchDefault
			if named := strings.TrimSpace(final.Source); named != "" && named != agent.ProvExplicit {
				// Both halves, in the two places built to carry them: the tier goes in
				// Source because that is what an operator acts on, and the fact that it
				// landed late goes in Note because that is what explains the response
				// differing from what the request looked like it would produce.
				field.Source = final.Source
				field.Note = agent.ProvLaunchFillNote
			}
		}
	}
	setAuditSpawnResolved(r, spawnAuditResolution(p, resolvedLaunch, namedProfileHandle, map[string]*db.SpawnProfile{
		"explicit":       namedProfile,
		"group_default":  groupProfile,
		"global_default": globalProfile,
	}))
	resp := map[string]any{
		"group":        g.Name,
		"conv_id":      outcome.ConvID,
		"label":        outcome.Label,
		"tmux_session": outcome.TmuxSession,
		"attach_cmd":   "tclaude session attach " + outcome.Label,
		// Echo the fully-resolved launch shape + per-field provenance so the
		// caller can see WHERE harness/model/effort came from — the TCL-304
		// mistake-preventer for a blank spawn silently inheriting a default
		// profile's vendor. Values come from executeSpawn's final params so the
		// echo remains truthful even if a default profile changed mid-request.
		"resolved": resolvedLaunch,
	}
	// Lead with the spawned actor's stable id. Pending Codex spawns reserve it
	// before their harness conv-id materialises; inline spawns resolve it from
	// the enrolled conversation as before.
	aid := outcome.AgentID
	if aid == "" {
		aid = peerAgentID(outcome.ConvID)
	}
	if aid != "" {
		resp["agent_id"] = aid
	}
	// Echo the requested task-reference link with its verified binding
	// state (TCL-568): "bound" only when the link is readable back off the
	// enrolled actor, "pending" when the spawn went pending (the persisted
	// pending row carries the link; the sweeper binds it at enrollment) or
	// the binding couldn't be confirmed. Never claim linkage that wasn't
	// verifiably written.
	if p.TaskURL != "" {
		resp["task_ref_url"] = p.TaskURL
		resp["task_ref_state"] = taskRefBindState(outcome.ConvID, p.TaskURL)
	}
	// FocusMode is only ever non-empty when the caller asked for auto-focus.
	// "browser" means the dashboard explicitly targeted a web terminal or
	// openTerminal couldn't pop a native window; in either case the spawn modal
	// points at focus_ws (see spawnOutcome.FocusMode).
	if outcome.FocusMode != "" {
		resp["focus_mode"] = outcome.FocusMode
		if outcome.FocusMode == "browser" {
			resp["focus_ws"] = spawnFocusWSPath(outcome.Label)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// spawnParams is the fully-resolved, validated input to executeSpawn.
// handleGroupSpawn builds one from the decoded HTTP body; the
// group-template instantiator builds one per template agent spec.
// Every field is already validated by the time it reaches executeSpawn
// — cwd absolute and existing, worktree path resolved, initial_message
// length/charset-checked, reply-to resolved to a conv-id — so the
// shared core does no HTTP-shaped validation of its own.
type spawnParams struct {
	// AgentID is a stable identity reserved before a pending harness conv-id
	// materialises. Empty on ordinary inline spawns, whose actor is allocated
	// together with the conv-id.
	AgentID string
	// EffectiveSandbox is the immutable additive capability snapshot resolved
	// at the trusted caller boundary. Daemon-managed launch paths pass a
	// versioned snapshot even when it is explicitly empty.
	EffectiveSandbox *sandboxpolicy.Snapshot
	Name             string
	Role             string
	Descr            string
	// TaskURL / TaskLabel are the optional per-agent task-reference link
	// (the dashboard Task column). Validated at the spawn boundary
	// (handleGroupSpawn) and persisted onto the new actor in
	// enrollSpawnedConv. Empty on paths with no task link.
	TaskURL        string
	TaskLabel      string
	InitialMessage string
	// Attachments are absolute file paths (uploaded screenshots / files the
	// dashboard wrote to a temp dir) to surface in the startup briefing as an
	// "Attached files" section, so the agent can Read them on its first turn.
	// Already sanitised at the spawn boundary (handleGroupSpawn); empty for a
	// spawn with no attachments.
	Attachments    []string
	Cwd            string // resolved absolute directory
	WorktreePath   string // resolved absolute directory, or ""
	WorktreeBranch string
	// DirWriteProofDirs are the symlink-resolved directories a dir write-proof
	// (spawn_dir_proof.go) verified for this spawn — the launch cwd and, when
	// present, the worktree. executeSpawn re-asserts each is still canonical
	// (unchanged, no symlink swapped in) immediately before the fork, closing
	// the window between HTTP-boundary verification and the child's launch.
	// Empty for exempt callers (humans, fully-open parents) — nothing to
	// re-assert.
	DirWriteProofDirs    []string
	DirWriteProofToken   string
	CwdWriteProofToken   string
	CleanupDirWriteProof bool
	// Historical field name: both the managed Codex profile and Claude Code's
	// per-session allowWrite overlay consume this pinned repository layout.
	CodexGitCommonDir          string
	CodexGitCommonDirPinned    bool
	GitWorktreeWriteDirs       []string
	GitWorktreeWriteDirsPinned bool
	AutoFocus                  bool
	AutoFocusWeb               bool
	// Effort is the validated Claude reasoning effort to forward to the
	// new session's `tclaude session new --effort`, or "" to omit it.
	Effort string
	// Model is the validated Claude model alias to forward to the new
	// session's `tclaude session new --model`. "" falls back to the
	// group/global default profiles inside executeSpawn (applyDefaultProfile);
	// if those are unset too, the flag is omitted entirely.
	Model string
	// Harness is the resolved harness name to launch ("" or "claude" =
	// Claude Code, the default; "codex" = Codex CLI). It forwards to
	// `tclaude session new --harness <h>` and is validated at the spawn
	// boundary (handleGroupSpawn resolves it against the harness registry
	// before building the params).
	Harness string
	// SSHWorkaround is the already-resolved Codex Git-over-SSH compatibility
	// posture. It is frozen into the durable relaunch profile at enrollment.
	SSHWorkaround bool
	// SSHWorkaroundSet preserves an explicit false through executeSpawn's
	// safety-net profile overlay.
	SSHWorkaroundSet bool
	// HarnessBuiltinMode is the resolved launch sandbox mode for a harness that
	// takes one (Codex: the managed "tclaude-agent" profile by default), or "" to omit the
	// flag (Claude Code, or no sandbox handling). Resolved + cwd-guarded at
	// the spawn boundary (handleGroupSpawn) before building the params; it
	// forwards to `tclaude session new --sandbox <mode>`.
	HarnessBuiltinMode string
	// HarnessBuiltinModeSource names the resolution tier that CHOSE HarnessBuiltinMode — an
	// explicit request field, or the named / group-default / global-default
	// spawn profile that carried it. It forwards to `tclaude session new
	// --sandbox-chosen-by` and is recorded beside the launch's sandbox verdict,
	// so the dashboard badge can attribute the containment to the profile that
	// imposed it instead of to an operator who never chose one. "" omits it.
	HarnessBuiltinModeSource string
	// SandboxImplementation is the resolved owner of OS-level containment:
	// "tclaude-layer" for the experimental tclaude-owned bubblewrap wrapper, or
	// "" / "harness-builtin" for the legacy harness-owned path. It forwards to
	// `tclaude session new --sandbox-impl`, but only when non-default — that is
	// what keeps the feature's default-off invariant visible in the argv itself
	// (see appendSandboxImplementationFlag). Resolved and host-gated at the
	// spawn boundary before the params are built.
	SandboxImplementation string
	DarwinRouteCapable    bool
	DarwinRouteAgentID    string
	// AllowUnenforcedSandbox is the already-authorized dashboard-only decision
	// to widen a closed-network/EnforceNone refusal or omit unsupported network
	// deny entries. It is birth-only:
	// resume, reincarnate, clone, and every non-dashboard spawn path leave it
	// false and therefore retain fail-closed behavior.
	AllowUnenforcedSandbox bool
	// ApprovalPolicy is the resolved launch approval policy for a harness that
	// takes one (Codex: "never" by default — non-escalating so the unattended
	// pane can't deadlock), or "" to omit the flag (Claude Code, or no
	// approval handling). Resolved at the spawn boundary (handleGroupSpawn)
	// before building the params; it forwards to `tclaude session new
	// --ask-for-approval <policy>`. See JOH-200.
	ApprovalPolicy string
	// ToolGovernance is OpenCode's resolved uniform action for the
	// bash/glob/grep/lsp/task/skill block. It forwards as --tools.
	ToolGovernance string
	// AutoReview opts the spawn into the harness's guardian subagent (Codex's
	// `-c approvals_reviewer=auto_review` — auto-decides approval prompts in
	// the human's place), forwarding `--auto-review` to `tclaude session new`.
	// false (the default) leaves the human as reviewer. Gated at the spawn
	// boundary (handleGroupSpawn → harness.ResolveAutoReview) before building
	// the params; experimental/undocumented upstream, so only an explicit
	// opt-in sets it true. See JOH-200 part 2.
	AutoReview bool
	// AutoReviewSet / TrustDirSet preserve a higher tier's explicit false
	// through executeSpawn's safety-net overlay.
	AutoReviewSet bool
	// TrustDir opts the spawn into pre-trusting its launch cwd, forwarding
	// `--trust-dir` to `tclaude session new` so the daemon seeds the harness's
	// trust record before launch and a detached pane doesn't freeze on the
	// trust-folder dialog (JOH-205 for Codex, JOH-369 for Claude Code). false
	// (the default) leaves the dialog in place. Available for any harness with
	// a trust dialog and strictly opt-in (it edits a config tclaude does not
	// own) — gated at the spawn boundary (handleGroupSpawn →
	// harness.ResolveTrustDir) and never set on a relaunch (reincarnate/clone),
	// exactly like AutoReview.
	TrustDir    bool
	TrustDirSet bool
	// RemoteControl arms the new agent's built-in Remote Access at launch
	// (Claude Code's --remote-control), forwarding `--remote-control` to
	// `tclaude session new` so the agent is reachable from the Claude app from
	// its first turn (JOH-258). false (the default) leaves it local. Gated at
	// the spawn boundary (handleGroupSpawn → harness.ResolveRemoteControl); a
	// harness with no Remote Access (Codex) rejects a true value. executeSpawn
	// also tags sessions.remote_control=1 once the row materialises, so the
	// toggle direction logic + dashboard indicator start armed.
	RemoteControl bool
	// AutoMemory keeps Claude Code's auto memory ON for the new agent,
	// forwarding `--auto-memory` to `tclaude session new`. false (the default,
	// and what an unset profile resolves to) instead has the launch inject
	// CLAUDE_CODE_DISABLE_AUTO_MEMORY=1, so agents sharing a repo don't
	// cross-pollute one per-project memory store. Gated at the spawn boundary
	// (handleGroupSpawn → harness.ResolveAutoMemory); a harness with no
	// auto-memory system (Codex) rejects a true value.
	AutoMemory bool
	// ContextFeatures is the resolved per-agent startup-context trim map (slug →
	// "on" | "off"), forwarding `--context-features <slug>=<state>,…` to `tclaude
	// session new`; nil/empty omits the flag so the agent keeps Claude Code's own
	// startup context. Resolved down the profile tier stack at the spawn boundary
	// (handleGroupSpawn → resolveContextFeaturesLaunchField) and harness-gated
	// there; a harness with no steerable startup context (Codex) rejects a
	// non-empty map. See TCL-597.
	ContextFeatures map[string]string
	// AutoCompactWindow is the resolved auto-compaction context capacity in
	// tokens, forwarding `--auto-compact-window <tokens>` to `tclaude session
	// new`; "" omits it so the model's own default threshold decides. A canonical
	// decimal string, resolved down the profile tier stack at the spawn boundary
	// (handleGroupSpawn → harness.ResolveAutoCompactWindow) and harness-gated
	// there; a harness with no such knob (Codex, OpenCode) rejects a value.
	AutoCompactWindow string
	// ContextWindowMax is the configured Copilot context cap used by tclaude's
	// meter. It is launch intent, not the observed context_window_size snapshot,
	// and is deliberately not forwarded as a harness launch argument.
	ContextWindowMax int64
	// CopilotAPI selects the API-backed Copilot drive for the new agent,
	// forwarding `--copilot-api` to `tclaude session new`. false (the default,
	// and what an unset profile chain resolves to) leaves the agent on the tmux
	// send-keys path unchanged. Resolved down the profile tier stack at the spawn
	// boundary (handleGroupSpawn → harness.ResolveCopilotAPI) and harness-gated
	// there; a harness with no API-backed mode rejects a true value. See
	// TCL-1053.
	CopilotAPI bool
	// CopilotAPISet preserves a higher tier's explicit answer through
	// applyDefaultProfileAtLaunch's safety-net overlay — mirroring AutoReviewSet.
	//
	// Load-bearing for the default-off promise, and easy to under-rate: the
	// overlay re-resolves the tier stack, so without this flag an answer of
	// "off" is indistinguishable from "nobody spoke" and a group/global default
	// profile flips a launch the operator explicitly left on send-keys. The
	// dashboard sends exactly that explicit false on every Copilot spawn, so the
	// gap is reachable from the most-used surface, not just a hand-built request.
	CopilotAPISet bool
	// CopilotAPISource / SSHWorkaroundSource name the tier that CHOSE the value
	// beside them, or the harness default when nobody did. resolveBoolLaunchField
	// has always computed this and every caller threw it away; these carry it as
	// far as relaunchProfileForSpawn so it reaches the durable record.
	//
	// The Set bits above cannot serve this purpose. They are consumed and
	// re-derived by executeSpawn's safety-net overlay, so by the time the launch
	// is frozen they answer "does this value survive the overlay", not "did an
	// operator decide it". A from-group snapshot asks the second question and
	// used to read the first (TCL-1090).
	CopilotAPISource     string
	CodexAppServer       bool
	CodexAppServerSet    bool
	CodexAppServerSource string
	CodexStateRoot       string
	CodexStateRootSource string
	SSHWorkaroundSource  string
	// HarnessSource / ModelSource / EffortSource / ContextWindowMaxSource /
	// FastModeSource / SandboxImplementationSource complete the set of
	// attributions the resolved-launch echo renders, alongside CopilotAPISource
	// and HarnessBuiltinModeSource above. Together they are what lets
	// resolveLaunchProvenance build that echo from the params ALONE, inside
	// executeSpawn, for every caller — including the ones that never reach the
	// HTTP boundary where the echo used to be assembled by hand (TCL-1097).
	//
	// A caller that resolved earlier tiers of its own (the template deploy walks
	// the template/role/profile tiers before it ever calls executeSpawn) seeds
	// them here; applyDefaultProfile merges its own answer over the top with
	// preferResolvedSource. A caller that seeds nothing still gets the default
	// tiers named correctly — which is the point: provenance by existing, not by
	// remembering.
	HarnessSource               string
	ModelSource                 string
	EffortSource                string
	ContextWindowMaxSource      string
	FastModeSource              string
	SandboxImplementationSource string
	// LaunchNotes are the disclosures applyDefaultProfile's own resolution
	// produced — today, a default-profile value SKIPPED because it targets a
	// different harness than the launch resolved to.
	//
	// They were discarded for as long as that function has existed, which meant
	// the direct spawn path told an operator "your group default profile's model
	// was ignored, and why" while every other caller said only "harness default",
	// leaving them unable to tell a tier that never spoke from one that spoke and
	// was rejected (found by cold review on TCL-1097). The echo carries them out.
	LaunchNotes []string
	// FastMode/FastModeSet preserve the nullable Codex service-tier choice:
	// unset inherits config.toml, false forces standard, true forces fast.
	FastMode    bool
	FastModeSet bool
	// FastModeAtLaunch records the effective state shown in the dashboard until
	// Codex emits a newer thread-settings event. It never becomes launch intent.
	FastModeAtLaunch *bool
	// AskUserQuestionTimeout is the resolved per-session Claude Code
	// AskUserQuestion idle-timeout override (never|60s|5m|10m), forwarding
	// `--ask-user-question-timeout <v>` to `tclaude session new`; "" omits it.
	// A Claude-Code-only settings.json override (delivered via `--settings`);
	// validated + harness-gated at the spawn boundary (handleGroupSpawn →
	// harness.ResolveAskTimeoutMode). Never defaulted — enabling auto-continue
	// is an explicit per-agent / per-profile / config opt-in.
	AskUserQuestionTimeout string
	// GroupContext is the shared startup context to fold into the
	// briefing, or "" to omit it. The caller has already applied any
	// opt-out, so executeSpawn injects it verbatim.
	GroupContext string
	// ProfileContext is guidance attached to the resolved spawn profile. It is
	// a distinct briefing section so the caller's task and group-context opt-out
	// cannot replace or suppress model-specific instructions.
	ProfileContext string
	// ReplyToConv is the resolved sender of the startup briefing —
	// "" for a human-initiated spawn.
	ReplyToConv string
	// SpawnedByConv is the conv-id of the agent that requested the
	// spawn, or "" for a human-initiated spawn. It drives the kickoff
	// welcome's attribution line — "spawned by <title>" for an agent
	// spawner, "spawned by the human" otherwise. Distinct from
	// ReplyToConv: the spawner is *who launched* the agent, the
	// reply-to is *where its brief-replies route*; a coordinator can
	// hand a worker off by setting them apart.
	SpawnedByConv string
	// ReplyToAgent / SpawnedByAgent are the stable agent_id companions of
	// ReplyToConv / SpawnedByConv (JOH-321 F2), set ONLY on the pending-spawn
	// sweeper path — it reconstructs spawnParams from a persisted row minutes
	// after the spawn, by which time the spawner may have rotated, so the
	// durable agent ref lets the briefing reply-target + welcome attribution
	// re-resolve the spawner's LIVE generation (liveConvForActor) rather than the
	// stale recorded conv. Empty on the synchronous path (the recorded conv IS
	// live), where resolution falls straight back to the conv.
	ReplyToAgent   string
	SpawnedByAgent string
	// IsOwner makes the spawned agent a group owner of the target group at
	// birth. enrollSpawnedConv applies it (best-effort, like the
	// group-template instantiator) right after the membership add, so the new
	// agent comes up already owning the group. false = ordinary member.
	IsOwner bool
	// PermissionOverrides is the new agent's permanent per-slug override set
	// to apply at birth: slug → grant/deny plus an optional canonical scope.
	// enrollSpawnedConv writes each via db.SetAgentPermissionOverrideWithScope
	// after the membership add, best-effort alongside IsOwner. Validated at the
	// spawn boundary (handleGroupSpawn) — every slug registered, every effect
	// in {grant,deny}, every scope canonical and within the granter's own
	// (§ checkGrantAttenuation). nil/empty = inherit the group's default
	// permissions.
	PermissionOverrides map[string]db.PermissionOverride
	// PermissionGranter overrides the ordinary spawn-actor audit label for
	// birth-time permission rows. It is reserved for trusted server-minted
	// correlation such as scribe summon approvals; ordinary spawn requests
	// leave it empty and retain the existing granterLabel(SpawnedByConv) path.
	PermissionGranter string
	// Timeout bounds the conv-id poll; <= 0 falls back to 30s. On the
	// synchronous path it is the hard deadline before a spawn fails; on the
	// Async path the poll is capped at the shorter asyncSpawnInlineGrace
	// before the spawn goes pending.
	Timeout time.Duration
	// Async makes executeSpawn non-blocking: when the conv-id has not
	// materialised within asyncSpawnInlineGrace, instead of failing it records
	// the spawn in pending_spawns and returns a PENDING outcome (empty
	// conv-id) for the sweeper to back-fill. The HTTP spawn endpoint sets it;
	// the group-template instantiator leaves it false so its owner/permission
	// grants on the conv-id keep working. Tradeoff: a gated Codex instantiated
	// via a template therefore still polls the full Timeout and hard-fails —
	// the freeze class is not eliminated on that path — but those grants need
	// the conv-id synchronously, so it stays blocking by design. See JOH-205
	// inc2.
	Async bool
	// pendingSpawnLabel marks the background continuation of a deferred
	// server-authoritative (OpenCode) spawn. The HTTP pass reserved this
	// pending_spawns label (and AgentID) and may already have answered with a
	// Pending row, so the continuation must reuse the label instead of
	// minting one, and must claim/clear the reservation once the conv binds.
	// Empty everywhere else. Unexported on purpose: only
	// executeServerSpawnDeferred sets it.
	pendingSpawnLabel string
	// privateAttachmentRootReserved says the deferred pass atomically claimed
	// pendingSpawnLabel's private root before publishing the Pending row. The
	// continuation may reuse that exact root; every fresh inline spawn must
	// instead create a new root so a label collision cannot cross generations.
	privateAttachmentRootReserved bool
	// privateAttachmentRootCleanup transfers the deferred pass's pre-created
	// root into executeSpawn. Registering it at function entry covers early
	// validation failures; the launch-success flag preserves it after the pane
	// exists, including later enrollment/claim failures.
	privateAttachmentRootCleanup func()
	// SpawnConfigJSON is the JSON of the agent.SpawnRequest this spawn came
	// from, captured at the HTTP boundary (handleGroupSpawn) AFTER its profile
	// tier stack resolves — so it states what the launch actually used, not what
	// the caller happened to type. enrollSpawnedConv records it onto the new
	// actor's agents.initial_spawn_config so there is a durable, agent-level
	// "what was this spawned with" record. Empty on the paths that have no
	// SpawnRequest to snapshot (the pending-spawn sweeper, the group-template
	// instantiator), where the column simply stays "".
	SpawnConfigJSON string
	// ProcessCommandID binds a process-owned spawn to its deterministic
	// command. It is metadata only (never sent through pane injection) and is
	// persisted on the stable agent identity during enrollment.
	ProcessCommandID string
}

// spawnOutcome is the success result of executeSpawn.
type spawnOutcome struct {
	AgentID     string
	ConvID      string
	Label       string
	TmuxSession string
	Harness     string
	Model       string
	Effort      string
	// FocusMode reports what the auto-focus attempt (if AutoFocus was
	// requested) actually did: "" (not requested, or the pane never came
	// up within the poll), "native" (a real GUI terminal window opened),
	// or "browser" (explicit web-terminal target, or no native window could be
	// popped and the caller should fall back to the in-browser terminal, same as
	// handleDashboardOpenWindowAPI's mode:"browser"). Usually set by the
	// focusSpawn closure; a deferred OpenCode response preserves the explicit
	// browser intent before that closure can run.
	FocusMode string
	// Resolved is the launch shape that actually took effect, per field, with the
	// tier that chose it — the same echo handleGroupSpawn renders on the direct
	// spawn path, built here so EVERY caller has one.
	//
	// Hung on the outcome rather than threaded out to the callers that want it:
	// a caller written next year gets provenance by existing rather than by
	// remembering to ask, which is the only version of this property that stays
	// true. It replaces the narrow copilot_api-only Notes channel TCL-1090 landed
	// as a stopgap; that field's one disclosure is now the CopilotAPI field here.
	Resolved *agent.ResolvedLaunch
}

// preferResolvedSource combines the attribution an earlier stage produced with
// the one this stage's re-resolution produced, keeping whichever actually names
// a decider.
//
// It exists because executeSpawn's safety-net overlay re-resolves fields that a
// caller has already resolved, and `resolveBoolLaunchField` reports ProvExplicit
// for anything arriving with its Set bit raised. That answer is an ARTIFACT of
// being asked a second time, not a finding: the template deploy path raises Set
// bits for values it resolved from profile tiers — and, for ssh_workaround, even
// for a pure harness default. Taking it would rewrite "group default profile X"
// or "harness default" as "explicit", which is both false and useless: explicit
// names no tier the operator can go and change.
//
// So a re-resolution wins only when it names a real tier. Otherwise the earlier
// attribution stands — and when neither says anything, the artifact is still
// better than nothing, because ProvHarnessDefault is itself a fact worth keeping.
func preferResolvedSource(existing, resolved string) string {
	if resolved != "" && resolved != agent.ProvExplicit {
		return resolved
	}
	if existing != "" {
		return existing
	}
	return resolved
}

// resolveLaunchProvenance builds the resolved-launch echo from the FINAL spawn
// params — the values that actually reached the spawner, each tagged with the
// tier that chose it.
//
// It is deliberately a function of spawnParams alone, so executeSpawn can call
// it in one place for every caller instead of each caller assembling an echo of
// its own. handleGroupSpawn built one by hand before this existed, which is why
// the template deploy, the wave runner, the process performers and the scribe
// summon reported no provenance at all: not because anyone decided they
// shouldn't, but because none of them was the HTTP boundary (TCL-1097).
//
// The field set matches the direct spawn echo exactly. A deploy echo richer than
// the spawn echo would be a second shape by another name — and the fields the
// echo does NOT cover are a hole in BOTH paths, tracked as TCL-1106 rather than
// papered over on one of them.
func resolveLaunchProvenance(p spawnParams) *agent.ResolvedLaunch {
	contextWindowMax := ""
	if p.ContextWindowMax > 0 {
		contextWindowMax = strconv.FormatInt(p.ContextWindowMax, 10)
	}
	// Named for what it selects, not for the flag that selects it, matching
	// handleGroupSpawn: "api" reads correctly whether or not send-keys is still
	// the other option.
	copilotAPI := ""
	if p.CopilotAPI {
		copilotAPI = "api"
	}
	codexAppServer := ""
	if harnessOrDefault(p.Harness) == harness.CodexName {
		codexAppServer = "send-keys"
		if p.CodexAppServer {
			codexAppServer = "app-server"
		}
	}
	fastMode := ""
	if p.FastModeSet {
		fastMode = harness.FastModeOff
		if p.FastMode {
			fastMode = harness.FastModeOn
		}
	}
	return &agent.ResolvedLaunch{
		// harnessOrDefault, not p.Harness: a launch that named no harness reaches
		// the spawner on the default one, and echoing "" would report a blank for a
		// field that always has an answer.
		Harness:          agent.ResolvedField{Value: harnessOrDefault(p.Harness), Source: p.HarnessSource},
		Model:            agent.ResolvedField{Value: p.Model, Source: p.ModelSource},
		Effort:           agent.ResolvedField{Value: p.Effort, Source: p.EffortSource},
		ContextWindowMax: agent.ResolvedField{Value: contextWindowMax, Source: p.ContextWindowMaxSource},
		CopilotAPI:       agent.ResolvedField{Value: copilotAPI, Source: p.CopilotAPISource},
		CodexAppServer:   agent.ResolvedField{Value: codexAppServer, Source: p.CodexAppServerSource},
		FastMode:         agent.ResolvedField{Value: fastMode, Source: p.FastModeSource},
		SandboxImpl: agent.ResolvedField{
			Value: p.SandboxImplementation, Source: p.SandboxImplementationSource},
		// The disclosures the safety-net overlay produced. Without them the echo
		// reports "harness default" for a field whose group default profile value
		// was consulted and SKIPPED, and the operator cannot tell "no tier spoke"
		// from "a tier spoke and was rejected for this harness".
		Notes: append([]string(nil), p.LaunchNotes...),
	}
}

// copilotDriveAcquisitionLog reports the launch's Copilot drive when something
// other than the caller chose it, naming the tier that did — the one disclosure
// that is logged rather than only rendered.
//
// It reads the echo built above rather than re-deriving the fact, so the log and
// the rendered field cannot drift: two independent computations of one fact is
// how a disclosure goes quietly stale while continuing to render.
//
// Only an acquisition is reported. A launch that stays on send-keys has nothing
// to warn about: send-keys is the known-good path, and a line on every ordinary
// spawn would train the operator to skip the one that matters.
func copilotDriveAcquisitionLog(rl *agent.ResolvedLaunch) string {
	if rl == nil || rl.CopilotAPI.Value == "" ||
		rl.CopilotAPI.Source == agent.ProvExplicit || rl.CopilotAPI.Source == "" {
		return ""
	}
	return fmt.Sprintf("copilot_api: %s (%s)", rl.CopilotAPI.Value, rl.CopilotAPI.Source)
}

// spawnFailure is a typed failure from executeSpawn. The HTTP handler
// maps Status/Kind/Msg straight onto writeError; the template
// instantiator ignores the HTTP-specific fields and reports Msg in its
// per-agent result.
type spawnFailure struct {
	Status int
	Kind   string
	Msg    string
}

// asyncSpawnInlineGrace bounds how long a non-blocking (Async) spawn waits
// for the conv-id before returning a PENDING agent. CC reports its conv-id
// via an immediate launch hook, and a trusted-dir Codex — self-starting its
// first turn from inc1's launch seed — materialises its rollout (and thus
// conv-id) within a second or two; this grace comfortably covers both, so the
// common case still returns a real conv-id inline. A spawn stuck behind a
// startup gate (untrusted dir / new-hooks-config / OpenAI auth modal) blows
// the grace and goes pending instead of hanging the request — the sweeper
// enrolls it once the operator clears the gate. The synchronous template path
// ignores this and keeps the full Timeout.
//
// A var, not a const, so a flow test can shrink it (SetAsyncSpawnInlineGrace-
// ForTest) and drive the pending path without a multi-second real wait.
var asyncSpawnInlineGrace = 6 * time.Second

// beforeExecuteSpawnForTest is a deterministic seam for proving that the
// resolved-shape echo follows the final parameters when default-profile state
// changes between the handler snapshot and executeSpawn's safety-net overlay.
// It is nil and unreachable in production behavior.
var beforeExecuteSpawnForTest func()

// codexAsyncSpawnResponseGrace bounds how long the HTTP spawn endpoint waits
// for a seed-needing harness (Codex) before returning a visible Pending row.
// Codex may still materialise its conv-id a second or two later, but that
// wait should not keep the spawn modal open; a background back-fill continues
// the old inline discovery window after the response returns.
var codexAsyncSpawnResponseGrace = 750 * time.Millisecond

// openCodeAsyncSpawnResponseGrace bounds how long the HTTP spawn endpoint
// waits for a server-authoritative harness (OpenCode) launch to complete
// inline before returning a visible Pending row. Unlike Codex — whose slow
// phase is conv-id materialisation after the fork — OpenCode's dominant cost
// is the managed `opencode serve` boot BEFORE the fork, so the whole launch
// runs in the background and the response only lingers this long. A warm
// healthy server is reused instantly and still answers inline with a real
// conv-id; a cold boot goes Pending and the dashboard shows the reserved row
// immediately instead of holding the spawn dialog open for seconds.
var openCodeAsyncSpawnResponseGrace = 750 * time.Millisecond

// groupDefaultProfile loads the group's default spawn profile (JOH-210), or nil
// when the group has none or the referenced row is missing/unreadable (the
// error is logged, not fatal — the spawn proceeds on its own fields, exactly as
// before the group had a default). Shared by handleGroupSpawn's request overlay
// and executeSpawn's applyDefaultProfile.
func groupDefaultProfile(g *db.AgentGroup) *db.SpawnProfile {
	if g == nil || g.DefaultProfile == "" {
		return nil
	}
	prof, err := db.ResolveSpawnProfile(g.DefaultProfile)
	if err != nil {
		slog.Warn("spawn: failed to load group default profile",
			"group", g.Name, "profile", g.DefaultProfile, "error", err)
		return nil
	}
	if prof == nil {
		slog.Warn("spawn: group default profile no longer exists",
			"group", g.Name, "profile", g.DefaultProfile)
		return nil
	}
	return prof
}

// dashboardDefaultProfilePrefKey is the one persisted value behind the
// dashboard-wide profile picker. Despite its historical dash-pref name it is
// server-side SQLite state, shared across browsers and daemon restarts. Spawn
// resolution promotes that same value to the global default tier so the UI,
// CLI and raw API cannot disagree about which global profile is selected.
const dashboardDefaultProfilePrefKey = "tclaude.dash.default_profile"
const dashboardDefaultProfileIDPrefKey = "tclaude.dash.default_profile_id"

// globalDefaultProfile loads the dashboard/global default spawn profile. A
// stale preference (the profile was subsequently renamed or deleted) is a
// graceful no-op: log it and let the spawn continue to the harness default.
func globalDefaultProfile() *db.SpawnProfile {
	idText, idOK, err := db.GetDashboardPref(dashboardDefaultProfileIDPrefKey)
	if err != nil {
		slog.Warn("spawn: failed to load global default profile id", "error", err)
		return nil
	}
	if idOK {
		id, parseErr := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
		if parseErr != nil {
			slog.Warn("spawn: invalid global default profile id", "value", idText, "error", parseErr)
			return nil
		}
		prof, getErr := db.GetSpawnProfileByID(id)
		if getErr != nil {
			slog.Warn("spawn: failed to load global default profile", "profile_id", id, "error", getErr)
			return nil
		}
		if prof == nil {
			slog.Warn("spawn: global default profile no longer exists", "profile_id", id)
		}
		return prof
	}

	name, ok, err := db.GetDashboardPref(dashboardDefaultProfilePrefKey)
	if err != nil {
		slog.Warn("spawn: failed to load global default profile preference", "error", err)
		return nil
	}
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return nil
	}
	prof, err := db.ResolveSpawnProfile(name)
	if err != nil {
		slog.Warn("spawn: failed to load global default profile", "profile", name, "error", err)
		return nil
	}
	if prof == nil {
		slog.Warn("spawn: global default profile no longer exists", "profile", name)
		return nil
	}
	return prof
}

// resolveRemoteControlIntent computes the effective spawn-time remote-control
// intent from the policy stack (JOH-262, revised). Precedence, highest first:
//
//	explicit per-spawn value  >  group policy (force on/off)  >  profile default  >  off
//
// The explicit per-spawn value is AUTHORITATIVE: the spawn form (dashboard
// checkbox / CLI flag) decides the spawn state, overriding BOTH the group policy
// and the profile default. The group's remote-control policy and the group
// default profile only PRE-FILL the dashboard form (client-side) and serve as
// the SERVER fallback for callers that reach handleGroupSpawn with no explicit
// value (CLI `tclaude agent spawn` without the flag, or `tclaude --join-group`):
// with requested nil, the group policy wins, then the profile default, then off.
// (The group-template instantiator does NOT route through here — it builds its
// spawnParams directly and leaves remote-control off; see instantiate in
// templates.go.)
//
// requested is the already-validated explicit per-spawn value, tri-state (*bool):
// non-nil = the form/flag stated an intent (true OR false); nil = unspecified, so
// the fallback applies. The result is NOT yet harness-clamped: the caller applies
// CanRemoteControl so a policy-derived force-on is silently dropped for a harness
// with no Remote Access (Codex), while an explicit opt-in for such a harness is
// rejected upstream by harness.ResolveRemoteControl.
func resolveRemoteControlIntent(groupPolicy, profileDefault, requested *bool) bool {
	switch {
	case requested != nil:
		return *requested
	case groupPolicy != nil:
		return *groupPolicy
	case profileDefault != nil:
		return *profileDefault
	default:
		return false
	}
}

// harnessOrDefault normalizes a (possibly blank) harness name to a canonical
// name for equality checks: a blank name means the default harness (Claude
// Code), so "" and "claude" compare equal.
func harnessOrDefault(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return harness.DefaultName
	}
	return name
}

type launchProfileTier struct {
	profile *db.SpawnProfile
	source  string
	// defaultTier marks a tier nobody typed at this launch: the group's default
	// spawn profile and the global default profile. Those are ambient
	// configuration, so the harness-pinned fields (see harnessPinnedLaunchField)
	// may only be filled from them when the profile targets the harness this
	// launch actually resolved to.
	defaultTier bool
}

func profileSource(prof *db.SpawnProfile, format func(string) string) string {
	if prof == nil {
		return ""
	}
	return format(prof.Name)
}

func profileMatchesHarness(prof *db.SpawnProfile, harnessName string) bool {
	return prof != nil && harnessOrDefault(prof.Harness) == harnessOrDefault(harnessName)
}

// resolveProfileStartupContext chooses the highest-precedence non-empty
// guidance whose profile belongs to the resolved harness. Free text has no
// catalog validator, so resolveStringLaunchField cannot provide this gate: its
// validator-success path intentionally permits generic cross-harness launch
// values. Startup guidance is not generic—it is attached to a model/profile—so
// a foreign higher tier must be disclosed and skipped.
func resolveProfileStartupContext(harnessName string, tiers []launchProfileTier) (string, string) {
	var notes []string
	for _, tier := range tiers {
		if tier.profile == nil {
			continue
		}
		context := strings.TrimSpace(tier.profile.StartupContext)
		if context == "" {
			continue
		}
		if profileMatchesHarness(tier.profile, harnessName) {
			return context, strings.Join(notes, "; ")
		}
		notes = append(notes, fmt.Sprintf("%s startup_context ignored (not valid for %s)", tier.source, harnessName))
	}
	return "", strings.Join(notes, "; ")
}

const (
	modelField               = "model"
	effortField              = "effort"
	contextWindowMaxField    = "context_window_max"
	includeGroupContextField = "include_group_context"
	autoFocusField           = "auto_focus"
	isOwnerField             = "is_owner"
	permissionOverridesField = "permission_overrides"
	nameField                = "name"
	roleField                = "role"
	roleRefField             = "role_ref"
	descrField               = "descr"
	initialMessageField      = "initial_message"
)

// harnessAgnosticLaunchField reports whether a profile field is policy about
// what the new agent is TOLD or WHO it is rather than about how its harness
// runs. Those fields are inherited from a profile whatever vendor it targets —
// the same rule the CLI's profile merge applies to the harness-agnostic
// toggles. Keyed on the field, inside the resolver, so no resolution path can
// forget it.
func harnessAgnosticLaunchField(field string) bool {
	switch field {
	case includeGroupContextField, autoFocusField, isOwnerField:
		return true
	}
	return false
}

// resolveIdentityLaunchField applies explicit > named profile > group default
// profile > global default profile to one of the spawn dialog's identity fields
// (name / role / descr / initial_message). Identity is harness-agnostic — it
// decides who the new agent IS, not how its harness runs — so a profile
// authored for another vendor still speaks, the same exemption
// include_group_context has.
//
// Precedence is keyed on PRESENCE rather than emptiness. A caller whose form
// HAS the field posts the key and is authoritative even when it posts "" — that
// is what keeps a dashboard operator who clears a profile-prefilled Role from
// having it silently restored. A caller whose form does NOT have the field
// omits it: the agentd TUI has no box for role or descr, so it inherits the
// profile's.
//
// normalize may reject a stored value, because a profile's agent_name is held
// to looser rules than a spawn name. A tier that cannot supply a usable value is
// skipped and disclosed rather than failing the launch — nobody typed it here.
func resolveIdentityLaunchField(
	field, explicitValue string, explicitSet bool,
	tiers []launchProfileTier,
	profileValue func(*db.SpawnProfile) string,
	normalize func(string) (string, bool),
) (value, source, note string) {
	if explicitSet {
		return strings.TrimSpace(explicitValue), agent.ProvExplicit, ""
	}
	var notes []string
	for _, tier := range tiers {
		if tier.profile == nil {
			continue
		}
		raw := strings.TrimSpace(profileValue(tier.profile))
		if raw == "" {
			continue
		}
		if normalize != nil {
			normalized, ok := normalize(raw)
			if !ok {
				notes = append(notes, fmt.Sprintf("%s %s ignored (not a usable %s)",
					tier.source, field, field))
				continue
			}
			raw = normalized
		}
		return raw, tier.source, strings.Join(notes, "; ")
	}
	return "", "", strings.Join(notes, "; ")
}

// resolveRoleRefsLaunchField is the list-valued twin of
// resolveIdentityLaunchField. A role set is one intentional composition: the
// highest tier that specifies any roles wins as a whole rather than merging
// ambient profile roles into an explicit selection.
func resolveRoleRefsLaunchField(body agent.SpawnRequest, tiers []launchProfileTier) ([]string, string) {
	if body.RoleRefsSpecified() || body.RoleRefSpecified() {
		refs := body.RoleRefs
		if !body.RoleRefsSpecified() && strings.TrimSpace(body.RoleRef) != "" {
			refs = []string{body.RoleRef}
		}
		return cleanRoleRefs(refs), agent.ProvExplicit
	}
	for _, tier := range tiers {
		if tier.profile == nil {
			continue
		}
		refs := tier.profile.RoleRefs
		if len(refs) == 0 && strings.TrimSpace(tier.profile.RoleRef) != "" {
			refs = []string{tier.profile.RoleRef}
		}
		if refs = cleanRoleRefs(refs); len(refs) > 0 {
			return refs, tier.source
		}
	}
	return nil, ""
}

func cleanRoleRefs(refs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// resolveOverridesLaunchField is resolveIdentityLaunchField for the birth-time
// permission-override map. The tiers do not merge: the winning profile's map is
// taken whole, for the same reason resolveContextFeaturesLaunchField takes one
// whole — an agent's permissions must be readable off one profile, not
// assembled from its lineage.
func resolveOverridesLaunchField(
	explicitValue map[string]db.PermissionOverride, explicitSet bool,
	tiers []launchProfileTier,
) (value map[string]db.PermissionOverride, source string) {
	if explicitSet {
		return explicitValue, agent.ProvExplicit
	}
	for _, tier := range tiers {
		if tier.profile == nil || len(tier.profile.PermissionOverrides) == 0 {
			continue
		}
		return tier.profile.PermissionOverrides, tier.source
	}
	return nil, ""
}

// launchTierIsDefault reports whether the tier that produced source is one
// nobody typed at this launch (the group's or the global default profile).
// Ambient tiers are treated more gently than direct intent by the birth-time
// access gates below: they fall through rather than refusing the spawn.
func launchTierIsDefault(tiers []launchProfileTier, source string) bool {
	for _, tier := range tiers {
		if tier.profile != nil && tier.source == source {
			return tier.defaultTier
		}
	}
	return false
}

// harnessPinnedLaunchField reports whether a launch field names something that
// belongs to ONE vendor's catalog rather than being a generic launch posture.
// Sandbox/approval/tools/timeouts describe how an agent is contained and are
// deliberately allowed to participate across vendors; a model slug and its
// effort level are not portable, they merely happen to pass a permissive
// validator. Copilot's ValidateModel accepts any bounded single token by design
// (it brokers multi-vendor models with no machine-readable catalog), so a
// Claude-targeted default profile's "opus[1m]" validated cleanly and reached the
// Copilot CLI. The gate is keyed on the FIELD, inside the resolver, so no
// current or future resolution path can forget to apply it.
func harnessPinnedLaunchField(field string) bool {
	return field == modelField || field == effortField || field == contextWindowMaxField
}

// harnessMismatchSkipNote discloses a default tier skipped because the profile
// targets another harness — not because its value failed validation.
func harnessMismatchSkipNote(source, field, profileHarness, harnessName string) string {
	return fmt.Sprintf("%s %s ignored (profile targets %s, launch is %s)",
		source, field, harnessOrDefault(profileHarness), harnessOrDefault(harnessName))
}

// resolveStringLaunchField applies explicit > named > group > global for one
// launch field. Explicit values are direct intent and fail loudly. A profile
// value invalid for a foreign resolved harness is ambient configuration: skip
// it, disclose the skip, and continue to the next tier. A profile claiming the
// resolved harness but carrying an invalid value is self-inconsistent and
// remains a loud error.
//
// Harness-pinned fields (model, effort) additionally refuse to take a value
// from a DEFAULT tier whose profile targets another harness, whether or not the
// value would validate. Default tiers are ambient — nobody typed them at this
// launch — so a harness mismatch there means the value was authored for a
// different vendor and must fall through to the next tier / harness default. An
// explicitly named -p profile is direct intent and keeps participating.
func resolveStringLaunchField(
	field, explicitValue, harnessName string,
	tiers []launchProfileTier,
	profileValue func(*db.SpawnProfile) string,
	validate func(string) (string, error),
) (value, source, note string, fail *spawnFailure) {
	if raw := strings.TrimSpace(explicitValue); raw != "" {
		value, err := validate(raw)
		if err != nil {
			status := http.StatusBadRequest
			if field == sandboxImplementationField {
				status = sandboxImplementationValidationStatus(err)
			}
			return "", "", "", &spawnFailure{status, "invalid_" + field, err.Error()}
		}
		return value, agent.ProvExplicit, "", nil
	}
	var notes []string
	for _, tier := range tiers {
		if tier.profile == nil {
			continue
		}
		raw := strings.TrimSpace(profileValue(tier.profile))
		if raw == "" {
			continue
		}
		if harnessPinnedLaunchField(field) && tier.defaultTier &&
			!profileMatchesHarness(tier.profile, harnessName) {
			notes = append(notes, harnessMismatchSkipNote(
				tier.source, field, tier.profile.Harness, harnessName))
			continue
		}
		value, err := validate(raw)
		if err == nil {
			return value, tier.source, strings.Join(notes, "; "), nil
		}
		if profileMatchesHarness(tier.profile, harnessName) {
			status := http.StatusBadRequest
			if field == sandboxImplementationField {
				status = sandboxImplementationValidationStatus(err)
			}
			return "", "", "", &spawnFailure{status, "invalid_" + field,
				fmt.Sprintf("profile %q: %v", tier.profile.Name, err)}
		}
		notes = append(notes, stringLaunchFieldSkipNote(
			tier.source, field, harnessName, err))
	}
	return "", agent.ProvHarnessDefault, strings.Join(notes, "; "), nil
}

// resolveIntLaunchField is the integer counterpart to
// resolveStringLaunchField. Zero means unset, while a non-zero explicit value
// is direct intent and fails loudly when it does not apply to the chosen
// harness. Profile values from a foreign default tier are ambient configuration
// and fall through, just like model and effort.
func resolveIntLaunchField(
	field string, explicitValue int64, harnessName string,
	tiers []launchProfileTier,
	profileValue func(*db.SpawnProfile) int64,
	validate func(int64) (int64, error),
) (value int64, source, note string, fail *spawnFailure) {
	if explicitValue != 0 {
		value, err := validate(explicitValue)
		if err != nil {
			return 0, "", "", &spawnFailure{http.StatusBadRequest, "invalid_" + field, err.Error()}
		}
		return value, agent.ProvExplicit, "", nil
	}
	var notes []string
	for _, tier := range tiers {
		if tier.profile == nil {
			continue
		}
		raw := profileValue(tier.profile)
		if raw == 0 {
			continue
		}
		if harnessPinnedLaunchField(field) && tier.defaultTier &&
			!profileMatchesHarness(tier.profile, harnessName) {
			notes = append(notes, harnessMismatchSkipNote(
				tier.source, field, tier.profile.Harness, harnessName))
			continue
		}
		value, err := validate(raw)
		if err == nil {
			return value, tier.source, strings.Join(notes, "; "), nil
		}
		if profileMatchesHarness(tier.profile, harnessName) {
			return 0, "", "", &spawnFailure{http.StatusBadRequest, "invalid_" + field,
				fmt.Sprintf("profile %q: %v", tier.profile.Name, err)}
		}
		notes = append(notes, stringLaunchFieldSkipNote(
			tier.source, field, harnessName, err))
	}
	return 0, agent.ProvHarnessDefault, strings.Join(notes, "; "), nil
}

func stringLaunchFieldSkipNote(source, field, harnessName string, validationErr error) string {
	reason := fmt.Sprintf("not valid for %s", harnessName)
	return fmt.Sprintf("%s %s ignored (%s)", source, field, reason)
}

// resolveBoolLaunchField is the tri-state counterpart to
// resolveStringLaunchField. It returns the resolved value, whether any tier
// actually spoke, the tier that supplied it, any disclosure note, and a
// failure. The source is reported for the same reason the string/int resolvers
// report one: a launch echo that says "on" without saying WHO turned it on
// cannot tell an explicit request apart from an inherited default profile.
func resolveBoolLaunchField(
	field string, explicitValue, explicitSet bool, harnessName string,
	tiers []launchProfileTier,
	profileValue func(*db.SpawnProfile) *bool,
	validate func(bool) (bool, error),
) (value, set bool, source, note string, fail *spawnFailure) {
	if explicitSet {
		value, err := validate(explicitValue)
		if err != nil {
			return false, false, "", "", &spawnFailure{http.StatusBadRequest, "invalid_" + field, err.Error()}
		}
		return value, true, agent.ProvExplicit, "", nil
	}
	for _, tier := range tiers {
		if tier.profile == nil || profileValue(tier.profile) == nil {
			continue
		}
		if !harnessAgnosticLaunchField(field) && !profileMatchesHarness(tier.profile, harnessName) {
			if note == "" {
				note = fmt.Sprintf("%s %s ignored (not valid for %s)", tier.source, field, harnessName)
			} else {
				note += "; " + fmt.Sprintf("%s %s ignored (not valid for %s)", tier.source, field, harnessName)
			}
			continue
		}
		value, err := validate(*profileValue(tier.profile))
		if err == nil {
			return value, true, tier.source, note, nil
		}
		return false, false, "", "", &spawnFailure{http.StatusBadRequest, "invalid_" + field,
			fmt.Sprintf("profile %q: %v", tier.profile.Name, err)}
	}
	return false, false, agent.ProvHarnessDefault, note, nil
}

// resolveContextFeaturesLaunchField resolves the startup-context trim map down
// the same tier stack as the scalar launch fields (explicit request → group
// default profile → global default profile), with one deliberate difference: the
// tiers do NOT merge.
//
// The winning tier's map is taken whole. Merging would make the effective
// startup context of an agent a function of every profile in its lineage, which
// is exactly the kind of action-at-a-distance this feature exists to remove — an
// operator reading one profile could not tell what their agent will actually
// load. "The most specific tier that says anything wins, entirely" keeps the
// answer readable from one place.
//
// An explicitly-supplied EMPTY map is still a decision ("trim nothing"), so
// explicitSet is tracked separately from len(explicitValue) and short-circuits
// the tiers just like an explicit false does for a bool field.
func resolveContextFeaturesLaunchField(
	explicitValue map[string]string, explicitSet bool, h *harness.Harness,
	tiers []launchProfileTier,
) (map[string]string, string, *spawnFailure) {
	const field = "context_features"
	if explicitSet {
		value, err := harness.ResolveContextFeatures(h, explicitValue)
		if err != nil {
			return nil, "", &spawnFailure{http.StatusBadRequest, "invalid_" + field, err.Error()}
		}
		return value, "", nil
	}
	var note string
	addNote := func(text string) {
		if note == "" {
			note = text
		} else {
			note += "; " + text
		}
	}
	for _, tier := range tiers {
		if tier.profile == nil || len(tier.profile.ContextFeatures) == 0 {
			continue
		}
		if !profileMatchesHarness(tier.profile, h.Name) {
			addNote(fmt.Sprintf("%s %s ignored (not valid for %s)", tier.source, field, h.Name))
			continue
		}
		value, err := harness.ResolveContextFeatures(h, tier.profile.ContextFeatures)
		if err != nil {
			return nil, "", &spawnFailure{http.StatusBadRequest, "invalid_" + field,
				fmt.Sprintf("profile %q: %v", tier.profile.Name, err)}
		}
		return value, note, nil
	}
	return nil, note, nil
}

// applyDefaultProfile fills blank launch fields on p from the group's default
// spawn profile and then the global default profile, then APPLIES the chosen
// harness's secure launch
// defaults to whatever is still blank and validates the result. A field the
// request already set wins; for a field both the request and the profile leave
// blank, the harness's secure default is applied (e.g. a Codex profile that
// omits sandbox/approval still launches the managed tclaude-agent profile /
// never — NOT an unsandboxed config.toml-driven agent). Returns a typed failure
// if a filled value is invalid for the harness.
//
// The harness resolves independently first. Each profile field is then checked
// against it: compatible generic values still participate across vendors,
// while foreign model/vendor-specific values are skipped and fall through.
//
// This is the SAFETY-NET fill for any caller that reaches executeSpawn WITHOUT
// going through handleGroupSpawn (templates, waves, processes, and scribes).
// handleGroupSpawn itself overlays the profiles onto the request BEFORE
// its own harness/model/sandbox resolution, leaving these fields already
// resolved here — so on that path the fills are no-ops and secure-default
// resolution is idempotent.
func applyDefaultProfile(g *db.AgentGroup, p *spawnParams) *spawnFailure {
	// Both tiers here are default tiers — this path never sees a -p profile —
	// so each is marked as such and carries the same provenance wording
	// handleGroupSpawn uses, keeping any skip note readable if it is ever
	// surfaced on this path (today it is discarded, as for every other field).
	profiles := []struct {
		profile *db.SpawnProfile
		source  func(string) string
	}{
		{groupDefaultProfile(g), agent.ProvGroupProfileSource},
		{globalDefaultProfile(), agent.ProvGlobalProfileSource},
	}
	// Captured BEFORE the tier loop can fill it: afterwards there is no way to
	// tell a harness the caller pinned from one a default profile supplied.
	harnessWasPinned := strings.TrimSpace(p.Harness) != ""
	tiers := make([]launchProfileTier, 0, len(profiles))
	for _, tier := range profiles {
		prof := tier.profile
		if prof != nil {
			if fail := profileSpawnFailure(prof, p.SpawnedByConv); fail != nil {
				return fail
			}
			tierSource := profileSource(prof, tier.source)
			tiers = append(tiers, launchProfileTier{
				profile: prof, source: tierSource, defaultTier: true})
			if strings.TrimSpace(p.Harness) == "" {
				p.Harness = harnessOrDefault(prof.Harness)
				// Attributed where it is decided. The harness does not go through
				// resolveStringLaunchField (it is resolved first, to gate every other
				// field), so this is the only place that knows which tier supplied it.
				p.HarnessSource = tierSource
			}
		}
	}

	// Apply the chosen harness's SECURE launch defaults to any field still
	// blank, and validate — the same resolution handleGroupSpawn runs before
	// building its params. Idempotent on the handleGroupSpawn path (already
	// resolved); the load-bearing case is any other caller that reaches
	// executeSpawn, where this keeps a Codex spawn sandboxed and gives lineage
	// authorization concrete defaults even when no profile participates.
	// Fill the harness attribution only when nothing above spoke: a caller that
	// seeded one (the template deploy names its own tier) keeps it, a caller that
	// merely passed a harness in its params gets the honest "explicit", and a
	// launch nobody steered gets the harness default — never a blank, which reads
	// as "unknown" on a field that is always decided by somebody.
	if strings.TrimSpace(p.HarnessSource) == "" {
		p.HarnessSource = agent.ProvHarnessDefault
		if harnessWasPinned {
			p.HarnessSource = agent.ProvExplicit
		}
	}
	h, err := resolveSpawnHarness(p.Harness)
	if err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_harness", err.Error()}
	}
	var fail *spawnFailure
	// The `source` return of every resolve below used to be discarded here; it is
	// now merged into the params so executeSpawn can echo WHICH TIER decided each
	// field for callers that never saw the HTTP boundary (TCL-1097).
	var fieldSource, fieldNote string
	// Collected rather than discarded: see spawnParams.LaunchNotes. Every resolve
	// below routes its note here, including the fields the echo does not render —
	// a skipped approval or sandbox value is a disclosure whether or not that
	// field has an echoed home of its own (the unrendered nine are TCL-1106).
	noteLaunch := func() {
		if strings.TrimSpace(fieldNote) != "" {
			p.LaunchNotes = append(p.LaunchNotes, fieldNote)
		}
		fieldNote = ""
	}
	p.Model, fieldSource, fieldNote, fail = resolveStringLaunchField(modelField, p.Model, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Model }, h.Models.ValidateModel)
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.ModelSource = preferResolvedSource(p.ModelSource, fieldSource)
	p.Effort, fieldSource, fieldNote, fail = resolveStringLaunchField(effortField, p.Effort, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Effort }, h.Models.ValidateEffort)
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.EffortSource = preferResolvedSource(p.EffortSource, fieldSource)
	p.HarnessBuiltinMode, _, fieldNote, fail = resolveStringLaunchField("sandbox", p.HarnessBuiltinMode, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Sandbox },
		func(raw string) (string, error) { return harness.ValidateHarnessBuiltinMode(h, raw) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.ApprovalPolicy, _, fieldNote, fail = resolveStringLaunchField("approval", p.ApprovalPolicy, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Approval },
		func(raw string) (string, error) { return harness.ValidateApprovalPolicy(h, raw) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.ToolGovernance, _, fieldNote, fail = resolveStringLaunchField("tools", p.ToolGovernance, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.ToolGovernance },
		func(raw string) (string, error) { return harness.ValidateToolGovernance(h, raw) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.AskUserQuestionTimeout, _, fieldNote, fail = resolveStringLaunchField("ask_user_question_timeout", p.AskUserQuestionTimeout, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.AskUserQuestionTimeout },
		func(raw string) (string, error) { return harness.ResolveAskTimeoutMode(h, raw) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.AutoCompactWindow, _, fieldNote, fail = resolveStringLaunchField("auto_compact_window", p.AutoCompactWindow, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.AutoCompactWindow },
		func(raw string) (string, error) { return harness.ResolveAutoCompactWindow(h, raw) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.ContextWindowMax, fieldSource, fieldNote, fail = resolveIntLaunchField(contextWindowMaxField, p.ContextWindowMax, h.Name, tiers,
		func(prof *db.SpawnProfile) int64 { return prof.ContextWindowMax },
		func(raw int64) (int64, error) { return harness.ResolveCopilotContextWindow(h, raw) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.ContextWindowMaxSource = preferResolvedSource(p.ContextWindowMaxSource, fieldSource)
	if strings.TrimSpace(p.ProfileContext) == "" {
		p.ProfileContext, fieldNote = resolveProfileStartupContext(h.Name, tiers)
		noteLaunch()
	}
	p.SandboxImplementation, fieldSource, fieldNote, fail = resolveStringLaunchField(
		sandboxImplementationField, p.SandboxImplementation, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.SandboxImplementation },
		func(raw string) (string, error) { return validateSandboxImplementationForHarness(h, raw) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.SandboxImplementationSource = preferResolvedSource(p.SandboxImplementationSource, fieldSource)
	// The host gate belongs here too, not only at the HTTP boundary. This
	// function is the safety net every non-HTTP caller passes through — the
	// template deploy path builds spawnParams directly — so a group or global
	// default profile carrying tclaude-layer would otherwise reach a spawner on
	// a host that cannot run it without ever meeting a refusal. Same predicate,
	// second call site: an unbypassable gate, not a second opinion.
	if fail := sandboxImplementationHostFailure(h.Name, p.SandboxImplementation); fail != nil {
		return fail
	}
	if fail := sandboxImplementationPostureFailure(h.Name, p.SandboxImplementation); fail != nil {
		return fail
	}
	p.AutoReview, p.AutoReviewSet, _, fieldNote, fail = resolveBoolLaunchField("auto_review", p.AutoReview,
		p.AutoReviewSet || p.AutoReview, h.Name, tiers, func(prof *db.SpawnProfile) *bool { return prof.AutoReview },
		func(v bool) (bool, error) { return harness.ResolveAutoReview(h, v) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.TrustDir, p.TrustDirSet, _, fieldNote, fail = resolveBoolLaunchField("trust_dir", p.TrustDir,
		p.TrustDirSet || p.TrustDir, h.Name, tiers, func(prof *db.SpawnProfile) *bool { return prof.TrustDir },
		func(v bool) (bool, error) { return harness.ResolveTrustDir(h, v) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	var copilotAPISource string
	p.CopilotAPI, p.CopilotAPISet, copilotAPISource, fieldNote, fail = resolveBoolLaunchField("copilot_api", p.CopilotAPI,
		p.CopilotAPISet || p.CopilotAPI, h.Name, tiers, func(prof *db.SpawnProfile) *bool { return prof.CopilotAPI },
		func(v bool) (bool, error) { return harness.ResolveCopilotAPI(h, &v) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.CopilotAPISource = preferResolvedSource(p.CopilotAPISource, copilotAPISource)
	var codexAppServerSource string
	p.CodexAppServer, p.CodexAppServerSet, codexAppServerSource, fieldNote, fail = resolveBoolLaunchField(
		"codex_app_server", p.CodexAppServer, p.CodexAppServerSet || p.CodexAppServer,
		h.Name, tiers, func(prof *db.SpawnProfile) *bool { return prof.CodexAppServer },
		func(v bool) (bool, error) { return harness.ResolveCodexAppServer(h, &v) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.CodexAppServerSource = preferResolvedSource(p.CodexAppServerSource, codexAppServerSource)
	p.FastMode, p.FastModeSet, fieldSource, fieldNote, fail = resolveBoolLaunchField("fast_mode", p.FastMode,
		p.FastModeSet, h.Name, tiers, func(prof *db.SpawnProfile) *bool { return prof.FastMode },
		func(v bool) (bool, error) {
			_, err := harness.ResolveFastMode(h, &v)
			return v, err
		})
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.FastModeSource = preferResolvedSource(p.FastModeSource, fieldSource)
	var sshWorkaroundSource string
	p.SSHWorkaround, p.SSHWorkaroundSet, sshWorkaroundSource, fieldNote, fail = resolveBoolLaunchField(
		"ssh_workaround", p.SSHWorkaround, p.SSHWorkaroundSet, h.Name, tiers,
		func(prof *db.SpawnProfile) *bool { return prof.SSHWorkaround },
		func(v bool) (bool, error) { return harness.ResolveSSHWorkaround(h, &v) })
	if fail != nil {
		return fail
	}
	noteLaunch()
	p.SSHWorkaroundSource = preferResolvedSource(p.SSHWorkaroundSource, sshWorkaroundSource)
	// NB: the block below force-sets SSHWorkaroundSet for the harness default, so
	// that bit says "this launch has a posture" and not "someone chose one" — the
	// same conflation this ticket is about. The attribution above is captured
	// BEFORE it runs and is what the durable record keys on.
	if !p.SSHWorkaroundSet {
		p.SSHWorkaround, err = harness.ResolveSSHWorkaround(h, nil)
		if err != nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_ssh_workaround", err.Error()}
		}
		p.SSHWorkaroundSet = true
	}
	if p.HarnessBuiltinMode, err = harness.ResolveHarnessBuiltinMode(h, p.HarnessBuiltinMode); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_sandbox", err.Error()}
	}
	if p.HarnessBuiltinMode, fail = resolveSandboxImplementationMode(
		h, p.HarnessBuiltinMode, p.SandboxImplementation); fail != nil {
		return fail
	}
	if p.HarnessBuiltinMode != harness.SandboxManagedProfile {
		p.SSHWorkaround = false
	}
	// As at the HTTP boundary: empty HERE (after the profile tiers above) means
	// no flag and no profile chose a posture, so the harness default may be
	// narrowed to one this caller is allowed to grant. A value already resolved
	// by the HTTP boundary arrives non-empty and is left exactly as it is.
	approvalUnset := strings.TrimSpace(p.ApprovalPolicy) == ""
	if p.ApprovalPolicy, err = harness.ResolveApprovalPolicy(h, p.ApprovalPolicy); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_approval", err.Error()}
	}
	if approvalUnset {
		p.ApprovalPolicy = narrowDefaultApprovalToCaller(p.SpawnedByConv, h.Name, p.ApprovalPolicy)
	}
	if p.ToolGovernance, err = harness.ResolveToolGovernance(h, p.ToolGovernance); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_tools", err.Error()}
	}
	if p.AskUserQuestionTimeout, err = harness.ResolveAskTimeoutMode(h, p.AskUserQuestionTimeout); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_ask_user_question_timeout", err.Error()}
	}
	if p.AutoCompactWindow, err = harness.ResolveAutoCompactWindow(h, p.AutoCompactWindow); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_auto_compact_window", err.Error()}
	}
	if p.ContextWindowMax, err = harness.ResolveCopilotContextWindow(h, p.ContextWindowMax); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_context_window_max", err.Error()}
	}
	if p.AutoReview, err = harness.ResolveAutoReview(h, p.AutoReview); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_auto_review", err.Error()}
	}
	if p.TrustDir, err = harness.ResolveTrustDir(h, p.TrustDir); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_trust_dir", err.Error()}
	}
	return nil
}

// executeSpawn runs the validated spawn sequence: it forks a detached
// `tclaude session new`, polls the sessions table for the conv-id, and —
// once the conv-id is known — joins the conv to the group, records the
// pending display name, drops the startup briefing into the new agent's
// inbox, and kicks off the post-init /rename + welcome injection (the
// shared finishSpawnEnrollment tail). It optionally opens a terminal as soon
// as the pane exists. It is the single code path behind the group spawn
// surfaces and ungrouped process-performer spawns.
//
// On the Async path (the HTTP endpoint) a conv-id that does not materialise
// within asyncSpawnInlineGrace does not fail: the spawn is recorded in
// pending_spawns and returned as a PENDING outcome (empty conv-id) for the
// sweeper to enroll later. On the synchronous path (a template instantiator
// or ungrouped process performer, both of which need the conv-id immediately)
// a timeout is still a hard failure.
//
// Returns either an outcome or a typed failure — never both. On an inline
// success the agent is fully spawned and, when a group was supplied,
// group-joined (post-membership best-effort steps — pending name, inbox insert
// — only log on failure); on
// an Async PENDING success the outcome carries an empty conv-id and the agent
// is enrolled later by the sweeper.
func executeSpawn(g *db.AgentGroup, p spawnParams) (outcome *spawnOutcome, failure *spawnFailure) {
	groupName := spawnGroupName(g)
	// Stamped here, once, rather than at each of the six spawnOutcome literals
	// below: an echo that has to be repeated at every return is an echo that will
	// be missing from the seventh, and missing silently. p is read at defer time,
	// so this sees the values applyDefaultProfile resolved.
	defer func() {
		if outcome == nil {
			return
		}
		// An echo already on the outcome is LEFT ALONE. The deferred-spawn path
		// returns an INNER executeSpawn's outcome pointer, and the inner call ran
		// its own applyDefaultProfile — later, against whatever the default
		// profiles said by then, which is the resolution the agent actually
		// launched with. Re-stamping from this frame's older params would replace a
		// truthful echo with a stale one whenever a profile changed inside the
		// deferred window (found by cold review; the earlier version of this
		// comment claimed both frames must agree, which holds only when nothing
		// changed between them).
		if outcome.Resolved == nil {
			outcome.Resolved = resolveLaunchProvenance(p)
		}
		// And the Copilot drive is additionally LOGGED, because the echo above only
		// reaches an operator on callers that render it. The template deploy does;
		// the scribe summon does not (measured: its response is
		// {agent_id, conv_id, focus_mode, focus_ws, name, reused} and nothing more —
		// TCL-1104), and a caller added later inherits silence by default. The
		// drive is unverified; "no agent acquires it silently" has to hold for
		// every path through this function, not just the ones whose result some
		// human actually reads.
		//
		// DO NOT "clean this up" as redundant with the echo now that one exists.
		// It is redundant only for the surfaces that RENDER the echo, and the
		// point of it is the surfaces that do not. It becomes removable when
		// TCL-1104 lands and every caller renders — and that is a decision to make
		// then, naming the surfaces, not a tidy-up to make in passing.
		//
		// DO NOT "clean this up" by threading the disclosure out to each caller
		// either: A SAFETY PROPERTY THAT DEPENDS ON EVERY FUTURE CALLER
		// REMEMBERING IS NOT A SAFETY PROPERTY.
		if disclosure := copilotDriveAcquisitionLog(outcome.Resolved); disclosure != "" {
			slog.Warn("spawn: agent placed on the Copilot API drive by a non-explicit tier",
				"conv", outcome.ConvID, "label", outcome.Label, "group", groupName,
				"disclosure", disclosure)
		}
	}()
	privateAttachmentCleanup := func() {}
	if p.privateAttachmentRootCleanup != nil {
		privateAttachmentCleanup = p.privateAttachmentRootCleanup
	}
	privateAttachmentsLaunched := false
	defer func() {
		if !privateAttachmentsLaunched {
			privateAttachmentCleanup()
		}
	}()
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Proof-marker cleanup is flag-gated (not a plain conditional defer) so
	// the deferred server-authoritative branch below can hand marker
	// ownership to its background continuation — which re-enters
	// executeSpawn with the same CleanupDirWriteProof intent and registers
	// its own cleanup. The markers were consumed at the HTTP boundary
	// (reassertDirWriteProof only re-checks canonical paths); the hand-off
	// exists to keep the launch-scoped lifetime — removal when the launch
	// actually ends, owned exactly once — rather than tying it to a response
	// that now returns mid-launch.
	cleanupProofOnReturn := p.CleanupDirWriteProof
	defer func() {
		if cleanupProofOnReturn {
			cleanupDirWriteProofMarkers(p.DirWriteProofToken, p.DirWriteProofDirs)
		}
	}()

	// Fill blank launch fields from group then global default spawn profiles
	// and apply the harness's secure launch defaults. On the handleGroupSpawn
	// path this is an idempotent no-op (the request overlay already resolved
	// these); it is the safety net for any other caller that reaches
	// executeSpawn with a profile-carrying group, keeping a Codex spawn
	// sandboxed. A value invalid for the harness is a typed failure.
	if fail := applyDefaultProfile(g, &p); fail != nil {
		return nil, fail
	}
	if strings.TrimSpace(p.Name) == "" && groupName != "" {
		p.Name = derivedGroupSpawnName(groupName, time.Now(), randomLabelToken())
	}
	// Defense in depth for template, wave, scribe, and process adapters that
	// call executeSpawn directly instead of passing through handleGroupSpawn.
	if fail := spawnHarnessPolicyFailure(g, p.SpawnedByConv, p.Harness); fail != nil {
		return nil, fail
	}
	// Keep non-HTTP launch paths consistent with handleGroupSpawn. In
	// particular, template/wave callers can arrive with a previously resolved
	// global/group policy even though this agent explicitly selects Codex's raw
	// no-sandbox mode.
	if sandboxProfilesDisabled(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) {
		omitted := sandboxpolicy.OmittedProfilesSnapshot()
		p.EffectiveSandbox = &omitted
	}
	if spawnUsesPinnedGitCommonDir(
		p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) && !p.CodexGitCommonDirPinned {
		gitCommonDir, err := spawnGitCommonDir(
			p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation, p.Cwd)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		p.CodexGitCommonDir = gitCommonDir
		p.CodexGitCommonDirPinned = true
	}
	if p.CodexGitCommonDirPinned && !p.GitWorktreeWriteDirsPinned {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		p.GitWorktreeWriteDirs = harness.GitWorktreeWriteDirs(p.Cwd, p.CodexGitCommonDir, home)
		p.GitWorktreeWriteDirsPinned = true
	}
	if p.GitWorktreeWriteDirsPinned {
		var fail *spawnFailure
		p.GitWorktreeWriteDirs, fail = canonicalizeRepositoryWriteDirs(
			p.GitWorktreeWriteDirs, p.DirWriteProofDirs, p.DirWriteProofToken)
		if fail != nil {
			return nil, fail
		}
	}
	if p.EffectiveSandbox != nil {
		validated, err := sandboxpolicy.RevalidateSnapshot(*p.EffectiveSandbox)
		if err != nil {
			return nil, &spawnFailure{http.StatusConflict, "sandbox_profile_changed", err.Error()}
		}
		p.EffectiveSandbox = &validated
	}
	if fail := sandboxProfileCapabilityFailure(
		p.Harness, p.HarnessBuiltinMode, p.EffectiveSandbox, p.SandboxImplementation); fail != nil {
		return nil, fail
	}
	if session.CodexNativeRegistryApplicable(p.CodexAppServer, harnessOrDefault(p.Harness),
		p.HarnessBuiltinMode, p.SandboxImplementation) {
		if err := codexNativeRegistryReadiness(); err != nil {
			return nil, &spawnFailure{http.StatusPreconditionFailed,
				codexNativeRegistryErrorCode(err), err.Error()}
		}
	}
	if fail := copilotAPILoopbackFailure(
		p.CopilotAPI, p.EffectiveSandbox, p.SandboxImplementation); fail != nil {
		return nil, fail
	}
	if _, fail := planSandboxProfileAccessForLaunch(
		p.Harness, p.HarnessBuiltinMode, p.EffectiveSandbox, p.SandboxImplementation,
		session.ModelTransportLaunchContext{
			Model: p.Model,
			Cwd:   p.Cwd,
		},
		p.AllowUnenforcedSandbox,
	); fail != nil {
		return nil, fail
	}
	if strings.TrimSpace(p.DirWriteProofToken) == "" {
		p.GitWorktreeWriteDirs = nil
		p.GitWorktreeWriteDirsPinned = false
	}
	autoTrustSiblingWorktree, err := defaultSiblingWorktreeTrust(p.Harness, p.Cwd, p.CodexGitCommonDir)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if autoTrustSiblingWorktree {
		p.TrustDir = true
	}
	// After the sibling-worktree auto-trust has had its say, because that is
	// what makes p.TrustDir the value the launch will actually act on. Asking
	// before it would refuse a worktree spawn that is about to be seeded
	// anyway — a refusal derived from a value that had not finished being
	// decided, which is the failure shape this series keeps producing.
	if fail := copilotAPIFolderTrustFailure(p); fail != nil {
		return nil, fail
	}
	if p.SpawnedByConv != "" && p.TrustDir && !autoTrustSiblingWorktree {
		callerOwned, ownErr := callerOwnedDirTrustProved(
			p.SpawnedByConv, p.Cwd, p.DirWriteProofToken, p.DirWriteProofDirs)
		if ownErr != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", ownErr.Error()}
		}
		if !callerOwned {
			return nil, &spawnFailure{http.StatusForbidden, "trust_dir_restricted", trustDirRestrictedMessage}
		}
	}
	// Judges the LAUNCH mode for the same reason handleGroupSpawn does: the
	// guard must reason about the posture the child's row will carry, which for
	// a tclaude-layer child is the forced single-wall mode (TCL-989).
	childHarness, hErr := harness.ResolveSpawnable(harnessOrDefault(p.Harness))
	if hErr != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", hErr.Error()}
	}
	childLaunchSandbox, fail := resolveLaunchHarnessBuiltinMode(
		childHarness, p.HarnessBuiltinMode, p.SandboxImplementation)
	if fail != nil {
		return nil, fail
	}
	if fail := spawnSandboxLineageFailure(
		p.SpawnedByConv, p.Harness, childLaunchSandbox, p.SandboxImplementation); fail != nil {
		return nil, fail
	}
	if fail := spawnApprovalLineageFailure(p.SpawnedByConv, p.Harness, p.ApprovalPolicy, p.AutoReview); fail != nil {
		return nil, fail
	}
	if strings.TrimSpace(p.DirWriteProofToken) == "" {
		p.CodexGitCommonDir = ""
		p.CodexGitCommonDirPinned = false
	}

	// Resolve the harness once for the rest of the spawn. A
	// server-authoritative harness needs its daemon-owned endpoint and
	// server-issued conversation id before enrollment or the pane fork; an
	// empty/unknown --harness yields a nil descriptor and every Supports*/
	// Uses* predicate degrades gracefully.
	spawnHarness, _ := harness.Resolve(p.Harness)

	// Async spawn of a server-authoritative harness (OpenCode): the managed
	// server boot dominates spawn latency — typically seconds, bounded by
	// openCodeStartupTimeout — and it runs BEFORE the pane fork, so the
	// Codex-style conv-id poll cap cannot shorten the response. Instead,
	// after the cheap validations above, reserve the pending row + stable
	// actor id and continue the whole launch in the background. The response
	// waits a short grace so a fast launch (a healthy warm server is reused
	// instantly) still returns its conv-id inline; past the grace the caller
	// gets the same Pending row the dashboard already renders for Codex, and
	// the reservation is claimed once the conv binds (or removed on failure).
	if p.Async && spawnHarness.UsesAuthoritativeServer() && p.pendingSpawnLabel == "" {
		return executeServerSpawnDeferred(g, p, &cleanupProofOnReturn)
	}

	// Generate a label that's unlikely to collide with existing
	// session IDs: crypto-random hex (like GenerateSessionID()), with
	// a "spwn-" prefix so these rows are easy to spot in
	// `tclaude session ls`. With config agent.spawn_label_from_name on it is
	// derived from p.Name instead, disambiguated against taken labels — see
	// spawnLabelSequence.
	//
	// The deferred server-authoritative continuation arrives with its label
	// already reserved (it keys the pending_spawns row the response returned),
	// so that label is authoritative and must not be re-minted.
	layeredLaunch := p.SandboxImplementation ==
		string(sandboxpolicy.ImplementationTclaudeLayer)
	label := p.pendingSpawnLabel
	if label == "" && layeredLaunch {
		nextLabel := spawnLabelSequence(p.Name)
		var reserveErr error
		label, privateAttachmentCleanup, reserveErr =
			reserveUniqueSpawnPrivateAttachmentRootWith(nextLabel)
		if reserveErr != nil {
			return nil, &spawnFailure{
				http.StatusInternalServerError,
				"spawn",
				"could not reserve private attachment root: " + reserveErr.Error(),
			}
		}
	} else if label == "" {
		label = spawnLabelSequence(p.Name)()
	} else if layeredLaunch {
		_, privateRootCreated, prepareErr :=
			tclcommon.PrepareSpawnAttachmentsPrivateDir(label)
		if prepareErr != nil {
			return nil, &spawnFailure{
				http.StatusInternalServerError,
				"spawn",
				"could not prepare private attachment root: " + prepareErr.Error(),
			}
		}
		if !privateRootCreated && !p.privateAttachmentRootReserved {
			return nil, &spawnFailure{
				http.StatusConflict,
				"spawn_label_collision",
				"private attachment root is already owned by another spawn generation",
			}
		}
		privateAttachmentCleanup = func() {
			_ = os.Remove(tclcommon.SpawnAttachmentsPrivateDir(label))
		}
	}
	if layeredLaunch {
		if len(p.Attachments) > 0 {
			promoted, batchCleanup, promoteErr :=
				promoteSpawnAttachments(label, p.Attachments)
			if promoteErr != nil {
				return nil, &spawnFailure{
					http.StatusBadRequest,
					"invalid_attachments",
					"could not promote daemon-staged attachments: " + promoteErr.Error(),
				}
			}
			rootCleanup := privateAttachmentCleanup
			privateAttachmentCleanup = func() {
				batchCleanup()
				rootCleanup()
			}
			p.Attachments = promoted
		}
	}

	// Agent-directory declarations are resolved only once a unique launch key
	// exists. Freeze their literal paths into the snapshot before enrollment or
	// the session-new handoff so every persistence/resume path sees the same
	// values. A failed pre-fork launch removes the newly-created empty tree;
	// once the subprocess starts, the tree belongs to that agent generation.
	agentDirectoryCleanup := func() {}
	agentDirectoriesLaunched := false
	defer func() {
		if !agentDirectoriesLaunched {
			agentDirectoryCleanup()
		}
	}()
	if p.EffectiveSandbox != nil {
		materialized, cleanup, err := prepareCodexSSHWorkaroundForNewLaunch(
			*p.EffectiveSandbox, label, p.SSHWorkaround)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "spawn", err.Error()}
		}
		p.EffectiveSandbox = &materialized
		agentDirectoryCleanup = cleanup
	}
	if p.CodexAppServer && p.AgentID == "" {
		p.AgentID = db.NewAgentID()
	}
	stateRoot, stateRootSource, stateRootErr := codexStateRootForLaunch(p.Harness, p.EffectiveSandbox)
	if stateRootErr != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity, "codex_state_root", stateRootErr.Error()}
	}
	p.CodexStateRoot, p.CodexStateRootSource = stateRoot, stateRootSource
	if harnessOrDefault(p.Harness) == harness.CodexName {
		p.FastModeAtLaunch = codexFastModeAtLaunch(
			fastModeLaunchValue(p.FastMode, p.FastModeSet), p.CodexStateRoot)
	}

	spawnArgs := clcommon.SpawnArgs{
		EffectiveSandbox:           p.EffectiveSandbox,
		Label:                      label,
		AgentID:                    p.AgentID,
		Cwd:                        p.Cwd,
		CwdWriteProof:              p.CwdWriteProofToken,
		CodexGitCommonDir:          p.CodexGitCommonDir,
		CodexGitCommonDirPinned:    p.CodexGitCommonDirPinned,
		GitWorktreeWriteDirs:       p.GitWorktreeWriteDirs,
		GitWorktreeWriteDirsPinned: p.GitWorktreeWriteDirsPinned,
		Effort:                     p.Effort,
		Model:                      p.Model,
		Harness:                    p.Harness,
		Sandbox:                    p.HarnessBuiltinMode,
		SandboxChosenBy:            p.HarnessBuiltinModeSource,
		SandboxImplementation:      p.SandboxImplementation,
		DarwinRouteCapable:         p.DarwinRouteCapable,
		DarwinRouteAgentID:         p.DarwinRouteAgentID,
		AllowUnenforcedSandbox:     p.AllowUnenforcedSandbox,
		AskUserQuestionTimeout:     p.AskUserQuestionTimeout,
		Approval:                   p.ApprovalPolicy,
		ToolGovernance:             p.ToolGovernance,
		AutoReview:                 p.AutoReview,
		TrustDir:                   p.TrustDir,
		RemoteControl:              p.RemoteControl,
		AutoMemory:                 p.AutoMemory,
		ContextFeatures:            p.ContextFeatures,
		AutoCompactWindow:          p.AutoCompactWindow,
		ContextWindowMax:           p.ContextWindowMax,
		CopilotAPI:                 p.CopilotAPI,
		CodexAppServer:             p.CodexAppServer,
		CodexStateRoot:             p.CodexStateRoot,
		FastMode:                   fastModeLaunchValue(p.FastMode, p.FastModeSet),
	}
	routeHelperConvID := ""
	routeHelperGeneration := ""
	routeHelperCommitted := false
	defer func() {
		if routeHelperConvID != "" && !routeHelperCommitted {
			revokeRouteHelperCredentials(routeHelperConvID, routeHelperGeneration)
		}
	}()

	var openCodeLaunch *openCodeLaunch
	if spawnHarness.UsesAuthoritativeServer() {
		resolvedCwd, err := resolveOpenCodeLaunchCwd(p.Cwd)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		p.Cwd = resolvedCwd
		spawnArgs.Cwd = resolvedCwd
		permissionJSON, err := openCodePermissionJSONForLaunch(
			p.Cwd, p.HarnessBuiltinMode, p.ApprovalPolicy, p.ToolGovernance, p.EffectiveSandbox)
		if err != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity, "invalid_opencode_permission_policy",
				"could not build OpenCode access-control policy: " + err.Error()}
		}
		implementation, implementationErr := sandboxpolicy.NormalizeImplementation(
			p.SandboxImplementation)
		if implementationErr != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity,
				"unsupported_sandbox_profile_network", implementationErr.Error()}
		}
		if implementation == sandboxpolicy.ImplementationTclaudeLayer {
			if p.AgentID == "" {
				p.AgentID = db.NewAgentID()
			}
			if _, allocationErr := allocatePrivateOpenCodeState(p.AgentID); allocationErr != nil {
				return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
					"failed to allocate private OpenCode state: " + allocationErr.Error()}
			}
		}
		sandboxSpec, err := openCodeTclaudeLayerLaunchSpec(
			p.SandboxImplementation,
			p.Cwd,
			p.GitWorktreeWriteDirs,
			p.EffectiveSandbox,
			p.AgentID,
			label,
		)
		if err != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network", err.Error()}
		}
		resourceCgroupDir, resourceCleanup, resourceErr := prepareManagedServerResourceCgroup(
			label, p.EffectiveSandbox, p.SandboxImplementation, p.AllowUnenforcedSandbox, false)
		if resourceErr != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity,
				"unsupported_sandbox_profile_resource_limits", resourceErr.Error()}
		}
		openCodeLaunch, err = startOpenCodeRuntimeForSpawn(
			label, p.Cwd, p.Name, "", permissionJSON,
			p.SandboxImplementation, sandboxSpec, resourceCgroupDir)
		if err != nil && resourceCgroupDir != "" &&
			errors.Is(err, errOpenCodeResourceCgroup) &&
			degradeManagedServerResourceCgroup(
				p.EffectiveSandbox, p.SandboxImplementation, p.AllowUnenforcedSandbox, false, err) {
			resourceCleanup()
			resourceCgroupDir = ""
			openCodeLaunch, err = startOpenCodeRuntimeForSpawn(
				label, p.Cwd, p.Name, "", permissionJSON,
				p.SandboxImplementation, sandboxSpec, "")
		}
		if err != nil {
			resourceCleanup()
			return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
				"failed to start managed OpenCode server: " + err.Error()}
		}
		spawnArgs.OpenCodeServerURL = openCodeLaunch.ServerURL
		spawnArgs.OpenCodeServerPassword = openCodeLaunch.Password
		spawnArgs.OpenCodeTransport = openCodeLaunch.Transport
		spawnArgs.OpenCodeControlSocketPath = openCodeLaunch.ControlSocketPath
		spawnArgs.OpenCodeControlSocketDevice = openCodeLaunch.ControlSocketDevice
		spawnArgs.OpenCodeControlSocketInode = openCodeLaunch.ControlSocketInode
		spawnArgs.OpenCodeServerPID = openCodeLaunch.PID
		spawnArgs.ResourceCgroupDir = resourceCgroupDir
		if sandboxSpec != nil {
			spawnArgs.OpenCodeEnvironment = append(
				[]sandboxpolicy.EnvironmentEntry(nil), sandboxSpec.Contract.Environment...)
			spawnArgs.OpenCodeStateIsolation = db.OpenCodeStatePrivate
		}
		spawnArgs.SessionID = openCodeLaunch.ConvID
	}

	// Launch-enrollment path (Claude Code, unless reverted via config): the
	// conv-id can be PRESET, so enroll the agent and bake its rename + welcome
	// into the launch command — no post-connect tmux injection, no conv-id
	// poll-wait. We generate the conv-id, enroll (group membership + inbox
	// briefing) BEFORE the fork (the welcome must reference the briefing's
	// message id), and forward the id/name/welcome as launch args. Harnesses
	// that can't preset a conv-id (Codex) keep the inject-after-connect flow.
	//
	// Resolve (not Get) so a blank p.Harness normalises to the Claude Code
	// default — callers like the template instantiator and the pending-spawn
	// sweeper leave Harness unset, and those CC spawns must take the same
	// launch-enrollment path as the HTTP spawn endpoint. Resolve also tolerates
	// an unknown name (returns nil), and SupportsLaunchEnrollment is nil-safe,
	// so a bad harness degrades to the legacy path rather than panicking.
	launchEnroll := spawnHarness.SupportsLaunchEnrollment() && !spawnUsesLegacyInjection()
	if p.DarwinRouteCapable && !launchEnroll {
		return nil, &spawnFailure{http.StatusUnprocessableEntity, "darwin_route_launch",
			"Darwin route-capable launches require the preset-conversation launch seam"}
	}
	routeEnabled := runtime.GOOS == "darwin" && p.DarwinRouteCapable && g != nil
	if runtime.GOOS == "linux" && g != nil {
		var routeErr error
		routeEnabled, routeErr = db.IsAgentGroupRouteEnabled(g.ID, PermRoutesPublish, PermRoutesConsume)
		if routeErr != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "route_authority", "could not resolve group route capability: " + routeErr.Error()}
		}
		if routeEnabled && (!layeredLaunch || !launchEnroll) {
			return nil, &spawnFailure{http.StatusUnprocessableEntity, "unsupported_group_route_launch", "Linux group routes require a pre-enrolled pane-authoritative tclaude-layer launch"}
		}
	}
	var preConvID string
	var preMsgID int64
	var preActorCreated bool
	// briefingInlined records whether the launch-enrollment prompt baked the
	// whole briefing inline (short enough to fit) rather than pointing at the
	// inbox copy. When it did, the inbox copy is inserted already delivered and
	// read — the agent has the text, so it must never enter the nudge queue.
	var briefingInlined bool
	if launchEnroll {
		if openCodeLaunch != nil {
			preConvID = openCodeLaunch.ConvID
		} else {
			preConvID = convops.GenerateUUID()
		}
		// Decide the briefing's launch state before inserting its inbox copy.
		// An inlined copy must be born delivered + read in the same INSERT;
		// inserting it unread and fixing it up after launch leaves a window where
		// the online-message flush can claim and inject a redundant nudge.
		spawnContextBody := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments)
		inlineCap := spawnInlineMaxChars()
		briefingInlined = spawnContextBody != "" && spawnBriefingFitsLaunch(spawnContextBody, inlineCap)
		mid, actorCreated, fail := enrollSpawnedConv(g, p, preConvID, briefingInlined)
		if fail != nil {
			// Enrollment can fail with partial state already committed (the
			// actor row, membership, task-ref). Unwind it: the conv-id was
			// preset and its pane will never start, so anything left behind is
			// a ghost the operator would have to clear by hand.
			rollbackSpawnEnrollment(g, preConvID, mid, actorCreated)
			if openCodeLaunch != nil {
				_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
			}
			return nil, fail
		}
		preMsgID = mid
		preActorCreated = actorCreated
		spawnArgs.SessionID = preConvID
		if p.DarwinRouteCapable {
			resolvedAgentID, resolveErr := db.AgentIDForConv(preConvID)
			if resolveErr != nil {
				rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
				return nil, &spawnFailure{http.StatusInternalServerError, "darwin_route_launch",
					"resolve Darwin route agent identity: " + resolveErr.Error()}
			}
			if p.DarwinRouteAgentID != "" && p.DarwinRouteAgentID != resolvedAgentID {
				rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
				return nil, &spawnFailure{http.StatusConflict, "darwin_route_launch",
					"darwin route agent identity does not match conversation owner"}
			}
			p.DarwinRouteAgentID = resolvedAgentID
			if p.DarwinRouteAgentID == "" {
				rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
				return nil, &spawnFailure{http.StatusConflict, "darwin_route_launch",
					"Darwin route-capable launch has no stable agent identity"}
			}
			spawnArgs.DarwinRouteAgentID = p.DarwinRouteAgentID
		}
		// Match the legacy path's title gate: a name that isn't a valid rename
		// title is not applied as the launch --name (claude records it as the
		// conversation title), but it is still kept as the pending name (set by
		// enrollSpawnedConv) so the dashboard shows the intended name.
		if p.Name != "" && isValidRenameTitle(p.Name) {
			spawnArgs.Name = p.Name
		} else if p.Name != "" {
			slog.Warn("spawn: name not a valid rename title; skipping launch --name",
				"conv", preConvID, "name", p.Name)
		}
		// Bake the welcome into the launch prompt. When the briefing is short
		// enough it is inlined right after the welcome so the agent acts on its
		// first turn (no `inbox read` round-trip); a long briefing keeps the
		// pointer welcome and stays in the inbox. buildSpawnContextBody is the
		// SAME assembly enrollSpawnedConv stored in the inbox, recomputed here
		// (a cheap pure function of the same inputs) so the inlined copy is
		// byte-identical to the inbox row — no shared mutable state to drift.
		// The inline decision above uses the SAME body and cap the prompt build
		// receives, so the inbox state matches what actually went into the launch
		// turn. The non-empty check keeps briefingInlined strict: an empty
		// briefing fits the launch prompt's clean "wait" welcome but has no inbox
		// row to consume.
		spawnArgs.InitialPrompt = buildSpawnLaunchPrompt(p.Name, p.Role, p.Descr, groupName,
			preMsgID, p.InitialMessage != "", spawnContextBody, p.WorktreePath, p.WorktreeBranch,
			resolveSpawnerTitle(p.SpawnedByConv, p.SpawnedByAgent), inlineCap)
	} else if spawnHarness.NeedsSpawnSeed() {
		// Seed-needing harness (Codex): the conv-id can't be preset, so
		// enrollment + the inbox briefing happen post-connect. But the pane still
		// needs a positional first-turn prompt to materialise its conv-id
		// (JOH-205) — and that prompt IS the [system: ...] welcome, replacing the
		// old inert "[tclaude] …" placeholder. A short/empty briefing rides in
		// full (inline brief, or "wait"), so the agent gets a single greeting
		// turn that looks like the Claude Code launch prompt and the post-connect
		// welcome is skipped (finishSpawnEnrollment gates that on the same
		// spawnBriefingFitsLaunch predicate). A long briefing's seed is a
		// stand-by welcome; its inbox-pointer welcome is injected post-connect,
		// once the inbox row + id exist. No conv-id is known here, so the welcome
		// carries no inbox-message id (msgID 0). (CC on the legacy-injection
		// revert reports its id via hook and needs no seed, so it is excluded.)
		spawnContextBody := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments)
		spawnArgs.InitialPrompt = buildSpawnSeedPrompt(p.Name, p.Role, p.Descr, groupName,
			p.InitialMessage != "", spawnContextBody, p.WorktreePath, p.WorktreeBranch,
			resolveSpawnerTitle(p.SpawnedByConv, p.SpawnedByAgent), spawnInlineMaxChars())
	}
	// A Linux route-capable group must launch the namespace helper with a
	// pre-enrolled identity. If the launch shape cannot prove that identity
	// before the pane starts (for example Codex's seed-based conv-id), refuse
	// the route-enabled launch instead of producing an agent whose advertised
	// routes can never attach.
	if routeEnabled {
		if preConvID == "" {
			rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
			return nil, &spawnFailure{http.StatusInternalServerError, "route_authority", "pre-enrolled route helper conversation identity is empty"}
		}
		agentID, agentErr := db.AgentIDForConv(preConvID)
		if agentErr != nil || strings.TrimSpace(agentID) == "" {
			rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
			if agentErr == nil {
				agentErr = errors.New("stable agent identity is empty")
			}
			return nil, &spawnFailure{http.StatusInternalServerError, "route_authority", "could not resolve route helper identity: " + agentErr.Error()}
		}
		routeCredential, routeGeneration, credentialErr := mintRouteHelperCredential(agentID, preConvID)
		if credentialErr != nil {
			rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
			if openCodeLaunch != nil {
				_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
			}
			return nil, &spawnFailure{http.StatusInternalServerError, "route_authority", "could not mint route helper credential: " + credentialErr.Error()}
		}
		spawnArgs.RouteHelperAgentID = agentID
		spawnArgs.RouteHelperConvID = preConvID
		spawnArgs.RouteHelperLaunchGeneration = routeGeneration
		spawnArgs.RouteHelperCredential = routeCredential
		spawnArgs.RouteHelperGroupIDs = []int64{g.ID}
		spawnArgs.RouteHelperProxyOnly = runtime.GOOS == "darwin"
		routeHelperConvID = preConvID
		routeHelperGeneration = routeGeneration
	}

	// Final dir write-proof re-assertion, as late as possible before the fork:
	// confirm every proof-verified dir is still exactly the canonical path it
	// was verified as (unchanged and not turned into a symlink). This shrinks
	// the verify→launch TOCTOU window to the microscopic gap between this check
	// and the child inheriting the cwd — a swap performed after verification is
	// caught here rather than launched into. Only proof-verified spawns carry
	// DirWriteProofDirs, so human / exempt / no-override spawns are untouched.
	if fail := reassertDirWriteProof(p.DirWriteProofDirs); fail != nil {
		if launchEnroll {
			rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
		}
		if openCodeLaunch != nil {
			_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
		}
		return nil, fail
	}

	// Async harnesses without launch enrollment may return before their conv-id
	// materialises. Reserve and persist the stable actor identity BEFORE the
	// process starts, so an immediate hook/reaper enrollment can only bind this
	// exact id. The row is atomically replaced by the actor binding once the conv
	// appears; a genuinely pending response simply leaves it for back-fill.
	//
	// The deferred server-authoritative continuation already holds its
	// reservation (pendingSpawnLabel) — it must NEVER re-reserve here, even
	// when the legacy-injection revert turns launchEnroll off: re-minting
	// would replace the row (INSERT OR REPLACE) with a second identity while
	// the first was already returned to the caller. pendingHeld is the "some
	// reservation exists for this label" predicate the shared claim/requeue/
	// launch-marker sites key on.
	reservedPending := p.Async && !launchEnroll && p.pendingSpawnLabel == ""
	pendingHeld := reservedPending || p.pendingSpawnLabel != ""
	if reservedPending {
		if g == nil {
			if openCodeLaunch != nil {
				_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
			}
			return nil, &spawnFailure{http.StatusInternalServerError, "spawn", "ungrouped asynchronous spawn is not supported"}
		}
		if p.AgentID == "" {
			p.AgentID = db.NewAgentID()
		}
		if err := db.InsertPendingSpawn(pendingSpawnFromParams(g, p, label)); err != nil {
			if openCodeLaunch != nil {
				_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
			}
			return nil, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to reserve pending spawn " + label + ": " + err.Error()}
		}
	}

	// launchFailed unwinds a spawn whose wrapper reported failure — whether
	// synchronously (the proof-carrying path waits for the wrapper) or via
	// the wrapper-failure signal (the fire-and-forget proofless path, whose
	// reaper goroutine reports a wrapper that died after the fork). Without
	// the signal path, a proofless wrapper failure left executeSpawn polling
	// to timeout and returning the preset conv-id as a success, stranding
	// the pre-fork enrollment as a ghost (cr-1363 finding).
	//
	// No session-row cleanup by label here, deliberately. The forked wrapper
	// deletes its own launch row when it fails after writing it, and
	// rollbackSpawnEnrollment's DeleteAgentByConvID sweeps any row carrying
	// the preset conv-id as a backstop. Deleting by label would be UNSAFE:
	// generateSpawnLabel is 24 random bits with no collision check, and on a
	// label collision the launch fails against a pre-existing LIVE session
	// (runNew's liveOwnerConflict guard) whose row a label-keyed delete
	// would then destroy.
	launchFailed := func(err error) (*spawnOutcome, *spawnFailure) {
		privateAttachmentCleanup()
		privateAttachmentCleanup = func() {}
		if pendingHeld {
			if deleteErr := db.DeletePendingSpawn(label); deleteErr != nil {
				slog.Warn("spawn: failed to remove reservation after launch failure",
					"label", label, "error", deleteErr)
			}
		}
		if launchEnroll {
			// The enrollment ran before the fork; roll it back so a failed
			// launch doesn't strand a group member + orphan briefing.
			rollbackSpawnEnrollment(g, preConvID, preMsgID, preActorCreated)
		}
		if openCodeLaunch != nil {
			_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
		}
		return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: " + err.Error()}
	}

	wrapperFailure := registerWrapperFailureSignal(label)
	defer unregisterWrapperFailureSignal(label)
	// Bound every row/pane observation below to this fork. A random spawn label
	// is deliberately short and can collide with durable predecessor state;
	// recording the boundary before the child starts lets non-preset harnesses
	// reject such a row, while launch enrollment has the stronger conv-id proof.
	launchedAt := time.Now()
	if err := SpawnDetachedTclaudeNew(spawnArgs); err != nil {
		return launchFailed(err)
	}
	agentDirectoriesLaunched = true
	privateAttachmentsLaunched = true
	if openCodeLaunch != nil {
		if err := sendOpenCodePromptForSpawn(openCodeLaunch, p.Cwd,
			spawnArgs.InitialPrompt, p.Model, p.Effort); err != nil {
			return launchFailed(err)
		}
	}

	// Auto-focus closure: when the caller asked for it, open a terminal
	// window attached to the freshly-spawned agent — via `tclaude session
	// attach`, never raw tmux, so the reattached session keeps its tclaude
	// features. A detached spawn has no window of its own, so this is what
	// lets the human watch and talk to the new agent right away and, for a
	// pending Codex spawn, clear whatever startup gate (dir-trust /
	// new-hooks-config / OpenAI auth modal) is holding its first turn.
	//
	// It is label-based and conv-id-independent, so it fires the moment the
	// pane exists — before the conv-id, which is precisely when a gated pane
	// needs a human at it. Fired at most once; best-effort, a failure to pop
	// a window is logged, never bubbled.
	focused := false
	var tmuxSession string
	// focusMode records what focusSpawn actually did, for the three
	// spawnOutcome literals below to report back to the caller — see
	// spawnOutcome.FocusMode. Left "" when AutoFocus is off or the pane
	// never came up within the poll, so focusSpawn never ran.
	focusMode := ""
	focusSpawn := func() {
		// A persisted tmux name is launch intent, not readiness: session new
		// writes it before creating the pane. Every caller must first have
		// observed that live pane and assigned tmuxSession.
		if !p.AutoFocus || focused || tmuxSession == "" {
			return
		}
		focused = true
		if p.AutoFocusWeb {
			// The requesting dashboard will attach the browser terminal from
			// focus_ws in the spawn response. In particular, do not briefly pop
			// a native window before handing the session back to the browser.
			focusMode = "browser"
			return
		}
		if err := openTerminal(openAttachCmd(label)); err != nil {
			// No native window — headless agentd (no DISPLAY/WAYLAND_DISPLAY)
			// or no terminal emulator installed. Don't just log and drop it:
			// report "browser" so the caller (handleGroupSpawn) can point the
			// dashboard at the in-browser terminal fallback, the same
			// mode:"browser" handshake handleDashboardOpenWindowAPI already
			// uses — otherwise auto-focus silently does nothing on a headless
			// host while claiming success.
			slog.Warn("spawn: auto-focus terminal failed to open natively; falling back to in-browser terminal",
				"label", label, "error", err)
			focusMode = "browser"
			return
		}
		focusMode = "native"
	}

	// Poll the sessions table for the conv-id. The hook callback writes it
	// shortly after the harness actually starts inside tmux — for Claude
	// Code that is an immediate SessionStart hook, so this poll wins.
	//
	// Codex fires NO hook until its first user turn. inc1's launch seed makes
	// a trusted-dir Codex self-submit that turn, so its rollout (carrying the
	// session-id) materialises within a second or two and the discovery
	// fallback below resolves the conv-id inline. A Codex held behind a
	// startup gate (untrusted dir / new-hooks-config / OpenAI auth modal)
	// never takes that turn, so its conv-id never materialises — polling it to
	// the full timeout was the JOH-205 spawn-freeze. An Async (dashboard)
	// spawn therefore polls only asyncSpawnInlineGrace before going pending;
	// the synchronous template path keeps the full timeout, since its caller
	// needs the conv-id for owner/permission grants.
	//
	// The harness is resolved once; an empty/unknown --harness yields a nil
	// descriptor and discoverSpawnedConvID no-ops, leaving CC on the hook.
	//
	// On the launch-enrollment path the forked `session new --session-id`
	// stamps the row's conv-id (= preConvID) the moment it writes the session
	// row — so this poll resolves to the preset id on its first iteration,
	// without waiting on the hook. It still polls (rather than skipping
	// straight through) so it confirms the pane actually came up and fires
	// auto-focus, and so a genuine launch failure is caught below.
	pollBudget := timeout
	if p.Async && asyncSpawnInlineGrace < pollBudget {
		pollBudget = asyncSpawnInlineGrace
	}
	backgroundBackfillBudget := pollBudget
	if p.Async && spawnHarness.NeedsSpawnSeed() && codexAsyncSpawnResponseGrace > 0 && codexAsyncSpawnResponseGrace < pollBudget {
		pollBudget = codexAsyncSpawnResponseGrace
	}
	deadline := launchedAt.Add(pollBudget)
	var convID string
	var lastDiscoveryScan time.Time
	remoteArmed := false
	pendingLaunchMarked := false
	for time.Now().Before(deadline) {
		// A wrapper that died after the fork reports here (the proofless path
		// is fire-and-forget, so its failure never comes back as a return
		// value). React exactly like a synchronous launch failure instead of
		// polling out the budget and mistaking the timeout for a slow pane.
		select {
		case werr := <-wrapperFailure:
			return launchFailed(werr)
		default:
		}
		s, err := db.LoadSession(label)
		if err == nil && s != nil {
			if !spawnRowBelongsToLaunch(s, launchEnroll, preConvID, launchedAt) {
				sleepSpawnPoll(deadline)
				continue
			}
			if pendingHeld && !pendingLaunchMarked {
				if err := db.MarkPendingSpawnLaunched(label); err != nil {
					slog.Warn("spawn: failed to clear pending launch marker", "label", label, "error", err)
				} else {
					pendingLaunchMarked = true
				}
			}
			// The session row is written before launchDetachedTmuxSession creates
			// the tmux pane. In particular, a launch-enrolled Claude row already
			// carries both its preset conv-id and tmux name at that point. Treating
			// either field as pane readiness lets a fast terminal (observed with
			// Ghostty on macOS) win the gap: `session attach` sees no tmux session,
			// marks the row exited, and closes before the pane comes up.
			//
			// Publish/focus only after tmux itself proves the pane is alive. Keep
			// polling otherwise; the child may still be between its row write and
			// `tmux new-session`.
			if s.TmuxSession == "" || !session.IsTmuxSessionAlive(s.TmuxSession) {
				// A retained dead pane is definitive startup-failure evidence. Its
				// callback also copies the bounded error tail into the Logs tab before
				// cleanup; fail the spawn response instead of enrolling an offline
				// actor and opening an attach command that can only close.
				if s.TmuxSession != "" {
					if detail, paneID, inspected := startupCorpseExitDetail(s.TmuxSession); inspected {
						// Before launchFailed, which rolls back the enrollment
						// and takes the session row with it.
						logStartupCorpseOutput(label, s.TmuxSession, paneID, detail)
						return launchFailed(fmt.Errorf("managed pane exited during startup (%s); see the Logs tab for its output", detail))
					}
				}
				// The authenticated pane callback records before cleaning the
				// retained corpse. If it won that race, an exit row created during
				// this launch attempt is now the durable failure evidence. The time
				// bound prevents a reused label from matching a predecessor's exit.
				if exits, auditErr := db.ListAuditLog(db.AuditLogFilter{
					Verb: db.AuditVerbAgentExit, SessionID: label, Limit: 1,
				}); auditErr == nil && len(exits) == 1 && !exits[0].At.Before(launchedAt) {
					detail := deadPaneExitDetail(exits[0].ExitCode, exits[0].Signal)
					if detail == "" {
						detail = "unknown exit status"
					}
					return launchFailed(fmt.Errorf("managed pane exited during startup (%s); see the Logs tab for its output", detail))
				}
				// The authenticated pane callback may already have recorded and
				// cleaned the corpse before this poll observed it.
				if s.Status == session.StatusExited {
					return launchFailed(errors.New("managed pane exited during startup; see the Logs tab for its output"))
				}
				sleepSpawnPoll(deadline)
				continue
			}
			tmuxSession = s.TmuxSession
			focusSpawn() // pane is up — open it now, conv-id or not
			// Arm best-known remote-control on the row the moment it
			// materialises (JOH-258). The --remote-control launch flag already
			// turned CC's Remote Access on; this records tclaude's best-known
			// state so the toggle's direction logic + the dashboard indicator
			// start armed. Tagged out-of-band here, NOT in the hook's
			// SaveSession — whose UPSERT must not clobber the flag and which has
			// no spawn intent (JOH-256). Done once; a write failure is logged,
			// not fatal: the launch flag already armed CC, so a missed tag is a
			// best-known-state drift the human can re-toggle, never a broken spawn.
			if spawnArgs.RemoteControl && !remoteArmed {
				if err := db.SetSessionRemoteControl(label, true); err != nil {
					slog.Warn("spawn: failed to arm remote-control on session row",
						"label", label, "error", err)
				} else {
					remoteArmed = true
				}
			}
			if s.ConvID != "" {
				convID = s.ConvID
				break
			}
		}
		// Fallback for a lazy-hook harness: once a pane exists but no hook has
		// reported a conv-id within the grace, ask the harness conv store.
		// Throttled so the tree-walking scan doesn't run every 250ms. Skipped on
		// the launch-enrollment path: that conv-id was preset (preConvID), so
		// the scan could only ever rediscover it — or, worse, pick a sibling
		// .jsonl in a busy shared cwd.
		if !launchEnroll && tmuxSession != "" && time.Since(launchedAt) >= convStoreDiscoveryGrace &&
			time.Since(lastDiscoveryScan) >= convStoreDiscoveryScanInterval {
			lastDiscoveryScan = time.Now()
			if id := discoverSpawnedConvID(spawnHarness, p.Cwd, launchedAt); id != "" {
				if err := db.SetSessionConvID(label, id); err != nil {
					slog.Warn("spawn: failed to persist discovered conv-id",
						"label", label, "conv", id, "error", err)
				}
				convID = id
				break
			}
		}
		sleepSpawnPoll(deadline)
	}

	// Launch-enrollment path: the conv-id was PRESET and enrollment ran before
	// the fork. Return it only after tmux has proved a live pane. A slow or
	// missing pane remains enrolled because it may still materialise; rolling
	// that state back could strand a live, named, greeted, group-less pane. But
	// it is reported as unconfirmed, never as a successful attachable spawn.
	if launchEnroll {
		// Final wrapper-failure check: distinguish a reported wrapper failure
		// from a pane that is merely too slow to confirm. A wrapper that fails
		// after this point is out of signal reach; the unconfirmed path below
		// preserves its enrollment so the operator can inspect or retire it.
		select {
		case werr := <-wrapperFailure:
			return launchFailed(werr)
		default:
		}
		// Copilot launch enrollment delivers the validated name as a native
		// launch argument rather than through deliverRename. Mirror that accepted
		// title into the shared conversation index now, just as its rename path
		// does. Copilot's cold ConvStore sync is demand-driven, so without this
		// write a long-lived named agent can retain an empty custom_title until
		// somebody happens to run a conversation listing. Claude Code is excluded:
		// its transcript follower owns this cache, and a speculative stamp can
		// mask a subsequent authoritative rename until another filesystem refresh.
		if spawnHarness.Name == harness.CopilotName && spawnArgs.Name != "" {
			cacheDeliveredTitle(preConvID, spawnArgs.Name, spawnHarness.Name)
		}
		// The row may have landed just after the last poll iteration. Preserve
		// the final best-effort focus, but apply the same tmux-readiness proof as
		// the loop above; a late row alone is still not a pane.
		if tmuxSession == "" {
			if s, err := db.LoadSession(label); err == nil && s != nil &&
				spawnRowBelongsToLaunch(s, launchEnroll, preConvID, launchedAt) &&
				s.TmuxSession != "" && session.IsTmuxSessionAlive(s.TmuxSession) {
				tmuxSession = s.TmuxSession
			}
		}
		if tmuxSession == "" {
			// The child may merely be slow, so preserve its pre-fork enrollment,
			// briefing, launch-owned directories, and route credentials. But an
			// unobserved pane is not a successful spawn: returning a conv-id here
			// makes fast terminal clients attach to a session that may never exist.
			routeHelperCommitted = true
			markBriefingConsumed(preConvID, preMsgID, briefingInlined)
			return nil, &spawnFailure{http.StatusGatewayTimeout, "spawn_unconfirmed",
				"launch enrollment was preserved, but no live tmux pane appeared before the startup deadline"}
		}
		focusSpawn()
		markBriefingConsumed(preConvID, preMsgID, briefingInlined)
		// Deferred server-authoritative continuation: atomically replace the
		// pending reservation with the (already-made) actor binding, so the
		// dashboard's Pending row promotes into the enrolled agent. A claim
		// miss is benign — the sweeper saw the session row first and cleared
		// the reservation against the same enrollment; a claim error leaves
		// the row for the sweeper's idempotent already-enrolled path.
		if p.pendingSpawnLabel != "" {
			if _, err := db.ClaimPendingSpawnAndBindAgent(label, preConvID, p.AgentID, "spawn"); err != nil {
				slog.Warn("spawn: failed to claim deferred pending reservation; leaving it for the sweeper",
					"label", label, "conv", preConvID, "error", err)
			}
		}
		// The tmux name is the label ONLY until `session new` has to
		// disambiguate it (UniqueTmuxSessionName appends "-N" when the base is
		// already alive). That is unreachable for a random label but reachable
		// for a name-derived one racing a tmux session created after the label
		// was minted, so prefer the name the child actually recorded on the
		// session row; the label is the fallback for a row that has not landed
		// yet, which is what this branch is written for.
		outcomeTmux := tmuxSession
		if outcomeTmux == "" {
			outcomeTmux = label
		}
		routeHelperCommitted = true
		return &spawnOutcome{AgentID: p.AgentID, ConvID: preConvID, Label: label, TmuxSession: outcomeTmux, FocusMode: focusMode,
			Harness: p.Harness, Model: p.Model, Effort: p.Effort}, nil
	}

	// Conv-id resolved within the poll: finish enrollment inline (Codex, or CC
	// with the legacy-injection revert flag) and inject the rename + welcome.
	if convID != "" {
		if p.CodexAppServer && !awaitCodexAppServerLaunchReady(convID, label) {
			stopFailedCodexAppServerLaunch(convID, label, label)
			return nil, &spawnFailure{http.StatusServiceUnavailable, "codex_app_server_unavailable",
				"Codex app-server was explicitly selected but the verified control handle did not become ready"}
		}
		if pendingHeld {
			claimed, err := db.ClaimPendingSpawnAndBindAgent(label, convID, p.AgentID, "spawn")
			if err != nil {
				return nil, &spawnFailure{http.StatusInternalServerError, "identity",
					"failed to bind reserved agent " + p.AgentID + " to spawned conv " + convID + ": " + err.Error()}
			}
			if !claimed {
				// A concurrent pending back-fill claimed the row and owns the
				// one-shot enrollment side effects. The actor binding is already
				// visible, so returning its identity is safe.
				bound, lookupErr := db.AgentIDForConv(convID)
				if lookupErr != nil || bound != p.AgentID {
					detail := fmt.Sprintf("reservation disappeared before binding (bound=%q)", bound)
					if lookupErr != nil {
						detail = lookupErr.Error()
					}
					return nil, &spawnFailure{http.StatusConflict, "identity",
						"pending spawn " + label + " was canceled or conflicted: " + detail}
				}
				return &spawnOutcome{AgentID: p.AgentID, ConvID: convID, Label: label, TmuxSession: tmuxSession, FocusMode: focusMode,
					Harness: p.Harness, Model: p.Model, Effort: p.Effort}, nil
			}
		}
		if fail := finishSpawnEnrollment(g, p, convID); fail != nil {
			if pendingHeld {
				// The agent process is already running and its identity bound;
				// a hard failure here would tell the caller the spawn failed —
				// the CLI would even remove a just-created worktree under the
				// live agent — and the claimed reservation would be gone, so
				// nothing would ever retry. Requeue the durable intent instead
				// (the sweeper re-runs the enrollment tail exactly as after a
				// daemon restart) and report the spawn with its real conv-id;
				// the response's task_ref_state read-back stays honest about
				// anything not yet bound.
				ps := pendingSpawnFromParams(g, p, label)
				ps.Launching = false
				requeuePendingSpawn(label, ps)
				slog.Warn("spawn: inline enrollment failed; requeued for sweeper back-fill",
					"label", label, "conv", convID, "error", fail.Msg)
				return &spawnOutcome{AgentID: p.AgentID, ConvID: convID, Label: label, TmuxSession: tmuxSession, FocusMode: focusMode,
					Harness: p.Harness, Model: p.Model, Effort: p.Effort}, nil
			}
			return nil, fail
		}
		return &spawnOutcome{AgentID: p.AgentID, ConvID: convID, Label: label, TmuxSession: tmuxSession, FocusMode: focusMode,
			Harness: p.Harness, Model: p.Model, Effort: p.Effort}, nil
	}

	// Conv-id did not materialise within the poll. An Async (dashboard) spawn
	// records its full enrollment intent in pending_spawns and returns a
	// PENDING outcome (empty conv-id) — the operator can already see + focus
	// the pane (auto-focus fired above as soon as it came up) to clear the
	// gate, and the sweeper back-fills the enrollment once the conv-id
	// appears. Restart-safe: the row carries everything finishSpawnEnrollment
	// needs.
	if p.Async {
		focusSpawn() // belt-and-suspenders: open the pane even if it came up slow
		ps, err := db.GetPendingSpawn(label)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to verify pending spawn reservation " + label + ": " + err.Error()}
		}
		if ps == nil || ps.AgentID != p.AgentID {
			return nil, &spawnFailure{http.StatusConflict, "identity",
				"pending spawn " + label + " was canceled before its response"}
		}
		slog.Info("spawn: conv-id not yet materialised; recorded pending spawn",
			"label", label, "group", g.Name, "harness", p.Harness)
		if spawnHarness.NeedsSpawnSeed() && backgroundBackfillBudget > pollBudget {
			goBackground(func() {
				backfillPendingSpawnInline(g, p, label, spawnHarness, launchedAt, backgroundBackfillBudget)
			})
		}
		return &spawnOutcome{AgentID: p.AgentID, ConvID: "", Label: label, TmuxSession: tmuxSession, FocusMode: focusMode,
			Harness: p.Harness, Model: p.Model, Effort: p.Effort}, nil
	}

	// Synchronous (template or process-performer) path: the caller needs the
	// conv-id now, so a timeout is a hard failure — unchanged from before inc2.
	return nil, &spawnFailure{http.StatusGatewayTimeout, "timeout",
		"spawned session " + label + " but conv-id never materialised within " + pollBudget.String() +
			" — the session may still come up; check `tclaude session attach " + label + "`"}
}

// derivedGroupSpawnName gives unnamed group agents a readable, valid title.
// The time keeps the operator's requested <group>-<date>-<HHmm> shape; a short
// random tail prevents a wave of same-minute spawns from becoming ambiguous.
func derivedGroupSpawnName(group string, now time.Time, token string) string {
	base := agent.NormalizeSpawnName(group)
	if base == "" {
		base = "agent"
	}
	token = agent.NormalizeSpawnName(token)
	if len(token) > 4 {
		token = token[:4]
	}
	if token == "" {
		token = "spawn"
	}
	suffix := now.Format("20060102-1504") + "-" + token
	budget := agent.MaxSpawnNameLen - len(suffix) - 1
	if len(base) > budget {
		base = strings.TrimRight(base[:budget], "-")
	}
	return base + "-" + suffix
}

func spawnRowBelongsToLaunch(s *db.SessionRow, launchEnroll bool, preConvID string, launchedAt time.Time) bool {
	if s == nil {
		return false
	}
	if launchEnroll {
		return preConvID != "" && s.ConvID == preConvID
	}
	return !s.CreatedAt.IsZero() && !s.CreatedAt.Before(launchedAt)
}

// executeServerSpawnDeferred runs an async server-authoritative (OpenCode)
// spawn without holding the HTTP response open for the managed server boot.
// The caller (executeSpawn) has already run every synchronous validation; this
// adds the two cheap OpenCode-specific pre-flights that should still fail the
// spawn dialog synchronously, reserves the pending_spawns row + stable actor
// id, and re-enters executeSpawn in the background with pendingSpawnLabel set.
// The response waits openCodeAsyncSpawnResponseGrace for that continuation:
// a fast launch (warm server reuse) returns its real outcome inline; past the
// grace the caller gets a Pending outcome (empty conv-id) and the continuation
// finishes on its own — claiming the reservation on success, or deleting it
// and surfacing the failure to the operator on error.
//
// syncProofCleanup is executeSpawn's proof-marker ownership flag: it is
// cleared the moment the continuation is scheduled, because the continuation
// re-registers the same cleanup — marker removal stays launch-scoped (it
// happens when the launch actually ends) and owned exactly once, instead of
// racing a response that returns mid-launch. Pre-flight failures return
// before that hand-off, leaving the synchronous cleanup in place.
func executeServerSpawnDeferred(g *db.AgentGroup, p spawnParams, syncProofCleanup *bool) (*spawnOutcome, *spawnFailure) {
	if g == nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "spawn", "ungrouped asynchronous spawn is not supported"}
	}
	// Cheap, deterministic pre-flights — a bad launch cwd or an unbuildable
	// access-control policy must fail the dialog now, exactly as the inline
	// path would, before anything durable is written.
	resolvedCwd, err := resolveOpenCodeLaunchCwd(p.Cwd)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	p.Cwd = resolvedCwd
	if _, err := openCodePermissionJSONForLaunch(
		p.Cwd, p.HarnessBuiltinMode, p.ApprovalPolicy, p.ToolGovernance, p.EffectiveSandbox); err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity, "invalid_opencode_permission_policy",
			"could not build OpenCode access-control policy: " + err.Error()}
	}
	p.AgentID = db.NewAgentID()
	implementation, err := sandboxpolicy.NormalizeImplementation(p.SandboxImplementation)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_network", err.Error()}
	}
	if implementation == sandboxpolicy.ImplementationTclaudeLayer {
		if _, err := allocatePrivateOpenCodeState(p.AgentID); err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
				"failed to allocate private OpenCode state: " + err.Error()}
		}
	}
	if _, err := openCodeTclaudeLayerLaunchSpec(
		p.SandboxImplementation, p.Cwd, p.GitWorktreeWriteDirs, p.EffectiveSandbox,
		p.AgentID); err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_network", err.Error()}
	}

	// The sequence is consumed at most once per spawn: either the layered
	// reservation walks it until it creates a private root exclusively, or the
	// plain path takes its first free candidate. Calling it here as well would
	// burn a candidate and push a name-derived label to its "-2" form for no
	// reason.
	nextLabel := spawnLabelSequence(p.Name)
	var label string
	privateRootCleanup := func() {}
	if p.SandboxImplementation == string(sandboxpolicy.ImplementationTclaudeLayer) {
		var reserveErr error
		label, privateRootCleanup, reserveErr =
			reserveUniqueSpawnPrivateAttachmentRootWith(nextLabel)
		if reserveErr != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
				"could not reserve private attachment root: " + reserveErr.Error()}
		}
	} else {
		label = nextLabel()
	}
	if err := db.InsertPendingSpawn(pendingSpawnFromParams(g, p, label)); err != nil {
		privateRootCleanup()
		return nil, &spawnFailure{http.StatusInternalServerError, "io",
			"failed to reserve pending spawn " + label + ": " + err.Error()}
	}

	continuation := p
	continuation.pendingSpawnLabel = label
	continuation.privateAttachmentRootReserved =
		p.SandboxImplementation == string(sandboxpolicy.ImplementationTclaudeLayer)
	continuation.privateAttachmentRootCleanup = privateRootCleanup
	*syncProofCleanup = false // the continuation owns the proof markers now

	type deferredResult struct {
		out  *spawnOutcome
		fail *spawnFailure
	}
	// mu + responded pick exactly one owner for the outcome: either the
	// request goroutine returns it inline (fast launch), or — past the grace
	// — the background goroutine owns failure surfacing while the caller
	// keeps the Pending row it already returned.
	var mu sync.Mutex
	responded := false
	done := make(chan deferredResult, 1)
	goBackground(func() {
		out, fail := executeSpawn(g, continuation)
		if fail != nil {
			// The failed continuation cannot claim this reservation. Do not
			// leave a forever-Pending ghost; a delete after a concurrent claim
			// is an idempotent no-op. The private-root cleanup is separately
			// launch-aware inside executeSpawn, because a late enrollment
			// failure can coexist with an already-running pane.
			if err := db.DeletePendingSpawn(label); err != nil {
				slog.Warn("spawn: failed to remove reservation after deferred launch failure",
					"label", label, "error", err)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if !responded {
			done <- deferredResult{out, fail}
			return
		}
		if fail != nil {
			slog.Error("spawn: deferred OpenCode launch failed after pending response",
				"label", label, "group", g.Name, "error", fail.Msg)
			surfaceDeferredSpawnFailure(g, p, label, fail)
		}
	})

	timer := time.NewTimer(openCodeAsyncSpawnResponseGrace)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.out, r.fail
	case <-timer.C:
	}
	mu.Lock()
	// Final drain under the lock: a continuation that finished right at the
	// grace boundary still answers inline rather than going Pending.
	select {
	case r := <-done:
		mu.Unlock()
		return r.out, r.fail
	default:
	}
	responded = true
	mu.Unlock()
	slog.Info("spawn: OpenCode launch continues in background; recorded pending spawn",
		"label", label, "group", g.Name)
	// A deferred OpenCode launch has no pane yet, so the continuation cannot
	// deliver its eventual browser-focus outcome back through this already-
	// returned request. Hand the dashboard the label-keyed websocket now; the
	// terminal client's bounded initial retries bridge the pane coming online.
	// Native auto-focus remains continuation-owned because agentd can open that
	// window itself once the pane exists.
	focusMode := ""
	if p.AutoFocus && p.AutoFocusWeb {
		focusMode = "browser"
	}
	return &spawnOutcome{AgentID: p.AgentID, ConvID: "", Label: label,
		Harness: p.Harness, Model: p.Model, Effort: p.Effort, FocusMode: focusMode}, nil
}

// surfaceDeferredSpawnFailure lands a deferred spawn failure in the dashboard
// Messages tab (the notify-human store). The operator watched the dialog close
// on a Pending row; when the background launch then fails, the row is deleted
// and this message is the remaining trace — without it, a failed spawn is
// indistinguishable from a spawn that silently vanished. FromConv is empty:
// the sender is the daemon, not an agent.
func surfaceDeferredSpawnFailure(g *db.AgentGroup, p spawnParams, label string, fail *spawnFailure) {
	name := p.Name
	if name == "" {
		name = p.Role
	}
	if name == "" {
		name = label
	}
	body := fmt.Sprintf("Spawn of OpenCode agent %q into group %q (label %s) failed after its dialog closed: %s",
		name, g.Name, label, fail.Msg)
	if _, err := recordHumanMessage("", "Agent spawn failed: "+name, body); err != nil {
		slog.Warn("spawn: failed to record deferred spawn failure message",
			"label", label, "error", err)
	}
}

func spawnGroupName(g *db.AgentGroup) string {
	if g == nil {
		return ""
	}
	return g.Name
}

func spawnGroupID(g *db.AgentGroup) int64 {
	if g == nil {
		return 0
	}
	return g.ID
}

func pendingSpawnFromParams(g *db.AgentGroup, p spawnParams, label string) *db.PendingSpawn {
	pending := &db.PendingSpawn{
		Label:               label,
		AgentID:             p.AgentID,
		Launching:           true,
		GroupID:             g.ID,
		Role:                p.Role,
		Descr:               p.Descr,
		Name:                p.Name,
		InitialMessage:      p.InitialMessage,
		GroupContext:        p.GroupContext,
		ProfileContext:      p.ProfileContext,
		ReplyToConv:         p.ReplyToConv,
		SpawnedByConv:       p.SpawnedByConv,
		WorktreePath:        p.WorktreePath,
		WorktreeBranch:      p.WorktreeBranch,
		IsOwner:             p.IsOwner,
		PermissionOverrides: p.PermissionOverrides,
		ProcessCommandID:    p.ProcessCommandID,
		TaskURL:             p.TaskURL,
		TaskLabel:           p.TaskLabel,
		EffectiveSandbox:    p.EffectiveSandbox,
	}
	if harnessOrDefault(p.Harness) == harness.CodexName {
		selected := p.CodexAppServer
		pending.CodexAppServer = &selected
		pending.CodexAppServerSource = p.CodexAppServerSource
		pending.CodexStateRoot = p.CodexStateRoot
		pending.CodexStateRootSource = p.CodexStateRootSource
		pending.FastModeAtLaunch = p.FastModeAtLaunch
	}
	return pending
}

// backfillPendingSpawnInline continues the old short Codex conv-id discovery
// window after the HTTP response has already returned a Pending row. This keeps
// the dashboard responsive while preserving the previous fast-path behavior:
// a trusted Codex pane whose rollout appears seconds after launch is promoted
// into the target group without waiting for the coarse pending-spawn sweeper.
//
// The sweep goroutine remains the restart-safe and long-lived fallback. This
// helper deliberately stops at the original inline budget, so conv-store
// discovery stays a seconds-wide launch-time heuristic rather than a minutes-
// wide guess that could confuse concurrent pending Codex spawns in one cwd.
func backfillPendingSpawnInline(g *db.AgentGroup, p spawnParams, label string, h *harness.Harness, launchedAt time.Time, budget time.Duration) {
	deadline := launchedAt.Add(budget)
	var lastDiscoveryScan time.Time
	launchMarked := false
	for time.Now().Before(deadline) {
		s, err := db.LoadSession(label)
		if err != nil {
			slog.Warn("spawn: pending inline back-fill load failed",
				"label", label, "error", err)
			sleepSpawnPoll(deadline)
			continue
		}
		if s == nil {
			return
		}
		if !spawnRowBelongsToLaunch(s, false, "", launchedAt) {
			sleepSpawnPoll(deadline)
			continue
		}
		if !launchMarked {
			if err := db.MarkPendingSpawnLaunched(label); err != nil {
				slog.Warn("spawn: pending inline back-fill failed to clear launch marker",
					"label", label, "error", err)
			} else {
				launchMarked = true
			}
		}
		convID := s.ConvID
		if convID == "" && s.TmuxSession != "" && time.Since(launchedAt) >= convStoreDiscoveryGrace &&
			time.Since(lastDiscoveryScan) >= convStoreDiscoveryScanInterval {
			lastDiscoveryScan = time.Now()
			if id := discoverSpawnedConvID(h, p.Cwd, launchedAt); id != "" {
				if err := db.SetSessionConvID(label, id); err != nil {
					slog.Warn("spawn: failed to persist discovered conv-id during pending back-fill",
						"label", label, "conv", id, "error", err)
				}
				convID = id
			}
		}
		if convID != "" {
			completePendingSpawnBackfill(g, p, label, convID)
			return
		}
		sleepSpawnPoll(deadline)
	}
}

// spawnDeadPaneStatusGrace bounds how long startupCorpseExitDetail waits for
// tmux to attach an exit status to a pane it has already marked dead, and
// spawnDeadPaneStatusPoll is how often it re-reads within that grace. The
// window only ever costs a launch that has ALREADY failed, so it is sized to
// outlast tmux's own reap latency rather than to keep a spawn snappy.
const (
	spawnDeadPaneStatusGrace = 1500 * time.Millisecond
	spawnDeadPaneStatusPoll  = 50 * time.Millisecond
)

// startupCorpseExitDetail renders the exit detail for a pane tmux has already
// marked dead during a spawn's startup window.
//
// tmux closes the pane's pty and reaps the pane's child as two separate
// events, so the first look at a fast-dying pane can report pane_dead=1 with
// NEITHER pane_dead_status nor pane_dead_signal. Reporting "unknown exit
// status" off that first look threw away the real code or signal that lands a
// beat later — and left the operator with the least informative message
// tclaude can produce for the failures that need it most. Re-read for a
// bounded moment before settling for "unknown".
//
// inspected is false when the corpse can no longer be read, which means the
// authenticated exit callback recorded and reaped it. Its durable audit row is
// then the better evidence, so the caller falls through to that rather than
// guessing from a pane that is gone.
func startupCorpseExitDetail(tmuxSession string) (string, string, bool) {
	evidence, err := session.InspectDeadTmuxSessionPane(tmuxSession)
	if err != nil {
		return "", "", false
	}
	for waited := time.Duration(0); ; waited += spawnDeadPaneStatusPoll {
		if detail := deadPaneExitDetail(evidence.ExitCode, evidence.Signal); detail != "" {
			return detail, evidence.PaneID, true
		}
		if waited >= spawnDeadPaneStatusGrace {
			return "unknown exit status", evidence.PaneID, true
		}
		time.Sleep(spawnDeadPaneStatusPoll)
		settled, settledErr := session.InspectDeadTmuxSessionPane(tmuxSession)
		if settledErr != nil {
			return "", "", false
		}
		evidence = settled
	}
}

// logStartupCorpseOutput copies a dead startup pane's output into the log
// before the caller fails the spawn.
//
// It cannot be left to the pane's own exit callback. Failing the spawn rolls
// back the launch enrollment, and deleting the enrolled agent cascades to
// `DELETE FROM sessions WHERE conv_id = ?` (DeleteAgentByConvID). A callback
// that has not run by then loads no session and rejects with "sql: no rows in
// result set" — so on exactly the launches this daemon reports as failed, the
// pane output the operator is told to go read was the thing most likely to be
// missing. Capture first, roll back second; a duplicate line when the callback
// does win the race is a far better failure than no line at all.
func logStartupCorpseOutput(label, tmuxSession, paneID, detail string) {
	if paneID == "" {
		return
	}
	slog.Error("spawn: managed pane died during startup",
		"label", label, "tmux_session", tmuxSession, "pane_id", paneID,
		"exit_detail", detail, "pane_output", session.DeadPaneDiagnostic(paneID))
}

// deadPaneExitDetail renders tmux's mutually exclusive dead-pane exit
// evidence, or "" when tmux has attached neither a status nor a signal yet.
func deadPaneExitDetail(code *int, signal string) string {
	switch {
	case signal != "":
		return "signal " + signal
	case code != nil:
		return "exit code " + strconv.Itoa(*code)
	}
	return ""
}

func sleepSpawnPoll(deadline time.Time) {
	const interval = 250 * time.Millisecond
	d := time.Until(deadline)
	if d <= 0 {
		return
	}
	if d > interval {
		d = interval
	}
	time.Sleep(d)
}

func completePendingSpawnBackfill(g *db.AgentGroup, p spawnParams, label, convID string) {
	ps, err := db.GetPendingSpawn(label)
	if err != nil {
		slog.Warn("spawn: pending inline back-fill lookup failed",
			"label", label, "conv", convID, "error", err)
		return
	}
	if ps == nil {
		return
	}
	claimed, err := db.ClaimPendingSpawnAndBindAgent(label, convID, p.AgentID, "spawn")
	if err != nil {
		slog.Warn("spawn: pending inline back-fill claim failed",
			"label", label, "conv", convID, "error", err)
		return
	}
	if !claimed {
		return
	}
	if m, err := db.FindMemberInGroup(g.ID, convID); err != nil {
		slog.Warn("spawn: pending inline back-fill membership check failed",
			"label", label, "conv", convID, "error", err)
		requeuePendingSpawn(label, ps)
		return
	} else if m != nil {
		// Same repairs the sweeper's already-enrolled path performs: a prior
		// attempt may have committed membership but lost authorization lineage
		// or the task-ref write. Requeue the only durable intent on failure.
		if !ensurePendingSpawnLineageBound(convID, ps) {
			requeuePendingSpawn(label, ps)
			return
		}
		if !ensurePendingTaskRefBound(convID, ps) {
			requeuePendingSpawn(label, ps)
		}
		return
	}
	if fail := finishSpawnEnrollment(g, p, convID); fail != nil {
		requeuePendingSpawn(label, ps)
		slog.Warn("spawn: pending inline back-fill enrollment failed; sweeper will retry",
			"label", label, "conv", convID, "error", fail.Msg)
		return
	}
	slog.Info("spawn: pending inline back-fill enrolled spawn",
		"label", label, "conv", convID, "group", g.Name)
}

func requeuePendingSpawn(label string, ps *db.PendingSpawn) {
	if err := db.InsertPendingSpawn(ps); err != nil {
		slog.Warn("spawn: failed to requeue claimed pending spawn",
			"label", label, "error", err)
	}
}

// finishSpawnEnrollment completes a spawn once its conv-id is known: it
// optionally joins the conv to a group, records the requested display name,
// drops the startup briefing into the new agent's inbox, and kicks off the post-init
// /rename + welcome injection. It is the shared tail of executeSpawn — run
// inline when the conv-id resolves during the spawn poll, and run later by
// the pending-spawn sweeper once a gated Codex finally takes its first turn
// and its conv-id materialises. For the sweeper path g and p are
// reconstructed from the persisted pending_spawns row.
//
// It deliberately does NOT auto-focus: the terminal is opened by executeSpawn
// at spawn time (label-based, conv-id-independent), so a pending spawn is
// already focusable while it waits.
//
// Returns a typed failure only for an applicable group membership write; the
// later steps (pending name, inbox insert) are best-effort and only log, since
// the agent is already spawned.
//
// SAFETY: runSpawnPostInit's pane injection (send-keys) runs ONLY from here,
// i.e. only after the conv-id exists — which for Codex means after it cleared
// its startup gates and took its first turn. That preserves JOH-205's
// no-send-keys-before-connection property through the non-blocking refactor.
func finishSpawnEnrollment(g *db.AgentGroup, p spawnParams, convID string) *spawnFailure {
	// Decide whether the welcome was already delivered as the launch seed.
	// A seed-needing harness (Codex) whose briefing fits the launch prompt
	// (short/empty) got the FULL welcome inline at launch, so re-injecting it
	// post-connect would double the greeting — skip it. A long briefing's seed
	// was only a stand-by, so its inbox-pointer welcome is delivered below. For
	// Claude Code on the legacy-injection revert (NeedsSpawnSeed false) this is
	// always false, so the welcome is injected over tmux exactly as before.
	//
	// Recomputed from the same inputs executeSpawn used to build the seed
	// (harness + briefing + inline cap), so the two agree — except if the inline
	// cap is reconfigured between launch and a gated Codex's eventual conv-id
	// (pathological): a raised cap would skip a now-"short" briefing's pointer
	// (the stand-by seed still tells the agent to read its inbox), a lowered cap
	// would inject a redundant pointer after an already-inlined seed. Neither
	// loses the briefing.
	//
	// welcomeInSeed also drives the read-marking below (a seed-inlined briefing
	// is marked read, since the agent already has its full text). The same
	// raised-cap pathological case therefore also marks a stand-by (NOT actually
	// inlined) briefing read — hiding it from the dashboard's unread list. The
	// briefing is still NOT lost: the stand-by seed explicitly tells the agent to
	// `tclaude agent inbox` for it, and a read message is still listed by a plain
	// `inbox ls`. Fully closing this would mean persisting the launch-time inline
	// decision on the pending_spawns row; deliberately skipped as disproportionate
	// to an operator-induced, recoverable, cosmetic window.
	h := harnessForConv(convID)
	groupName := spawnGroupName(g)
	contextBody := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments)
	welcomeInSeed := h.NeedsSpawnSeed() && spawnBriefingFitsLaunch(contextBody, spawnInlineMaxChars())
	briefingInlined := contextBody != "" && welcomeInSeed
	// actorCreated is deliberately dropped here: this path runs post-connect
	// against a LIVE conversation, so even a partially-failed enrollment must
	// never delete the real, running agent's identity.
	spawnContextMsgID, _, fail := enrollSpawnedConv(g, p, convID, briefingInlined)
	if fail != nil {
		return fail
	}

	// Post-spawn injection: rename the new pane to the agent's name and
	// drop a [system: ...] welcome describing the agent's identity. It
	// also materialises the .jsonl (CC only writes the file once it has
	// content), so `agent resume` has something to resume. Runs in a
	// goroutine so the caller returns promptly; the goroutine waits for
	// the pane to come alive before injecting.
	goBackground(func() {
		runSpawnPostInit(convID, p.Name, p.Role, p.Descr, groupName,
			spawnContextMsgID, p.InitialMessage != "", p.WorktreePath, p.WorktreeBranch,
			p.SpawnedByConv, p.SpawnedByAgent, welcomeInSeed)
	})

	return nil
}

// enrollSpawnedConv performs the DB-only enrollment for a spawned conv: add it
// to the group, record its pending display name, and drop its startup briefing
// (group context + task brief) into its inbox as a single "Startup context"
// agent_messages row. It returns that message's id (0 when there was no
// briefing) so the caller can reference it in the welcome. An inlined briefing
// is inserted already delivered and read so it can never race the nudge
// dispatcher; a pointer briefing is marked delivered once its welcome lands.
//
// It is the shared enrollment step of both spawn paths:
//   - the legacy inject-after-connect path (finishSpawnEnrollment) calls it
//     once the conv-id is polled, then injects the rename + welcome over tmux;
//   - the launch-enrollment path calls it BEFORE the fork — the welcome baked
//     into the launch command must reference this briefing's message id — and
//     forwards the rename + welcome as launch args.
//
// Only the membership write is fatal — the agent cannot join without it; the
// pending name + inbox insert are best-effort and only log, since the agent is
// already (about to be) spawned and grouped. The pending name is stored even
// when it isn't a valid rename title, so the dashboard can show the intended
// name during the brief window before the title materialises.
func enrollSpawnedConv(g *db.AgentGroup, p spawnParams, convID string, briefingInlined bool) (int64, bool, *spawnFailure) {
	// Stable agent-identity (JOH-26): a spawn is the birth of a new actor. Mint
	// its agent_id BEFORE the group-add so created_via is the precise "spawn"
	// rather than the "group" tag AddAgentGroupMember's own EnsureAgentForConv
	// would otherwise stamp (that call is a no-op once this conv is already
	// linked). Idempotent. actorCreated reports whether THIS call minted the
	// actor row — the launch-enrollment caller uses it to delete the actor
	// again when the fork never starts, so a failed launch cannot strand a
	// ghost in the dashboard's virtual "Ungrouped" group.
	var agentID string
	var actorCreated bool
	var err error
	if p.AgentID != "" {
		agentID, actorCreated, err = db.EnsureAgentForConvWithID(convID, p.AgentID, "spawn")
	} else {
		agentID, actorCreated, err = db.EnsureAgentForConv(convID, "spawn")
	}
	if err != nil {
		// A reserved id may already have been returned to the caller. Never
		// continue by minting/substituting another actor through group-add.
		if p.AgentID != "" {
			return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "identity",
				"failed to bind reserved agent " + p.AgentID + " to spawned conv " + convID + ": " + err.Error()}
		}
		slog.Warn("spawn: failed to ensure agent identity", "conv", convID, "error", err)
	}

	// Persist resolved relaunch intent as soon as the stable actor exists. On
	// the launch-enrollment path this runs before the fork, so the process can
	// never become authoritative before its durable agent-level settings exist.
	// The post-connect/pending path may arrive after a session writer already
	// projected the same data; preserve any existing profile so an idempotent
	// enrollment retry cannot roll back a setting changed in the meantime.
	relaunchProfileBound := false
	bindRelaunchProfile := func() error {
		if agentID == "" {
			return nil
		}
		existing, err := db.AgentRelaunchProfileForConv(convID)
		if err != nil {
			return err
		}
		profile := relaunchProfileForSpawn(p)
		// Unlike durable launch intent, this observation belongs to the launch
		// being enrolled. Do not let an older agent profile win the composition.
		fastModeAtLaunch := profile.FastModeAtLaunch
		// A pending Codex spawn is enrolled after its session row has
		// materialised. Its persisted pending-spawn intent predates some
		// launch flags, while SaveSession has already recorded the exact
		// observable posture as the unmanaged conversation fallback. Overlay
		// that snapshot, then any stable agent values already written by a
		// concurrent projection/retry. Birth-time-only fields remain underneath:
		// SSHWorkaround is not a session column, so replacing the whole profile
		// would turn an explicit opt-out into unknown and then back into Codex's
		// default-on posture on clone/reincarnation.
		conversation, err := db.ConversationResumeProfileForConv(convID)
		if err != nil {
			return err
		}
		if conversation != nil && conversation.FallbackRelaunch != nil {
			profile = *db.ComposeAgentRelaunchProfile(&profile, conversation.FallbackRelaunch)
		}
		if existing != nil {
			profile = *db.ComposeAgentRelaunchProfile(&profile, existing)
		}
		profile.FastModeAtLaunch = fastModeAtLaunch
		if err := db.SetAgentRelaunchProfile(agentID, profile); err != nil {
			return err
		}
		relaunchProfileBound = true
		return nil
	}
	if err := bindRelaunchProfile(); err != nil {
		return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "io",
			"failed to record durable relaunch profile: " + err.Error()}
	}

	// Record the per-agent task-reference link (dashboard Task column) when
	// the spawn requested one. The URL was already scheme-validated at the
	// spawn boundary, so this only ever stores a good value. Fatal, NOT
	// best-effort like the audit snapshot below: the caller was told the
	// spawn carries this link, so a lost write must fail the enrollment
	// rather than silently dropping it (TCL-568). Deliberately placed BEFORE
	// the membership write: a failure here leaves nothing committed, so the
	// sweeper / back-fill retry re-runs the FULL enrollment (briefing
	// included) instead of hitting the already-enrolled skip. The write is a
	// keyed upsert, so the retry re-applying it is idempotent.
	taskRefBound := false
	if agentID != "" && p.TaskURL != "" {
		if _, err := db.SetAgentTaskRef(agentID, p.TaskURL, p.TaskLabel); err != nil {
			return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to record task-reference link: " + err.Error()}
		}
		taskRefBound = true
	}

	// Membership is optional for process-owned v1 agents. Ordinary spawn
	// callers still pass a group and retain the existing fatal membership
	// contract.
	if g != nil {
		if err := db.AddAgentGroupMember(&db.AgentGroupMember{
			GroupID: g.ID,
			ConvID:  convID,
			Role:    p.Role,
			Descr:   p.Descr,
		}); err != nil {
			return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "io",
				"spawned conv " + convID + " but failed to add to group: " + err.Error()}
		}
	}

	// If the up-front EnsureAgentForConv failed transiently, AddAgentGroupMember's
	// own EnsureAgentForConv may have minted the actor anyway (stamped "group").
	// Re-resolve so a successful spawn still records its pending name below.
	if agentID == "" {
		if id, rErr := db.AgentIDForConv(convID); rErr != nil {
			slog.Warn("spawn: failed to re-resolve actor after group add", "conv", convID, "error", rErr)
		} else {
			agentID = id
		}
	}
	if !relaunchProfileBound {
		if err := bindRelaunchProfile(); err != nil {
			return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to record durable relaunch profile: " + err.Error()}
		}
	}

	// Record the verbatim spawn config onto the new actor — the durable,
	// agent-level "what was this spawned with" record (JOH-334). Best-effort:
	// the agent is already spawned + grouped, so a failed write just means no
	// audit snapshot, never a stranded spawn. Empty on paths with no
	// SpawnRequest to snapshot (sweeper / template), where it stays "".
	if agentID != "" && p.SpawnConfigJSON != "" {
		if err := db.SetAgentInitialSpawnConfig(agentID, p.SpawnConfigJSON); err != nil {
			slog.Warn("spawn: failed to record initial spawn config",
				"agent", agentID, "error", err)
		}
	}
	if agentID != "" && p.EffectiveSandbox != nil {
		if err := db.SetAgentEffectiveSandboxConfig(agentID, p.EffectiveSandbox); err != nil {
			return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to record effective sandbox snapshot: " + err.Error()}
		}
	}
	if agentID != "" && p.ProcessCommandID != "" {
		if err := db.SetAgentProcessCommand(agentID, p.ProcessCommandID); err != nil {
			return 0, actorCreated, &spawnFailure{http.StatusConflict, "process_command", "failed to bind spawned agent to process command: " + err.Error()}
		}
	}

	// Task-ref fallback for the rare path where the up-front identity ensure
	// failed transiently and the actor was only minted by the group-add above
	// (agentID was "" at the pre-membership write). Same fatal contract; the
	// membership is already committed here, so a failure lands in the
	// already-enrolled retry branch, whose repair re-applies the link off the
	// requeued pending row.
	if !taskRefBound && agentID != "" && p.TaskURL != "" {
		if _, err := db.SetAgentTaskRef(agentID, p.TaskURL, p.TaskLabel); err != nil {
			return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to record task-reference link: " + err.Error()}
		}
	}

	// Durable spawn lineage is an authorization fact: @self-spawned and
	// @descendants grants range over this edge. Resolve the stable parent from
	// the pending-spawn companion when available, otherwise from the synchronous
	// caller conv. An agent-initiated spawn must not silently land without its
	// edge; failing enrollment is the safe direction because a missing edge
	// would make a deliberately delegated capability unexpectedly unusable.
	// Human-initiated spawns have no parent and therefore write no row. Clone is
	// a separate lifecycle path and deliberately never calls this writer.
	if err := recordSpawnLineage(agentID, p.SpawnedByAgent, p.SpawnedByConv); err != nil {
		return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "identity",
			"failed to record spawning agent lineage: " + err.Error()}
	}

	// Birth-time access controls: make the new agent a group owner
	// and/or apply its requested per-slug permission overrides, the same writes
	// the group-template instantiator performs after executeSpawn — but folded
	// into enrollment so they reach EVERY spawn path uniformly: the launch-
	// enrollment (CC, pre-fork), the inline-resolve (Codex), and the pending-
	// spawn sweeper, which reconstructs p.IsOwner / p.PermissionOverrides from
	// the persisted row. Both are best-effort and only log on failure — the
	// agent is already spawned + grouped, and the human can re-apply from the
	// Edit-agent modal; a failed grant must not strand the spawn. The grants
	// were authorised at the boundary (handleGroupSpawn requires the same slug
	// the dedicated endpoints do — groups.owners.manage / permissions.grant — for an agent
	// caller), so granter just records who requested it. We use granterLabel
	// rather than auditedCaller, so a (narrow) sudo-elevated spawn-time grant is
	// NOT via-sudo-annotated in the audit row the way the dedicated endpoints
	// annotate it — an accepted residual; the authorization is identical.
	granter := granterLabel(p.SpawnedByConv)
	if p.IsOwner && g != nil {
		if err := db.AddAgentGroupOwner(g.ID, convID, granter); err != nil {
			slog.Warn("spawn: failed to grant group ownership at birth",
				"conv", convID, "group", g.Name, "error", err)
		}
	}
	permissionGranter := p.PermissionGranter
	if permissionGranter == "" {
		permissionGranter = granter
	}
	for _, slug := range db.SortedOverrideSlugs(p.PermissionOverrides) {
		override := p.PermissionOverrides[slug]
		if err := db.SetAgentPermissionOverrideWithScope(convID, slug, override.Effect, override.Scope, permissionGranter); err != nil {
			slog.Warn("spawn: failed to apply birth permission override",
				"conv", convID, "slug", slug, "effect", override.Effect, "scope", override.Scope, "error", err)
		}
	}

	// Record the requested name as the actor's pending display name. Until
	// the title materialises (a tick later on the legacy path; at launch on
	// the launch-enrollment path) the dashboard would otherwise show
	// "(unknown)". agent.FreshTitle reads pending_name as a fallback; the
	// real custom title supersedes it. Keyed on the actor so the name survives
	// conv rotations.
	if name := strings.TrimSpace(p.Name); name != "" {
		if agentID != "" {
			if err := db.SetAgentPendingName(agentID, name); err != nil {
				slog.Warn("spawn: failed to record actor pending name",
					"agent", agentID, "name", name, "error", err)
			}
		}
	}

	// Spawn context: assemble the new agent's startup briefing and drop
	// it in its inbox as a single agent_messages row. Two pieces feed in
	// — the (already opt-out-applied) group context and the per-spawn
	// initial_message. They go to the inbox rather than the pane: a
	// large briefing bracketed-pasted into CC's input box risks
	// overflowing its input-size limit, and the inbox keeps newlines
	// verbatim regardless. The welcome line points the agent at the
	// message; the spawn path marks it delivered once the welcome lands.
	groupName := spawnGroupName(g)
	spawnContext := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments)
	var spawnContextMsgID int64
	if spawnContext != "" {
		// Address the briefing FROM the reply-to actor's LIVE generation. On the
		// sweeper path ReplyToConv is a minutes-old snapshot whose actor may have
		// rotated; liveConvForActor re-resolves it from the durable ReplyToAgent
		// companion (JOH-321 F2) so a reply routes to the current generation,
		// falling back to the recorded conv when the companion is empty.
		replyToConv := liveConvForActor(p.ReplyToConv, p.ReplyToAgent)
		briefing := &db.AgentMessage{
			GroupID:      spawnGroupID(g),
			FromConv:     replyToConv,
			ToConv:       convID,
			Subject:      db.StartupContextSubject,
			Body:         spawnContext,
			ToRecipients: []string{convID},
		}
		if briefingInlined {
			// The launch prompt already carries the full briefing, so its durable
			// inbox copy is archival from birth. Stamp both fields in the INSERT
			// itself: a follow-up UPDATE would leave a race where the online or
			// health flush can claim this unread row and inject a duplicate nudge.
			consumedAt := time.Now()
			briefing.CreatedAt = consumedAt
			briefing.DeliveredAt = consumedAt
			briefing.ReadAt = consumedAt
		}
		mid, msgErr := db.InsertAgentMessage(briefing)
		if msgErr != nil {
			// Best-effort: the agent has already spawned and joined the
			// group. A failed insert just means no briefing — logged,
			// not bubbled — and the welcome falls back to "wait".
			slog.Warn("spawn: failed to deliver startup context to inbox",
				"conv", convID, "error", msgErr)
		} else {
			spawnContextMsgID = mid
		}
	}
	return spawnContextMsgID, actorCreated, nil
}

// recordSpawnLineage resolves an agent-initiated spawn's stable parent and
// records the immutable child edge. A human spawn has neither parent field and
// is a no-op. Once a parent is present, an unresolved child identity is an
// error: lineage is authorization state and must never be skipped silently.
func recordSpawnLineage(childAgentID, spawnedByAgent, spawnedByConv string) error {
	parentAgentID := strings.TrimSpace(spawnedByAgent)
	if parentAgentID == "" && spawnedByConv != "" {
		var err error
		parentAgentID, err = db.AgentIDForConv(spawnedByConv)
		if err != nil {
			return fmt.Errorf("resolve spawning agent %s: %w", spawnedByConv, err)
		}
		if parentAgentID == "" {
			return fmt.Errorf("resolve spawning agent %s: no stable actor", spawnedByConv)
		}
	}
	if parentAgentID == "" {
		return nil
	}
	childAgentID = strings.TrimSpace(childAgentID)
	if childAgentID == "" {
		return fmt.Errorf("resolve spawned child actor: no stable actor")
	}
	return db.RecordAgentLineage(childAgentID, parentAgentID, time.Now().UTC())
}

// rollbackSpawnEnrollment undoes enrollSpawnedConv when a launch-enrollment
// spawn's fork itself fails to start (the `tclaude session new` subprocess
// never even launches — e.g. the binary is missing from PATH). The enrollment
// ran before the fork (the welcome had to reference the briefing's message id),
// so without this the failed spawn would strand a group member + orphan
// briefing for a conv-id that will never exist. It is NOT called on a slow/
// missing conv-id poll: there the pane is most likely coming up, so the spawn
// is returned as a success against the preset id rather than rolled back (see
// the launch-enrollment branch in executeSpawn). All removals are best-effort
// — a failure here only leaves a harmless orphan the operator can clear from
// the dashboard — so they log rather than bubble.
//
// It also undoes the birth-time access controls enrollSpawnedConv may have
// written (the group-owner row + per-slug overrides): both are applied before
// the fork on the launch-enrollment path, so a failed launch would otherwise
// strand a ghost owner of the group (which could mask an ownerless-group
// warning) and dangling override rows for a conv that never exists. Both calls
// are no-ops when nothing was written, so this is unconditional — rollback has
// no spawnParams to consult.
//
// actorCreated: when the enrollment MINTED the actor row (the normal
// launch-enrollment case — the conv-id is a fresh UUID), the actor itself is
// deleted too, pending name and task-ref with it. Membership removal alone is
// not enough: the dashboard's virtual "Ungrouped" group lists EVERY active
// actor row (dashboardSnapshot.Ungrouped / handlePeers pass 2), so a
// surviving actor for a conv that will never exist shows up there as a
// dangling agent the operator has to retire by hand — the observed second bug
// of the "command too long" incident. DeleteAgentByConvID also clears the
// conv's inbox rows and any session row carrying the preset conv-id, giving
// the per-row removals above a second chance if one individually failed.
// false leaves the actor alone — a reserved or pre-existing identity must
// never be destroyed by a rollback.
func rollbackSpawnEnrollment(g *db.AgentGroup, convID string, msgID int64, actorCreated bool) {
	if msgID > 0 {
		if _, err := db.DeleteAgentMessageByID(msgID, convID); err != nil {
			slog.Warn("spawn: rollback failed to delete orphan briefing",
				"conv", convID, "msg_id", msgID, "error", err)
		}
	}
	if g != nil {
		if err := db.RemoveAgentGroupMember(g.ID, convID); err != nil {
			slog.Warn("spawn: rollback failed to remove group member",
				"conv", convID, "group", g.Name, "error", err)
		}
		if _, err := db.RemoveAgentGroupOwner(g.ID, convID); err != nil {
			slog.Warn("spawn: rollback failed to remove birth owner grant",
				"conv", convID, "group", g.Name, "error", err)
		}
	}
	if _, err := db.RevokeAllAgentPermissionsForConv(convID); err != nil {
		slog.Warn("spawn: rollback failed to revoke birth permission overrides",
			"conv", convID, "error", err)
	}
	if err := db.ClearAgentProcessCommandForConv(convID); err != nil {
		slog.Warn("spawn: rollback failed to clear process command metadata", "conv", convID, "error", err)
	}
	if actorCreated {
		agentID, err := db.AgentIDForConv(convID)
		if err != nil {
			slog.Warn("spawn: rollback failed to resolve stranded actor lineage",
				"conv", convID, "error", err)
		} else if agentID != "" {
			if _, err := db.DeleteAgentLineageForChild(agentID); err != nil {
				slog.Warn("spawn: rollback failed to delete stranded actor lineage",
					"conv", convID, "agent", agentID, "error", err)
			}
		}
		if _, err := db.DeleteAgentByConvID(convID); err != nil {
			slog.Warn("spawn: rollback failed to delete stranded actor",
				"conv", convID, "error", err)
		}
	}
}

// spawnUsesLegacyInjection reports whether the operator has reverted the
// Claude Code spawn flow to the legacy inject-after-connect path via
// config.Agent.SpawnLegacyInjection. The default (no config / false) uses the
// faster launch-enrollment path. A config read error falls back to the default
// (false) so a malformed config never silently disables the new path without a
// log; config.Load already logs parse failures.
func spawnUsesLegacyInjection() bool {
	cfg, err := config.Load()
	if err != nil || cfg == nil || cfg.Agent == nil || cfg.Agent.SpawnLegacyInjection == nil {
		return false
	}
	return *cfg.Agent.SpawnLegacyInjection
}

// spawnInlineMaxChars returns the briefing-inline threshold (in runes): when a
// spawned agent's startup briefing fits within it, the whole briefing is baked
// into the launch prompt instead of pointing at the inbox copy — for both Claude
// Code's launch-enrollment prompt (buildSpawnLaunchPrompt) and Codex's conv-id
// seed (buildSpawnSeedPrompt). An unset config knob yields
// config.DefaultSpawnInlineMaxChars; a configured <= 0 disables inlining
// (always pointer). A config read error falls back to the default so a
// malformed config never silently changes the spawn UX without a log
// (config.Load already logs parse failures).
func spawnInlineMaxChars() int {
	cfg, err := config.Load()
	if err != nil || cfg == nil || cfg.Agent == nil || cfg.Agent.SpawnInlineMaxChars == nil {
		return config.DefaultSpawnInlineMaxChars
	}
	return *cfg.Agent.SpawnInlineMaxChars
}

// markBriefingConsumed records that a spawned agent's startup-briefing inbox
// message has reached the agent. A pointer briefing is stamped delivered once
// its welcome lands, so the inbox copy is no longer pending first delivery.
//
// When the briefing was INLINED into the launch prompt (inlined true), the
// inbox row was already inserted with delivered_at and read_at set atomically.
// This function deliberately does no follow-up writes for that case: inserting
// unread and fixing it up here would let the nudge dispatcher claim the row in
// between. A briefing that stayed a pointer (inlined false — a legacy CC
// injection, or a Codex briefing too long to inline) is left unread, because
// the agent still has to open it from the inbox.
//
// A msgID of 0 or less (no briefing was inserted) is a no-op. The pointer-case
// delivery write is best-effort and only logs on failure — the spawn has
// already succeeded.
func markBriefingConsumed(convID string, msgID int64, inlined bool) {
	if msgID <= 0 {
		return
	}
	if inlined {
		return
	}
	if err := db.MarkAgentMessageDelivered(msgID); err != nil {
		slog.Warn("spawn: failed to mark startup context delivered",
			"conv", convID, "msg_id", msgID, "error", err)
	}
}

// runSpawnPostInit fires asynchronously after a successful spawn. It
// waits for the new tmux pane to come online, then delivers, in order:
//
//  1. /rename <name> — when name is a valid rename title. This is the
//     agent's single name; it becomes the conversation title.
//  2. The welcome [system: ...] line orienting the agent.
//
// Each is its own turn. Failures are logged, never bubbled — the spawn
// already succeeded as far as the caller is concerned.
//
// OpenCode's welcome uses its managed prompt API; legacy harness paths type
// the welcome into the pane. The agent's startup briefing (group context +
// task brief) is never typed into the pane — the handler placed it in the agent's
// inbox as agent_messages row #spawnContextMsgID, which keeps newlines
// verbatim and sidesteps CC's input-box size limit. The welcome line
// names that message id; once the welcome lands we mark the message
// delivered, since the welcome doubles as its inbox nudge.
//
// welcomeInSeed says the welcome was ALREADY delivered as the launch seed
// (a seed-needing harness like Codex whose briefing fit the launch prompt):
// the seed self-submitted the [system: ...] welcome at launch, so injecting
// it again here would double the greeting — the welcome step is skipped. The
// rename (out-of-band for Codex) and the mark-delivered still run.
//
// Why /rename first: it's a slash command CC processes immediately,
// landing a write on the .jsonl before any other turn happens. Even
// if a later injection fails, the file exists and `agent resume` can
// find it.
//
// spawnedByConv is the conv-id of the agent that requested the spawn
// ("" for a human-initiated one); it is resolved to a display name
// here so the welcome's attribution line names the real spawner.
func runSpawnPostInit(convID, name, role, descr, groupName string, spawnContextMsgID int64, hasInitialMessage bool, worktreePath, worktreeBranch, spawnedByConv, spawnedByAgent string, welcomeInSeed bool) {
	if !waitForConvAlive(convID) {
		slog.Warn("spawn: new conv never came online; post-init injection abandoned",
			"conv", convID)
		return
	}
	if pickAliveSession(convID) == nil {
		slog.Warn("spawn: no alive tmux session for post-init injection", "conv", convID)
		return
	}
	h := harnessForConv(convID)
	codexSelected := false
	if h.Name == harness.CodexName {
		var err error
		codexSelected, err = codexAppServerSelected(convID)
		if err != nil {
			slog.Error("spawn: Codex app-server posture unreadable; post-init delivery abandoned without pane fallback",
				"conv", convID, "error", err)
			return
		}
	}

	// An API-driven Copilot launch is not ready for post-init the moment its
	// pane is alive. Its bootstrap creates a session under the conversation id
	// and foregrounds it, so everything below — the rename and the welcome —
	// must wait for that, or it lands in the startup session the bootstrap is
	// about to replace and is lost. See awaitCopilotAPISession, which waits on
	// the CONNECTION and not on the launch posture, and explains why the posture
	// cannot answer this.
	//
	if h.Name == harness.CopilotName && copilotLaunchIntentForConv(convID).API &&
		!awaitCopilotAPISession(convID) {
		slog.Error("spawn: the Copilot API channel never came up within the bootstrap "+
			"budget; post-init delivery is abandoned because the failed launch is being "+
			"shut down as crashed and API posture never falls back to pane input",
			"conv", convID, "budget", copilotAPIBootstrapTimeout())
		return
	}
	if codexSelected && !awaitCodexAppServerReady(convID) {
		slog.Error("spawn: the Codex app-server channel never became ready; post-init delivery is abandoned without pane fallback",
			"conv", convID, "budget", codexAppServerStartupTimeout)
		return
	}

	// Re-resolved AFTER the wait, not before it. The liveness check above is
	// the precondition for waiting at all; the tmux target is an address the
	// deliveries below use, and the wait can now spend the bootstrap's whole
	// budget between the two. (It could not before TCL-1080 — the wait returned
	// immediately, so a target read above it was always fresh. That is the kind
	// of assumption a fix silently invalidates.) The rename path re-resolves on
	// its own through aliveSessionForConv; this is the welcome's copy.
	sess := pickAliveSession(convID)
	if sess == nil {
		slog.Warn("spawn: the pane went away while waiting for the Copilot API "+
			"channel; post-init injection abandoned", "conv", convID)
		return
	}
	target := sess.TmuxSession + ":0.0"

	// Apply the agent's name as the conversation title. The two harness
	// rename styles bracket the welcome injection differently:
	//
	//   - In-pane (Claude Code's /rename): inject FIRST, so the rename turn
	//     lands on the .jsonl before any other turn (see below). The charset
	//     gate lives in deliverRename; isValidRenameTitle pre-validates here.
	//   - Out-of-band title store (Codex's threads.title): the harness only
	//     materialises the conversation's row once the FIRST message (the
	//     welcome) has been processed, so the title write must wait until
	//     AFTER the welcome — and retry until the row exists. Done below.
	//
	// Skipped when name is empty or not a valid rename title (some callers
	// pass human-friendly names that don't fit the rename charset); the
	// welcome below still materialises the conversation in that case.
	renameWanted := name != "" && isValidRenameTitle(name)
	if name != "" && !renameWanted {
		slog.Warn("spawn: name not a valid rename title; skipping rename",
			"conv", convID, "name", name)
	}
	if renameWanted && h.SupportsRename() {
		if !deliverRenameOn(convID, name, deliveryChannelRouted) {
			slog.Warn("spawn: rename delivery failed",
				"conv", convID, "name", name)
		}
	}

	// Welcome: a single-line [system: ...] turn orienting the agent
	// (identity, role, descr, group, where its startup briefing waits,
	// and — for a sub-repo worktree — where to make code edits). Skipped
	// when the welcome already rode in as the launch seed (Codex with a
	// briefing that fit the launch prompt); re-injecting it would double the
	// greeting. The out-of-band rename below still runs (and, for the seed
	// case, the seed already materialised the conversation row it lands on).
	if !welcomeInSeed {
		welcome := buildSpawnWelcome(name, role, descr, groupName,
			spawnContextMsgID, hasInitialMessage, worktreePath, worktreeBranch,
			resolveSpawnerTitle(spawnedByConv, spawnedByAgent))
		var err error
		switch {
		case h.Name == harness.OpenCodeName:
			err = sendOpenCodeNudge(convID, welcome)
		case copilotAPIDriven(convID):
			// The welcome is caller-derived text (name, role, descr, group, the
			// spawner's title) and was the last piece of it still going into an
			// API-driven pane as keystrokes.
			//
			err = sendCopilotAPIMessage(convID, welcome)
		case codexSelected:
			err = sendCodexAppServerMessage(convID, spawnContextMsgID, welcome)
		default:
			err = injectTextAndSubmit(target, welcome)
		}
		if err != nil {
			slog.Warn("spawn: welcome injection failed", "conv", convID, "error", err)
			return
		}
	}

	// Out-of-band title harness (Codex): now that the first turn has run —
	// the post-connect welcome above, or the launch seed when welcomeInSeed —
	// persist the name into the title store, retrying until the harness has
	// created the conversation's row (JOH-216). Runs in its own goroutine so
	// the bounded retry never delays the rest of post-init.
	if renameWanted && !h.SupportsRename() && h.SupportsConvs() {
		if codexSelected {
			if err := renameCodexAppServerThread(convID, name); err != nil {
				slog.Warn("spawn: Codex app-server rename failed without title-store fallback",
					"conv", convID, "name", name, "error", err)
			} else {
				cacheDeliveredTitle(convID, name, h.Name)
			}
		} else {
			goBackground(func() { persistSpawnTitle(convID, name) })
		}
	}

	// The startup briefing (group context + task brief) already sits in
	// the agent's inbox — the handler inserted the agent_messages row
	// before this goroutine fired. It reached the agent either as the
	// post-connect welcome above (which named its message id) or, for a
	// seed-delivered welcome, inline in the launch turn — so mark it
	// delivered now that the greeting has landed. welcomeInSeed also means
	// the briefing rode in inline (buildSpawnSeedPrompt inlines exactly when
	// the welcome fits the seed), so the agent already has its full text and
	// the inbox copy is marked read too; a pointer welcome leaves it unread.
	markBriefingConsumed(convID, spawnContextMsgID, welcomeInSeed)
}

// spawnTitlePersist* bound the post-welcome retry that writes an out-of-band
// harness's title (Codex's threads.title). Codex creates the conversation's
// row only after the first message is processed, so the write may need a few
// seconds of retries; the timeout is generous because the cost of a stray
// retry loop is one idle background goroutine.
const (
	spawnTitlePersistTimeout  = 30 * time.Second
	spawnTitlePersistInterval = 1 * time.Second
)

// persistSpawnTitle writes name into an out-of-band harness's title store
// (ConvStore.SetTitle), retrying until the harness has materialised the
// conversation's row or the timeout elapses. It is the spawn-path counterpart
// to the in-pane /rename: for Codex the threads row does not exist until the
// spawn welcome (the first message) has been processed, so a single
// spawn-time write hits zero rows and is silently lost, leaving the agent
// showing its raw first prompt instead of its name (JOH-216).
//
// SetTitle is called directly (not deliverRename) so a not-yet-materialised
// row produces one final warning rather than a warning per retry.
func persistSpawnTitle(convID, name string) {
	h := harnessForConv(convID)
	if h.Convs == nil {
		return
	}
	deadline := time.Now().Add(spawnTitlePersistTimeout)
	for {
		err := h.Convs.SetTitle(convID, name)
		if err == nil {
			return
		}
		if !time.Now().Before(deadline) {
			slog.Warn("spawn: out-of-band title never persisted; conversation row never materialised",
				"conv", convID, "name", name, "harness", h.Name, "error", err)
			return
		}
		time.Sleep(spawnTitlePersistInterval)
	}
}

// buildSpawnContextBody assembles the startup briefing delivered to a
// freshly-spawned agent's inbox. It stitches together up to four
// sections — the group's shared context, profile-specific guidance and the
// per-spawn task brief — under plain-text headers, with dividers when needed.
// present.
//
// Every input may be empty (or whitespace-only); when all are empty, the
// result is "" and the caller skips the inbox insert entirely, so an
// agent with nothing to brief never gets an empty message.
func buildSpawnContextBody(groupName, groupContext, profileContext, initialMessage string, attachments []string) string {
	groupContext = strings.TrimSpace(groupContext)
	profileContext = strings.TrimSpace(profileContext)
	initialMessage = strings.TrimSpace(initialMessage)

	var sections []string
	if groupContext != "" {
		sections = append(sections, fmt.Sprintf(
			"Group %q startup context — shared guidance for every agent spawned into this group:\n\n%s",
			groupName, groupContext))
	}
	if profileContext != "" {
		sections = append(sections,
			"Agent preset startup context — guidance attached to this agent's selected profile and role:\n\n"+profileContext)
	}
	if initialMessage != "" {
		sections = append(sections, "Your task brief:\n\n"+initialMessage)
	}
	if s := buildSpawnAttachmentsSection(attachments); s != "" {
		sections = append(sections, s)
	}
	return strings.Join(sections, "\n\n---\n\n")
}

// buildSpawnAttachmentsSection renders the briefing's "Attached files" block
// from a list of file paths, or "" when there are none. The paths were written
// to a temp dir by the dashboard's upload endpoint (screenshots pasted from the
// clipboard, or files chosen with the native picker) and are listed here so the
// new agent can open them with its own Read tool on the first turn — the daemon
// never reads them itself. Rendered as a markdown bullet list so it stays
// readable both inline in the launch prompt and in `tclaude agent inbox read`.
func buildSpawnAttachmentsSection(attachments []string) string {
	var lines []string
	for _, a := range attachments {
		if a = strings.TrimSpace(a); a != "" {
			lines = append(lines, "- "+a)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Attached files:\n\n" + strings.Join(lines, "\n")
}

// buildSpawnWelcome composes the [system: ...] welcome text. Brackets
// signal "this is metadata from tclaude, not a human prompt" — same
// convention agent-message nudges use. Kept to a single line so it
// renders cleanly in CC's prompt history.
//
// spawnedBy is the attribution shown in the opening clause. "" means a
// human-initiated spawn — the clause stays "spawned by the human". A
// non-empty value is the spawning agent's display name, so an agent
// the PO spawned reads "spawned by <po-name>" rather than being
// misattributed to the human. resolveSpawnerTitle produces it from
// the spawner's conv-id.
//
// The trailing instruction has three forms, set by the spawn-context
// inbox message the handler may have queued:
//
//   - spawnContextMsgID == 0 — no briefing at all → "wait for the
//     first instruction".
//   - a briefing that includes a task brief (hasInitialMessage) →
//     point the agent at the inbox message and tell it to act.
//   - a briefing with only the group's shared startup context →
//     point at the inbox message, then tell it to wait.
func buildSpawnWelcome(name, role, descr, groupName string, spawnContextMsgID int64, hasInitialMessage bool, worktreePath, worktreeBranch, spawnedBy string) string {
	body := spawnWelcomePrefix(name, role, descr, groupName, worktreePath, worktreeBranch, spawnedBy)
	switch {
	case spawnContextMsgID <= 0:
		body += " Wait for the first instruction."
	case hasInitialMessage:
		body += fmt.Sprintf(" Your startup context and task brief are waiting in your inbox"+
			" as message #%d — read it with `tclaude agent inbox read %d`, then act on the brief.",
			spawnContextMsgID, spawnContextMsgID)
	default:
		body += fmt.Sprintf(" Your group's startup context is waiting in your inbox as"+
			" message #%d — read it with `tclaude agent inbox read %d`, then wait for the"+
			" first instruction.",
			spawnContextMsgID, spawnContextMsgID)
	}
	return "[system: " + body + "]"
}

// spawnWelcomePrefix builds the identity/orientation half of the welcome —
// everything up to (but not including) the trailing "where's my briefing"
// instruction: attribution, name, role, group, description, sub-repo worktree
// note, and the `tclaude agent` pointer. It is shared by the two welcome
// shapes — buildSpawnWelcome's single-line pointer form and
// buildSpawnLaunchPrompt's inline form — so the metadata they surface can't
// drift apart. The result has no [system: ...] wrapper and no trailing
// newline; callers append their own closing instruction and wrap.
func spawnWelcomePrefix(name, role, descr, groupName, worktreePath, worktreeBranch, spawnedBy string) string {
	attribution := "spawned by the human"
	if spawnedBy != "" {
		attribution = "spawned by " + spawnedBy
	}
	parts := []string{attribution}
	if name != "" {
		parts = append(parts, fmt.Sprintf("as %q", name))
	}
	if role != "" {
		parts = append(parts, fmt.Sprintf("(role: %s)", role))
	}
	if groupName != "" {
		parts = append(parts, fmt.Sprintf("in group %q", groupName))
	}
	body := strings.Join(parts, " ") + "."
	if descr != "" {
		body += " Descr: " + descr + "."
	}
	// When the spawn targeted a sub-repo of a monorepo launch dir, the
	// agent's cwd is the parent dir but its code work belongs in the
	// worktree. Spell that out so it doesn't edit the parent's repos.
	if worktreePath != "" {
		body += " Your git worktree for code changes is at " + worktreePath
		if worktreeBranch != "" {
			body += " (branch " + worktreeBranch + ")"
		}
		body += " — make code edits there, not elsewhere under your start directory."
	}
	body += " Use `tclaude agent` commands (whoami / --help / inbox ls) to introspect and coordinate."
	return body
}

// buildSpawnLaunchPrompt builds the positional launch prompt for the
// launch-enrollment path (Claude Code). Unlike the legacy send-keys welcome it
// can be MULTI-LINE: it rides in as a single shell-quoted argv positional
// (clcommon.ShellQuoteArg handles every metacharacter, newlines included), not
// typed into a tmux pane where a newline would submit early. So when the
// startup briefing (already inserted into the inbox as message
// #spawnContextMsgID) is short enough — at most inlineMaxChars runes — the
// whole briefing is appended right after the [system: ...] welcome, and the
// agent acts on its first turn without a `tclaude agent inbox read` round-trip.
//
// It falls back to the single-line pointer welcome (buildSpawnWelcome) when:
//   - there is nothing to inline (contextBody is empty — no group context and
//     no task brief; buildSpawnWelcome then tells the agent to wait), OR
//   - inlining is disabled (inlineMaxChars <= 0), OR
//   - the briefing is longer than inlineMaxChars (kept in the inbox, where it's
//     scrollable and doesn't balloon the launch command / first turn).
//
// A failed inbox insert does NOT force the fallback: contextBody is recomputed
// from the spawn inputs (not read back from the inbox), so it stays non-empty
// and is still inlined — the inbox-copy note is just dropped (spawnContextMsgID
// <= 0), making the inline copy the agent's only copy.
//
// contextBody is the exact inbox body (buildSpawnContextBody's output), so the
// inlined copy is byte-identical to what `tclaude agent inbox read` would show.
func buildSpawnLaunchPrompt(name, role, descr, groupName string, spawnContextMsgID int64, hasInitialMessage bool, contextBody, worktreePath, worktreeBranch, spawnedBy string, inlineMaxChars int) string {
	body := strings.TrimSpace(contextBody)
	if body == "" || inlineMaxChars <= 0 || utf8.RuneCountInString(body) > inlineMaxChars {
		return buildSpawnWelcome(name, role, descr, groupName, spawnContextMsgID,
			hasInitialMessage, worktreePath, worktreeBranch, spawnedBy)
	}

	welcome := spawnWelcomePrefix(name, role, descr, groupName, worktreePath, worktreeBranch, spawnedBy)
	// Note the inbox copy only when we actually have its id — the briefing was
	// inserted (the common case). If the insert failed (spawnContextMsgID <= 0)
	// the inline copy below is the agent's only copy, so we don't claim an inbox
	// message that doesn't exist.
	inboxNote := ""
	if spawnContextMsgID > 0 {
		inboxNote = fmt.Sprintf(" (also saved to your inbox as message #%d)", spawnContextMsgID)
	}
	if hasInitialMessage {
		welcome += " Your startup context and task brief are below" + inboxNote + "; act on the brief."
	} else {
		welcome += " Your group's startup context is below" + inboxNote +
			"; read it, then wait for the first instruction."
	}
	return "[system: " + welcome + "]\n\n" + body
}

// spawnBriefingFitsLaunch reports whether a spawn's startup briefing can be
// delivered IN FULL by the launch positional prompt — so no post-connect
// welcome is needed. True for an empty briefing (the welcome is just "wait")
// and for one short enough to inline; false for a long briefing that must keep
// its inbox copy and a pointer welcome. It mirrors buildSpawnLaunchPrompt's own
// inline-vs-pointer decision so a caller can predict, before connection,
// whether the launch prompt already carried the whole welcome.
func spawnBriefingFitsLaunch(contextBody string, inlineMaxChars int) bool {
	body := strings.TrimSpace(contextBody)
	return body == "" || (inlineMaxChars > 0 && utf8.RuneCountInString(body) <= inlineMaxChars)
}

// buildSpawnSeedPrompt builds the positional first-turn prompt for a
// seed-needing harness (Codex). Codex must self-submit a turn to materialise
// its conv-id (JOH-205), and the conv-id doesn't exist until then — so unlike
// the Claude Code launch-enrollment path, there is no inbox-message id to
// reference at launch (the briefing row is inserted post-connect). The prompt
// therefore carries the welcome built with spawnContextMsgID 0:
//
//   - short / empty briefing (spawnBriefingFitsLaunch) → the FULL welcome rides
//     in the seed (the brief inlined, or a "wait" line), looking like the Claude
//     Code launch prompt; the post-connect welcome is then skipped (the caller
//     gates that on the same predicate). Single [system: ...] turn.
//   - long briefing → the seed is a stand-by welcome (buildSpawnStandbySeed):
//     the briefing stays in the inbox and its pointer welcome is injected
//     post-connect, once the inbox row + its id exist (race-safe).
//
// The inbox copy is created post-connect regardless, so an inlined Codex
// briefing is still also in `tclaude agent inbox` — same as Claude Code.
func buildSpawnSeedPrompt(name, role, descr, groupName string, hasInitialMessage bool, contextBody, worktreePath, worktreeBranch, spawnedBy string, inlineMaxChars int) string {
	if spawnBriefingFitsLaunch(contextBody, inlineMaxChars) {
		return buildSpawnLaunchPrompt(name, role, descr, groupName, 0, hasInitialMessage,
			contextBody, worktreePath, worktreeBranch, spawnedBy, inlineMaxChars)
	}
	return buildSpawnStandbySeed(name, role, descr, groupName, worktreePath, worktreeBranch, spawnedBy)
}

// buildSpawnStandbySeed is the launch seed for a seed-needing harness (Codex)
// whose briefing is too long to inline at launch. It materialises the conv-id
// (the turn runs) and orients the agent with the same [system: ...] welcome
// metadata, then tells it the detailed briefing is being delivered to its inbox
// — so it stands by rather than acting blindly. The real inbox-pointer welcome
// (with the message id) is injected post-connect, once that row exists.
func buildSpawnStandbySeed(name, role, descr, groupName, worktreePath, worktreeBranch, spawnedBy string) string {
	welcome := spawnWelcomePrefix(name, role, descr, groupName, worktreePath, worktreeBranch, spawnedBy)
	welcome += " Your detailed startup briefing is being delivered to your inbox now —" +
		" stand by for it (a `tclaude agent inbox` message), then act on the brief."
	return "[system: " + welcome + "]"
}

// resolveSpawnerTitle turns the spawning agent's conv-id into the
// display name buildSpawnWelcome puts in its attribution clause.
//
//   - "" (a human-initiated spawn) stays "" — the welcome then keeps
//     its "spawned by the human" framing.
//   - an agent conv-id resolves through agent.FreshTitle, the same
//     name listing surfaces show.
//   - anything that isn't a clean agent name — FreshTitle's
//     "(unknown)" placeholder, or a title that fails isValidRenameTitle
//     — is downgraded to the generic "another agent".
//
// The isValidRenameTitle gate is load-bearing, not cosmetic.
// FreshTitle falls back to a conversation summary or first prompt when
// a conv has no custom title, and a custom title set via Claude Code's
// own /rename (as opposed to the daemon's gated endpoint) is never
// charset-checked either — so the resolved string can carry newlines
// or other control characters. The welcome is injected into the new
// agent's pane with tmux send-keys, where a raw newline lands as a
// premature submit; routing the title through the same gate every
// tclaude-side rename passes keeps the welcome a safe single line.
// "(unknown)" is rejected explicitly because it happens to satisfy
// isValidRenameTitle.
func resolveSpawnerTitle(spawnedByConv, spawnedByAgent string) string {
	spawnedByConv = liveConvForActor(spawnedByConv, spawnedByAgent)
	if spawnedByConv == "" {
		return ""
	}
	title := agent.FreshTitle(spawnedByConv)
	if title == "" || title == agent.UnknownTitle || !isValidRenameTitle(title) {
		return "another agent"
	}
	return title
}

// liveConvForActor returns the actor's current live generation when the stable
// agent_id companion is known (JOH-321 F2) — so routing/attribution survives a
// rotation that happened while a spawn sat pending — falling back to the
// recorded conv snapshot when the companion is empty (synchronous path / old
// rows / a non-actor conv) or the agent has since vanished.
func liveConvForActor(convSnapshot, agentID string) string {
	if agentID == "" {
		return convSnapshot
	}
	if cur, err := db.CurrentConvForAgent(agentID); err == nil && cur != "" {
		return cur
	}
	return convSnapshot
}

// generateSpawnLabel produces a "spwn-XXXXXX" identifier. 6 hex
// chars from crypto/rand gives ~16M values — collisions in the
// session table are vanishingly rare in practice.
func generateSpawnLabel() string {
	return "spwn-" + randomLabelToken()
}

// randomLabelToken is generateSpawnLabel's 6-hex-char body, shared with the
// name-derived label sequence's last-resort disambiguation suffix.
func randomLabelToken() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// reserveUniqueSpawnPrivateAttachmentRootWith couples the short human-facing
// spawn label to a newly-created private root before any upload is promoted.
// A stale/live label collision is reminted rather than reusing another
// generation's inode and briefly exposing its batches — so nextLabel must
// yield a FRESH candidate per call (both generateSpawnLabel and
// spawnLabelSequence do).
func reserveUniqueSpawnPrivateAttachmentRootWith(
	nextLabel func() string,
) (string, func(), error) {
	const maxAttempts = 16
	for range maxAttempts {
		label := nextLabel()
		root, created, err := tclcommon.PrepareSpawnAttachmentsPrivateDir(label)
		if err != nil {
			return "", func() {}, err
		}
		if !created {
			continue
		}
		return label, func() { _ = os.Remove(root) }, nil
	}
	return "", func() {}, fmt.Errorf(
		"could not mint an unused private attachment root after %d attempts",
		maxAttempts,
	)
}

// SpawnDetachedTclaudeNew is a thin facade over Spawn.SpawnNew.
// Tests substitute a behavior-accurate fake by assigning Spawn at
// setup; production keeps the LiveSpawner default. See clcommon.SpawnArgs
// for the per-field semantics.
func SpawnDetachedTclaudeNew(args clcommon.SpawnArgs) error {
	if err := prepareCodexAppServerRuntime(&args); err != nil {
		return err
	}
	if err := prepareCopilotAPIPort(&args); err != nil {
		failPreparedCodexAppServerRuntime(args, err)
		return err
	}
	if err := Spawn.SpawnNew(args); err != nil {
		failPreparedCodexAppServerRuntime(args, err)
		return err
	}
	startCodexAppServerBootstrap(args)
	// SessionID is the PRESET conv id, and is empty on a launch that lets the
	// harness mint one. Those callers complete the launch themselves once their
	// discovery poll resolves the id; doing nothing here is correct rather than a
	// gap. See completeCopilotAPILaunch.
	//
	// Fresh, unconditionally: this facade forks `tclaude session new` with no
	// -r, so whatever conversation it lands on starts empty by construction.
	completeCopilotAPILaunch(args.SessionID, copilotAPILaunchFresh, args)
	return nil
}

// SpawnDetachedTclaudeResume is a thin facade over Spawn.SpawnResume.
// Args.Effort and Args.Model ("" = omit the flag) ride through to the resumed
// invocation — `claude --resume` does NOT restore the conversation's previous
// model on its own, so resume surfaces pass the predecessor's inherited flags
// to keep the agent on its model. Args.Sandbox ("" = omit) likewise rides
// through so a relaunched Codex agent stays sandboxed (the mode isn't persisted
// per-conv; callers re-resolve the harness default). Args.Approval ("" = omit)
// rides through the same way so a relaunched unattended Codex agent keeps its
// non-escalating posture and can't deadlock on an approval prompt (JOH-200).
// Args.AutoReview (false = the human reviews, the default) rides through the
// same way; relaunch never re-engages the experimental guardian, so resume
// callers leave it false.
func SpawnDetachedTclaudeResume(args clcommon.SpawnArgs) error {
	return spawnDetachedTclaudeResumeAs(args, copilotAPILaunchResume)
}

// spawnDetachedTclaudeResumeAs is SpawnDetachedTclaudeResume with the launch
// kind stated rather than assumed.
//
// It exists for one caller: the COPY clone. That path also forks `session new
// -r`, but the id it resumes was minted seconds earlier by forking a
// conversation file, so the harness has never seen it — the launch is a resume
// in argv shape and a fresh conversation in fact. Under the Copilot API drive
// those two want opposite calls, and a resume of a session that does not exist
// is refused rather than created, so a clone that inherited the assumption
// would come up with no channel and no briefing at all.
//
// Unreachable today, because convops.CopyConversationToPath reads Claude Code's
// own project tree and so a Copilot clone cannot take the copy branch. Stated
// here anyway: the whole point of threading the kind is that it is a fact the
// caller knows and the callee cannot recover, and a caller passing the wrong one
// by inheritance is the same bug one level up.
func spawnDetachedTclaudeResumeAs(args clcommon.SpawnArgs, kind copilotAPILaunchKind) error {
	// A COPY clone has resume-shaped argv but names a newly forked conversation;
	// only an ordinary durable resume may bind a known thread without waiting
	// for a TUI hook that Codex does not emit on every resume.
	args.CodexAppServerExistingThread = kind == copilotAPILaunchResume
	if err := prepareCodexAppServerRuntime(&args); err != nil {
		return err
	}
	if err := prepareCopilotAPIPort(&args); err != nil {
		failPreparedCodexAppServerRuntime(args, err)
		return err
	}
	if err := Spawn.SpawnResume(args); err != nil {
		failPreparedCodexAppServerRuntime(args, err)
		return err
	}
	startCodexAppServerBootstrap(args)
	// A resume always knows its conversation — that is what it is resuming — so
	// unlike the fresh-spawn path this never defers the record.
	//
	// For an ordinary resume the kind is a RESUME, which under the Copilot API
	// drive is not a label but the difference between reloading the conversation
	// and replacing it: the pane was started `copilot --resume=<convID>`, so its
	// conversation has history, and a bootstrap that created a session at that
	// id would discard it. See bootstrapCopilotAPISession.
	completeCopilotAPILaunch(args.ConvID, kind, args)
	return nil
}

// sessionNewArgs builds the argv for the detached `tclaude session new`
// that a spawn forks. --effort and --model are each appended only when
// an explicit value was chosen; an empty value leaves claude on its own
// default. Kept pure so it can be unit-tested without forking a
// subprocess.
func sessionNewArgs(a clcommon.SpawnArgs) []string {
	args := []string{"session", "new", "--managed-launch", "-d", "--global", "--label", a.Label}
	if a.Cwd != "" {
		args = append(args, "-C", a.Cwd)
	}
	if a.SandboxSnapshotPath != "" {
		args = append(args, "--sandbox-snapshot-path", a.SandboxSnapshotPath,
			"--sandbox-snapshot-digest", a.SandboxSnapshotDigest)
	}
	if a.ResourceCgroupDir != "" {
		args = append(args, "--resource-cgroup-dir", a.ResourceCgroupDir)
	}
	if a.AllowUnenforcedSandbox {
		args = append(args, "--allow-unenforced-sandbox")
	}
	if a.SandboxContinuation {
		args = append(args, "--sandbox-continuation")
	}
	if a.CwdWriteProof != "" {
		args = append(args, "--cwd-write-proof", a.CwdWriteProof)
	}
	if a.DirWriteProof != "" {
		args = append(args, "--dir-write-proof", a.DirWriteProof)
	}
	if a.CodexGitCommonDir != "" {
		args = append(args, "--codex-git-common-dir", a.CodexGitCommonDir)
	}
	if a.CodexGitCommonDirPinned {
		args = append(args, "--codex-git-common-dir-pinned")
	}
	for _, dir := range a.GitWorktreeWriteDirs {
		args = append(args, "--git-worktree-write-dir", dir)
	}
	if a.GitWorktreeWriteDirsPinned {
		args = append(args, "--git-worktree-write-dirs-pinned")
	}
	// Launch-enrollment fields (set only on the launch-args spawn path, CC):
	// the preset conv-id, display name, and welcome ride in as launch flags so
	// `claude` is named + greeted at startup with no post-connect injection.
	if a.SessionID != "" {
		args = append(args, "--session-id", a.SessionID)
	}
	if a.RouteHelperAgentID != "" && a.RouteHelperConvID != "" {
		args = append(args,
			"--route-helper-agent-id", a.RouteHelperAgentID,
			"--route-helper-conv-id", a.RouteHelperConvID,
			"--route-helper-launch-generation", a.RouteHelperLaunchGeneration,
			"--route-helper-credential-handoff-socket", a.RouteHelperCredentialHandoffSocketPath)
		if a.RouteHelperProxyOnly {
			args = append(args, "--route-helper-proxy-only")
		}
		for _, groupID := range a.RouteHelperGroupIDs {
			args = append(args, "--route-helper-group-id", strconv.FormatInt(groupID, 10))
		}
	}
	if a.Name != "" {
		args = append(args, "--name", a.Name)
	}
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	args = appendHarnessFlag(args, a.Harness)
	args = appendSandboxArgs(args, a.Harness, a.Sandbox)
	args = appendSandboxImplementationFlag(args, a.SandboxImplementation)
	args = appendDarwinRouteFlags(args, a.DarwinRouteCapable, a.DarwinRouteAgentID)
	args = appendSandboxChosenByFlag(args, a.SandboxChosenBy)
	args = appendAskTimeoutFlag(args, a.AskUserQuestionTimeout)
	args = appendApprovalFlag(args, a.Approval)
	args = appendToolGovernanceFlag(args, a.ToolGovernance)
	args = appendAutoReviewFlag(args, a.AutoReview)
	args = appendTrustDirFlag(args, a.TrustDir)
	args = appendRemoteControlFlag(args, a.RemoteControl)
	args = appendAutoMemoryFlag(args, a.AutoMemory)
	args = appendContextFeaturesFlag(args, a.ContextFeatures)
	args = appendAutoCompactWindowFlag(args, a.AutoCompactWindow)
	args = appendContextWindowMaxFlag(args, a.ContextWindowMax)
	args = appendCopilotAPIFlag(args, a.CopilotAPI)
	args = appendCodexAppServerArgs(args, a)
	args = appendFastModeFlag(args, a.FastMode)
	args = appendCopilotAPIPortFlag(args, a.CopilotAPIPort)
	args = appendInitialPromptArg(args, a)
	return args
}

// appendCopilotAPIFlag adds `--copilot-api` to a `tclaude session new` argv when
// the launch selected the API-backed Copilot drive. false omits it — the
// send-keys default — so a launch that never asked for the API produces exactly
// the argv it produced before this flag existed. Bare boolean flag; the forked
// `session new` re-validates it against the harness (a non-Copilot harness
// rejects an explicit opt-in).
func appendCopilotAPIFlag(args []string, copilotAPI bool) []string {
	if copilotAPI {
		args = append(args, "--copilot-api")
	}
	return args
}

func appendCodexAppServerArgs(args []string, a clcommon.SpawnArgs) []string {
	if !a.CodexAppServer {
		return args
	}
	return append(args,
		"--codex-app-server",
		"--codex-app-server-generation", a.CodexAppServerGeneration,
		"--codex-app-server-socket", a.CodexAppServerSocket,
		"--codex-app-server-url", a.CodexAppServerURL,
		"--codex-app-server-token-sha256", a.CodexAppServerTokenSHA256,
		"--codex-app-server-token-handoff", a.CodexAppServerTokenHandoff,
		"--codex-app-server-pid-file", a.CodexAppServerPIDFile,
		"--codex-app-server-log-file", a.CodexAppServerLogFile,
	)
}

func fastModeLaunchValue(value, set bool) string {
	if !set {
		return harness.FastModeInherit
	}
	if value {
		return harness.FastModeOn
	}
	return harness.FastModeOff
}

// appendFastModeFlag carries all three service-tier states into the forked
// session launcher. Empty means inherit and is omitted; on/off are explicit.
func appendFastModeFlag(args []string, mode string) []string {
	if mode != "" {
		args = append(args, "--fast-mode", mode)
	}
	return args
}

// appendCopilotAPIPortFlag adds `--copilot-api-port <n>` for a launch whose
// port agentd already allocated. 0 omits it, which is every send-keys launch
// and every other harness.
//
// Emitted next to `--copilot-api` rather than folded into it because they
// answer different questions — which drive, and which port — and the forked
// `session new` validates them together: a port without the drive, or the
// drive without a port, is refused there rather than half-applied here.
//
// That second clause was false until TCL-1084: `ResolveCopilotAPIPort` allocated
// a port for a drive that arrived without one instead of refusing it, which is
// how a hand-typed `session new --copilot-api` ended up binding an endpoint
// nothing would ever dial. It survived because THIS caller always allocates
// first (prepareCopilotAPIPort), so agentd never emits the drive without a port
// and the observable behaviour on the only exercised path matched the sentence.
// A comment describing a callee from the caller's vantage cannot be falsified by
// the path that exercises it — the reason claims like this belong next to the
// refusal or next to a test that proves it, and the reason the refusal now
// exists.
func appendCopilotAPIPortFlag(args []string, port int) []string {
	if port > 0 {
		args = append(args, "--copilot-api-port", strconv.Itoa(port))
	}
	return args
}

// appendAutoCompactWindowFlag adds `--auto-compact-window <tokens>` to a
// `tclaude session new` argv when the launch pinned Claude Code's
// auto-compaction capacity. "" omits it, leaving the model's own threshold in
// charge. The value is already canonical decimal by this point; the forked
// `session new` re-validates and re-normalizes it, and rejects it for a harness
// with no such knob.
func appendAutoCompactWindowFlag(args []string, window string) []string {
	if window = strings.TrimSpace(window); window != "" {
		args = append(args, "--auto-compact-window", window)
	}
	return args
}

// appendContextWindowMaxFlag carries tclaude's configured Copilot meter
// denominator into the child session launcher. It is intent, not a Copilot
// CLI option, so the child records it without forwarding it to the harness.
func appendContextWindowMaxFlag(args []string, max int64) []string {
	if max > 0 {
		args = append(args, "--context-window-max", strconv.FormatInt(max, 10))
	}
	return args
}

// appendContextFeaturesFlag adds `--context-features <slug>=<state>,…` to a
// `tclaude session new` argv when the launch resolved any startup-context trims.
// An empty map appends nothing: there is no flag spelling for "trim nothing"
// that differs from omitting it, because the forked `session new` has no profile
// tier stack of its own to override — the daemon already resolved the answer.
func appendContextFeaturesFlag(args []string, features map[string]string) []string {
	if rendered := harness.FormatContextFeatures(features); rendered != "" {
		args = append(args, "--context-features", rendered)
	}
	return args
}

// appendAutoMemoryFlag adds `--auto-memory` to a `tclaude session new` argv
// when the spawn opted back INTO Claude Code's auto memory. false omits it,
// which is the recommended posture and makes the forked `session new` inject
// CLAUDE_CODE_DISABLE_AUTO_MEMORY=1. Bare boolean flag; the forked `session
// new` re-validates it against the harness (a non-Claude harness rejects an
// explicit opt-in).
func appendAutoMemoryFlag(args []string, autoMemory bool) []string {
	if autoMemory {
		args = append(args, "--auto-memory")
	}
	return args
}

// appendAskTimeoutFlag adds `--ask-user-question-timeout <v>` to a `tclaude
// session new` argv when the spawn chose a Claude Code AskUserQuestion
// idle-timeout override (never|60s|5m|10m). "" omits it. The forked `session
// new` re-validates it against the harness (a non-Claude harness rejects it) and
// the CC spawner folds it into its merged `--settings` payload alongside the
// sandbox block.
func appendAskTimeoutFlag(args []string, askTimeout string) []string {
	if askTimeout != "" {
		args = append(args, "--ask-user-question-timeout", askTimeout)
	}
	return args
}

// appendRemoteControlFlag adds `--remote-control` to a `tclaude session new`
// argv when the spawn asked to start with Remote Access on (JOH-258). false
// omits it. It is a bare boolean flag; the forked `session new` re-validates it
// against the harness (a non-Claude-Code harness rejects it) and the CC spawner
// emits `claude --remote-control`. Position in THIS argv is irrelevant (boa
// parses flags); the load-bearing ordering is in claudeSpawner.BuildCommand,
// which emits the flag first so its optional [name] can't swallow the prompt.
func appendRemoteControlFlag(args []string, remoteControl bool) []string {
	if remoteControl {
		args = append(args, "--remote-control")
	}
	return args
}

// sessionResumeArgs builds the argv for the detached `tclaude session
// new -r <conv>` that a resume forks. Same flag discipline as
// sessionNewArgs: --effort and --model are appended only when a value
// was chosen, so "" keeps claude on its own default. Kept pure so it
// can be unit-tested without forking a subprocess.
func sessionResumeArgs(a clcommon.SpawnArgs) []string {
	args := []string{"session", "new", "--managed-launch", "-r", a.ConvID, "-d", "--global"}
	if a.Cwd != "" {
		args = append(args, "-C", a.Cwd)
	}
	if a.SandboxSnapshotPath != "" {
		args = append(args, "--sandbox-snapshot-path", a.SandboxSnapshotPath,
			"--sandbox-snapshot-digest", a.SandboxSnapshotDigest)
	}
	// Without this the pane prepares its own boundary under the SAME
	// deterministic per-session path the daemon just started the managed server
	// in — and preparation reclaims a populated dir by killing its members, so
	// the omission is not a degraded launch but a dead server.
	if a.ResourceCgroupDir != "" {
		args = append(args, "--resource-cgroup-dir", a.ResourceCgroupDir)
	}
	if a.CwdWriteProof != "" {
		args = append(args, "--cwd-write-proof", a.CwdWriteProof)
	}
	if a.DirWriteProof != "" {
		args = append(args, "--dir-write-proof", a.DirWriteProof)
	}
	if a.CodexGitCommonDir != "" {
		args = append(args, "--codex-git-common-dir", a.CodexGitCommonDir)
	}
	if a.CodexGitCommonDirPinned {
		args = append(args, "--codex-git-common-dir-pinned")
	}
	for _, dir := range a.GitWorktreeWriteDirs {
		args = append(args, "--git-worktree-write-dir", dir)
	}
	if a.GitWorktreeWriteDirsPinned {
		args = append(args, "--git-worktree-write-dirs-pinned")
	}
	if a.RouteHelperAgentID != "" && a.RouteHelperConvID != "" {
		args = append(args,
			"--route-helper-agent-id", a.RouteHelperAgentID,
			"--route-helper-conv-id", a.RouteHelperConvID,
			"--route-helper-launch-generation", a.RouteHelperLaunchGeneration,
			"--route-helper-credential-handoff-socket", a.RouteHelperCredentialHandoffSocketPath)
		if a.RouteHelperProxyOnly {
			args = append(args, "--route-helper-proxy-only")
		}
		for _, groupID := range a.RouteHelperGroupIDs {
			args = append(args, "--route-helper-group-id", strconv.FormatInt(groupID, 10))
		}
	}
	// Launch-enrollment fields on a RESUME: a clone forks its source's jsonl and
	// resumes into it, so its display name and first-turn handoff have to ride
	// this argv rather than being injected once the pane answers (TCL-732).
	//
	// No --session-id here — a resume already has a conversation id. And no
	// harness-default seed fallback either (unlike sessionNewArgs): the seed
	// exists to make Codex mint an id at first turn, which a resumed
	// conversation has by definition, so an unset InitialPrompt must stay unset.
	if a.Name != "" {
		args = append(args, "--name", a.Name)
	}
	if a.InitialPrompt != "" {
		args = append(args, "--initial-prompt", a.InitialPrompt)
	}
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	args = appendHarnessFlag(args, a.Harness)
	args = appendSandboxArgs(args, a.Harness, a.Sandbox)
	args = appendSandboxImplementationFlag(args, a.SandboxImplementation)
	args = appendDarwinRouteFlags(args, a.DarwinRouteCapable, a.DarwinRouteAgentID)
	args = appendSandboxChosenByFlag(args, a.SandboxChosenBy)
	args = appendAskTimeoutFlag(args, a.AskUserQuestionTimeout)
	args = appendApprovalFlag(args, a.Approval)
	args = appendToolGovernanceFlag(args, a.ToolGovernance)
	args = appendAutoReviewFlag(args, a.AutoReview)
	// Re-arm Claude Code's built-in Remote Access on the relaunched pane when
	// the SOURCE conv was armed (JOH-261). claudeSpawner.BuildCommand emits
	// `--remote-control` LAST on the resume (--resume) path too, so its optional
	// [name] stays empty and the flag is unambiguous. Omitted when false; a
	// non-CC harness never sets it (remoteControlForRelaunch gates on the
	// harness capability), so the forked `session new -r` never rejects it.
	args = appendRemoteControlFlag(args, a.RemoteControl)
	// Preserve the SOURCE conv's auto-memory opt-in across the relaunch.
	// Omitted when false, which is the recommended posture and makes the forked
	// `session new -r` inject CLAUDE_CODE_DISABLE_AUTO_MEMORY=1.
	args = appendAutoMemoryFlag(args, a.AutoMemory)
	args = appendContextFeaturesFlag(args, a.ContextFeatures)
	// Preserve the SOURCE conv's pinned auto-compaction window across the
	// relaunch — otherwise the successor to an agent deliberately compacting at
	// 450K comes back running to the model's full window.
	args = appendAutoCompactWindowFlag(args, a.AutoCompactWindow)
	args = appendContextWindowMaxFlag(args, a.ContextWindowMax)
	// Preserve the SOURCE conv's drive across the relaunch: an agent deliberately
	// spawned onto the API must not come back on send-keys (or vice versa).
	args = appendCopilotAPIFlag(args, a.CopilotAPI)
	args = appendCodexAppServerArgs(args, a)
	args = appendFastModeFlag(args, a.FastMode)
	args = appendCopilotAPIPortFlag(args, a.CopilotAPIPort)
	return args
}

// appendHarnessFlag adds `--harness <h>` to a `tclaude session new` argv
// when h names a non-default harness. The empty string and the default
// harness (Claude Code) both omit the flag, so an untagged spawn keeps the
// exact pre-JOH-160 argv and Claude Code stays the zero-config default.
// h is a registered harness name (validated at the spawn boundary), never
// user free-text, so it is safe as a bare arg.
func appendHarnessFlag(args []string, h string) []string {
	if h != "" && h != harness.DefaultName {
		args = append(args, "--harness", h)
	}
	return args
}

// codexSpawnSeedPrompt is the first-turn prompt a daemon-spawned Codex pane
// submits to ITSELF at launch. Codex generates its conversation id at launch
// but only persists/exposes it (rollout file, threads row, hooks) once a turn
// runs (JOH-205); an unattended pane has no human to type that first message,
// so without a seed the conv-id never materialises and the spawn hangs. The
// prompt is deliberately inert — it asks the agent to acknowledge and WAIT, so
// the turn happens (materialising the id) without the agent acting before its
// real identity/role/task briefing arrives via the post-connection welcome +
// inbox. It does not touch the agentd socket, so it is unaffected by JOH-207.
const codexSpawnSeedPrompt = "[tclaude] You are being started as a managed agent. " +
	"Reply with a brief acknowledgement to confirm you are up, then wait — your identity, role, and task " +
	"briefing will be delivered to you next. Do not take any other action until you receive it."

// appendInitialPromptFlag seeds a daemon-spawned Codex pane with the first-turn
// prompt above so its conv-id materialises without a human (JOH-205). Emitted
// only for Codex — Claude Code reports its conv-id at launch (SessionStart
// hook) and needs no seed. It lives on the daemon spawn path (sessionNewArgs),
// NOT the shared `tclaude session new` entrypoint, so a human's direct
// `session new` never gets a seed and still types their own first message. The
// forked `session new` re-validates; codexSpawner emits the positional [PROMPT]
// only on a fresh launch, so a resume (where the id is already known) ignores it.
func appendInitialPromptFlag(args []string, h string) []string {
	if h == harness.CodexName {
		args = append(args, "--initial-prompt", codexSpawnSeedPrompt)
	}
	return args
}

// appendInitialPromptArg forwards the first-turn launch prompt. When the
// caller supplied one explicitly (the launch-enrollment path, where it is the
// agent's welcome turn), it rides through verbatim. Otherwise it falls back to
// the harness's default seed (Codex's conv-id seed; nothing for Claude Code on
// the legacy injection path, where the welcome is sent over tmux instead).
func appendInitialPromptArg(args []string, a clcommon.SpawnArgs) []string {
	if a.InitialPrompt != "" {
		return append(args, "--initial-prompt", a.InitialPrompt)
	}
	return appendInitialPromptFlag(args, a.Harness)
}

// appendSandboxArgs adds the launch-containment flag(s) to a `tclaude session
// new` argv. For a Codex spawn whose resolved mode is the managed-profile
// pseudo-mode (SandboxManagedProfile — the secure default), it emits
// `--permission-profile tclaude-agent` INSTEAD of `--sandbox`: that managed
// profile gives workspace-write containment AND allowlists the agentd Unix
// socket, so the spawned agent can run `tclaude agent …` (JOH-207). Codex
// ignores a permission profile whenever a `--sandbox`/sandbox_mode is present,
// so the two can't be combined. All other cases — the raw workspace-write,
// read-only, or danger-full-access `--sandbox` modes, or a non-Codex harness —
// fall back to `--sandbox`. (Those raw modes intentionally do NOT get the
// managed profile, so a caller can pick Codex's native containment; note an
// agent under a raw `--sandbox` mode cannot reach the agentd socket.) h is the
// param name because sessionNewArgs shadows the harness package with a
// `harness` string parameter.
func appendSandboxArgs(args []string, h, sandbox string) []string {
	if h == harness.CodexName && sandbox == harness.SandboxManagedProfile {
		return appendPermissionProfileFlag(args, harness.CodexAgentProfile)
	}
	return appendSandboxFlag(args, sandbox)
}

// appendSandboxFlag adds `--sandbox <mode>` to a `tclaude session new` argv
// when a mode is set. "" omits it (no sandbox handling — Claude Code, or a
// caller that didn't resolve one). The mode is a validated enum resolved at
// the spawn boundary (harness.ResolveHarnessBuiltinMode), never user free-text, so
// it is safe as a bare arg. The forked `tclaude session new` re-validates.
func appendSandboxFlag(args []string, mode string) []string {
	if mode != "" {
		args = append(args, "--sandbox", mode)
	}
	return args
}

// appendSandboxImplementationFlag leaves the legacy harness-owned default
// unpinned, while every explicit non-default implementation is carried into
// the session boundary and its durable launch record.
func appendSandboxImplementationFlag(args []string, implementation string) []string {
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err == nil && strings.TrimSpace(implementation) != "" &&
		normalized != sandboxpolicy.ImplementationHarnessBuiltin {
		args = append(args, "--sandbox-impl", string(normalized))
	}
	return args
}

func appendDarwinRouteFlags(args []string, capable bool, agentID string) []string {
	if !capable {
		return args
	}
	args = append(args, "--darwin-route-capable")
	if strings.TrimSpace(agentID) != "" {
		args = append(args, "--darwin-route-agent-id", agentID)
	}
	return args
}

// appendSandboxChosenByFlag carries the resolved sandbox mode's PROVENANCE —
// which tier of the profile stack supplied it — into the forked `tclaude
// session new`, which records it beside the launch's sandbox verdict. "" omits
// it, leaving the launch unattributed exactly as before the flag existed.
//
// Unlike the mode beside it, this value embeds an operator-authored spawn
// profile NAME, so it is emitted in `--flag=value` form: a name beginning with
// a dash would otherwise be parsed as the next flag rather than as this one's
// value. The forked session new bounds and scrubs it before recording.
func appendSandboxChosenByFlag(args []string, chosenBy string) []string {
	if strings.TrimSpace(chosenBy) != "" {
		args = append(args, "--sandbox-chosen-by="+chosenBy)
	}
	return args
}

// appendPermissionProfileFlag adds `--permission-profile <name>` to a `tclaude
// session new` argv when a profile is set. "" omits it. The name is a
// validated identifier (a tclaude-owned constant on the daemon path), never
// user free-text, so it is safe as a bare arg; the forked `tclaude session
// new` re-validates and ensures the managed profile file exists.
func appendPermissionProfileFlag(args []string, profile string) []string {
	if profile != "" {
		args = append(args, "--permission-profile", profile)
	}
	return args
}

// appendApprovalFlag adds `--ask-for-approval <policy>` to a `tclaude session
// new` argv when a policy is set. "" omits it (no override — e.g. a Claude
// inherit, or a caller that didn't resolve one). `--ask-for-approval` is the
// harness-agnostic session-new flag name; the forked `session new` re-validates
// it per-harness (a Codex policy vs a Claude --permission-mode value) and the
// spawner emits the harness-appropriate flag. The value is a validated enum
// resolved at the spawn boundary (harness.ResolveApprovalPolicy), never user
// free-text, so it is safe as a bare arg. See JOH-200.
func appendApprovalFlag(args []string, policy string) []string {
	if policy != "" {
		args = append(args, "--ask-for-approval", policy)
	}
	return args
}

// appendToolGovernanceFlag carries OpenCode's resolved homogeneous tool action
// through the internal session-new round trip. Empty omits the axis.
func appendToolGovernanceFlag(args []string, policy string) []string {
	if policy != "" {
		args = append(args, "--tools", policy)
	}
	return args
}

// appendAutoReviewFlag adds `--auto-review` to a `tclaude session new` argv when
// the spawn opted into the harness's guardian subagent. false (the default)
// omits it, so an ordinary spawn keeps the human as approval reviewer. It is a
// boolean flag — no value — gated at the spawn boundary (harness.ResolveAutoReview
// rejects it for a harness with no guardian), and the forked `tclaude session
// new` re-validates. Experimental/undocumented upstream, hence opt-in. See
// JOH-200 part 2.
func appendAutoReviewFlag(args []string, autoReview bool) []string {
	if autoReview {
		args = append(args, "--auto-review")
	}
	return args
}

// appendTrustDirFlag adds `--trust-dir` to a `tclaude session new` argv when
// the spawn opted into pre-trusting its launch dir. false (the default) omits
// it, so an ordinary spawn leaves the harness's trust-folder dialog in place.
// It is a boolean flag — no value — gated at the spawn boundary
// (harness.ResolveTrustDir rejects it for a harness with no trust dialog), and
// the forked `tclaude session new` re-validates and performs the actual write
// into that harness's own store. Opt-in only because it edits a config tclaude
// does not own (JOH-205 inc4).
func appendTrustDirFlag(args []string, trustDir bool) []string {
	if trustDir {
		args = append(args, "--trust-dir")
	}
	return args
}

// wrapperFailureSignals routes a detached spawn wrapper's failure back to
// the executeSpawn that forked it. The proofless launch path is
// fire-and-forget — liveSpawnNew returns as soon as the wrapper STARTS — so
// a wrapper that dies after the fork (bad cwd, launch-script write failure,
// tmux refusal) would otherwise never be distinguishable from a slow pane:
// executeSpawn polled to timeout and returned the preset conv-id as a
// success, stranding the pre-fork enrollment as a ghost. Keyed by spawn
// label; channels are buffered(1) and delivery is non-blocking, so an
// unregistered/late signal is dropped harmlessly.
var wrapperFailureSignals sync.Map // label -> chan error

func registerWrapperFailureSignal(label string) chan error {
	ch := make(chan error, 1)
	wrapperFailureSignals.Store(label, ch)
	return ch
}

func unregisterWrapperFailureSignal(label string) {
	wrapperFailureSignals.Delete(label)
}

func signalWrapperFailure(label string, err error) {
	v, ok := wrapperFailureSignals.Load(label)
	if !ok {
		return
	}
	select {
	case v.(chan error) <- err:
	default:
	}
}

// SignalSpawnWrapperFailureForTest lets flow tests exercise the
// fire-and-forget wrapper-failure path: a fake Spawner returns nil (the
// wrapper started) and then reports the wrapper's death the way
// liveSpawnNew's reaper goroutine would.
func SignalSpawnWrapperFailureForTest(label string, err error) {
	signalWrapperFailure(label, err)
}

// liveSpawnNew runs `tclaude session new -d --global --label <label>`
// as a fully-detached subprocess. Same detachment story as
// liveSpawnResume — see its doc comment for the full rationale on
// why this doesn't trip CC's process-ownership checks.
//
// The label is the tclaude-side session ID (used to look up the row
// in SQLite once the conv-id materialises). It must be unique in the
// sessions table.
func liveSpawnNew(a clcommon.SpawnArgs) error {
	var cleanup func()
	var err error
	a, cleanup, err = spawnArgsWithSandboxHandoff(a)
	if err != nil {
		return err
	}
	routeCredentialCleanup := func() {}
	if a.RouteHelperCredential != "" {
		a.RouteHelperCredentialHandoffSocketPath, routeCredentialCleanup, err = prepareRouteHelperCredentialHandoff(a.RouteHelperCredential)
		if err != nil {
			cleanup()
			return err
		}
	}
	label := a.Label
	// effort, model, sandbox, approval, autoReview and trustDir are validated at
	// the spawn boundary (handleGroupSpawn / the `agent spawn` CLI) before they
	// reach here; the forked `tclaude session new` re-validates too, though by
	// then a bad value would only surface as a non-zero exit in the daemon
	// log. sessionNewArgs omits each flag entirely when its value is "" / false.
	cmd := exec.Command("tclaude", sessionNewArgs(a)...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	// Capture stderr so a silent subprocess failure (PATH issue, bad
	// cwd, broken tmux server, etc.) shows up in the daemon log
	// instead of disappearing into /dev/null. Bounded so a runaway
	// child can't grow the buffer unboundedly.
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	// Spawned agents must not inherit the human's operator token.
	cmd.Env = environmentWithCodexStateRoot(spawnEnvWithoutOperatorToken(), a.CodexStateRoot)
	if len(a.OpenCodeEnvironment) > 0 {
		cmd.Env = openCodeAttachProcessEnvironment(cmd.Env)
	}
	if a.OpenCodeServerURL != "" {
		cmd.Env = append(cmd.Env, "TCLAUDE_OPENCODE_SERVER_URL="+a.OpenCodeServerURL)
	}
	if a.OpenCodeServerPassword != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+a.OpenCodeServerPassword)
	}
	appendOpenCodeTransportEnvironment(cmd, a)
	for _, entry := range a.OpenCodeEnvironment {
		cmd.Env = append(cmd.Env, entry.Name+"="+entry.Value)
	}
	if a.OpenCodeStateIsolation != "" {
		cmd.Env = append(cmd.Env,
			clcommon.OpenCodeStateIsolationEnv+"="+a.OpenCodeStateIsolation)
	}
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		routeCredentialCleanup()
		cleanup()
		return err
	}
	pid := cmd.Process.Pid
	if a.CwdWriteProof != "" || a.DirWriteProof != "" {
		defer cleanup()
		if err := cmd.Wait(); err != nil {
			routeCredentialCleanup()
			slog.Error("spawn subprocess exited with error",
				"label", label, "pid", pid, "err", err,
				"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
			return fmt.Errorf("spawn session wrapper failed: %w: %s", err, stderr.String())
		}
		return nil
	}
	go func() {
		defer cleanup()
		if err := cmd.Wait(); err != nil {
			routeCredentialCleanup()
			slog.Error("spawn subprocess exited with error",
				"label", label, "pid", pid, "err", err,
				"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
			// Report the death to the executeSpawn that forked us (if it is
			// still polling) so the failure surfaces to the caller instead of
			// timing out into a false success. See wrapperFailureSignals.
			signalWrapperFailure(label, fmt.Errorf("spawn session wrapper failed: %w: %s", err, stderr.String()))
		}
	}()
	return nil
}

func appendOpenCodeTransportEnvironment(cmd *exec.Cmd, a clcommon.SpawnArgs) {
	if a.OpenCodeTransport == "" {
		return
	}
	cmd.Env = append(cmd.Env, clcommon.OpenCodeTransportEnv+"="+a.OpenCodeTransport)
	if a.OpenCodeTransport != db.OpenCodeTransportUnixRelay {
		return
	}
	cmd.Env = append(cmd.Env,
		clcommon.OpenCodeControlSocketPathEnv+"="+a.OpenCodeControlSocketPath,
		clcommon.OpenCodeControlSocketDeviceEnv+"="+strconv.FormatInt(
			a.OpenCodeControlSocketDevice, 10),
		clcommon.OpenCodeControlSocketInodeEnv+"="+strconv.FormatInt(
			a.OpenCodeControlSocketInode, 10),
		clcommon.OpenCodeServerPIDEnv+"="+strconv.Itoa(a.OpenCodeServerPID),
	)
}

func openCodeAttachProcessEnvironment(environment []string) []string {
	out := make([]string, 0, len(environment))
	for _, entry := range environment {
		if openCodePrivateEnvironmentName(strings.SplitN(entry, "=", 2)[0]) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// liveSpawnResume runs `tclaude session new -r <conv> -d --global`
// as a fully-detached subprocess.
//
// Detachment story:
//   - `tclaude session new -d` only means "don't attach this terminal
//     to the new tmux session." The wrapper still runs in foreground
//     and inherits whatever stdio its parent gave it.
//   - We explicitly null stdio so nothing leaks back into the
//     daemon's logs.
//   - detachSpawn (unix-only) sets Setsid so the wrapper has its own
//     session and process group — no controlling tty inherited from
//     the daemon, and on daemon exit the wrapper gets reparented to
//     init/PID 1 cleanly.
//   - The actual CC process is parented to the long-running tmux
//     server (because `tclaude session new -d` shells out to
//     `tmux new-session -d ...` which forks the command as a child of
//     the tmux server, not of the caller). So the CC process never
//     has *us* in its ancestry chain — important so it doesn't trip
//     CC's own process-ownership / sandbox checks via parent walks.
//
// The wrapper is reaped synchronously. It exits immediately after tmux accepts
// or rejects the detached launch, so callers learn whether this resume won the
// launch reservation and can roll back refreshed actor state on failure.
func liveSpawnResume(a clcommon.SpawnArgs) error {
	if err := prepareRouteHelperResumeArgs(&a); err != nil {
		return err
	}
	privateAttachmentRootCreated := false
	resumeLaunched := false
	defer func() {
		if privateAttachmentRootCreated && !resumeLaunched {
			_ = os.Remove(tclcommon.SpawnAttachmentsPrivateDir(a.ConvID))
		}
	}()
	var openCodeLaunch *openCodeLaunch
	if a.Harness == harness.OpenCodeName {
		resolvedCwd, cwdErr := resolveOpenCodeLaunchCwd(a.Cwd)
		if cwdErr != nil {
			return cwdErr
		}
		a.Cwd = resolvedCwd
		permissionJSON, policyErr := openCodePermissionJSONForLaunch(
			a.Cwd, a.Sandbox, a.Approval, a.ToolGovernance, a.EffectiveSandbox)
		if policyErr != nil {
			return policyErr
		}
		implementation, implementationErr := sandboxpolicy.NormalizeImplementation(
			a.SandboxImplementation,
		)
		if implementationErr != nil {
			return implementationErr
		}
		if implementation == sandboxpolicy.ImplementationTclaudeLayer {
			_, created, prepareErr :=
				tclcommon.PrepareSpawnAttachmentsPrivateDir(a.ConvID)
			if prepareErr != nil {
				return prepareErr
			}
			privateAttachmentRootCreated = created
		}
		agentID, identityErr := db.AgentIDForConv(a.ConvID)
		if identityErr != nil {
			return identityErr
		}
		if agentID == "" && implementation == sandboxpolicy.ImplementationTclaudeLayer {
			return fmt.Errorf("OpenCode tclaude-layer resume has no stable agent identity")
		}
		sandboxSpec, sandboxErr := openCodeTclaudeLayerLaunchSpec(
			a.SandboxImplementation,
			a.Cwd,
			a.GitWorktreeWriteDirs,
			a.EffectiveSandbox,
			agentID,
			a.ConvID,
		)
		if sandboxErr != nil {
			return sandboxErr
		}
		resourceCgroupDir, resourceCleanup, resourceErr := prepareManagedServerResourceCgroup(
			a.ConvID, a.EffectiveSandbox, a.SandboxImplementation, a.AllowUnenforcedSandbox, true)
		if resourceErr != nil {
			return resourceErr
		}
		var err error
		openCodeLaunch, err = startOpenCodeRuntimeForSpawn(
			a.ConvID, a.Cwd, "", a.ConvID, permissionJSON,
			a.SandboxImplementation, sandboxSpec, resourceCgroupDir)
		if err != nil && resourceCgroupDir != "" &&
			errors.Is(err, errOpenCodeResourceCgroup) &&
			degradeManagedServerResourceCgroup(
				a.EffectiveSandbox, a.SandboxImplementation, a.AllowUnenforcedSandbox, true, err) {
			resourceCleanup()
			resourceCgroupDir = ""
			openCodeLaunch, err = startOpenCodeRuntimeForSpawn(
				a.ConvID, a.Cwd, "", a.ConvID, permissionJSON,
				a.SandboxImplementation, sandboxSpec, "")
		}
		if err != nil {
			resourceCleanup()
			return err
		}
		a.OpenCodeServerURL = openCodeLaunch.ServerURL
		a.OpenCodeServerPassword = openCodeLaunch.Password
		a.OpenCodeTransport = openCodeLaunch.Transport
		a.OpenCodeControlSocketPath = openCodeLaunch.ControlSocketPath
		a.OpenCodeControlSocketDevice = openCodeLaunch.ControlSocketDevice
		a.OpenCodeControlSocketInode = openCodeLaunch.ControlSocketInode
		a.OpenCodeServerPID = openCodeLaunch.PID
		a.ResourceCgroupDir = resourceCgroupDir
		if sandboxSpec != nil {
			a.OpenCodeEnvironment = append(
				[]sandboxpolicy.EnvironmentEntry(nil), sandboxSpec.Contract.Environment...)
			allocation, allocationErr := requireOpenCodeStateAllocation(agentID)
			if allocationErr != nil {
				return allocationErr
			}
			a.OpenCodeStateIsolation = allocation.Mode
		}
	}
	var cleanup func()
	var err error
	a, cleanup, err = spawnArgsWithSandboxHandoff(a)
	if err != nil {
		if openCodeLaunch != nil {
			_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
		}
		return err
	}
	routeCredentialCleanup := func() {}
	if a.RouteHelperCredential != "" {
		a.RouteHelperCredentialHandoffSocketPath, routeCredentialCleanup, err = prepareRouteHelperCredentialHandoff(a.RouteHelperCredential)
		if err != nil {
			cleanup()
			if openCodeLaunch != nil {
				_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
			}
			return err
		}
	}
	convID := a.ConvID
	args := sessionResumeArgs(a)
	cmd := exec.Command("tclaude", args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	// Spawned agents must not inherit the human's operator token.
	cmd.Env = environmentWithCodexStateRoot(spawnEnvWithoutOperatorToken(), a.CodexStateRoot)
	if len(a.OpenCodeEnvironment) > 0 {
		cmd.Env = openCodeAttachProcessEnvironment(cmd.Env)
	}
	if a.OpenCodeServerURL != "" {
		cmd.Env = append(cmd.Env, "TCLAUDE_OPENCODE_SERVER_URL="+a.OpenCodeServerURL)
	}
	if a.OpenCodeServerPassword != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+a.OpenCodeServerPassword)
	}
	appendOpenCodeTransportEnvironment(cmd, a)
	for _, entry := range a.OpenCodeEnvironment {
		cmd.Env = append(cmd.Env, entry.Name+"="+entry.Value)
	}
	if a.OpenCodeStateIsolation != "" {
		cmd.Env = append(cmd.Env,
			clcommon.OpenCodeStateIsolationEnv+"="+a.OpenCodeStateIsolation)
	}
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		routeCredentialCleanup()
		cleanup()
		if openCodeLaunch != nil {
			_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
		}
		return err
	}
	pid := cmd.Process.Pid
	defer cleanup()
	if err := cmd.Wait(); err != nil {
		routeCredentialCleanup()
		if openCodeLaunch != nil {
			_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
		}
		slog.Error("resume subprocess exited with error",
			"conv", convID, "pid", pid, "err", err,
			"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
		return fmt.Errorf("resume session wrapper failed: %w: %s", err, stderr.String())
	}
	resumeLaunched = true
	return nil
}

// prepareRouteHelperResumeArgs restores the namespace-local helper for every
// known resume surface (group resume, recovery, reincarnation, and clone).
// The resumed conversation already has a stable identity, so this path can
// support both Claude and Codex; OpenCode's server boundary has no pane-local
// helper and is refused when its group has opted into routes.
func prepareRouteHelperResumeArgs(a *clcommon.SpawnArgs) error {
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || a == nil || strings.TrimSpace(a.ConvID) == "" {
		return nil
	}
	if runtime.GOOS == "darwin" && !a.DarwinRouteCapable {
		return nil
	}
	groups, err := db.ListGroupsForConv(a.ConvID)
	if err != nil {
		return fmt.Errorf("resolve route-enabled groups for resume: %w", err)
	}
	var routeGroups []int64
	for _, group := range groups {
		enabled, enabledErr := db.IsAgentGroupRouteEnabled(group.ID, PermRoutesPublish, PermRoutesConsume)
		if enabledErr != nil {
			return fmt.Errorf("resolve route capability for group %d: %w", group.ID, enabledErr)
		}
		if enabled {
			routeGroups = append(routeGroups, group.ID)
		}
	}
	if len(routeGroups) == 0 {
		return nil
	}
	if a.SandboxImplementation != string(sandboxpolicy.ImplementationTclaudeLayer) || a.Harness == harness.OpenCodeName {
		return errors.New("linux group routes require a pane-authoritative tclaude-layer resume")
	}
	agentID, err := db.AgentIDForConv(a.ConvID)
	if err != nil || strings.TrimSpace(agentID) == "" {
		if err == nil {
			err = errors.New("stable agent identity is empty")
		}
		return fmt.Errorf("resolve route helper identity for resume: %w", err)
	}
	a.RouteHelperAgentID = agentID
	a.RouteHelperConvID = a.ConvID
	credential, generation, credentialErr := mintRouteHelperCredential(agentID, a.ConvID)
	if credentialErr != nil {
		return fmt.Errorf("mint route helper credential for resume: %w", credentialErr)
	}
	a.RouteHelperLaunchGeneration = generation
	a.RouteHelperCredential = credential
	a.RouteHelperGroupIDs = routeGroups
	a.RouteHelperProxyOnly = runtime.GOOS == "darwin"
	return nil
}

func spawnArgsWithSandboxHandoff(a clcommon.SpawnArgs) (clcommon.SpawnArgs, func(), error) {
	cleanup := func() {}
	if a.EffectiveSandbox == nil {
		return a, cleanup, nil
	}
	path, digest, err := sandboxpolicy.WriteSnapshotFile(config.DataDir(), *a.EffectiveSandbox)
	if err != nil {
		return clcommon.SpawnArgs{}, cleanup, err
	}
	a.SandboxSnapshotPath = path
	a.SandboxSnapshotDigest = digest
	cleanup = func() { _ = os.Remove(path) }
	return a, cleanup, nil
}

// spawnStderrCapture is a bounded io.Writer used for capturing
// subprocess stderr of detached `tclaude session new` invocations.
// Caps at spawnStderrMax bytes; further writes are silently dropped
// and Truncated() reports whether truncation happened. Concurrent
// writes are not expected (exec.Cmd has a single writer goroutine)
// but the mutex makes accidental concurrent String() calls safe.
const spawnStderrMax = 8 << 10

type spawnStderrCapture struct {
	buf       []byte
	truncated bool
}

func newSpawnStderrCapture() *spawnStderrCapture {
	return &spawnStderrCapture{buf: make([]byte, 0, 512)}
}

func (c *spawnStderrCapture) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	remaining := spawnStderrMax - len(c.buf)
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf = append(c.buf, p[:remaining]...)
		c.truncated = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *spawnStderrCapture) String() string {
	if c == nil {
		return ""
	}
	return strings.TrimRight(string(c.buf), "\r\n ")
}

func (c *spawnStderrCapture) Truncated() bool {
	if c == nil {
		return false
	}
	return c.truncated
}
