package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// cloneSpawnError carries enough context to surface either an HTTP
// error (when called from the single-clone handler) or accumulate
// into a per-member result (when called from groups-clone). The two
// callers differ in how they report failure but agree on which
// statuses + codes apply.
type cloneSpawnError struct {
	Status int
	Code   string
	Msg    string
}

func (e *cloneSpawnError) Error() string { return e.Msg }
func (e *cloneSpawnError) write(w http.ResponseWriter) {
	writeError(w, e.Status, e.Code, e.Msg)
}

// cloneSpawnParams collects cloneSpawnOnce's inputs. It replaces a positional
// signature that had grown to ten arguments — long past the point where a call
// site could be read without counting commas — for the same reason the spawn
// path threads clcommon.SpawnArgs.
//
// Title, FollowUp, HandoffFrom and HandoffGroupID drive launch enrollment (see
// cloneSpawnOnce); every other field is launch authority the clone inherits
// from its source.
type cloneSpawnParams struct {
	// SourceConv is the conversation being cloned; Cwd is where the clone runs.
	SourceConv string
	Cwd        string
	// NoCopyConv skips the jsonl fork, so the clone inherits identity only.
	NoCopyConv bool
	// Effort and Model are conservative standalone-conversation fallbacks.
	// Managed clones resolve their launch flags from durable agent intent
	// inside cloneSpawnOnce; "" omits the flag.
	Effort string
	Model  string
	// ProofToken, ProofCwd and ProofDirs carry the write-proof-verified cwd
	// and/or repository roots. cloneSpawnOnce re-asserts they are still
	// canonical immediately before each fork, closing the verify→launch window
	// the same way executeSpawn does.
	ProofToken string
	ProofCwd   bool
	ProofDirs  []string
	// CodexGitCommonDir and GitWriteDirs are the Codex worktree write grants.
	CodexGitCommonDir string
	GitWriteDirs      []string

	// Title is the clone's display name (`<base>-c-<N>`). Each caller derives
	// it its own way — per-conv clone reads the harness-native title, groups
	// clone reads the member's fresh title, export uses its own throwaway
	// naming — so it is supplied rather than computed here. On the
	// launch-enrollment path it rides in as `--name`; otherwise the caller
	// still injects it post-connect.
	Title string
	// FollowUp is the clone's first-turn handoff, "" for none. It is also what
	// makes a clone eligible for launch enrollment at all (see cloneSpawnOnce).
	// When the clone is enrolled, cloneSpawnOnce inserts the inbox row BEFORE
	// the fork — so the launch prompt can name it by id — and reports that id
	// in the result; the caller must not enqueue it a second time.
	FollowUp string
	// HandoffFrom is the conv that authored FollowUp: the source itself for a
	// self-clone, a manager for a cross-clone.
	HandoffFrom string
	// HandoffGroupID routes the handoff through a group the clone belongs to.
	// 0 — a solo clone with no groups — is a direct message, the universal-
	// inbox transport.
	HandoffGroupID int64
	// RouteHelperGroupIDs is the route-enabled destination-group plan captured
	// from the source before either clone shape starts. Membership is still
	// copied by the existing post-spawn identity step.
	RouteHelperGroupIDs []int64
}

// cloneSpawnResult is cloneSpawnOnce's success value.
type cloneSpawnResult struct {
	// NewConv is the clone's conversation id, NewTmux its tmux session.
	NewConv string
	NewTmux string
	// Label may be empty in the copy path when the session row's id field
	// hasn't materialised within the deadline; that's not an error, since the
	// conv-id is already known.
	Label string
	// Warn is non-empty when the spawn succeeded but the tmux session never
	// registered within the polling deadline (copy path only) — the caller
	// surfaces it as an HTTP response `warning` field so the dashboard can show
	// "started but not online yet" instead of a generic success toast.
	Warn string
	// ReleaseLaunchClaim ends the in-flight claim that keeps the clone's live
	// pane out of the terminal console's plain-session listing (see
	// agentLaunchIdentities). It is HANDED OVER rather than released here: the
	// clone is only an agent once the caller's EnsureAgentForConv has linked
	// it, so the claim has to outlive this function. The caller must defer it.
	// Non-nil on success; an error return releases the claim itself.
	ReleaseLaunchClaim func()

	// LaunchEnrolled reports that the clone was born named (and, when a
	// FollowUp was given, briefed) from its own launch argv. Callers MUST skip
	// their post-connect rename and handoff injection when it is true —
	// re-sending either would double the clone's greeting.
	LaunchEnrolled bool
	// HandoffMsgID is the pre-fork handoff row's id, 0 when there was no
	// follow-up, the insert failed, or the launch was not enrolled.
	HandoffMsgID int64
	// HandoffInlined records whether the launch prompt baked the whole handoff
	// inline rather than pointing at the inbox copy. When it did, the inbox
	// copy was inserted already delivered AND read — the agent has the text, so
	// it must never enter the nudge queue.
	HandoffInlined bool
}

// cloneSupportsArgvEnrollment narrows TCL-732's launch-argv contract to Claude
// Code. LaunchEnrollment alone is not sufficient: OpenCode also advertises the
// capability, but implements it by pre-minting a server-side ses_ id and
// submitting the welcome through prompt_async. cloneSpawnOnce does neither of
// those things; its --session-id / --name / positional-prompt path is the
// Claude Code contract specifically.
func cloneSupportsArgvEnrollment(h *harness.Harness) bool {
	return h != nil && h.Name == harness.DefaultName && h.SupportsLaunchEnrollment()
}

func cloneSandboxPosture(relaunch *durableRelaunchConfig) (mode, source string) {
	if relaunch.TemporarySandboxMode {
		return relaunch.NormalSandbox, relaunch.NormalSandboxSource
	}
	return relaunch.Sandbox, relaunch.SandboxModeSource
}

func cloneSSHWorkaround(relaunch *durableRelaunchConfig) bool {
	if relaunch.TemporarySandboxMode {
		return relaunch.NormalSSHWorkaround
	}
	return relaunch.SSHWorkaround
}

