package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

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
)

// memberOpResult is the per-member outcome of a bulk lifecycle op
// (stop / resume). The CLI prints these as a summary table so the
// human can see which members succeeded, which were no-ops, and
// which failed.
type memberOpResult struct {
	// AgentID is the member's stable actor key — the canonical ID the CLI
	// leads with in the result table; ConvID is the live generation behind it.
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title,omitempty"`
	Action  string `json:"action"`           // "soft_stopped", "killed", "killed_no_soft_exit", "resumed", "skipped:already_online", "skipped:no_conv_id", "error"
	Detail  string `json:"detail,omitempty"` // human-readable note (e.g. error message)
	TmuxSes string `json:"tmux_session,omitempty"`
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
	if _, ok := requireGroupPermission(w, r, PermGroupsStop, g); !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	action := db.AgentExitActionStop
	if force {
		action = db.AgentExitActionForceStop
	}
	requestEventID := auditRequestEventID(r)
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := groupOpResp{Group: g.Name, Action: "stop", Members: []memberOpResult{}}
	for _, m := range members {
		res := stopOneConvWithIntent(m.ConvID, force, action, requestEventID)
		res.AgentID = peerAgentID(m.ConvID)
		res.Title = agent.FreshTitle(m.ConvID)
		out.Members = append(out.Members, res)
	}
	writeJSON(w, http.StatusOK, out)
}

// stopOneConv soft-stops (or force-kills with `force=true`) the live
// tmux session for convID. Returns the per-conv result. Shared between
// the bulk groups.stop loop and the single-conv agent.stop endpoint.
//
// Result shape mirrors the existing memberOpResult so the bulk
// summary table renders the same regardless of how the call was
// initiated. Idempotent: convs already offline come back as
// `skipped:already_offline`.
func stopOneConv(convID string, force bool) memberOpResult {
	return stopOneConvWithIntent(convID, force, "", "")
}

// stopOneConvWithIntent adds audit-only lifecycle attribution to a stop. The
// intent is armed immediately before the tmux mutation and cleared whenever
// that mutation fails. Persistence is deliberately best-effort: an audit I/O
// failure is logged but never changes the established stop result.
func stopOneConvWithIntent(convID string, force bool, lifecycleAction, relatedEventID string) memberOpResult {
	res := memberOpResult{ConvID: convID}
	sess := pickAliveSession(convID)
	if sess == nil {
		res.Action = "skipped:already_offline"
		return res
	}
	res.TmuxSes = sess.TmuxSession
	target, targetErr := captureLifecycleTarget(sess)
	if targetErr != nil {
		res.Action = "error"
		res.Detail = "capture selected pane: " + targetErr.Error()
		return res
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
			return res
		}
		if err := killLifecycleTarget(target); err != nil {
			clearFailedExitIntent(intentSet)
			res.Action = "error"
			res.Detail = "kill-session: " + err.Error()
		} else {
			res.Action = "killed"
		}
		return res
	}
	// Soft stop: inject the harness's exit command (CC's `/exit`). The
	// harness closes the conversation cleanly and the tmux session goes
	// away when it exits. The command is sourced from the harness's
	// Lifecycle so a non-CC pane is never typed `/exit` if that's not its
	// exit command.
	h := harnessForConv(convID)
	if h.SupportsSoftExit() {
		exitCmd := h.Life.SoftExitCommand()
		intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
		if lifecycleAction != "" && intentSet == nil {
			res.Action, res.Detail = "error", "selected launch intent became stale"
			return res
		}
		if injectSoftExitTarget(target, exitCmd, "soft-exit", intentSet) {
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
			res.Detail = "send-keys " + exitCmd + " failed"
		}
		return res
	}
	// No soft-exit command for this harness → hard kill so the pane never
	// lingers because we couldn't type a graceful exit.
	intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
	if lifecycleAction != "" && intentSet == nil {
		res.Action, res.Detail = "error", "selected launch intent became stale"
		return res
	}
	if err := killLifecycleTarget(target); err != nil {
		clearFailedExitIntent(intentSet)
		res.Action = "error"
		res.Detail = "kill-session (harness has no soft-exit): " + err.Error()
	} else {
		res.Action = "killed_no_soft_exit"
	}
	return res
}

type lifecycleTarget struct {
	sessionID   string
	tmuxSession string
	generation  string
	paneID      string
	panePID     int
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
	return &lifecycleTarget{sessionID: sess.ID, tmuxSession: sess.TmuxSession, generation: identity.Generation, paneID: p.paneID, panePID: p.panePID}, nil
}

func probeLifecyclePane(tmuxSession string) (lifecyclePaneProbe, error) {
	format := "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}"
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", clcommon.ExactTarget(tmuxSession)+":", format).Output()
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
	if p.state != paneProbeLive || p.paneID != t.paneID || (t.panePID > 0 && p.panePID != t.panePID) || (t.generation != "" && p.generation != t.generation) {
		return p, fmt.Errorf("selected pane identity changed")
	}
	return p, nil
}

func killLifecycleTarget(t *lifecycleTarget) error {
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := t.revalidate(); err != nil {
		return err
	}
	return clcommon.TmuxCommand("kill-pane", "-t", t.paneID).Run()
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
	return &ref
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

func injectSoftExitTarget(target *lifecycleTarget, exitCmd, reason string, intentRef *db.SessionExitIntentRef) bool {
	if target == nil {
		return false
	}
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := target.revalidate(); err != nil {
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
		return false
	}
	if err := injectTextAndSubmit(target.paneID, exitCmd); err != nil {
		return false
	}
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
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
		return false
	}
	scheduleSoftExitRetryTarget(target, exitCmd, reason, intentRef)
	return true
}

func lifecycleProbeMatchesTarget(probe lifecyclePaneProbe, target *lifecycleTarget) bool {
	return probe.paneID == target.paneID && (target.panePID <= 0 || probe.panePID == target.panePID) && (target.generation == "" || probe.generation == "" || probe.generation == target.generation)
}

