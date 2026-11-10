package agentd

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

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
		if member.ConvID != "" && !online && !db.IsExternalStewardConv(member.ConvID) {
			affected = append(affected, member.ConvID)
			selected[member.ConvID] = true
		}
	}
	_, ok := requireGroupPermission(w, r, PermGroupsMembersResume, g,
		ActionContext{affectedConvs: affected})
	if !ok {
		return
	}
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
				action := "skipped:already_online"
				if db.IsExternalStewardConv(m.ConvID) {
					action = "skipped:external_steward"
				}
				return memberOpResult{ConvID: m.ConvID, AgentID: peerAgentID(m.ConvID),
					Title: agent.FreshTitle(m.ConvID), Action: action}, true
			}
			res := resumeOneConvLocked(m.ConvID, false)
			confirmResumedConvOnline(m.ConvID, &res)
			res.AgentID = peerAgentID(m.ConvID)
			res.Title = agent.FreshTitle(m.ConvID)
			return res, true
		})
	if out.Members == nil {
		out.Members = []memberOpResult{}
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
	return resumeOneConvLocked(convID, recreateMissingDir)
}

var resumeLaunchLocks sync.Map         // map[stable actor or unowned conv]*sync.Mutex
var recoveryLaunchCommitLocks sync.Map // map[convID]*sync.Mutex

// relaunchCwd is the directory a relaunch starts in: the physical launch
// spelling captured at admission when it is available, else the durable
// profile cwd. This is an address selection, not an authorization check: the
// continuation neither re-captures nor compares provenance, and malformed or
// legacy metadata simply falls back. "" when neither is absolute.
func relaunchCwd(cfg *durableRelaunchConfig) string {
	cwd := cfg.Cwd
	if recorded, err := resumeprovenance.Decode(cfg.ResumeProvenance); err == nil &&
		recorded.Cwd.Path != "" && filepath.IsAbs(recorded.Cwd.Path) {
		cwd = recorded.Cwd.Path
	}
	if !filepath.IsAbs(cwd) {
		return ""
	}
	return cwd
}

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

func resumeOneConvLocked(convID string, recreateMissingDir bool) memberOpResult {
	// Flag the whole attempt — including time queued on the launch lock — so
	// dashboards render the multi-second wake as "waking" rather than a dead
	// offline dot. See waking.go.
	defer markConvWaking(convID)()
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	return resumeOneConvClaimedUnderLaunchLock(convID, recreateMissingDir)
}