// cloneSpawnOnce mints a clone's conv-id (and optionally its jsonl).
// Two branches:
//   - copy: use convops to fork the existing jsonl onto a fresh
//     conv-id; spawn `tclaude session new -r <new-conv>` so CC loads
//     the cloned conversation.
//   - no-copy: spawn `tclaude session new --label <label>` and poll
//     for whatever conv-id CC mints, same as reincarnate.
//
// Extracted from runCloneOrchestration so groups-clone and the export clone can
// reuse the same race handling without duplicating it.
func cloneSpawnOnce(p cloneSpawnParams) (spawned cloneSpawnResult, cerr *cloneSpawnError) {
	sourceConv, cwd, noCopyConv := p.SourceConv, p.Cwd, p.NoCopyConv
	effort, model := p.Effort, p.Model
	proofToken, proofCwd, proofDirs := p.ProofToken, p.ProofCwd, p.ProofDirs
	codexGitCommonDir, gitWriteDirs := p.CodexGitCommonDir, p.GitWriteDirs
	var newConv, newTmux, label, warn string
	// The clone's session row and pane exist before its conversation is linked
	// to an actor, so for that window nothing durable says the row is an
	// agent's and the terminal console would list it — and offer to kill it —
	// as a plain session. Each branch below claims whichever identity it mints
	// first, as early as it has one. A successful return hands the release to
	// the caller, which holds it through EnsureAgentForConv; every error return
	// releases here, so no error path can strand a claim.
	releaseLaunchClaim := func() {}
	defer func() {
		if cerr == nil {
			spawned.ReleaseLaunchClaim = releaseLaunchClaim
			return
		}
		releaseLaunchClaim()
	}()
	relaunch, err := durableRelaunchConfigForConv(sourceConv)
	if err != nil {
		state, stateErr := db.AgentState(sourceConv)
		if stateErr != nil || state != db.AgentStateNone {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusConflict, Code: "relaunch_profile", Msg: err.Error()}
		}
		// Standalone export/conv cloning predates agent enrollment and may have
		// only a harness-native conversation. Preserve that plain CLI surface
		// with conservative defaults; managed agents never take this branch.
		h := harnessForConv(sourceConv).Name
		approval, autoReview := approvalForRelaunch(sourceConv, h)
		relaunch = &durableRelaunchConfig{
			Harness: h, Cwd: cwd, Sandbox: sandboxForHarness(h),
			Approval: approval, AutoReview: autoReview,
			Effort: effort, Model: model,
		}
	}
	effort, model = relaunch.Effort, relaunch.Model
	effectiveSandbox, err := db.AgentEffectiveSandboxConfigForConv(sourceConv)
	if err != nil {
		return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusInternalServerError, Code: "io", Msg: "load source sandbox snapshot: " + err.Error()}
	}
	if effectiveSandbox != nil {
		validated, err := ensureAgentDirectoriesForRelaunch(*effectiveSandbox)
		if err != nil {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusConflict, Code: "sandbox_profile_changed", Msg: err.Error()}
		}
		effectiveSandbox = &validated
		if _, launchErr := sandboxpolicy.FilesystemForLaunch(validated.Effective); launchErr != nil {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusConflict, Code: "sandbox_profile_changed", Msg: launchErr.Error()}
		}
	}
	reassertFail := func() *cloneSpawnError {
		if fail := reassertDirWriteProof(proofDirs); fail != nil {
			return &cloneSpawnError{Status: fail.Status, Code: fail.Kind, Msg: fail.Msg}
		}
		return nil
	}
	// Clone under the same harness the source ran on — a Codex agent's
	// clone must relaunch as Codex. "" for an untagged/claude source omits
	// the flag (the default).
	srcHarness := relaunch.Harness
	// Carry the source's armed Remote Access to the sibling (JOH-261). A clone
	// becoming a SECOND phone-reachable session alongside the still-running
	// original is the operator-decided semantics — drive either from the phone.
	// False (and so omitted) for an unarmed source or a Codex source.
	remoteControl := relaunch.RemoteControl
	autoMemory := relaunch.AutoMemory
	// A clone is meant to be the same agent working alongside the original, so it
	// inherits the source's startup-context shape too — a lean source must not
	// produce a fat sibling.
	contextFeatures := relaunch.ContextFeatures
	// Same reasoning for the auto-compaction window: a source pinned to compact
	// at 450K must not produce a sibling that runs to the model's full window.
	autoCompactWindow := relaunch.AutoCompactWindow
	// The temporary unlock belongs to the source's stable agent. A clone is a
	// new agent and must inherit the preserved normal posture, otherwise one
	// temporary debugging action would mint a permanently-unconfined sibling.
	cloneSandbox, cloneSandboxSource := cloneSandboxPosture(relaunch)
	cloneSSH := cloneSSHWorkaround(relaunch)
	// A clone inherits the source's launch CHOICE, never its verification. For
	// a harness whose own OS sandbox lives in mutable configuration, the
	// source's recorded single-boundary posture says only what was true when
	// the source launched — and a clone is precisely where a stale check would
	// be invisible, because nobody restates the sandbox choice.
	if fail := sandboxImplementationPostureFailure(
		srcHarness, relaunch.SandboxImplementation); fail != nil {
		return cloneSpawnResult{}, &cloneSpawnError{
			Status: fail.Status, Code: fail.Kind, Msg: fail.Msg,
		}
	}
	if _, fail := planSandboxProfileAccessForLaunch(
		srcHarness, cloneSandbox, effectiveSandbox, relaunch.SandboxImplementation,
		session.ModelTransportLaunchContext{Model: model, Cwd: cwd},
		false,
	); fail != nil {
		return cloneSpawnResult{}, &cloneSpawnError{
			Status: fail.Status,
			Code:   fail.Kind,
			Msg:    fail.Msg,
		}
	}
	codexGitCommonDirPinned := spawnUsesPinnedGitCommonDir(
		srcHarness, cloneSandbox, relaunch.SandboxImplementation)
	if codexGitCommonDirPinned && gitWriteDirs == nil {
		if home, err := os.UserHomeDir(); err == nil {
			gitWriteDirs = harness.GitWorktreeWriteDirs(cwd, codexGitCommonDir, home)
		}
	}
	var grantFail *spawnFailure
	gitWriteDirs, grantFail = canonicalizeRepositoryWriteDirs(gitWriteDirs, proofDirs, proofToken)
	if grantFail != nil {
		return cloneSpawnResult{}, &cloneSpawnError{Status: grantFail.Status, Code: grantFail.Kind, Msg: grantFail.Msg}
	}
	exactGrantPinned := codexGitCommonDirPinned && proofToken != ""
	if !exactGrantPinned {
		gitWriteDirs = nil
		codexGitCommonDir = ""
		codexGitCommonDirPinned = false
	}
	// Preserve the source's per-agent AskUserQuestion timeout onto the sibling
	// (schema v97). "" for a source
	// that recorded none (a non-Claude or pre-column source).
	askTimeout := relaunch.AskUserQuestionTimeout
	approval, autoReview := relaunch.Approval, relaunch.AutoReview
	if len(p.RouteHelperGroupIDs) > 0 {
		if srcHarness != harness.DefaultName || relaunch.SandboxImplementation != string(sandboxpolicy.ImplementationTclaudeLayer) {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusUnprocessableEntity, Code: "unsupported_group_route_launch", Msg: "Linux group routes require a pane-authoritative tclaude-layer clone"}
		}
		if effectiveSandbox == nil {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusUnprocessableEntity, Code: "unsupported_group_route_launch", Msg: "Linux group routes require a frozen private network posture for the clone"}
		}
		posture, postureErr := session.TclaudeLayerNetworkPosture(effectiveSandbox.Effective)
		if postureErr != nil {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusUnprocessableEntity, Code: "unsupported_group_route_launch", Msg: "resolve clone route posture: " + postureErr.Error()}
		}
		if posture == sandboxpolicy.NetworkHostOpen {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusUnprocessableEntity, Code: "unsupported_group_route_launch", Msg: "Linux group routes require a private tclaude-layer network namespace"}
		}
	}

	// Launch enrollment (TCL-732) — the clone twin of TCL-731's reincarnation
	// fix. A Claude Code clone can be born named, and (when the caller gave a
	// follow-up) briefed, from its own launch argv, so neither has to be
	// send-keys'd into a pane whose input readiness the daemon cannot observe.
	// A pre-TUI tty buffers the literal text but drops the Enter keypresses, so
	// the post-connect `/rename <title>` and the handoff nudge can replay as ONE
	// merged line: the clone ends up titled after its own handoff, and the
	// handoff is never delivered as a turn yet is durably stamped delivered.
	// The settle gap in runClonePostInit narrows that window; it does not close
	// it.
	//
	// Both branches can enroll. The no-copy branch presets the conv-id with
	// `--session-id` exactly as reincarnate does. The copy branch resumes a
	// forked jsonl whose id it already knows, and `claude --resume <id> --name
	// <n> '<prompt>'` submits the positional as a turn on the resumed
	// conversation and records the name as a custom-title turn without forking
	// the id (verified against claude 2.1.220).
	//
	// Harnesses that cannot preset a conv-id or take a launch prompt — Codex
	// mints its id at first turn and renames out-of-band through ConvStore —
	// keep the inject-after-connect flow in runClonePostInit, unchanged, as
	// does the agent.spawn_legacy_injection revert.
	//
	// A follow-up is REQUIRED to enroll, and that is load-bearing rather than
	// incidental. `claude --session-id <id> --name <n>` with no positional
	// prompt applies the name to the running TUI but writes no transcript at
	// all (verified against claude 2.1.220): the conversation materialises on
	// its FIRST TURN. A name-only enrollment would therefore leave a clone with
	// no .jsonl — showing as "(unknown)" and unrecoverable when never used —
	// which is the very trap runClonePostInit's /rename exists to avoid. It
	// also costs nothing to require: with no follow-up there is only ONE
	// injected stream, so the two-streams-merge-into-one-line bug this fixes
	// cannot arise in the first place.
	cloneHarness, _ := harness.Resolve(srcHarness)
	launchEnroll := cloneSupportsArgvEnrollment(cloneHarness) && !spawnUsesLegacyInjection() && p.FollowUp != ""
	res := cloneSpawnResult{LaunchEnrolled: launchEnroll}
	routeHelperPrepared := false
	routeHelperCommitted := false
	defer func() {
		if routeHelperPrepared && !routeHelperCommitted {
			_, _ = db.DeleteAgentByConvID(newConv)
			revokeRouteHelperCredentials(newConv, "")
		}
	}()
	prepareRouteHelper := func(args *clcommon.SpawnArgs) *cloneSpawnError {
		if len(p.RouteHelperGroupIDs) == 0 {
			return nil
		}
		agentID, _, ensureErr := db.EnsureAgentForConv(newConv, "clone")
		if ensureErr != nil {
			return &cloneSpawnError{Status: http.StatusInternalServerError, Code: "route_authority", Msg: "reserve clone route identity: " + ensureErr.Error()}
		}
		credential, generation, credentialErr := mintRouteHelperCredential(agentID, newConv)
		if credentialErr != nil {
			_, _ = db.DeleteAgentByConvID(newConv)
			return &cloneSpawnError{Status: http.StatusInternalServerError, Code: "route_authority", Msg: "mint clone route helper credential: " + credentialErr.Error()}
		}
		args.RouteHelperAgentID = agentID
		args.RouteHelperConvID = newConv
		args.RouteHelperLaunchGeneration = generation
		args.RouteHelperCredential = credential
		args.RouteHelperGroupIDs = append([]int64(nil), p.RouteHelperGroupIDs...)
		routeHelperPrepared = true
		return nil
	}
	commitRouteHelper := func() { routeHelperCommitted = true }
	// enrollLaunch stamps the clone's name and first-turn handoff onto the
	// launch args once its conv-id is known — preset by the caller on the
	// no-copy branch, minted by the jsonl fork on the copy branch.
	enrollLaunch := func(args *clcommon.SpawnArgs, convID string) {
		if !launchEnroll {
			return
		}
		// Match the spawn path's title gate: a name that isn't a valid rename
		// title is not applied as the launch `--name`, since Claude Code records
		// it verbatim as the conversation title.
		if p.Title != "" && isValidRenameTitle(p.Title) {
			args.Name = p.Title
		} else if p.Title != "" {
			slog.Warn("clone: title not a valid rename title; skipping launch --name",
				"conv", convID, "title", p.Title)
		}
		// Read the inline cap ONCE and thread it through both the bookkeeping
		// decision and the prompt build (spawn and reincarnate do the same):
		// config.Load is uncached, so two reads could disagree and leave a row
		// born delivered+read whose launch prompt only pointed at the inbox.
		inlineCap := spawnInlineMaxChars()
		res.HandoffInlined = spawnBriefingFitsLaunch(p.FollowUp, inlineCap)
		res.HandoffMsgID = insertCloneHandoff(p.HandoffGroupID, p.HandoffFrom, convID, p.FollowUp, res.HandoffInlined)
		// The prompt names the clone by the title it was ACTUALLY launched with
		// (args.Name), not the raw one — a title the gate above rejected must
		// not be echoed back at the agent as its own identity.
		args.InitialPrompt = buildCloneLaunchPrompt(args.Name,
			cloneHandoffAuthor(p.HandoffFrom, sourceConv), res.HandoffMsgID, p.FollowUp, inlineCap)
	}
	// rollbackHandoff removes the pre-fork handoff row when the launch never
	// happened, so a failed clone cannot strand an orphan inbox message
	// addressed to a conv-id that will never exist.
	rollbackHandoff := func() {
		if res.HandoffMsgID <= 0 {
			return
		}
		if _, err := db.DeleteAgentMessagesByIDs([]int64{res.HandoffMsgID}); err != nil {
			slog.Warn("clone: rollback of pre-fork handoff message failed",
				"msg_id", res.HandoffMsgID, "error", err)
		}
		res.HandoffMsgID = 0
	}

	if noCopyConv {
		label = generateSpawnLabel()
		// No conv-id exists yet on this path; the row this launch writes is
		// keyed by the label.
		releaseLaunchClaim = claimAgentLaunchIdentity(label)
		agentDirectoryCleanup := func() {}
		if effectiveSandbox != nil {
			materialized, cleanup, materializeErr := prepareCodexSSHWorkaroundForNewLaunch(
				*effectiveSandbox, label, cloneSSH)
			if materializeErr != nil {
				return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusInternalServerError, Code: "spawn", Msg: materializeErr.Error()}
			}
			effectiveSandbox = &materialized
			agentDirectoryCleanup = cleanup
		}
		// A clone preserves recorded approval and auto-review authority, but does
		// not pre-trust the cwd (that edits ~/.codex/config.toml and remains a
		// fresh-spawn opt-in).
		if fail := reassertFail(); fail != nil {
			agentDirectoryCleanup()
			return cloneSpawnResult{}, fail
		}
		proofArgs := clcommon.SpawnArgs{
			DirWriteProof:    proofToken,
			EffectiveSandbox: effectiveSandbox,
		}
		if proofCwd {
			proofArgs.CwdWriteProof = proofToken
			proofArgs.DirWriteProof = ""
		}
		proofArgs.Label = label
		proofArgs.Cwd = cwd
		proofArgs.Effort = effort
		proofArgs.Model = model
		proofArgs.Harness = srcHarness
		proofArgs.Sandbox = cloneSandbox
		proofArgs.SandboxImplementation = relaunch.SandboxImplementation
		proofArgs.SandboxChosenBy = cloneSandboxSource
		proofArgs.CodexGitCommonDir = codexGitCommonDir
		proofArgs.CodexGitCommonDirPinned = codexGitCommonDirPinned
		proofArgs.GitWorktreeWriteDirs = gitWriteDirs
		proofArgs.GitWorktreeWriteDirsPinned = exactGrantPinned
		proofArgs.Approval = approval
		proofArgs.ToolGovernance = relaunch.ToolGovernance
		proofArgs.AutoReview = autoReview
		proofArgs.AskUserQuestionTimeout = askTimeout
		proofArgs.RemoteControl = remoteControl
		proofArgs.AutoMemory = autoMemory
		proofArgs.ContextFeatures = contextFeatures
		proofArgs.AutoCompactWindow = autoCompactWindow
		// A launch-enrolled no-copy clone presets its conv-id so the name and
		// handoff can ride the same argv, exactly as reincarnate's successor
		// does. Without enrollment the id is whatever the harness mints, and the
		// poll below discovers it.
		if launchEnroll || len(p.RouteHelperGroupIDs) > 0 {
			newConv = convops.GenerateUUID()
			proofArgs.SessionID = newConv
		}
		if routeErr := prepareRouteHelper(&proofArgs); routeErr != nil {
			agentDirectoryCleanup()
			return cloneSpawnResult{}, routeErr
		}
		enrollLaunch(&proofArgs, newConv)
		// A wrapper that dies before the pane exists (a bad executable, a
		// rejected launch flag) would otherwise only surface as the poll
		// timeout below. Watch for it so a failed launch rolls its pre-fork
		// handoff row back promptly instead of stranding it.
		wrapperFailure := registerWrapperFailureSignal(label)
		defer unregisterWrapperFailureSignal(label)
		if err := SpawnDetachedTclaudeNew(proofArgs); err != nil {
			agentDirectoryCleanup()
			rollbackHandoff()
			return cloneSpawnResult{}, &cloneSpawnError{
				Status: http.StatusInternalServerError, Code: "spawn",
				Msg: "failed to launch tclaude session new: " + err.Error(),
			}
		}
		deadline := time.Now().Add(reincarnateSpawnTimeout)
		for time.Now().Before(deadline) {
			select {
			case werr := <-wrapperFailure:
				rollbackHandoff()
				return cloneSpawnResult{}, &cloneSpawnError{
					Status: http.StatusInternalServerError, Code: "spawn",
					Msg: "the clone's session wrapper exited before its pane came up: " + werr.Error(),
				}
			default:
			}
			s, err := db.LoadSession(label)
			if err == nil && s != nil {
				newTmux = s.TmuxSession
				// Without enrollment the session row IS the discovery channel for
				// the harness-minted conv-id, so a non-empty ConvID is proof
				// enough. With enrollment the row is born already carrying the
				// preset id — `session new` writes it BEFORE it creates the tmux
				// session — so the id alone proves nothing. Wait for the pane to
				// actually register instead, the same gate reincarnate uses.
				if launchEnroll {
					if newTmux != "" && isConvOnline(newConv) {
						if s.ConvID != "" && s.ConvID != newConv {
							slog.Warn("clone: session row reports a different conv-id than the preset one",
								"label", label, "preset", newConv, "row", s.ConvID)
						}
						if remoteControl {
							armRemoteControlOnNewRow(label)
						}
						res.NewConv, res.NewTmux, res.Label = newConv, newTmux, label
						commitRouteHelper()
						return res, nil
					}
				} else if s.ConvID != "" {
					// Tag the sibling row's best-known remote-control ON (JOH-261);
					// the --remote-control launch flag already armed its pane.
					if remoteControl {
						armRemoteControlOnNewRow(label)
					}
					res.NewConv, res.NewTmux, res.Label = s.ConvID, newTmux, label
					commitRouteHelper()
					return res, nil
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		missing := "conv-id"
		if launchEnroll {
			missing = "a live pane"
		}
		slog.Warn("clone: no-copy poll timed out; clone never came up",
			"label", label, "missing", missing, "deadline", reincarnateSpawnTimeout)
		// The pre-fork handoff is about to be rolled back, so do not leave a
		// late-starting pane whose launch prompt points at that deleted row.
		// This mirrors reincarnate's timeout cleanup; the generated label is
		// unique to this launch.
		tmuxToKill := newTmux
		if tmuxToKill == "" {
			tmuxToKill = label
		}
		if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxToKill)).Run(); err != nil {
			slog.Warn("clone: timed-out no-copy launch kill failed",
				"session", tmuxToKill, "error", err)
		}
		rollbackHandoff()
		return cloneSpawnResult{NewTmux: newTmux, Label: label}, &cloneSpawnError{
			Status: http.StatusGatewayTimeout, Code: "timeout",
			Msg: "spawned session " + label + " but " + missing + " never materialised within " +
				reincarnateSpawnTimeout.String() + "; the timed-out clone was stopped",
		}
	}
	// Copy path: fork the jsonl first, then resume into it.
	copyResult, err := convops.CopyConversationToPath(sourceConv, cwd, true /* global */)
	if err != nil {
		return cloneSpawnResult{}, &cloneSpawnError{
			Status: http.StatusInternalServerError, Code: "copy",
			Msg: "failed to copy conversation jsonl: " + err.Error(),
		}
	}
	newConv = copyResult.NewConvID
	// The forked jsonl fixes the conv-id before the launch, so that is what
	// this path claims: the row it writes carries this conv, and its label is
	// only discovered from the row afterwards. Without this the clone is a
	// killable "(session)" row for the whole poll below whenever no sandbox
	// snapshot triggers the early EnsureAgentForConv.
	releaseLaunchClaim = claimAgentLaunchIdentity(newConv)
	agentDirectoryCleanup := func() {}
	if effectiveSandbox != nil {
		materialized, cleanup, materializeErr := prepareCodexSSHWorkaroundForNewLaunch(
			*effectiveSandbox, newConv, cloneSSH)
		if materializeErr != nil {
			return cloneSpawnResult{}, &cloneSpawnError{Status: http.StatusInternalServerError, Code: "spawn", Msg: materializeErr.Error()}
		}
		effectiveSandbox = &materialized
		agentDirectoryCleanup = cleanup
	}
	if fail := reassertFail(); fail != nil {
		agentDirectoryCleanup()
		return cloneSpawnResult{}, fail
	}
	proofArgs := clcommon.SpawnArgs{
		DirWriteProof:    proofToken,
		EffectiveSandbox: effectiveSandbox,
	}
	if proofCwd {
		proofArgs.CwdWriteProof = proofToken
		proofArgs.DirWriteProof = ""
	}
	proofArgs.ConvID = newConv
	proofArgs.Cwd = cwd
	proofArgs.Effort = effort
	proofArgs.Model = model
	proofArgs.Harness = srcHarness
	proofArgs.Sandbox = cloneSandbox
	proofArgs.SandboxImplementation = relaunch.SandboxImplementation
	proofArgs.SandboxChosenBy = cloneSandboxSource
	proofArgs.CodexGitCommonDir = codexGitCommonDir
	proofArgs.CodexGitCommonDirPinned = codexGitCommonDirPinned
	proofArgs.GitWorktreeWriteDirs = gitWriteDirs
	proofArgs.GitWorktreeWriteDirsPinned = exactGrantPinned
	proofArgs.Approval = approval
	proofArgs.ToolGovernance = relaunch.ToolGovernance
	proofArgs.AutoReview = autoReview
	proofArgs.AskUserQuestionTimeout = askTimeout
	proofArgs.RemoteControl = remoteControl
	proofArgs.AutoMemory = autoMemory
	proofArgs.ContextFeatures = contextFeatures
	proofArgs.AutoCompactWindow = autoCompactWindow
	if routeErr := prepareRouteHelper(&proofArgs); routeErr != nil {
		agentDirectoryCleanup()
		return cloneSpawnResult{}, routeErr
	}
	// The forked jsonl already fixes the clone's conv-id, so the copy path needs
	// no --session-id: the name and handoff ride the resume argv directly.
	enrollLaunch(&proofArgs, newConv)
	if err := SpawnDetachedTclaudeResume(proofArgs); err != nil {
		agentDirectoryCleanup()
		rollbackHandoff()
		return cloneSpawnResult{}, &cloneSpawnError{
			Status: http.StatusInternalServerError, Code: "spawn",
			Msg: "failed to launch tclaude session new -r: " + err.Error(),
		}
	}
	// Persist the launch snapshot before waiting for the session row. The copy
	// path already knows its conversation ID, and may return success with a
	// warning if registration times out; retaining the snapshot here prevents a
	// later resume from falling back to the source agent's generated dirs.
	if effectiveSandbox != nil {
		agentID, _, persistErr := db.EnsureAgentForConv(newConv, "clone")
		if persistErr == nil {
			persistErr = db.SetAgentEffectiveSandboxConfig(agentID, effectiveSandbox)
		}
		if persistErr != nil {
			return cloneSpawnResult{}, &cloneSpawnError{
				Status: http.StatusInternalServerError, Code: "io",
				Msg: "persist clone sandbox snapshot: " + persistErr.Error(),
			}
		}
	}
	deadline := time.Now().Add(reincarnateSpawnTimeout)
	for time.Now().Before(deadline) {
		if s, err := db.FindSessionByConvID(newConv); err == nil && s != nil && s.TmuxSession != "" {
			newTmux = s.TmuxSession
			if s.ID != "" {
				label = s.ID
			}
			// A launch-enrolled clone's first turn is already recorded in the
			// copied jsonl, so the row and transcript can both exist after the
			// harness has died at startup. Require the pane to survive before
			// reporting success. Legacy clones keep their historical
			// registration-only gate.
			if launchEnroll && !isConvOnline(newConv) {
				time.Sleep(250 * time.Millisecond)
				continue
			}
			// Tag the sibling row's best-known remote-control ON (JOH-261); the
			// --remote-control launch flag (on the resume) already armed its
			// pane. The copy path discovers the label from the row (s.ID), so
			// tag only when it materialised — an empty label can't be keyed.
			if remoteControl && label != "" {
				armRemoteControlOnNewRow(label)
			}
			res.NewConv, res.NewTmux, res.Label = newConv, newTmux, label
			commitRouteHelper()
			return res, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	if launchEnroll {
		if newTmux != "" {
			if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(newTmux)).Run(); err != nil {
				slog.Warn("clone: timed-out copy launch kill failed",
					"session", newTmux, "error", err)
			}
		}
		rollbackHandoff()
		return cloneSpawnResult{NewConv: newConv, NewTmux: newTmux, Label: label}, &cloneSpawnError{
			Status: http.StatusGatewayTimeout, Code: "timeout",
			Msg: "spawned session for " + short8(newConv) + " but a live pane never materialised within " +
				reincarnateSpawnTimeout.String() + "; the timed-out clone was stopped",
		}
	}
	// Spawn was best-effort fire-and-forget: the conv-id is already
	// known and the .jsonl exists, so we don't fail the request. But
	// we DO surface a warning — silently returning success here is the
	// "clone modal sat for 30s then showed a toast but nothing appeared"
	// trap. Logs (~/.tclaude/output.log) and the captured subprocess
	// stderr (see liveSpawnResume) tell the rest of the story.
	warn = fmt.Sprintf("spawned tclaude session for %s but its tmux session never registered within %s — the new agent may still come online; check ~/.tclaude/output.log for subprocess errors",
		short8(newConv), reincarnateSpawnTimeout)
	slog.Warn("clone: copy-path poll timed out; tmux session never registered",
		"new_conv", newConv, "deadline", reincarnateSpawnTimeout)
	res.NewConv, res.NewTmux, res.Label, res.Warn = newConv, newTmux, label, warn
	commitRouteHelper()
	return res, nil
}