func lifecycleSessionAlive(tmuxSession string) (alive, known bool) {
	err := clcommon.TmuxCommand("has-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
	if err == nil {
		return true, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

func scheduleUnknownIntentCleanup(target *lifecycleTarget, intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		time.Sleep(softExitRetryDelay)
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
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

func scheduleSoftExitRetryTarget(target *lifecycleTarget, exitCmd, reason string, intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		for attempt := 2; attempt <= softExitMaxAttempts; attempt++ {
			time.Sleep(softExitRetryDelay)
			if beforeSoftExitTargetRetryProbeForTest != nil {
				beforeSoftExitTargetRetryProbeForTest(attempt)
			}
			probe, err := probeLifecyclePane(target.tmuxSession)
			if err != nil || probe.state == paneProbeUnknown {
				clearFailedExitIntentTarget(intentRef, target.tmuxSession)
				return
			}
			if probe.state == paneProbeDead {
				return
			}
			if !lifecycleProbeMatchesTarget(probe, target) {
				clearFailedExitIntentTarget(intentRef, target.tmuxSession)
				return
			}
			if err := injectTextAndSubmit(target.paneID, exitCmd); err != nil {
				clearFailedExitIntentTarget(intentRef, target.tmuxSession)
				return
			}
			if attempt == softExitMaxAttempts {
				clearFailedExitIntentTarget(intentRef, target.tmuxSession)
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
	// Capture before injection: a responsive pane can exit synchronously after
	// Enter, but that successful exit still owns the lifecycle intent and must
	// remain correlatable by callback/reaper.
	panePID := livePanePID(sess.TmuxSession)
	if !injectSlashCommand(convID, exitCmd, "", reason) {
		return false
	}
	// Capture the pane's live OS pid so the retry can tell THIS process apart
	// from a later one that reused the same tmux name (a resume re-derives the
	// name from the conv-id — see scheduleSoftExitRetry). 0 = couldn't read
	// it; skip the retry rather than risk re-injecting blind.
	if panePID > 0 {
		scheduleSoftExitRetry(convID, sess.TmuxSession, panePID, exitCmd, reason, intentRef)
	} else {
		clearFailedExitIntent(intentRef)
	}
	return true
}

// softExitRetryDelay is how long the background soft-exit retry waits
// before each re-check of a pane it asked to /exit. A package var so flow
// tests can shrink it (SetSoftExitRetryDelayForTest); production keeps a
// few seconds so a pane that's honouring /exit has time to close before
// we bother re-injecting.
var softExitRetryDelay = 4 * time.Second

// softExitMaxAttempts bounds the TOTAL number of soft-exit injections per
// stop (the initial one + retries). The first retry recovers an /exit
// lost to input-buffer junk (see injectSoftExit); the remaining margin
// covers an unlucky pane that was mid-render. Capped so a pane that simply
// will not exit isn't typed /exit forever — the escalation paths
// (escalateShutdown) own the force-kill fallback.
const softExitMaxAttempts = 3

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
func scheduleSoftExitRetry(convID, tmuxSession string, panePID int, exitCmd, reason string, intentRef *db.SessionExitIntentRef) {
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
			if err := injectTextAndSubmit(target, exitCmd); err != nil {
				slog.Warn("soft-exit retry inject failed",
					"error", err, "tmux_session", tmuxSession, "reason", reason)
				clearFailedExitIntent(intentRef)
				return
			}
		}
		if livePanePID(tmuxSession) == panePID {
			clearFailedExitIntent(intentRef)
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
	caller, ok := requireGroupPermission(w, r, PermGroupsResume, g)
	if !ok {
		return
	}
	authTarget := caller
	requestTrustRoot := caller == "" || hasHumanApprovalContinuation(r, PermGroupsResume, authTarget)
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := groupOpResp{Group: g.Name, Action: "resume", Members: []memberOpResult{}}
	for _, m := range members {
		res := resumeOneConvLocked(m.ConvID, false, requestTrustRoot)
		if res.Action == "error:resume_provenance" && !requestTrustRoot && parseAskHumanHeader(r) > 0 {
			if !requestResumeRecoveryApproval(w, r, PermGroupsResume, authTarget, m.ConvID) {
				return
			}
			res = resumeOneConvLocked(m.ConvID, false, true)
		}
		res.AgentID = peerAgentID(m.ConvID)
		res.Title = agent.FreshTitle(m.ConvID)
		out.Members = append(out.Members, res)
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
	}
	writeJSON(w, http.StatusOK, out)
}

// resumeOneConv spawns a detached `tclaude session new -r <conv>`
// for convID if it isn't already online. Returns the per-conv
// result. Shared between the bulk groups.resume loop and the
// single-conv agent.resume endpoint.
//
// Idempotent: convs already online come back as
// `skipped:already_online`. Empty conv-ids (placeholder members
// with no conv yet) come back as `skipped:no_conv_id` since we
// have no .jsonl to resume from — those are template-based
// spawns, deferred to a future "groups create --team" pass.
// resolveConvLaunchMetadata resolves how to (re)launch convID — its cwd, the
// inherited effort/model, and the harness it last ran on — WITHOUT requiring a
// live session, so an offline conv can be resumed or cloned straight from its
// stored conversation. The cascade prefers the freshest session row (precise cwd
// + inherited model/effort + harness; rows are updated_at DESC, [0] is freshest),
// then conv_index metadata (older/imported convs, which carry no effort/model),
// then the harness-native conversation store (e.g. a Codex rollout). ok=false
// when none resolve — the conv isn't locatable to relaunch.
//
// Shared by resumeOneConv and the clone-based export (JOH-266): the export needs
// the original's cwd to spawn the summary-writer clone into, and a clone works
// offline (it resumes from the .jsonl), so it must not depend on a live session.
func resolveConvLaunchMetadata(convID string) (cwd, effort, model, harnessName string, ok bool) {
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
// over resumeOneConvRecreate — the default for the bulk groups.resume /
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

var resumeLaunchLocks sync.Map // map[convID]*sync.Mutex

func resumeLaunchLock(convID string) *sync.Mutex {
	lock, _ := resumeLaunchLocks.LoadOrStore(convID, &sync.Mutex{})
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
	res := memberOpResult{ConvID: convID}
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
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
	// Offline managed resume requires a session-generation record carrying
	// daemon-private physical provenance. Conversation-index/native-store
	// pathnames remain useful for clone/export discovery, but they cannot safely
	// authorize an unattended relaunch after the pane that owned the cwd exited.
	sessions, sessionErr := db.FindSessionsByConvID(convID)
	if sessionErr != nil {
		res.Action = "error"
		res.Detail = "load resumable session metadata: " + sessionErr.Error()
		return res
	}
	if len(sessions) == 0 {
		res.Action = "error"
		res.Detail = "no resumable session row for this agent; delete/recreate the orphaned agent or restore it from a real conversation"
		return res
	}
	sourceSession := sessions[0]
	effort, model := inheritedLaunchFlags(sourceSession.ID)
	harnessName := sourceSession.Harness
	expected, provenanceErr := resumeprovenance.Decode(sourceSession.ResumeProvenance)
	cwd := strings.TrimSpace(sourceSession.Cwd)
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
		if err := db.SetSessionResumeProvenance(sourceSession.ID, encoded); err != nil {
			res.Action = "error"
			res.Detail = "human recovery could not persist current resume provenance: " + err.Error()
			return res
		}
		expected = observed
		slog.Info("resume: human trust root recovered target provenance",
			"conv", short8(convID), "session", sourceSession.ID, "cwd", expected.Cwd.Path)
	}
	// Re-arm Remote Access if the conv's own persisted best-known state was on
	// (JOH-261). Read BEFORE relaunch: resume keeps the conv-id but mints a NEW
	// session row defaulting remote_control=0, so the freshest row reads OFF the
	// moment the new pane reports in — the armed flag lives on the old/dead row,
	// which is still the most-recent until then.
	remoteControl := remoteControlForRelaunch(convID, harnessName)
	resumePolicy, snapshotErr := resolveResumeSandboxPolicy(convID)
	if snapshotErr != nil {
		res.Action = "error"
		res.Detail = "sandbox_profile_changed: " + snapshotErr.Error()
		return res
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
	// Relaunch never re-engages the experimental guardian (auto-review is an
	// explicit fresh-spawn opt-in, not persisted per-conv), so AutoReview stays false.
	relaunchSandbox := sandboxForHarness(harnessName)
	if fail := sandboxProfileCapabilityFailure(harnessName, relaunchSandbox, effectiveSandbox); fail != nil {
		res.Action = "error"
		res.Detail = "sandbox_profile_changed: " + fail.Msg
		return res
	}
	// Derive repository grants only from the verified durable identity. Calling
	// git rev-parse here would follow a mutable .git file a second time and could
	// turn a post-verification retarget into new write authority.
	codexGitCommonDirPinned := spawnUsesPinnedGitCommonDir(harnessName, relaunchSandbox)
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
	if effectiveSandbox != nil {
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
	approval, autoReview := approvalForRelaunch(convID, harnessName)
	if err := SpawnDetachedTclaudeResume(clcommon.SpawnArgs{
		EffectiveSandbox:           effectiveSandbox,
		ConvID:                     convID,
		Cwd:                        cwd,
		CwdWriteProof:              pinToken,
		CodexGitCommonDir:          codexGitCommonDir,
		CodexGitCommonDirPinned:    codexGitCommonDirPinned,
		GitWorktreeWriteDirs:       gitWriteDirs,
		GitWorktreeWriteDirsPinned: true,
		Effort:                     effort,
		Model:                      model,
		Harness:                    harnessName,
		Sandbox:                    relaunchSandbox,
		Approval:                   approval,
		AutoReview:                 autoReview,
		AskUserQuestionTimeout:     askTimeoutForRelaunch(convID),
		RemoteControl:              remoteControl,
	}); err != nil {
		res.Action = "error"
		res.Detail = "spawn: " + err.Error()
		if resumePolicy != nil && resumePolicy.Previous != nil && effectiveSandbox != nil {
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
		if resumePolicy != nil && resumePolicy.Previous != nil && effectiveSandbox != nil {
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

// groupRetireResp is the response shape of the bulk groups.retire
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
// groups.stop / groups.resume lifecycle family (which until now had no
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
// Permission: groups.retire (not in the global defaults — retiring
// agents is a sensitive cleanup the human normally drives; the slug
// delegates it to a trusted coordinator). Gated with
// requireGroupPermission, like the other bulk group endpoints
// (stop/resume/spawn): owning THIS group raises the slug by default
// (the owner-state bypass), so an owner can run its own team's
// lifecycle without an explicit grant. The bypass fills only the
// permUndecided gap — an explicit deny override is always
// authoritative and suppresses it.
func handleGroupRetire(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	caller, ok := requireGroupPermission(w, r, PermGroupsRetire, g)
	if !ok {
		return
	}
	filter, ferr := parseRetireStatusFilter(r.URL.Query().Get("status"))
	if ferr != nil {
		writeError(w, http.StatusBadRequest, "status", ferr.Error())
		return
	}
	out, err := bulkRetireGroupMembers(g, caller,
		strings.TrimSpace(r.URL.Query().Get("reason")),
		retireShouldShutdown(r), retireShouldDeleteWorktree(r), filter, nil,
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

	eg := new(errgroup.Group)
	eg.SetLimit(bulkRetireGroupConcurrency)
	for i, m := range members {
		eg.Go(func() error {
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
						return nil // not in the explicit selection — omit
					}
				case filter != nil:
					online, status := convLiveStatus(m.ConvID, alive)
					if !filter.matches(online, status) {
						return nil // filtered out — omit from the response
					}
				}
				res, ownerGroupsPer[i] = retireGroupMember(
					m.ConvID, by, reason, shutdown, deleteWorktree, retireWorktrees[m.ConvID], res, relatedEventID)
			}
			results[i] = &res
			return nil
		})
	}
	_ = eg.Wait() // workers never return an error — per-member failures live in res.Action

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

	if shutdown {
		sd := stopOneConvWithIntent(convID, false /* soft exit */, db.AgentExitActionRetire, relatedEventID)
		res.TmuxSes = sd.TmuxSes
		if sd.Action == "soft_stopped" {
			res.Detail = joinDetail(res.Detail, "/exit sent")
		}
	}
	cleanupAgentDirectoriesAfterRetire(convID, shutdown)
	if deleteWorktree {
		plan := scheduleRetireWorktreeCleanup(convID, wt, shutdown)
		res.Worktree = &plan
		if plan.Detail != "" {
			res.Detail = joinDetail(res.Detail, plan.Detail)
		}
	}
	return res, ownerGroups
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
// the bulk groups.stop. Auth: agent.stop slug OR caller is owner of
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
	writeJSON(w, http.StatusOK, resp)
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
	stopRes := stopOneConv(targetConv, force)
	if stopRes.Action == "error" {
		writeError(w, http.StatusInternalServerError, "stop", stopRes.Detail)
		return
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
// tmux session. Sibling of the bulk groups.resume. Auth:
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
	res := resumeOneConvLocked(targetConv, recreate, trustRoot)
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
// overrides off a SpawnRequest and returns the canonical slug→effect map to
// apply at enrollment. Each slug must be registered and each effect
// must be "grant" or "deny"; a "default"/"" effect is a no-op and is dropped
// (the agent inherits the global default for that slug), so an editor that
// posts every slug — most at Default — collapses to just the real overrides.
// An unknown slug or an unrecognised effect returns a non-empty human-readable
// error string (the caller maps it to a 400); the map is nil for no overrides.
func normalizeSpawnPermissionOverrides(in map[string]string) (map[string]string, string) {
	if len(in) == 0 {
		return nil, ""
	}
	out := make(map[string]string, len(in))
	for slug, effect := range in {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		switch strings.TrimSpace(effect) {
		case "", "default":
			continue // no override — inherits the global default
		case db.PermEffectGrant, db.PermEffectDeny:
			if !IsKnownPermSlug(slug) {
				return nil, fmt.Sprintf("unknown permission slug %q. Known slugs: %s.",
					slug, strings.Join(knownSlugs(), ", "))
			}
			out[slug] = strings.TrimSpace(effect)
		default:
			return nil, fmt.Sprintf("permission override for %q must be \"grant\", \"deny\", or \"default\"; got %q",
				slug, effect)
		}
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// Permission: groups.spawn (default human-only — this lets an agent
// run arbitrary CC instances on the human's machine, blast radius
// matches `agent.spawn` in the design doc).
// marshalSpawnConfig serialises a spawn request to the verbatim JSON stored on
// agents.initial_spawn_config. It marshals the already-validated, already-decoded
// request, so an error is not expected; on the off chance one occurs it returns
// "" (the spawn proceeds without the audit snapshot rather than failing).
func marshalSpawnConfig(req agent.SpawnRequest) string {
	b, err := json.Marshal(req)
	if err != nil {
		slog.Warn("spawn: failed to marshal spawn config for audit", "error", err)
		return ""
	}
	return string(b)
}

func handleGroupSpawn(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	// requireGroupPermission also hands back the caller's conv-id: a real
	// agent (e.g. a PO orchestrating workers) resolves to its conv-id,
	// the human resolves to "". It is the default reply-to target for
	// the startup briefing assembled further down. Owners of g pass
	// without an explicit groups.spawn grant (owner-state default); the
	// spawn guardrails below still bind them (member cap, rate limit) and
	// already treat an owner as allowed for the group restriction.
	spawnerConvID, ok := requireGroupPermission(w, r, PermGroupsSpawn, g)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	// agent.SpawnRequest is the single shared request shape — the same
	// type `tclaude agent spawn`, `tclaude --join-group`, and the
	// dashboard's spawn modal marshal — so the wire contract can't drift
	// between the CLI and the dashboard. See its doc comment for the
	// per-field semantics.
	var body agent.SpawnRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
	}
	body.SandboxProfile = strings.TrimSpace(body.SandboxProfile)
	if body.SandboxProfile != "" && classify(peerFromContext(r.Context())) != classHuman {
		writeError(w, http.StatusForbidden, "sandbox_profile_restricted",
			"only the human operator may select an explicit sandbox profile; agents may inherit existing policy")
		return
	}

	// Spawn guardrails — runaway-prevention for an agent that the human
	// granted `groups.spawn`. The group's hard member cap (binds the human
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

	// Birth-time access controls: make the new agent a group owner
	// and/or seed its permanent per-slug permission overrides, the same grants
	// the Edit-agent modal applies to a live agent — but applied at enrollment
	// so the agent's first turn already has them. Validate here, at the
	// boundary, before any subprocess launches:
	//   - every override slug must be registered and every effect in
	//     {grant,deny} ("default"/"" carries no override and is dropped);
	//   - the privilege is gated so a spawn confers no MORE authority than the
	//     post-spawn path: a human (dashboard) caller always passes, and an
	//     agent caller must hold the SAME slug the dedicated endpoints require —
	//     groups.own to mint an owner (handleGroupOwnersAdd) and
	//     permissions.grant to set per-slug overrides (handlePermissionsGrant).
	//     Group ownership is deliberately NOT sufficient: owner-state confers
	//     only the owner-implied lifecycle slugs (groups.spawn/stop/…), NOT
	//     groups.own or permissions.grant — so keying on ownership would let an
	//     owner mint a child holding permissions.grant and escalate globally.
	//     resolvePermission (no owner bypass) is the same evaluation those
	//     endpoints run.
	permOverrides, povErr := normalizeSpawnPermissionOverrides(body.PermissionOverrides)
	if povErr != "" {
		writeError(w, http.StatusBadRequest, "invalid_permission_overrides", povErr)
		return
	}
	if spawnerConvID != "" {
		if body.IsOwner && resolvePermission(spawnerConvID, PermGroupsOwn) != permAllow {
			writeError(w, http.StatusForbidden, "forbidden",
				"making the spawned agent a group owner requires the "+PermGroupsOwn+" permission")
			return
		}
		if len(permOverrides) > 0 && resolvePermission(spawnerConvID, PermPermissionsGrant) != permAllow {
			writeError(w, http.StatusForbidden, "forbidden",
				"setting the spawned agent's permission overrides requires the "+PermPermissionsGrant+" permission")
			return
		}
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
		if fail := disabledProfileFailure(namedProfile); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}
	groupProfile := groupDefaultProfile(g)
	globalProfile := globalDefaultProfile()
	for _, prof := range []*db.SpawnProfile{groupProfile, globalProfile} {
		if fail := disabledProfileFailure(prof); fail != nil {
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
		{profile: groupProfile, source: profileSource(groupProfile, agent.ProvGroupProfileSource)},
		{profile: globalProfile, source: profileSource(globalProfile, agent.ProvGlobalProfileSource)},
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
		"model", body.Model, h.Name, profileTiers, func(p *db.SpawnProfile) string { return p.Model }, validateModel)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	body.Effort, effortSource, effortNote, fieldFail = resolveStringLaunchField(
		"effort", body.Effort, h.Name, profileTiers, func(p *db.SpawnProfile) string { return p.Effort }, h.Models.ValidateEffort)
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	var sandboxNote, approvalNote, askTimeoutNote string
	body.SandboxMode, _, sandboxNote, fieldFail = resolveStringLaunchField(
		"sandbox", body.SandboxMode, h.Name, profileTiers, func(p *db.SpawnProfile) string { return p.Sandbox },
		func(raw string) (string, error) { return harness.ValidateSandboxMode(h, raw) })
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
	body.AskUserQuestionTimeout, _, askTimeoutNote, fieldFail = resolveStringLaunchField(
		"ask_user_question_timeout", body.AskUserQuestionTimeout, h.Name, profileTiers,
		func(p *db.SpawnProfile) string { return p.AskUserQuestionTimeout },
		func(raw string) (string, error) { return harness.ResolveAskTimeoutMode(h, raw) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	var autoReviewSet, trustDirSet bool
	var autoReviewNote, trustDirNote string
	body.AutoReview, autoReviewSet, autoReviewNote, fieldFail = resolveBoolLaunchField(
		"auto_review", body.AutoReview, body.AutoReviewSpecified(), h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.AutoReview }, func(v bool) (bool, error) { return harness.ResolveAutoReview(h, v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	body.TrustDir, trustDirSet, trustDirNote, fieldFail = resolveBoolLaunchField(
		"trust_dir", body.TrustDir, body.TrustDirSpecified(), h.Name, profileTiers,
		func(p *db.SpawnProfile) *bool { return p.TrustDir }, func(v bool) (bool, error) { return harness.ResolveTrustDir(h, v) })
	if fieldFail != nil {
		writeError(w, fieldFail.Status, fieldFail.Kind, fieldFail.Msg)
		return
	}
	resolvedLaunch := &agent.ResolvedLaunch{
		Harness: agent.ResolvedField{Value: h.Name, Source: harnessSource},
		Model:   agent.ResolvedField{Value: body.Model, Source: modelSource, Note: modelNote},
		Effort:  agent.ResolvedField{Value: body.Effort, Source: effortSource, Note: effortNote},
	}
	for _, note := range []string{sandboxNote, approvalNote, askTimeoutNote, autoReviewNote, trustDirNote} {
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
	sandboxMode, sbErr := harness.ResolveSandboxMode(h, body.SandboxMode)
	if sbErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox", sbErr.Error())
		return
	}
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
		approvalPolicy = narrowDefaultApprovalToCaller(spawnerConvID, h.Name, approvalPolicy)
	}
	autoReview, arErr := harness.ResolveAutoReview(h, body.AutoReview)
	if arErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_auto_review", arErr.Error())
		return
	}
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(sandboxMode, cwd, home) {
		writeError(w, http.StatusBadRequest, "invalid_cwd", fmt.Sprintf(
			"refusing to spawn a %s agent in %q under sandbox %q: it would expose "+
				"~/.tclaude / ~/.codex / ~/.claude to the agent's writes; spawn in a "+
				"project subdirectory or set sandbox %q to opt out",
			h.Name, cwd, sandboxMode, harness.SandboxDangerFull))
		return
	}

	// Sandbox lineage — the child may not launch with a looser sandbox MODE
	// than the caller currently has (spawn_sandbox_guard.go). Checked HERE,
	// before claimSpawnRateSlot below, so a refused escalation costs the
	// caller no rate slot. executeSpawn re-runs the same (idempotent) check
	// for the other spawn callers (templates/waves/process adapters) that
	// don't pass through this HTTP boundary.
	if fail := spawnSandboxLineageFailure(spawnerConvID, h.Name, sandboxMode); fail != nil {
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
	// The dashboard mirrors this by hiding and clearing its explicit selector;
	// this server-side rule also covers CLI callers and older dashboard tabs.
	effectiveSandbox := sandboxpolicy.EmptySnapshot()
	var policyErr error
	if !sandboxProfilesDisabled(h.Name, sandboxMode) {
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
	if fail := sandboxProfileCapabilityFailure(h.Name, sandboxMode, &effectiveSandbox); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
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
	codexGitCommonDirPinned := spawnUsesPinnedGitCommonDir(h.Name, sandboxMode)
	var gitWorktreeWriteDirs []string
	if codexGitCommonDirPinned {
		var gerr error
		codexGitCommonDir, gerr = spawnGitCommonDir(h.Name, sandboxMode, cwd)
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
	if spawnerConvID != "" && childSandboxGrantsDirWrite(h.Name, sandboxMode) {
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

	// Gate the opt-in dir-trust request: it is Codex-only (pre-trusting the
	// cwd in ~/.codex/config.toml) and, unlike sandbox/approval, edits the
	// user's config — so requesting it for a harness with no trust modal
	// (Claude Code) is a 400 here rather than a flag silently dropped. Off by
	// default except for a verified tclaude-style sibling worktree, which must
	// be trusted before a detached Codex child starts. See JOH-205 inc4.
	trustDir, tdErr := harness.ResolveTrustDir(h, body.TrustDir)
	if tdErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_trust_dir", tdErr.Error())
		return
	}
	if autoTrustSiblingWorktree {
		trustDir = true
	}
	if spawnerConvID != "" && trustDir && !autoTrustSiblingWorktree {
		writeError(w, http.StatusForbidden, "trust_dir_restricted",
			"agent-initiated spawns may pre-trust only tclaude's verified default sibling worktrees; leave trust_dir off or ask the human to spawn this child")
		return
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
	var profileRemoteControl *bool
	for _, tier := range profileTiers[1:] { // named --profile remote_control remains CLI-inapplicable by contract
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
		Effort:                     effort,
		Model:                      model,
		Harness:                    h.Name,
		SandboxMode:                sandboxMode,
		AskUserQuestionTimeout:     askTimeout,
		ApprovalPolicy:             approvalPolicy,
		AutoReview:                 autoReview,
		AutoReviewSet:              autoReviewSet,
		TrustDir:                   trustDir,
		TrustDirSet:                trustDirSet,
		RemoteControl:              remoteControl,
		ReplyToConv:                replyToConv,
		SpawnedByConv:              spawnerConvID,
		IsOwner:                    body.IsOwner,
		PermissionOverrides:        permOverrides,
		Timeout:                    timeout,
		// Verbatim snapshot of the spawn request, recorded onto the new actor's
		// agents.initial_spawn_config in enrollSpawnedConv (a durable, agent-level
		// "what was this spawned with" record). Marshalling the already-decoded
		// body cannot fail in practice; on the off chance it does, leave it empty
		// rather than fail a spawn over an audit field.
		SpawnConfigJSON: marshalSpawnConfig(body),
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
	// out.
	if body.IncludeGroupContext == nil || *body.IncludeGroupContext {
		p.GroupContext = g.DefaultContext
	}

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
	for field, launched := range map[*agent.ResolvedField]string{
		&resolvedLaunch.Harness: outcome.Harness,
		&resolvedLaunch.Model:   outcome.Model,
		&resolvedLaunch.Effort:  outcome.Effort,
	} {
		if field.Value != launched {
			field.Value = launched
			field.Source = agent.ProvLaunchDefault
		}
	}
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
	// FocusMode is only ever non-empty when the caller asked for
	// auto-focus. "browser" means openTerminal couldn't pop a native
	// window — the dashboard's spawn modal points at focus_ws instead of
	// claiming success and opening nothing (see spawnOutcome.FocusMode).
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
	// SandboxMode is the resolved launch sandbox mode for a harness that
	// takes one (Codex: the managed "tclaude-agent" profile by default), or "" to omit the
	// flag (Claude Code, or no sandbox handling). Resolved + cwd-guarded at
	// the spawn boundary (handleGroupSpawn) before building the params; it
	// forwards to `tclaude session new --sandbox <mode>`.
	SandboxMode string
	// ApprovalPolicy is the resolved launch approval policy for a harness that
	// takes one (Codex: "never" by default — non-escalating so the unattended
	// pane can't deadlock), or "" to omit the flag (Claude Code, or no
	// approval handling). Resolved at the spawn boundary (handleGroupSpawn)
	// before building the params; it forwards to `tclaude session new
	// --ask-for-approval <policy>`. See JOH-200.
	ApprovalPolicy string
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
	// TrustDir opts the spawn into pre-trusting its launch cwd for Codex,
	// forwarding `--trust-dir` to `tclaude session new` so the daemon writes
	// the [projects."<cwd>"] trust entry into ~/.codex/config.toml before
	// launch and a detached pane doesn't freeze on the trust-folder modal
	// (JOH-205). false (the default) leaves the modal in place. Codex-only and
	// strictly opt-in (it edits the user's config) — gated at the spawn
	// boundary (handleGroupSpawn → harness.ResolveTrustDir) and never set on a
	// relaunch (reincarnate/clone), exactly like AutoReview.
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
	// to apply at birth: slug → "grant" | "deny". enrollSpawnedConv
	// writes each via db.SetAgentPermissionOverride after the membership add,
	// best-effort alongside IsOwner. Validated at the spawn boundary
	// (handleGroupSpawn) — every slug registered, every effect in {grant,deny}.
	// nil/empty = inherit the group's default permissions.
	PermissionOverrides map[string]string
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
	// SpawnConfigJSON is the verbatim JSON of the agent.SpawnRequest this spawn
	// came from, captured at the HTTP boundary (handleGroupSpawn). enrollSpawnedConv
	// records it onto the new actor's agents.initial_spawn_config so there is a
	// durable, agent-level "what was this spawned with" record. Empty on the
	// paths that have no SpawnRequest to snapshot (the pending-spawn sweeper,
	// the group-template instantiator), where the column simply stays "".
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
	// or "browser" (no native window could be popped — headless agentd,
	// or no terminal emulator installed — so the caller should fall back
	// to the in-browser terminal, same as handleDashboardOpenWindowAPI's
	// mode:"browser"). Set once, by the focusSpawn closure in executeSpawn.
	FocusMode string
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

// resolveStringLaunchField applies explicit > named > group > global for one
// launch field. Explicit values are direct intent and fail loudly. A profile
// value invalid for a foreign resolved harness is ambient configuration: skip
// it, disclose the skip, and continue to the next tier. A profile claiming the
// resolved harness but carrying an invalid value is self-inconsistent and
// remains a loud error.
func resolveStringLaunchField(
	field, explicitValue, harnessName string,
	tiers []launchProfileTier,
	profileValue func(*db.SpawnProfile) string,
	validate func(string) (string, error),
) (value, source, note string, fail *spawnFailure) {
	if raw := strings.TrimSpace(explicitValue); raw != "" {
		value, err := validate(raw)
		if err != nil {
			return "", "", "", &spawnFailure{http.StatusBadRequest, "invalid_" + field, err.Error()}
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
		value, err := validate(raw)
		if err == nil {
			return value, tier.source, strings.Join(notes, "; "), nil
		}
		if profileMatchesHarness(tier.profile, harnessName) {
			return "", "", "", &spawnFailure{http.StatusBadRequest, "invalid_" + field,
				fmt.Sprintf("profile %q: %v", tier.profile.Name, err)}
		}
		notes = append(notes, fmt.Sprintf("%s %s ignored (not valid for %s)", tier.source, field, harnessName))
	}
	return "", agent.ProvHarnessDefault, strings.Join(notes, "; "), nil
}

func resolveBoolLaunchField(
	field string, explicitValue, explicitSet bool, harnessName string,
	tiers []launchProfileTier,
	profileValue func(*db.SpawnProfile) *bool,
	validate func(bool) (bool, error),
) (bool, bool, string, *spawnFailure) {
	if explicitSet {
		value, err := validate(explicitValue)
		if err != nil {
			return false, false, "", &spawnFailure{http.StatusBadRequest, "invalid_" + field, err.Error()}
		}
		return value, true, "", nil
	}
	var note string
	for _, tier := range tiers {
		if tier.profile == nil || profileValue(tier.profile) == nil {
			continue
		}
		if !profileMatchesHarness(tier.profile, harnessName) {
			if note == "" {
				note = fmt.Sprintf("%s %s ignored (not valid for %s)", tier.source, field, harnessName)
			} else {
				note += "; " + fmt.Sprintf("%s %s ignored (not valid for %s)", tier.source, field, harnessName)
			}
			continue
		}
		value, err := validate(*profileValue(tier.profile))
		if err == nil {
			return value, true, note, nil
		}
		return false, false, "", &spawnFailure{http.StatusBadRequest, "invalid_" + field,
			fmt.Sprintf("profile %q: %v", tier.profile.Name, err)}
	}
	return false, false, note, nil
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
	profiles := []*db.SpawnProfile{groupDefaultProfile(g), globalDefaultProfile()}
	tiers := make([]launchProfileTier, 0, len(profiles))
	for _, prof := range profiles {
		if prof != nil {
			if fail := disabledProfileFailure(prof); fail != nil {
				return fail
			}
			tiers = append(tiers, launchProfileTier{profile: prof})
			if strings.TrimSpace(p.Harness) == "" {
				p.Harness = harnessOrDefault(prof.Harness)
			}
		}
	}

	// Apply the chosen harness's SECURE launch defaults to any field still
	// blank, and validate — the same resolution handleGroupSpawn runs before
	// building its params. Idempotent on the handleGroupSpawn path (already
	// resolved); the load-bearing case is any other caller that reaches
	// executeSpawn, where this keeps a Codex spawn sandboxed and gives lineage
	// authorization concrete defaults even when no profile participates.
	h, err := resolveSpawnHarness(p.Harness)
	if err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_harness", err.Error()}
	}
	var fail *spawnFailure
	p.Model, _, _, fail = resolveStringLaunchField("model", p.Model, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Model }, h.Models.ValidateModel)
	if fail != nil {
		return fail
	}
	p.Effort, _, _, fail = resolveStringLaunchField("effort", p.Effort, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Effort }, h.Models.ValidateEffort)
	if fail != nil {
		return fail
	}
	p.SandboxMode, _, _, fail = resolveStringLaunchField("sandbox", p.SandboxMode, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Sandbox },
		func(raw string) (string, error) { return harness.ValidateSandboxMode(h, raw) })
	if fail != nil {
		return fail
	}
	p.ApprovalPolicy, _, _, fail = resolveStringLaunchField("approval", p.ApprovalPolicy, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.Approval },
		func(raw string) (string, error) { return harness.ValidateApprovalPolicy(h, raw) })
	if fail != nil {
		return fail
	}
	p.AskUserQuestionTimeout, _, _, fail = resolveStringLaunchField("ask_user_question_timeout", p.AskUserQuestionTimeout, h.Name, tiers,
		func(prof *db.SpawnProfile) string { return prof.AskUserQuestionTimeout },
		func(raw string) (string, error) { return harness.ResolveAskTimeoutMode(h, raw) })
	if fail != nil {
		return fail
	}
	p.AutoReview, p.AutoReviewSet, _, fail = resolveBoolLaunchField("auto_review", p.AutoReview,
		p.AutoReviewSet || p.AutoReview, h.Name, tiers, func(prof *db.SpawnProfile) *bool { return prof.AutoReview },
		func(v bool) (bool, error) { return harness.ResolveAutoReview(h, v) })
	if fail != nil {
		return fail
	}
	p.TrustDir, p.TrustDirSet, _, fail = resolveBoolLaunchField("trust_dir", p.TrustDir,
		p.TrustDirSet || p.TrustDir, h.Name, tiers, func(prof *db.SpawnProfile) *bool { return prof.TrustDir },
		func(v bool) (bool, error) { return harness.ResolveTrustDir(h, v) })
	if fail != nil {
		return fail
	}
	if p.SandboxMode, err = harness.ResolveSandboxMode(h, p.SandboxMode); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_sandbox", err.Error()}
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
	if p.AskUserQuestionTimeout, err = harness.ResolveAskTimeoutMode(h, p.AskUserQuestionTimeout); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_ask_user_question_timeout", err.Error()}
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
func executeSpawn(g *db.AgentGroup, p spawnParams) (*spawnOutcome, *spawnFailure) {
	groupName := spawnGroupName(g)
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if p.CleanupDirWriteProof {
		defer cleanupDirWriteProofMarkers(p.DirWriteProofToken, p.DirWriteProofDirs)
	}

	// Fill blank launch fields from group then global default spawn profiles
	// and apply the harness's secure launch defaults. On the handleGroupSpawn
	// path this is an idempotent no-op (the request overlay already resolved
	// these); it is the safety net for any other caller that reaches
	// executeSpawn with a profile-carrying group, keeping a Codex spawn
	// sandboxed. A value invalid for the harness is a typed failure.
	if fail := applyDefaultProfile(g, &p); fail != nil {
		return nil, fail
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
	if sandboxProfilesDisabled(p.Harness, p.SandboxMode) {
		empty := sandboxpolicy.EmptySnapshot()
		p.EffectiveSandbox = &empty
	}
	if spawnUsesPinnedGitCommonDir(p.Harness, p.SandboxMode) && !p.CodexGitCommonDirPinned {
		gitCommonDir, err := spawnGitCommonDir(p.Harness, p.SandboxMode, p.Cwd)
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
	if fail := sandboxProfileCapabilityFailure(p.Harness, p.SandboxMode, p.EffectiveSandbox); fail != nil {
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
	if p.SpawnedByConv != "" && p.TrustDir && !autoTrustSiblingWorktree {
		return nil, &spawnFailure{http.StatusForbidden, "trust_dir_restricted",
			"agent-initiated spawns may pre-trust only tclaude's verified default sibling worktrees; leave trust_dir off or ask the human to spawn this child"}
	}
	if fail := spawnSandboxLineageFailure(p.SpawnedByConv, p.Harness, p.SandboxMode); fail != nil {
		return nil, fail
	}
	if fail := spawnApprovalLineageFailure(p.SpawnedByConv, p.Harness, p.ApprovalPolicy, p.AutoReview); fail != nil {
		return nil, fail
	}
	if strings.TrimSpace(p.DirWriteProofToken) == "" {
		p.CodexGitCommonDir = ""
		p.CodexGitCommonDirPinned = false
	}

	// Generate a label that's unlikely to collide with existing
	// session IDs: crypto-random hex (like GenerateSessionID()), with
	// a "spwn-" prefix so these rows are easy to spot in
	// `tclaude session ls`.
	label := generateSpawnLabel()

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
	if p.EffectiveSandbox != nil && len(p.EffectiveSandbox.Effective.AgentDirectories) > 0 {
		materialized, cleanup, err := materializeAgentDirectories(*p.EffectiveSandbox, label)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "spawn", err.Error()}
		}
		p.EffectiveSandbox = &materialized
		agentDirectoryCleanup = cleanup
	}

	spawnArgs := clcommon.SpawnArgs{
		EffectiveSandbox:           p.EffectiveSandbox,
		Label:                      label,
		Cwd:                        p.Cwd,
		CwdWriteProof:              p.CwdWriteProofToken,
		CodexGitCommonDir:          p.CodexGitCommonDir,
		CodexGitCommonDirPinned:    p.CodexGitCommonDirPinned,
		GitWorktreeWriteDirs:       p.GitWorktreeWriteDirs,
		GitWorktreeWriteDirsPinned: p.GitWorktreeWriteDirsPinned,
		Effort:                     p.Effort,
		Model:                      p.Model,
		Harness:                    p.Harness,
		Sandbox:                    p.SandboxMode,
		AskUserQuestionTimeout:     p.AskUserQuestionTimeout,
		Approval:                   p.ApprovalPolicy,
		AutoReview:                 p.AutoReview,
		TrustDir:                   p.TrustDir,
		RemoteControl:              p.RemoteControl,
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
	spawnHarness, _ := harness.Resolve(p.Harness)
	launchEnroll := spawnHarness.SupportsLaunchEnrollment() && !spawnUsesLegacyInjection()
	var preConvID string
	var preMsgID int64
	// briefingInlined records whether the launch-enrollment prompt baked the
	// whole briefing inline (short enough to fit) rather than pointing at the
	// inbox copy. When it did, the inbox copy is inserted already delivered and
	// read — the agent has the text, so it must never enter the nudge queue.
	var briefingInlined bool
	if launchEnroll {
		preConvID = convops.GenerateUUID()
		// Decide the briefing's launch state before inserting its inbox copy.
		// An inlined copy must be born delivered + read in the same INSERT;
		// inserting it unread and fixing it up after launch leaves a window where
		// the online-message flush can claim and inject a redundant nudge.
		spawnContextBody := buildSpawnContextBody(groupName, p.GroupContext, p.InitialMessage, p.Attachments)
		inlineCap := spawnInlineMaxChars()
		briefingInlined = spawnContextBody != "" && spawnBriefingFitsLaunch(spawnContextBody, inlineCap)
		mid, fail := enrollSpawnedConv(g, p, preConvID, briefingInlined)
		if fail != nil {
			return nil, fail
		}
		preMsgID = mid
		spawnArgs.SessionID = preConvID
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
		spawnContextBody := buildSpawnContextBody(groupName, p.GroupContext, p.InitialMessage, p.Attachments)
		spawnArgs.InitialPrompt = buildSpawnSeedPrompt(p.Name, p.Role, p.Descr, groupName,
			p.InitialMessage != "", spawnContextBody, p.WorktreePath, p.WorktreeBranch,
			resolveSpawnerTitle(p.SpawnedByConv, p.SpawnedByAgent), spawnInlineMaxChars())
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
			rollbackSpawnEnrollment(g, preConvID, preMsgID)
		}
		return nil, fail
	}

	// Async harnesses without launch enrollment may return before their conv-id
	// materialises. Reserve and persist the stable actor identity BEFORE the
	// process starts, so an immediate hook/reaper enrollment can only bind this
	// exact id. The row is atomically replaced by the actor binding once the conv
	// appears; a genuinely pending response simply leaves it for back-fill.
	reservedPending := p.Async && !launchEnroll
	if reservedPending {
		if g == nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "spawn", "ungrouped asynchronous spawn is not supported"}
		}
		p.AgentID = db.NewAgentID()
		if err := db.InsertPendingSpawn(pendingSpawnFromParams(g, p, label)); err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to reserve pending spawn " + label + ": " + err.Error()}
		}
	}

	if err := SpawnDetachedTclaudeNew(spawnArgs); err != nil {
		if reservedPending {
			if deleteErr := db.DeletePendingSpawn(label); deleteErr != nil {
				slog.Warn("spawn: failed to remove reservation after launch failure",
					"label", label, "error", deleteErr)
			}
		}
		if launchEnroll {
			// The enrollment ran before the fork; roll it back so a failed
			// launch doesn't strand a group member + orphan briefing.
			rollbackSpawnEnrollment(g, preConvID, preMsgID)
		}
		return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: " + err.Error()}
	}
	agentDirectoriesLaunched = true

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
	// focusMode records what focusSpawn actually did, for the three
	// spawnOutcome literals below to report back to the caller — see
	// spawnOutcome.FocusMode. Left "" when AutoFocus is off or the pane
	// never came up within the poll, so focusSpawn never ran.
	focusMode := ""
	focusSpawn := func() {
		if !p.AutoFocus || focused {
			return
		}
		focused = true
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
	launchedAt := time.Now()
	pollBudget := timeout
	if p.Async && asyncSpawnInlineGrace < pollBudget {
		pollBudget = asyncSpawnInlineGrace
	}
	backgroundBackfillBudget := pollBudget
	if p.Async && spawnHarness.NeedsSpawnSeed() && codexAsyncSpawnResponseGrace > 0 && codexAsyncSpawnResponseGrace < pollBudget {
		pollBudget = codexAsyncSpawnResponseGrace
	}
	deadline := launchedAt.Add(pollBudget)
	var convID, tmuxSession string
	var lastDiscoveryScan time.Time
	remoteArmed := false
	pendingLaunchMarked := false
	for time.Now().Before(deadline) {
		s, err := db.LoadSession(label)
		if err == nil && s != nil {
			if reservedPending && !pendingLaunchMarked {
				if err := db.MarkPendingSpawnLaunched(label); err != nil {
					slog.Warn("spawn: failed to clear pending launch marker", "label", label, "error", err)
				} else {
					pendingLaunchMarked = true
				}
			}
			tmuxSession = s.TmuxSession
			if tmuxSession != "" {
				focusSpawn() // pane is up — open it now, conv-id or not
			}
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

	// Launch-enrollment path: the conv-id was PRESET, the enrollment
	// (membership + inbox briefing) ran before the fork, and the rename +
	// welcome are baked into the launch command — so the spawn has already
	// succeeded as far as the daemon is concerned, whether or not the poll
	// confirmed the session row in time. Return the preset id (NOT the polled
	// convID, which the loop may have left empty on a slow fork). The poll
	// above fired focus when the pane came up; fire once more in case the row
	// landed late. We deliberately do NOT roll back on a slow/missing row: the
	// pane is most likely just coming up, and rolling back would strand a live,
	// named, greeted, group-less pane whose welcome points at a deleted inbox
	// message. A genuinely failed launch (Start error) was already caught and
	// rolled back above; a pane that dies at startup leaves an offline member
	// the operator can retire, exactly like any agent that crashes on boot.
	if launchEnroll {
		focusSpawn()
		markBriefingConsumed(preConvID, preMsgID, briefingInlined)
		return &spawnOutcome{ConvID: preConvID, Label: label, TmuxSession: label, FocusMode: focusMode,
			Harness: p.Harness, Model: p.Model, Effort: p.Effort}, nil
	}

	// Conv-id resolved within the poll: finish enrollment inline (Codex, or CC
	// with the legacy-injection revert flag) and inject the rename + welcome.
	if convID != "" {
		if reservedPending {
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
			if reservedPending {
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
	return &db.PendingSpawn{
		Label:               label,
		AgentID:             p.AgentID,
		Launching:           true,
		GroupID:             g.ID,
		Role:                p.Role,
		Descr:               p.Descr,
		Name:                p.Name,
		InitialMessage:      p.InitialMessage,
		GroupContext:        p.GroupContext,
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
		// Same repair the sweeper's already-enrolled path performs: a prior
		// attempt may have committed the membership but lost the task-ref
		// write, and the claimed row is the only durable copy of the intent —
		// requeue it on a failed repair so the sweeper retries.
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
	contextBody := buildSpawnContextBody(groupName, p.GroupContext, p.InitialMessage, p.Attachments)
	welcomeInSeed := h.NeedsSpawnSeed() && spawnBriefingFitsLaunch(contextBody, spawnInlineMaxChars())
	briefingInlined := contextBody != "" && welcomeInSeed
	spawnContextMsgID, fail := enrollSpawnedConv(g, p, convID, briefingInlined)
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
func enrollSpawnedConv(g *db.AgentGroup, p spawnParams, convID string, briefingInlined bool) (int64, *spawnFailure) {
	// Stable agent-identity (JOH-26): a spawn is the birth of a new actor. Mint
	// its agent_id BEFORE the group-add so created_via is the precise "spawn"
	// rather than the "group" tag AddAgentGroupMember's own EnsureAgentForConv
	// would otherwise stamp (that call is a no-op once this conv is already
	// linked). Idempotent.
	var agentID string
	var err error
	if p.AgentID != "" {
		agentID, _, err = db.EnsureAgentForConvWithID(convID, p.AgentID, "spawn")
	} else {
		agentID, _, err = db.EnsureAgentForConv(convID, "spawn")
	}
	if err != nil {
		// A reserved id may already have been returned to the caller. Never
		// continue by minting/substituting another actor through group-add.
		if p.AgentID != "" {
			return 0, &spawnFailure{http.StatusInternalServerError, "identity",
				"failed to bind reserved agent " + p.AgentID + " to spawned conv " + convID + ": " + err.Error()}
		}
		slog.Warn("spawn: failed to ensure agent identity", "conv", convID, "error", err)
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
			return 0, &spawnFailure{http.StatusInternalServerError, "io",
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
			return 0, &spawnFailure{http.StatusInternalServerError, "io",
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
			return 0, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to record effective sandbox snapshot: " + err.Error()}
		}
	}
	if agentID != "" && p.ProcessCommandID != "" {
		if err := db.SetAgentProcessCommand(agentID, p.ProcessCommandID); err != nil {
			return 0, &spawnFailure{http.StatusConflict, "process_command", "failed to bind spawned agent to process command: " + err.Error()}
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
			return 0, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to record task-reference link: " + err.Error()}
		}
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
	// the dedicated endpoints do — groups.own / permissions.grant — for an agent
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
	for slug, effect := range p.PermissionOverrides {
		if err := db.SetAgentPermissionOverride(convID, slug, effect, permissionGranter); err != nil {
			slog.Warn("spawn: failed to apply birth permission override",
				"conv", convID, "slug", slug, "effect", effect, "error", err)
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
	spawnContext := buildSpawnContextBody(groupName, p.GroupContext, p.InitialMessage, p.Attachments)
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
	return spawnContextMsgID, nil
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
// the dashboard — so they log rather than bubble. The pending-name row is keyed
// by a conv-id that now never materialises, so it is never read again and is
// left in place.
//
// It also undoes the birth-time access controls enrollSpawnedConv may have
// written (the group-owner row + per-slug overrides): both are applied before
// the fork on the launch-enrollment path, so a failed launch would otherwise
// strand a ghost owner of the group (which could mask an ownerless-group
// warning) and dangling override rows for a conv that never exists. Both calls
// are no-ops when nothing was written, so this is unconditional — rollback has
// no spawnParams to consult.
func rollbackSpawnEnrollment(g *db.AgentGroup, convID string, msgID int64) {
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
// waits for the new tmux pane to come online, then injects, in order:
//
//  1. /rename <name> — when name is a valid rename title. This is the
//     agent's single name; it becomes the conversation title.
//  2. The welcome [system: ...] line orienting the agent.
//
// Each is its own turn. Failures are logged, never bubbled — the spawn
// already succeeded as far as the caller is concerned.
//
// The agent's startup briefing (group context + task brief) is NOT
// typed into the pane — the handler already placed it in the agent's
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

	sess := pickAliveSession(convID)
	if sess == nil {
		slog.Warn("spawn: no alive tmux session for post-init injection", "conv", convID)
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
	h := harnessForConv(convID)
	renameWanted := name != "" && isValidRenameTitle(name)
	if name != "" && !renameWanted {
		slog.Warn("spawn: name not a valid rename title; skipping rename",
			"conv", convID, "name", name)
	}
	if renameWanted && h.SupportsRename() {
		if !deliverRename(convID, name) {
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
		if err := injectTextAndSubmit(target, welcome); err != nil {
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
		goBackground(func() { persistSpawnTitle(convID, name) })
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
// freshly-spawned agent's inbox. It stitches together up to two
// sections — the group's shared startup context and the per-spawn
// task brief — under plain-text headers, with a divider when both are
// present.
//
// Either input may be empty (or whitespace-only); when both are, the
// result is "" and the caller skips the inbox insert entirely, so an
// agent with nothing to brief never gets an empty message.
func buildSpawnContextBody(groupName, groupContext, initialMessage string, attachments []string) string {
	groupContext = strings.TrimSpace(groupContext)
	initialMessage = strings.TrimSpace(initialMessage)

	var sections []string
	if groupContext != "" {
		sections = append(sections, fmt.Sprintf(
			"Group %q startup context — shared guidance for every agent spawned into this group:\n\n%s",
			groupName, groupContext))
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
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "spwn-" + hex.EncodeToString(b[:])
}

// SpawnDetachedTclaudeNew is a thin facade over Spawn.SpawnNew.
// Tests substitute a behavior-accurate fake by assigning Spawn at
// setup; production keeps the LiveSpawner default. See clcommon.SpawnArgs
// for the per-field semantics.
func SpawnDetachedTclaudeNew(args clcommon.SpawnArgs) error {
	return Spawn.SpawnNew(args)
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
	return Spawn.SpawnResume(args)
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
	args = appendAskTimeoutFlag(args, a.AskUserQuestionTimeout)
	args = appendApprovalFlag(args, a.Approval)
	args = appendAutoReviewFlag(args, a.AutoReview)
	args = appendTrustDirFlag(args, a.TrustDir)
	args = appendRemoteControlFlag(args, a.RemoteControl)
	args = appendInitialPromptArg(args, a)
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
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	args = appendHarnessFlag(args, a.Harness)
	args = appendSandboxArgs(args, a.Harness, a.Sandbox)
	args = appendAskTimeoutFlag(args, a.AskUserQuestionTimeout)
	args = appendApprovalFlag(args, a.Approval)
	args = appendAutoReviewFlag(args, a.AutoReview)
	// Re-arm Claude Code's built-in Remote Access on the relaunched pane when
	// the SOURCE conv was armed (JOH-261). claudeSpawner.BuildCommand emits
	// `--remote-control` LAST on the resume (--resume) path too, so its optional
	// [name] stays empty and the flag is unambiguous. Omitted when false; a
	// non-CC harness never sets it (remoteControlForRelaunch gates on the
	// harness capability), so the forked `session new -r` never rejects it.
	args = appendRemoteControlFlag(args, a.RemoteControl)
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
// the spawn boundary (harness.ResolveSandboxMode), never user free-text, so
// it is safe as a bare arg. The forked `tclaude session new` re-validates.
func appendSandboxFlag(args []string, mode string) []string {
	if mode != "" {
		args = append(args, "--sandbox", mode)
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
// the spawn opted into pre-trusting its launch dir for Codex. false (the
// default) omits it, so an ordinary spawn leaves Codex's trust-folder modal in
// place. It is a boolean flag — no value — gated at the spawn boundary
// (harness.ResolveTrustDir rejects it for a non-Codex harness), and the forked
// `tclaude session new` re-validates and performs the actual ~/.codex/config.toml
// write. Opt-in only because it edits the user's config (JOH-205 inc4).
func appendTrustDirFlag(args []string, trustDir bool) []string {
	if trustDir {
		args = append(args, "--trust-dir")
	}
	return args
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
	cmd.Env = spawnEnvWithoutOperatorToken()
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		cleanup()
		return err
	}
	pid := cmd.Process.Pid
	if a.CwdWriteProof != "" || a.DirWriteProof != "" {
		defer cleanup()
		if err := cmd.Wait(); err != nil {
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
			slog.Error("spawn subprocess exited with error",
				"label", label, "pid", pid, "err", err,
				"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
		}
	}()
	return nil
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
	var cleanup func()
	var err error
	a, cleanup, err = spawnArgsWithSandboxHandoff(a)
	if err != nil {
		return err
	}
	convID := a.ConvID
	args := sessionResumeArgs(a)
	cmd := exec.Command("tclaude", args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	// Spawned agents must not inherit the human's operator token.
	cmd.Env = spawnEnvWithoutOperatorToken()
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		cleanup()
		return err
	}
	pid := cmd.Process.Pid
	defer cleanup()
	if err := cmd.Wait(); err != nil {
		slog.Error("resume subprocess exited with error",
			"conv", convID, "pid", pid, "err", err,
			"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
		return fmt.Errorf("resume session wrapper failed: %w: %s", err, stderr.String())
	}
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