func resumeOneConvClaimedUnderLaunchLock(convID string, recreateMissingDir bool) memberOpResult {
	if isConvOnline(convID) {
		return resumeOneConvUnderLaunchLock(convID, recreateMissingDir, nil)
	}
	now := time.Now()
	manualClaim, err := db.BeginManualAgentRecovery(convID, now)
	if err != nil {
		slog.Warn("resume: claim pending automatic recovery failed", "conv", short8(convID), "error", err)
	} else if manualClaim != nil {
		_ = db.RecordAgentRecoveryAudit(*manualClaim, db.AuditVerbAgentRecoveryManual, "manual_resume", now)
	}
	res := resumeOneConvUnderLaunchLock(convID, recreateMissingDir, manualClaim)
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

func resumeOneConvWithCodexRollbackLocked(convID string, recreateMissingDir bool) memberOpResult {
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
		return resumeOneConvClaimedUnderLaunchLock(convID, recreateMissingDir)
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
	return resumeOneConvClaimedUnderLaunchLock(convID, recreateMissingDir)
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
func resumeOneConvUnderLaunchLock(convID string, recreateMissingDir bool, recoveryClaim *db.AgentRecovery) memberOpResult {
	return resumeOneConvUnderLaunchLockForTeleport(convID, recreateMissingDir, recoveryClaim, false)
}
func resumeOneConvUnderLaunchLockForTeleport(convID string, recreateMissingDir bool, recoveryClaim *db.AgentRecovery, teleportRecovery bool) memberOpResult {
	if !teleportRecovery {
		if err := teleportResumeBlocked(convID); err != nil {
			return memberOpResult{ConvID: convID, Action: "error", Detail: err.Error()}
		}
	}

	defer db.NotifyStatusChanged()
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
	// Resume authority is durable agent + conversation state. A missing legacy
	// conversation profile may be reconstructed from the real harness store; a
	// predecessor session row is never required.
	conversationProfile, profileErr := db.ConversationResumeProfileForConv(convID)
	if profileErr != nil {
		res.Action = "error"
		res.Detail = "load durable conversation resume profile: " + profileErr.Error()
		return res
	}
	if conversationProfile == nil {
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
	cwd := relaunchCwd(launchConfig)
	if cwd == "" {
		res.Action = "error:resume_profile"
		res.Detail = "no absolute launch directory is available; recreate this agent"
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
	// Re-arm Remote Access if the conv's own persisted best-known state was on
	// (JOH-261). Read BEFORE relaunch: resume keeps the conv-id but mints a NEW
	// session row defaulting remote_control=0, so the freshest row reads OFF the
	// moment the new pane reports in — the armed flag lives on the old/dead row,
	// which is still the most-recent until then.
	// The relaunch policy's group_remote_control row may apply the group's
	// current policy instead (relaunchRemoteControl).
	remoteControl, remoteControlChange, remoteControlNote := relaunchRemoteControl(
		convID, launchConfig.Harness, launchConfig.RemoteControl)
	if remoteControlNote != "" {
		res.Warnings = append(res.Warnings, remoteControlNote)
	}
	if note := relaunchOverridesNote(convID); note != "" {
		res.Warnings = append(res.Warnings, note)
	}
	sshLaunchKey := generateSpawnLabel()
	// Preserve the mode this conversation was launched under; the harness
	// default would silently drop an enforced `sandbox on` posture on resume.
	// Auto-review rides the durable relaunch profile with the approval it
	// belongs to (relaunchProfileForSpawn records it; reconstructApproval
	// re-validates it against the harness), so a relaunch keeps the guardian
	// the agent was launched with.
	relaunchSandbox := launchConfig.Sandbox
	harnessName := launchConfig.Harness
	relaunchSandboxImplementation := launchConfig.activeSandboxImplementation()
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
	prepared, sandboxFail := prepareRelaunchSandbox(relaunchSandboxRequestFor(convID, cwd, sshLaunchKey, launchConfig))
	if sandboxFail != nil {
		res.Action = "error"
		res.Detail = "sandbox_profile_changed: " + sandboxFail.Msg
		if sandboxFail.Kind == "spawn" {
			res.Detail = sandboxFail.Msg
		}
		return res
	}
	resumePolicy, effectiveSandbox := prepared.policy, prepared.effective
	res.Warnings = append(res.Warnings, prepared.warnings...)
	// This is a continuation of authority admitted at the initial spawn. The
	// session launcher derives any repository grants from the current cwd and
	// applies the current operator-authored sandbox profile; resume does not
	// manufacture write-proof markers in inherited directories.
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
	managedAgentID, managedErr := db.AgentIDForConv(convID)
	if managedErr != nil {
		res.Action = "error"
		res.Detail = "managed instructions: " + managedErr.Error()
		return res
	}
	managedParams := spawnParams{AgentID: managedAgentID, Harness: harnessName}
	cleanupManaged, managedFail := prepareManagedInstructions(nil, &managedParams)
	if managedFail != nil {
		res.Action = "error"
		res.Detail = managedFail.Msg
		return res
	}
	defer func() { cleanupManaged(!strings.HasPrefix(res.Action, "error")) }()
	if err := SpawnDetachedTclaudeResume(clcommon.SpawnArgs{
		ManagedInstructionsRoot: managedParams.ManagedInstructionsRoot,
		InitialPrompt:           managedInstructionResumePrompt(managedParams),
		EffectiveSandbox:        effectiveSandbox,
		AgentID:                 persistedAgentID,
		ConvID:                  convID,
		Cwd:                     cwd,
		Effort:                  launchConfig.Effort,
		Model:                   launchConfig.Model,
		ModelProxy:              launchConfig.ModelProxy,
		Harness:                 harnessName,
		Sandbox:                 relaunchSandbox,
		SandboxImplementation:   relaunchSandboxImplementation,
		SandboxChosenBy:         launchConfig.HarnessBuiltinModeSource,
		Approval:                approval,
		AutoReview:              autoReview,
		AskUserQuestionTimeout:  launchConfig.AskUserQuestionTimeout,
		ToolGovernance:          launchConfig.ToolGovernance,
		RemoteControl:           remoteControl,
		AutoMemory:              launchConfig.AutoMemory,
		PeerMessaging:           launchConfig.PeerMessaging,
		ContextFeatures:         launchConfig.ContextFeatures,
		AutoCompactWindow:       launchConfig.AutoCompactWindow,
		ContextWindowMax:        launchConfig.ContextWindowMax,
		CopilotAPI:              launchConfig.CopilotAPI,
		CodexAppServer:          launchConfig.CodexAppServer,
		CodexStateRoot:          launchConfig.CodexStateRoot,
		FastMode:                launchConfig.FastMode,
	}); err != nil {
		res.Action = "error"
		if isClaudeSubscriptionLost(err) {
			res.Action = "error:" + claudeSubscriptionLostCode
		}
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
		// The echo describes what this launch changed, so only a launch that
		// happened and was recorded reports it: the temporary built-in mode
		// never persists its policy, and would repeat the same lines.
		if !launchConfig.TemporaryHarnessBuiltinMode {
			res.Changes = prepared.changes
		}
		if remoteControlChange != "" {
			res.Changes = append(res.Changes, remoteControlChange)
		}
		if managedParams.ManagedInstructionsBody != "" {
			if _, err := db.InsertAgentMessage(&db.AgentMessage{ToConv: convID, Subject: "Managed instructions (launch)", Body: managedParams.ManagedInstructionsBody}); err != nil {
				res.Warnings = append(res.Warnings, "Managed instructions inbox copy could not be saved: "+err.Error())
			}
		}

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
// real harness store. The harness conversation must still exist; a stale
// conv_index row alone is not enough. Once resolved, recovery persists the
// canonical cwd on the conversation before returning to the normal path.
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
	physicalCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve recovered launch directory: %w", err)
	}
	profile, err := recordBaselineResumeProfile(convID, harnessName, physicalCwd)
	if err != nil {
		return nil, err
	}
	slog.Info("resume: recovered missing conversation profile from harness store",
		"conv", short8(convID), "harness", harnessName, "cwd", physicalCwd)
	return profile, nil
}

// recordBaselineResumeProfile persists the launch baseline of a conversation
// with no recorded posture, on the conversation and on its agent if it has one.
func recordBaselineResumeProfile(convID, harnessName, physicalCwd string) (*db.ConversationResumeProfile, error) {
	legacy := baselineRelaunchProfile(harnessName)
	profile := db.ConversationResumeProfile{
		Version: db.RelaunchProfileVersion, Harness: harnessName, Cwd: physicalCwd,
		FallbackRelaunch: &legacy,
	}
	if err := db.SetConversationResumeProfile(convID, profile); err != nil {
		return nil, fmt.Errorf("legacy recovery could not persist a conversation resume profile: %w", err)
	}
	// TCL-636's compatibility path historically materialized a blank legacy
	// session anchor, whose launch readers reconstructed the same explicit
	// baseline below. Persist that baseline on the stable agent now so the
	// recovery remains one-shot and later session pruning cannot erase it.
	if agentProfile, err := db.AgentRelaunchProfileForConv(convID); err != nil {
		return nil, fmt.Errorf("legacy recovery could not inspect agent relaunch profile: %w", err)
	} else if agentProfile == nil {
		agentID, err := db.AgentIDForConv(convID)
		if err != nil {
			return nil, fmt.Errorf("legacy recovery could not resolve stable agent: %w", err)
		}
		if agentID != "" {
			if err := db.SetAgentRelaunchProfile(agentID, legacy); err != nil {
				return nil, fmt.Errorf("legacy recovery could not persist agent relaunch profile: %w", err)
			}
		}
	}
	return &profile, nil
}

// baselineRelaunchProfile is the launch baseline of a conversation with no
// recorded posture.
func baselineRelaunchProfile(harnessName string) db.AgentRelaunchProfile {
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
	return db.AgentRelaunchProfile{
		Version:            db.RelaunchProfileVersion,
		HarnessBuiltinMode: &empty, ApprovalPolicy: &approval,
		ApprovalAutoReview: &no, ModelID: &empty, Effort: &empty,
		ContextWindowSize: &zero, AskUserQuestionTimeout: &empty,
		RemoteControl: &no, AutoMemory: &no, PeerMessaging: &no, AutoCompactWindow: &empty,
		SSHWorkaround: &sshWorkaround,
	}
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