// CloneHandoffSubject is the subject stamped on the follow-up a clone is born
// with. Shared by the launch-enrollment path — where the row is written BEFORE
// the fork so the launch prompt can name it by id — and the legacy
// inject-after-connect path, so the two never drift apart. Exported so flow
// tests can find the row without re-spelling the literal.
const CloneHandoffSubject = "clone handoff"

// insertCloneHandoff writes the clone's follow-up as an inbox row and returns
// its id. 0 on failure: a missing handoff row is logged, never fatal, because
// the clone itself already exists either way.
//
// inlined marks the row consumed at birth — the launch prompt carried the whole
// body, so the copy must never enter the nudge queue. Mirrors
// insertReincarnationHandoff.
func insertCloneHandoff(groupID int64, fromConv, toConv, body string, inlined bool) int64 {
	m := &db.AgentMessage{
		GroupID:  groupID,
		FromConv: fromConv,
		ToConv:   toConv,
		Subject:  CloneHandoffSubject,
		Body:     body,
	}
	if inlined {
		consumedAt := time.Now()
		m.CreatedAt = consumedAt
		m.DeliveredAt = consumedAt
		m.ReadAt = consumedAt
	}
	id, err := db.InsertAgentMessage(m)
	if err != nil {
		slog.Warn("clone: insert handoff message failed", "conv", toConv, "error", err)
		return 0
	}
	return id
}

// cloneHandoffAuthor resolves who WROTE the follow-up, for the clone's launch
// prompt. A self-clone ("" here) needs no attribution — the prompt already
// frames the text as the agent's own instruction to itself. A cross-agent clone
// names the manager that triggered it, resolved through the same display-name
// path the spawn welcome uses.
func cloneHandoffAuthor(caller, sourceConv string) string {
	if caller == "" || caller == sourceConv {
		return ""
	}
	return resolveSpawnerTitle(caller, "")
}

// buildCloneLaunchPrompt builds the positional launch prompt a launch-enrolled
// Claude Code clone submits as its FIRST turn (TCL-732). It is the clone twin of
// buildReincarnationLaunchPrompt: it rides in as a single shell-quoted argv
// positional, so — unlike the send-keys nudge it replaces — it can be multi-line
// and cannot be dropped, merged, or mistimed by a pane that is not reading input
// yet.
//
// Unlike a reincarnation, a clone gets a prompt ONLY when the caller supplied a
// follow-up: a clone with nothing to do stays idle (as it does today) rather
// than being woken by an orientation turn it never needed. Callers rely on the
// empty return for that, so keep it.
//
// The body is inlined right after the [system: ...] orientation when it fits
// inlineMaxChars runes, so the clone acts on its first turn with no `tclaude
// agent inbox read` round-trip. A longer follow-up keeps the inbox pointer form,
// where it is scrollable and doesn't balloon the launch command; both forms name
// the inbox copy by id.
func buildCloneLaunchPrompt(title, handoffAuthor string, msgID int64, followUp string, inlineMaxChars int) string {
	body := strings.TrimSpace(followUp)
	if body == "" {
		return ""
	}
	welcome := "you are a fresh clone"
	if title != "" {
		welcome += " named " + strconv.Quote(title)
	}
	welcome += " of another agent: a sibling that inherits its identity" +
		" (groups, permissions, ownerships) and now runs alongside it, independently."
	if handoffAuthor != "" {
		welcome += " The follow-up below was written by " + handoffAuthor + "."
	}
	welcome += " Use `tclaude agent` commands (whoami / --help / inbox ls) to introspect and coordinate."

	fits := inlineMaxChars > 0 && utf8.RuneCountInString(body) <= inlineMaxChars
	if msgID > 0 && !fits {
		welcome += fmt.Sprintf(" Your follow-up is waiting in your inbox as message #%d"+
			" — read it with `tclaude agent inbox read %d`, then act on it.", msgID, msgID)
		return "[system: " + welcome + "]"
	}
	inboxNote := ""
	if msgID > 0 {
		inboxNote = fmt.Sprintf(" (also saved to your inbox as message #%d)", msgID)
	}
	welcome += " Your follow-up is below" + inboxNote + "; act on it."
	return "[system: " + welcome + "]\n\n" + body
}

// defaultCloneCooldown is the built-in fallback for CloneCooldown when
// neither the `--agent-clone-cooldown` serve flag nor the
// agent.clone_cooldown config field is set. resolveCloneCooldown
// returns it as the lowest-priority tier.
const defaultCloneCooldown = time.Minute

// CloneCooldown is the minimum time between two clones of the same
// source conv. The clone handler does an atomic INSERT-WHERE-NOT-
// EXISTS against agent_clone_history to enforce this — see
// db.ClaimCloneSlot. Defaults to defaultCloneCooldown; `tclaude agentd
// serve` overwrites it at startup from resolveCloneCooldown (flag >
// config > default), and flow tests shrink it via t.Cleanup-restored
// assignment to drive the locked/unlocked branches without sleeping.
//
// Keyed by source conv, and applied only to agent-initiated clones:
// the runaway scenario the TODO flagged is "an agent cloning the same
// conv in a tight loop". Human-initiated clones are exempt (see
// isHumanCloneCaller) — a human can't loop at machine speed and clones
// deliberately. A manager agent that fans out clones of *different*
// sources hits the limit only if it tries the *same* source twice
// within cooldown.
var CloneCooldown = defaultCloneCooldown

// isHumanCloneCaller reports whether a clone was initiated by a human
// rather than an agent. Humans are exempt from CloneCooldown — the gate
// exists to bound a runaway *agent* loop, and a human can't fire clones
// at machine speed. A human reaches runCloneOrchestration with one of
// two caller shapes, both of which this must recognise:
//
//   - "": the /v1 endpoints, where requireCrossAgentPermission returns
//     "" for a classHuman peer (CLI or cookie-auth with no agent
//     ancestor).
//   - dashboardGranter ("<human-dashboard>"): the dashboard endpoint
//     (dashboardCloneAgent) records the human-dashboard sentinel as the
//     caller so the audit trail shows the human acted — but it is still
//     a human and must not be throttled.
//
// Both are empty / angle-bracketed pseudo-identities; an agent caller is
// always a real conv-id (a UUID), which never starts with "<". Keying
// the exemption here — rather than on a bare caller=="" — is what stops a
// dashboard clone from being wrongly treated as a runaway agent.
func isHumanCloneCaller(caller string) bool {
	return caller == "" || strings.HasPrefix(caller, "<")
}

// `tclaude agent clone` — fork the calling agent into a sibling that
// inherits its identity (groups, permissions, ownerships) but
// continues running independently. Unlike reincarnate, the original
// is NOT shut down and its identity rows are NOT removed.
//
// Two modes:
//
//   - default: copy the original's conv jsonl onto a fresh conv-id,
//     then spawn a new tclaude session with `-r <new-conv>`. The
//     clone starts with the SAME context as the original — useful for
//     "fork a worker to try a parallel approach."
//   - --no-copy-conv: skip the jsonl copy, spawn fresh CC. The clone
//     inherits identity only — useful for "stand up a peer in the
//     same role without dragging the conversation history along."
//
// Identity: the clone is renamed to `<original-title>-c-<N>` (or
// `c-<N>` if the original had no title) — the same `-c-` scheme
// `groups clone` uses, and the title sibling of reincarnate's `-r-`.
// The clone joins each of the original's groups, but membership rows
// carry no name of their own: the clone's single name is its title.

// cloneSuffixRegex matches a trailing clone suffix in either the
// current short form `-c-<digits>` or the legacy long form
// `-clone-<digits>`. Recognising both lets a legacy
// `worker-clone-3` cleanly transition to `worker-c-1` (rather than
// nesting as `worker-clone-3-c-1`) the next time it's cloned. Same
// idea for reincarnateSuffixRegex.
var cloneSuffixRegex = regexp.MustCompile(`^(.*?)-(?:c|clone)-\d+$`)

// uniqueCloneTitle computes the clone's conversation title. The format
// is ALWAYS `<base>-c-<N>` (or `c-<N>` when the original had no
// title). base is origTitle with any existing `-c-<digits>` /
// `-clone-<digits>` stripped, so a clone-of-a-clone bumps N rather
// than nesting suffixes (`worker-c-3` clones to `worker-c-4`, not
// `worker-c-3-c-1`). The short `-c-` keeps clone titles compact enough
// to tile in dashboard rows. (Reincarnation no longer has a parallel
// live-side suffix — post-JOH-319 the living generation keeps its plain
// base name; see agentd.retiredGenerationTitle.)
//
// N is monotonically larger than the previous clone's N: we start
// the search at `prevN + 1`, then advance to the smallest free slot
// from that floor. Without the floor, a previously-used N whose
// conv_index row has since disappeared (pruned, retitled, file
// deleted) gets recycled — chronologically confusing when the new
// clone descends from a higher-numbered ancestor. The "used" set
// scans every conv_index title so parallel clones don't collide;
// legacy `-clone-N` titles don't reserve a number in the new
// namespace.
//
// Lookup error → fall back to `prevN + 1` (or 1 when prevN is 0).
func uniqueCloneTitle(origTitle string) string {
	base := origTitle
	prevN := 0
	if m := cloneSuffixRegex.FindStringSubmatch(base); m != nil {
		base = m[1]
		// Re-extract N from the final dash-separated token; the regex
		// only captures the base. Same shape as the reincarnate
		// counterpart for symmetry.
		if i := strings.LastIndex(origTitle, "-"); i >= 0 {
			if n, err := strconv.Atoi(origTitle[i+1:]); err == nil {
				prevN = n
			}
		}
	}
	prefix := "c-"
	if base != "" {
		prefix = base + "-c-"
	}
	used := scanCloneSuffixes(prefix)
	start := max(prevN+1, 1)
	for n := start; ; n++ {
		if !used[n] {
			return prefix + strconv.Itoa(n)
		}
	}
}

// scanCloneSuffixes walks every conv_index row and returns the set of
// integers N where some custom_title equals `<prefix><N>`. Used by
// uniqueCloneTitle to pick the smallest free N.
func scanCloneSuffixes(prefix string) map[int]bool {
	used := map[int]bool{}
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return used
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.CustomTitle, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(r.CustomTitle, prefix)
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		used[n] = true
	}
	return used
}

// handleWhoamiClone handles POST /v1/whoami/clone (self path).
// Gated on self.clone (default-granted alongside self.compact). Delegates to
// runCloneOrchestration with
// target == caller.
func handleWhoamiClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requirePermission(w, r, PermSelfClone)
	if !ok {
		return
	}
	if caller == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint clones the calling agent's own conversation; humans should use `tclaude conv copy` directly, or use POST /v1/agent/{conv}/clone to clone another agent")
		return
	}
	body, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, r, caller, caller, PermSelfClone, body)
}

// handleAgentClone handles POST /v1/agent/{conv}/clone (cross-agent).
// Gated on agent.clone OR group-owner-of-target.
func handleAgentClone(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentClone, targetConv)
	if !ok {
		return
	}
	body, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, r, targetConv, caller, PermAgentClone, body)
}

// cloneBody is the decoded, validated POST body shared by the clone
// endpoints (self / cross-agent / dashboard).
type cloneBody struct {
	// FollowUp is the optional first-turn prompt for the clone.
	FollowUp string `json:"follow_up"`
	// NoCopyConv spawns the clone with a fresh context instead of copying
	// the source's conversation jsonl.
	NoCopyConv bool `json:"no_copy_conv"`
	// Cwd is an optional override for where the clone's session is spawned —
	// empty means "inherit the source's cwd" (the historical behaviour); a
	// worktree path lets the human fork a clone onto a parallel branch.
	Cwd string `json:"cwd"`
	// WriteProofToken answers the dir write-proof challenge an agent caller
	// receives when it sets Cwd — same contract as SpawnRequest's field: the
	// caller must prove its own sandbox can write in the override directory
	// before it may aim a clone's cwd-based access there. The source's inherited
	// sandbox-profile roots are target-authorized and never require this proof.
	// Unused for humans and for clones that inherit the source's cwd.
	WriteProofToken string `json:"write_proof_token"`
}

// decodeCloneBody parses + validates the optional follow_up, no_copy_conv,
// cwd and write_proof_token body fields.
func decodeCloneBody(w http.ResponseWriter, r *http.Request) (cloneBody, bool) {
	var body cloneBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return cloneBody{}, false
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	// Charset/length: validate against the inbox rule. Every clone
	// handoff — grouped or solo — rides the inbox as an agent_messages
	// row (the universal-inbox transport), so it tolerates the same
	// ≤16384-byte, newline-friendly charset as a spawn --initial-message.
	if body.FollowUp != "" && !isValidInitialMessage(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			fmt.Sprintf("REJECTED. follow_up must be at most %d characters; newlines "+
				"and tabs are allowed (a grouped clone receives the handoff in its "+
				"inbox, like a spawn brief), but NUL / escape / other control "+
				"characters are not.", agent.MaxInitialMessageBytes))
		return cloneBody{}, false
	}
	body.Cwd = strings.TrimSpace(body.Cwd)
	return body, true
}

// runCloneOrchestration is the target-agnostic body shared by self
// and cross-agent clone endpoints.
//
//   - target is the conv being cloned (its identity gets copied to the
//     new conv-id).
//   - caller is the conv that triggered the clone; recorded in the
//     audit trail (`system:clone:by=<caller>` for cross calls) and
//     used as the FromConv on the optional handoff message.
//   - perm is the slug requirePermission gated this call on
//     (PermSelfClone / PermAgentClone / "" for human dashboard). Used
//     to annotate `granted_by` with `:via-sudo:grant-id=<n>` when the
//     call only passed because of a sudo grant.
//   - body.Cwd, when non-empty, is the directory the clone's CC
//     session is spawned into instead of the source's cwd — typically
//     a git worktree path so a clone can pick up work on a parallel
//     branch. It's validated (exists, is a directory, "~" expanded)
//     before use; a bad value fails the whole clone with a 400. An
//     AGENT caller must additionally pass the dir write-proof for it
//     (see below).
func runCloneOrchestration(w http.ResponseWriter, r *http.Request, target, caller, perm string, body cloneBody) {
	followUp, noCopyConv, cwdOverride := body.FollowUp, body.NoCopyConv, body.Cwd
	// 1. Snapshot target state. Same shape as reincarnate's snapshot
	// pass.
	oldSess := pickAliveSession(target)
	if oldSess == nil {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session; can't clone without a cwd to spawn the sibling into")
		return
	}
	relaunch, relaunchErr := durableRelaunchConfigForConv(target)
	if relaunchErr != nil {
		writeError(w, http.StatusConflict, "relaunch_profile", relaunchErr.Error())
		return
	}
	cwd := oldSess.Cwd
	if cwdOverride == "" {
		var cwdErr error
		cwd, cwdErr = livePaneCwd(oldSess.TmuxSession)
		if cwdErr != nil {
			writeError(w, http.StatusInternalServerError, "io", cwdErr.Error())
			return
		}
	}
	var proofDirs []string
	var proofToken string
	srcHarness := relaunch.Harness
	cloneSandbox, _ := cloneSandboxPosture(relaunch)
	if cwdOverride != "" {
		resolved, err := resolveSpawnCwd(cwdOverride)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
		cwd = resolved
	}
	codexGitCommonDir, gerr := spawnGitCommonDir(
		srcHarness, cloneSandbox, relaunch.SandboxImplementation, cwd)
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, "io", gerr.Error())
		return
	}
	var gitWriteDirs []string
	if spawnUsesPinnedGitCommonDir(
		srcHarness, cloneSandbox, relaunch.SandboxImplementation) {
		if home, err := os.UserHomeDir(); err == nil {
			gitWriteDirs = harness.GitWorktreeWriteDirs(cwd, codexGitCommonDir, home)
		}
	}
	// Cloning the target in place carries only authority the target already
	// holds, so the lifecycle permission is sufficient. A caller-selected cwd
	// is the one scope-changing input: prove it and the repository roots derived
	// from it, while retaining the target's sandbox-profile roots unchanged.
	if !isHumanCloneCaller(caller) && cwdOverride != "" {
		dirs := appendUniqueDirs([]string{cwd}, gitWriteDirs...)
		if len(dirs) > 0 {
			proofed, ok := requireDirWriteProof(w, r, caller, body.WriteProofToken, dirs)
			if !ok {
				return
			}
			if proofed != nil {
				proofToken = strings.TrimSpace(body.WriteProofToken)
				if v := proofed[cwd]; v != "" {
					cwd = v
				}
				for i, dir := range gitWriteDirs {
					if v := proofed[dir]; v != "" {
						gitWriteDirs[i] = v
					}
				}
				proofDirs = make([]string, 0, len(dirs))
				for _, raw := range dirs {
					proofDirs = appendUniqueDirs(proofDirs, proofed[raw])
				}
			}
		}
	}
	if proofToken != "" {
		defer cleanupDirWriteProofMarkers(proofToken, proofDirs)
	}

	// Snapshot group membership up-front — before the rate-limit claim and
	// spawn — so the clone can inherit the source's identity consistently.
	oldGroups, err := db.ListGroupsForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot groups: "+err.Error())
		return
	}
	oldMembers := make([]*db.AgentGroupMember, 0, len(oldGroups))
	for _, g := range oldGroups {
		m, err := db.FindMemberInGroup(g.ID, target)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"snapshot membership: "+err.Error())
			return
		}
		if m != nil {
			oldMembers = append(oldMembers, m)
		}
	}
	var routeHelperGroupIDs []int64
	if runtime.GOOS == "linux" {
		for _, group := range oldGroups {
			enabled, enabledErr := db.IsAgentGroupRouteEnabled(group.ID, PermRoutesPublish, PermRoutesConsume)
			if enabledErr != nil {
				writeError(w, http.StatusInternalServerError, "route_authority", "could not resolve clone route capability: "+enabledErr.Error())
				return
			}
			if enabled {
				routeHelperGroupIDs = append(routeHelperGroupIDs, group.ID)
			}
		}
	}
	oldOwnedIDs, err := db.ListGroupsOwnedBy(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot ownerships: "+err.Error())
		return
	}
	// The clone inherits both memberships and ownerships. Build the union of
	// those destination groups so an owner-only relationship cannot evade a
	// group-local deny. Keep oldGroups unchanged because membership copy and
	// handoff routing intentionally use membership rows only.
	policyGroups := append([]*db.AgentGroup(nil), oldGroups...)
	seenPolicyGroup := make(map[int64]struct{}, len(policyGroups)+len(oldOwnedIDs))
	for _, g := range policyGroups {
		seenPolicyGroup[g.ID] = struct{}{}
	}
	for _, groupID := range oldOwnedIDs {
		if _, seen := seenPolicyGroup[groupID]; seen {
			continue
		}
		g, err := db.GetAgentGroupByID(groupID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"snapshot owned group: "+err.Error())
			return
		}
		if g != nil {
			policyGroups = append(policyGroups, g)
			seenPolicyGroup[groupID] = struct{}{}
		}
	}
	// A clone is a fresh agent process even though it inherits the target's
	// conversation and identity settings. For an agent caller, require every
	// destination membership's effective group/global cross-harness edge to
	// allow caller-harness → target-harness. An ungrouped clone uses global
	// policy. Human dashboard/CLI clone callers retain their normal bypass.
	policyCaller := caller
	if isHumanCloneCaller(caller) {
		policyCaller = ""
	}
	if fail := spawnHarnessPolicyFailureForGroups(policyGroups, policyCaller, srcHarness); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	// Rate limit: refuse a second clone of the same source within
	// CloneCooldown — but only for agent-initiated clones. The gate
	// exists to bound a runaway *agent*: clone is the only default-
	// granted, agent-reachable fork-doubling verb (self.clone is
	// granted by default; reincarnate is 1-in-1-out, spawn is
	// human-only), so an agent stuck in a tight loop could fork itself
	// unboundedly. A human can't loop at machine speed and clones
	// deliberately, so human-initiated clones — CLI (caller == "") or
	// dashboard (caller == "<human-dashboard>") — skip the cooldown
	// entirely and don't even record a slot; isHumanCloneCaller spans
	// both shapes. Manager *agents* cloning peers via agent.clone still
	// have a real conv-id as caller and stay limited. Atomic at the DB
	// layer so two concurrent claim attempts can't both pass.
	if !isHumanCloneCaller(caller) {
		if err := db.ClaimCloneSlot(target, CloneCooldown, time.Now().UTC()); err != nil {
			if errors.Is(err, db.ErrCloneRateLimited) {
				writeError(w, http.StatusTooManyRequests, "rate_limited",
					"clone of "+short8(target)+" too recent; cooldown is "+CloneCooldown.String()+
						" between consecutive clones of the same source conv")
				return
			}
			writeError(w, http.StatusInternalServerError, "io",
				"clone rate-limit check: "+err.Error())
			return
		}
	}

	// Copy the full permission posture — grant AND deny overrides — so
	// the clone inherits the source's lockdown, not just its grants.
	oldPerms, err := db.ListAgentPermissionOverridesForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot perms: "+err.Error())
		return
	}

	// Resolve the original's display title so the clone's title can be
	// derived as `<base>-c-<N>`. Best-effort — an empty originalTitle
	// just means uniqueCloneTitle falls back to a bare `c-<N>`.
	// A non-CC harness (Codex) keeps its title in its own store
	// (threads.title); read it through the harness ConvStore so the clone
	// inherits the source's name. CC falls through to the conv_index path
	// unchanged.
	//
	// Computed BEFORE the spawn: on the launch-enrollment path the title is a
	// launch argument, so it has to exist before the fork rather than being
	// injected once the pane answers.
	originalTitle := ""
	if t, ok := harnessNativeTitle(target); ok {
		originalTitle = t
	} else if row := agent.FreshConvRowResolved(target); row != nil {
		originalTitle = agent.DisplayTitle(row)
	}
	newTitle := uniqueCloneTitle(originalTitle)
	// Route the handoff through the first group the source belongs to; the
	// clone joins exactly that set below. group_id 0 — a solo clone with no
	// groups — is a direct message, the universal-inbox transport.
	var handoffGroupID int64
	if len(oldMembers) > 0 {
		handoffGroupID = oldMembers[0].GroupID
	}

	// 2. Mint the clone's conv-id (and optionally its jsonl). The
	// branching logic + race-handling lives in cloneSpawnOnce so the
	// groups-clone orchestration can reuse the same code path without
	// duplicating it. The clone is launched with the source agent's durable
	// model + effort; "" falls back to the harness default.
	effort, model := relaunch.Effort, relaunch.Model
	spawned, spawnErr := cloneSpawnOnce(cloneSpawnParams{
		SourceConv:          target,
		Cwd:                 cwd,
		NoCopyConv:          noCopyConv,
		Effort:              effort,
		Model:               model,
		ProofToken:          proofToken,
		ProofCwd:            proofToken != "",
		ProofDirs:           proofDirs,
		CodexGitCommonDir:   codexGitCommonDir,
		GitWriteDirs:        gitWriteDirs,
		Title:               newTitle,
		FollowUp:            followUp,
		HandoffFrom:         caller,
		HandoffGroupID:      handoffGroupID,
		RouteHelperGroupIDs: routeHelperGroupIDs,
	})
	if spawnErr != nil {
		spawnErr.write(w)
		return
	}
	newConv, newTmux, label, warn := spawned.NewConv, spawned.NewTmux, spawned.Label, spawned.Warn
	// Take over the launch claim rather than re-claiming: releasing in
	// cloneSpawnOnce and claiming again here would leave the new row briefly
	// indistinguishable from a plain session, which is the window the claim
	// exists to close. It runs out at the end of this handler, well after
	// EnsureAgentForConv below has made the clone an agent for good.
	defer spawned.ReleaseLaunchClaim()

	// A clone is an agent in its own right. The identity copy below
	// registers it via the group/grant DB hooks when the original had
	// any, but a clone of a bare ungrouped agent would otherwise only
	// get an actor on its first /v1 call — make it explicit so it shows
	// on the roster the moment it spawns.
	// Stable agent-identity (JOH-26): a clone gets its OWN fresh agent_id here —
	// a clone is a FORK (no succession edge links it to the source), and newConv
	// is unlinked, so EnsureAgentForConv mints a new actor rather than inheriting
	// the source's.
	if _, _, err := db.EnsureAgentForConv(newConv, "clone"); err != nil {
		slog.Warn("clone: ensure new actor failed", "conv", newConv, "error", err)
	}
	// A launch-enrolled handoff row was written BEFORE the clone's actor
	// existed, so it landed with to_agent "" and would drop out of the
	// actor-keyed inbox at the next rotation. Re-derive its companions now that
	// EnsureAgentForConv has minted one.
	if spawned.HandoffMsgID > 0 {
		if err := db.RederiveAgentMessageActorRefs(spawned.HandoffMsgID); err != nil {
			slog.Warn("clone: failed to re-derive handoff actor refs",
				"conv", newConv, "msg_id", spawned.HandoffMsgID, "error", err)
		}
	}
	if err := inheritEffectiveSandboxSnapshot(target, newConv); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "persist clone sandbox snapshot: "+err.Error())
		return
	}

	// 3. Copy identity to the new conv. Crucially, this is ADD-only —
	// the original keeps every membership / permission / ownership it
	// had. Best-effort per row; partial failure is recoverable via
	// the CLI.
	granter := "system:clone"
	if caller != target {
		granter = "system:clone:by=" + auditedCaller(caller, perm)
	} else if grantID, _ := db.LookupActiveSudoGrantID(caller, perm); grantID > 0 {
		// Self-clone via sudo: no :by= (it's just the target itself)
		// but still surface the via-sudo annotation so forensics can
		// tie the new conv's grants back to the elevation window.
		granter = fmt.Sprintf("system:clone:via-sudo:grant-id=%d", grantID)
	}
	copied := applyClonedIdentity(newConv, granter, oldMembers, oldPerms, oldOwnedIDs)

	// 4. Settle the follow-up's inbox row, then run post-init.
	//
	// Launch-enrollment path: the row was inserted BEFORE the fork (its id is
	// named by the launch prompt) and the clone's very first turn already
	// carried it — inline, or as the read-it-from-your-inbox pointer. So it is
	// consumed at birth and must never enter the nudge queue: an inlined copy
	// was born delivered AND read, a pointer copy is stamped delivered here,
	// exactly mirroring reincarnate. The name rode the same argv, so there is
	// no /rename to inject and runClonePostInit is skipped entirely — typing
	// either again would double the clone's greeting.
	//
	// Legacy path (Codex, the config revert, or a clone with no follow-up to
	// launch on): enqueue the row and let the single ordered post-init
	// goroutine deliver it — wait-for-alive → /rename → settle gap → flush.
	// The rename and the handoff nudge MUST run in that order inside ONE
	// goroutine. They were once two racing goroutines that both woke when the
	// pane came alive and send-keys'd into it concurrently, so the nudge text
	// landed inside the still-unsubmitted /rename line and baked itself into
	// the clone's title ("worker-c-1[system: new agent message #N for you.
	// ...]"). The settle gap narrows that window; only launch enrollment
	// closes it.
	//
	// A solo (groupless) clone still gets a row either way — group_id 0 is a
	// direct message, the universal-inbox transport. FromConv is the caller
	// (the original for a self-clone, a manager for a cross-clone), so the new
	// clone sees who asked it to pick up work. Renaming likewise happens
	// regardless of group membership: without that startup write a
	// never-messaged clone ends up an orphan, the same trap that bit `tclaude
	// agent spawn` before bc7ec81.
	var msgID int64
	if spawned.LaunchEnrolled {
		msgID = spawned.HandoffMsgID
		if msgID > 0 && !spawned.HandoffInlined {
			if err := db.MarkAgentMessageDelivered(msgID); err != nil {
				slog.Warn("clone: failed to mark launch-delivered handoff",
					"conv", newConv, "msg_id", msgID, "error", err)
			}
		}
		// The handoff itself needs no nudge, but other mail may already be
		// queued to this agent. Take waitForConvAlive's settle sleep first: the
		// spawn poll only established that the PANE exists, which is true the
		// moment tmux new-session returns — i.e. inside the very pre-TUI window
		// this change is about — and any nudge is still send-keys.
		goBackground(func() {
			if !waitForConvAlive(newConv) {
				slog.Warn("clone: new conv never came online; queued mail left for the next drain",
					"conv", newConv)
				return
			}
			enqueueDeliveryForConv(newConv)
		})
	} else {
		if followUp != "" {
			msgID = insertCloneHandoff(handoffGroupID, caller, newConv, followUp, false)
		}
		goBackground(func() {
			runClonePostInit(newConv, newTitle, target, caller)
		})
	}

	// NB: no /exit on the original — that's the whole difference vs
	// reincarnate.

	resp := map[string]any{
		"old_conv":     target,
		"new_conv":     newConv,
		"label":        label,
		"tmux_session": newTmux,
		"copied":       copied,
		"copy_conv":    !noCopyConv,
	}
	if caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	if newTmux != "" && label != "" {
		resp["attach_cmd"] = "tclaude session attach " + label
	} else {
		resp["attach_cmd"] = "tclaude session resume " + newConv
	}
	if followUp != "" {
		resp["follow_up"] = followUp
		// Say which way the follow-up actually reached the clone: a launch-
		// enrolled clone already took it as its first turn, so "queued" would
		// misdescribe a message that is done.
		delivery := "queued as message"
		if spawned.LaunchEnrolled {
			delivery = "launched with the clone as its first turn; saved as message"
		}
		if msgID > 0 {
			resp["message_id"] = msgID
			resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; follow-up %s #%d",
				short8(newConv), short8(target), delivery, msgID)
		} else {
			resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; follow-up will be injected into the new pane once it's ready",
				short8(newConv), short8(target))
		}
	} else {
		resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; both are now running",
			short8(newConv), short8(target))
	}
	if warn != "" {
		resp["warning"] = warn
	}
	writeJSON(w, http.StatusOK, resp)
}

func inheritEffectiveSandboxSnapshot(sourceConv, targetConv string) error {
	return inheritEffectiveSandboxSnapshotForGroup(sourceConv, targetConv, 0)
}

// inheritEffectiveSandboxSnapshotForGroup copies clone launch authority while
// rebinding its resume provenance to the group the clone actually joined. A
// group clone must not retain the source group's ID after membership diverges.
func inheritEffectiveSandboxSnapshotForGroup(sourceConv, targetConv string, resolutionGroupID int64) error {
	var snapshot *sandboxpolicy.Snapshot
	if session, err := db.FindSessionByConvID(targetConv); err != nil {
		return err
	} else if session != nil {
		snapshot = session.EffectiveSandbox
	}
	var err error
	if snapshot == nil {
		snapshot, err = db.AgentEffectiveSandboxConfigForConv(targetConv)
	}
	if err == nil && snapshot == nil {
		snapshot, err = db.AgentEffectiveSandboxConfigForConv(sourceConv)
	}
	if err != nil || snapshot == nil {
		return err
	}
	if resolutionGroupID != 0 {
		rebound := *snapshot
		rebound.ResolutionGroupID = resolutionGroupID
		snapshot = &rebound
	}
	agentID, _, err := db.EnsureAgentForConv(targetConv, "clone")
	if err != nil {
		return err
	}
	return db.SetAgentEffectiveSandboxConfig(agentID, snapshot)
}

// applyClonedIdentity copies a source agent's identity onto newConv: its group
// memberships, permission overrides (grants AND denies), and group ownerships.
// ADD-only — the source keeps everything it had. Best-effort per row (a failure
// is logged, not fatal); returns the descriptors copied (for the response /
// audit). Shared by the human/agent clone orchestration and the same-group
// export clone (JOH-266), which snapshot the source's identity and pass it here.
func applyClonedIdentity(newConv, granter string, members []*db.AgentGroupMember, perms map[string]string, ownedIDs []int64) []string {
	copied := []string{}
	for _, m := range members {
		// Membership rows carry no name of their own — the clone's single name
		// is its title, set by the caller's /rename.
		newMember := &db.AgentGroupMember{
			GroupID: m.GroupID,
			ConvID:  newConv,
			Role:    m.Role,
			Descr:   m.Descr,
		}
		if err := db.AddAgentGroupMember(newMember); err != nil {
			slog.Warn("clone: add new member failed", "group", m.GroupID, "error", err)
			continue
		}
		copied = append(copied, fmt.Sprintf("group:%d", m.GroupID))
	}

	for slug, effect := range perms {
		if err := db.SetAgentPermissionOverride(newConv, slug, effect, granter); err != nil {
			slog.Warn("clone: copy new perm failed", "slug", slug, "effect", effect, "error", err)
			continue
		}
		copied = append(copied, "perm:"+slug)
	}

	for _, gID := range ownedIDs {
		if err := db.AddAgentGroupOwner(gID, newConv, granter); err != nil {
			slog.Warn("clone: add new owner failed", "group", gID, "error", err)
			continue
		}
		copied = append(copied, fmt.Sprintf("owner:%d", gID))
	}
	return copied
}

// runClonePostInit fires asynchronously after a successful clone — as
// the SINGLE post-spawn goroutine, mirroring reincarnate's
// runReincarnatePostSpawn. It waits for the new pane to come online,
// injects /rename to the computed clone title (materialising the .jsonl
// with a meaningful name), then — after a settle gap — flushes any
// pending handoff/inbox nudges. Same purpose as runSpawnPostInit, just
// for the clone path: the original used to silently leave clones
// unrenamed (so they showed up as "(unknown)" with whatever conv-id-
// derived label tmux picked) and unrecoverable when never used.
//
// Why rename → gap → flush in ONE goroutine: the rename and the handoff
// nudge are both send-keys streams into the same pane. Running them
// concurrently (the old two-goroutine layout) let the nudge text land
// inside the still-unsubmitted /rename line, so the clone's title became
// "<base>-c-<N>[system: new agent message #N ...]". Serialising them
// with a settle gap keeps the rename a clean line of its own.
//
// Failures log; never bubble — the clone already succeeded as far as
// the caller is concerned.
func runClonePostInit(newConv, title, target, caller string) {
	if !waitForConvAlive(newConv) {
		slog.Warn("clone: new conv never came online; rename + handoff abandoned", "conv", newConv)
		return
	}
	// Rename first so the clone's CC title shows the proper
	// `<base>-c-<N>` before any handoff output streams. Skip only when
	// the title is empty or fails the rename charset gate.
	if title == "" || !isValidRenameTitle(title) {
		if title != "" {
			slog.Warn("clone: title not a valid rename title; skipping /rename",
				"conv", newConv, "title", title)
		}
	} else {
		if !deliverRename(newConv, title) {
			slog.Warn("clone: rename delivery failed", "conv", newConv, "title", title)
		}
		// Settle gap so CC processes the rename before the handoff
		// nudge's send-keys lands — without it the two keystroke streams
		// interleave into a single /rename line (the
		// "<base>-c-<N>[system: new agent message ...]" title bug).
		time.Sleep(reincarnateReadyDelay)
	}
	// Deliver any pending handoff / inbox nudges now that the rename has
	// settled. The orchestration enqueued the clone-handoff row (when a
	// follow-up was given) before launching this goroutine; the per-agent
	// dispatcher claims + delivers it through the normal nudge pipeline. No
	// synthetic welcome (unlike spawn) — the handoff row, when present, is
	// the clone's first prompt; the /rename alone already materialised the
	// .jsonl.
	enqueueDeliveryForConv(newConv)
}
