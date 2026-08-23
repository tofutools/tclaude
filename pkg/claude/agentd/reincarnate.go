package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
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

// reincarnateSuffixRegex matches a trailing reincarnation suffix in
// either the short form `-r-<digits>` or the long form
// `-reincarnate-<digits>`. Pre-JOH-319 this was the LIVING successor's
// marker; now reincarnateBase uses it to strip such a suffix off an
// old-scheme living name (`worker-r-6`) during the changeover so the
// successor falls back to the plain base `worker`. cloneSuffixRegex is
// the still-live `-c-<N>` sibling for clones.
var reincarnateSuffixRegex = regexp.MustCompile(`^(.*?)-(?:r|reincarnate)-\d+$`)

// reincarnateBase strips any trailing `-r-<digits>` / `-reincarnate-<digits>`
// suffix from a title and returns the stable base name. Post-JOH-319 the
// living generation keeps this base name (the `-r-<N>` was the OLD
// successor marker — see the doc on runReincarnationOrchestration). A
// title with no such suffix is returned unchanged.
//
// It is the successor's title: a steady-state living name (`worker`)
// passes through untouched, while a legacy old-scheme living name from
// the changeover window (`worker-r-6`) sheds its suffix back to `worker`
// rather than carrying it forward.
func reincarnateBase(title string) string {
	if m := reincarnateSuffixRegex.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return title
}

// retiredGenerationTitle computes the title to stamp on a reincarnation's
// RETIRING predecessor, plus whether a rename should happen at all.
//
// The archive convention is unchanged from before JOH-319: the
// predecessor's current title gets the `-x` archive marker appended.
// Post-JOH-320 the `-x` is a pure DISPLAY convention — `conv ls` decides
// visibility from the conv_index.archived_at column the orchestrator stamps
// alongside this rename, not from the suffix. What changed in JOH-319 is only
// that the living successor no longer carries an
// incrementing `-r-<N>` — it keeps its plain base name — so every
// retirement of `worker` now arrives at `worker-x` instead of a distinct
// `worker-r-<N>-x`. uniqueArchiveTitle therefore appends a `-<N>` counter
// when an earlier retired generation already holds the bare `-x` form:
//
//	reincarnation #1 retires:  worker      -> worker-x
//	reincarnation #2 retires:  worker      -> worker-x-2
//	reincarnation #3 retires:  worker      -> worker-x-3
//
// A legacy old-scheme predecessor (`worker-r-6`, seen only during the
// changeover) keeps its full title and just gains `-x` -> `worker-r-6-x`,
// byte-identical to the pre-JOH-319 naming.
//
// An empty title yields ok=false (nothing to mark). A title that already
// ends in `-x` is unusual for a LIVING gen — `-x` is the archive marker —
// but still gets archived (`project-x` -> `project-x-x`): the successor
// keeps the un-suffixed base name, so appending `-x` here always yields a
// title distinct from the successor's, never a collision.
func retiredGenerationTitle(prevTitle string) (title string, ok bool) {
	if prevTitle == "" {
		return "", false
	}
	return uniqueArchiveTitle(prevTitle), true
}

// uniqueArchiveTitle returns `<prevTitle>-x`, or — when that exact title
// is already taken by an earlier retired generation — the smallest free
// `<prevTitle>-x-<N>` (N >= 2). The bare `-x` form is kept for the first
// retirement (the historical convention); the counter only appears on
// repeat retirements of the same base, which now happen on every
// reincarnation because the living generation keeps its base name.
func uniqueArchiveTitle(prevTitle string) string {
	first := prevTitle + "-x"
	taken := customTitlesInUse()
	if !taken[first] {
		return first
	}
	for n := 2; ; n++ {
		cand := first + "-" + strconv.Itoa(n)
		if !taken[cand] {
			return cand
		}
	}
}

// customTitlesInUse returns the set of every non-empty conv_index
// custom_title. Used by uniqueArchiveTitle to find a free archive name.
// A lookup error yields an empty set (fail-open): a collision then keeps
// the bare `-x` form, no worse than the pre-JOH-319 behaviour.
func customTitlesInUse() map[string]bool {
	inUse := map[string]bool{}
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return inUse
	}
	for _, r := range rows {
		if r.CustomTitle != "" {
			inUse[r.CustomTitle] = true
		}
	}
	return inUse
}

// waitForConvAlive polls for newConv's tmux pane to come online,
// then sleeps reincarnateReadyDelay so CC's TUI is ready to accept
// keystrokes. Returns true if the pane became alive within
// reincarnateAliveTimeout, false otherwise.
func waitForConvAlive(newConv string) bool {
	deadline := time.Now().Add(reincarnateAliveTimeout)
	for time.Now().Before(deadline) {
		if isConvOnline(newConv) {
			time.Sleep(reincarnateReadyDelay)
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// `tclaude agent reincarnate` — replace the calling agent with a fresh
// CC instance that inherits its identity (groups, per-conv permission
// grants, group ownerships) and picks up a follow-up prompt (REQUIRED)
// as its first turn. The follow-up is required because the new pane
// comes up with a clean context window and would otherwise sit idle;
// when the caller has no concrete next directive, the convention is to
// pass a short summary of the previous "life" (what was happening,
// where the relevant files are) so the successor has something to
// start from.
//
// Why not just inject /clear? CC's /clear rotates the conv-id, which
// orphans every row in the agentd DB that's keyed on it: group
// memberships, granted permissions, ownerships. The agent comes back
// stripped of identity. Reincarnate does the orchestration to migrate
// that state onto the new conv-id atomically (best-effort transaction;
// see "what can go wrong" notes inline).
//
// Sequence:
//  1. Snapshot old conv state from SQLite + sessions table.
//  2. Resolve the successor / archive titles, then spawn a fresh tclaude
//     session in the same cwd. For a harness that supports launch
//     enrollment (Claude Code) the successor's conv-id is minted here and
//     its title + handoff ride in as launch args; otherwise (Codex) the
//     conv-id is polled for, as before (mirrors handleGroupSpawn).
//  3. Migrate memberships / permissions / ownerships old → new.
//  4. Settle the handoff's agent_messages row addressed to the new conv.
//     Launch-enrolled it was inserted pre-fork and is consumed at birth;
//     on the legacy path a background goroutine waits for the new pane to
//     come online, renames it, and runs flush() to deliver via the existing
//     nudge pipeline, including solo agents via direct group_id 0 mail.
//  5. Soft-stop the old pane via /exit.
//
// Identity is preserved; task state is *not* migrated — the agent is
// expected to persist work-in-progress to disk before calling, per
// the agent-lifecycle skill. Naming (JOH-319): the living successor
// KEEPS the plain base name (`<prev>` with any legacy `-r-<N>` stripped)
// and is renamed to it BEFORE the follow-up so the new pane shows the
// proper title from the start; the RETIRING predecessor gets the
// unchanged `-x` archive marker — `<prev>-x`, or `<prev>-x-<N>` when an
// earlier retired generation already holds the bare form.

// reincarnateSpawnTimeout caps how long we wait for the new tclaude
// session's conv-id to materialise. Mirrors handleGroupSpawn's
// default. A timeout kills the incomplete successor and rolls policy back.
// A var so flow tests can exercise that failure path without waiting 30s.
var reincarnateSpawnTimeout = 30 * time.Second

// reincarnateAliveTimeout caps how long the post-spawn delivery
// goroutine waits for the new pane to be online before giving up on
// proactive delivery. The follow-up message stays in the inbox
// regardless; this is just about whether the nudge fires
// automatically.
//
// Declared as `var` (not `const`) so flow tests can shrink it via
// SetWaitTimingsForTest — otherwise the post-init drain in newFlow's
// cleanup can sit on the full 60s when a test scenario never brings
// the conv online.
var reincarnateAliveTimeout = 60 * time.Second

// reincarnateReadyDelay is how long we sleep after the new pane is
// "alive" before injecting any keys. CC's TUI takes a moment after
// startup before the input box is ready; without this, follow-up
// keystrokes can land mid-render.
//
// Same `var` rationale as reincarnateAliveTimeout above.
var reincarnateReadyDelay = 1 * time.Second

// writeReincarnateOfflineError keeps the CLI and dashboard surfaces on the
// same user-facing contract. Reincarnation is a live-agent handoff: even the
// dashboard's graceful self mode must not leave a dormant instruction queued
// for an offline agent to discover on some later resume.
func writeReincarnateOfflineError(w http.ResponseWriter, target string) {
	writeError(w, http.StatusServiceUnavailable, "no_tmux",
		"cannot reincarnate "+short8(target)+": the agent is offline. "+
			"Reincarnation can only run on a live agent; resume it first with "+
			"`tclaude agent resume "+short8(target)+"`.")
}

// handleWhoamiReincarnate handles POST /v1/whoami/reincarnate (self path).
// A confirmed active agent may always replace itself: reincarnation cannot
// select a different cwd or sandbox policy, so an additional permission gate
// would only prevent an agent from relaunching authority it already holds.
func handleWhoamiReincarnate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if isHuman || caller == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own conversation; humans should manage CC sessions directly, or use POST /v1/agent/{conv}/reincarnate to reincarnate another agent")
		return
	}
	if state, err := db.AgentState(caller); err != nil || state == db.AgentStateRetired {
		writeError(w, http.StatusForbidden, "auth", "caller is not an active agent")
		return
	}
	body, ok := decodeReincarnateBody(w, r)
	if !ok {
		return
	}
	runReincarnationOrchestration(w, caller, caller, "", 0, body, auditRequestEventID(r))
}

// handleAgentReincarnate handles POST /v1/agent/{conv}/reincarnate
// (cross-agent path). Gated on agent.reincarnate OR group-owner-of-target.
// Routed via handleAgentByConv, which has already resolved targetConv.
func handleAgentReincarnate(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentReincarnate, targetConv)
	if !ok {
		return
	}
	body, ok := decodeReincarnateBody(w, r)
	if !ok {
		return
	}
	runReincarnationOrchestration(w, targetConv, caller,
		authorizedPermissionForRequest(r, PermAgentReincarnate), authorizedSudoGrantIDForRequest(r),
		body, auditRequestEventID(r))
}

type reincarnateBody struct {
	FollowUp string `json:"follow_up"`
}

// decodeReincarnateBody parses + validates the REQUIRED follow_up
// body field. Returns (followUp, true) on success; on failure the error
// response is already written and the caller should return. An empty
// or missing follow_up is rejected: the new pane comes up with a clean
// context window and would otherwise sit idle. Callers with no
// concrete next directive should pass a short summary of the previous
// "life" (what was being worked on, where the relevant files are) so
// the successor has something to start from.
func decodeReincarnateBody(w http.ResponseWriter, r *http.Request) (reincarnateBody, bool) {
	var body reincarnateBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return reincarnateBody{}, false
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	if body.FollowUp == "" {
		writeError(w, http.StatusBadRequest, "missing_follow_up",
			"follow_up is required. The new agent comes up with a clean context "+
				"window and would otherwise sit idle. If you have no concrete next "+
				"directive, summarise your previous 'life' (what you were doing, "+
				"where the relevant files are, what's next) so the successor has "+
				"something to start from.")
		return reincarnateBody{}, false
	}
	// Charset/length: validate against the inbox rule. Every handoff —
	// grouped or solo — rides the inbox as an agent_messages row (the
	// universal-inbox transport), so it tolerates the same ≤16384-byte,
	// newline-friendly charset as a spawn --initial-message.
	if !isValidInitialMessage(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			fmt.Sprintf("REJECTED. follow_up must be at most %d characters; newlines "+
				"and tabs are allowed (a grouped successor receives the handoff in "+
				"its inbox, like a spawn brief), but NUL / escape / other control "+
				"characters are not.", agent.MaxInitialMessageBytes))
		return reincarnateBody{}, false
	}
	return body, true
}

// runReincarnationOrchestration is the target-agnostic body shared by
// the self and cross-agent endpoints.
//
//   - target is the conv being reincarnated (its identity migrates onto
//     the new conv-id, its tmux pane is /exit-ed at the end).
//
//   - caller is the conv that triggered the reincarnation (recorded in
//     the audit trail as `system:reincarnate:by=<caller>` for cross-agent,
//     plain `system:reincarnate` when caller == target). It's also the
//     handoff message's FromConv so the new agent sees who asked it to
//     pick up.
//
//   - followUp is an optional first-turn prompt; empty means "just
//     reincarnate, no handoff message".
//
//   - perm is the cross-agent permission slug used by auditedCaller to
//     annotate via-sudo grants in the audit trail. It is empty for self calls,
//     which are unconditionally available to active agents.
//
// Writes the JSON response (or error) directly to w.
func runReincarnationOrchestration(w http.ResponseWriter, target, caller, perm string, sudoGrantID int64, body reincarnateBody, relatedEventID string) {
	launchLock := resumeLaunchLock(target)
	launchLock.Lock()
	defer launchLock.Unlock()
	actor, actorErr := db.GetAgentByConv(target)
	if actorErr != nil {
		writeError(w, http.StatusInternalServerError, "io", "resolve target agent: "+actorErr.Error())
		return
	}
	if actor != nil && actor.CurrentConvID != target {
		writeError(w, http.StatusConflict, "stale_generation",
			"target conversation is no longer the agent's current generation")
		return
	}
	followUp := body.FollowUp
	// 1. Snapshot target conv state. We require an alive tmux session
	// for the target — that's the cwd source and the target of the
	// final /exit injection.
	oldSess := pickAliveSession(target)
	if oldSess == nil {
		writeReincarnateOfflineError(w, target)
		return
	}
	relaunch, relaunchErr := durableRelaunchConfigForConv(target)
	if relaunchErr != nil {
		writeError(w, http.StatusConflict, "relaunch_profile", relaunchErr.Error())
		return
	}
	cwd, cwdErr := livePaneCwd(oldSess.TmuxSession)
	if cwdErr != nil {
		writeError(w, http.StatusInternalServerError, "io", cwdErr.Error())
		return
	}
	label := generateSpawnLabel()
	// The successor's session row and pane exist before RotateAgentConv links
	// its conversation to the actor, so for that window nothing durable says
	// the row is an agent's. Claim it for the whole orchestration, which is
	// where that link lands — see agentLaunchIdentities.
	defer claimAgentLaunchIdentity(label)()
	relaunchPolicy, policyErr := resolveResumeSandboxPolicy(
		target, relaunch.SSHWorkaround, label, relaunch.Harness,
		relaunch.Sandbox, relaunch.activeSandboxImplementation())
	if policyErr != nil {
		writeEffectiveSandboxLoadError(w, &effectiveSandboxChangedError{err: policyErr})
		return
	}
	if relaunchPolicy != nil && relaunchPolicy.Snapshot != nil {
		refreshed, refreshErr := finalizeCodexSSHWorkaroundForRelaunch(
			*relaunchPolicy.Snapshot, relaunchPolicy.SSHWorkaround)
		if refreshErr != nil {
			detail := "prepare SSH workaround: " + refreshErr.Error()
			if cleanupErr := cleanupUncommittedResumeSandboxPolicy(relaunchPolicy); cleanupErr != nil {
				detail += "; remove unused agent-owned directories: " + cleanupErr.Error()
			}
			writeError(w, http.StatusInternalServerError, "spawn",
				detail)
			return
		}
		relaunchPolicy.Snapshot = &refreshed
	}
	var effectiveSandbox *sandboxpolicy.Snapshot
	if relaunchPolicy != nil {
		effectiveSandbox = relaunchPolicy.Snapshot
	}
	stableEffectiveSandbox := effectiveSandbox

	// 2. Spawn a fresh tclaude session in the same cwd, carrying the stable
	// agent's durable model + reasoning effort. An unknown or removed model is
	// omitted so the harness can use its default, preserving historical
	// fail-open behavior for non-authority model selection.
	effort, model := relaunch.Effort, relaunch.Model
	// Carry the predecessor's armed Remote Access to the successor (JOH-261):
	// a reincarnation is a directed handoff of the same identity, so an agent
	// the operator armed for phone access stays phone-reachable across it.
	// False (and so omitted) for an unarmed source or a Codex predecessor.
	remoteControl := relaunch.RemoteControl
	// Reincarnate under the conversation's durable harness — a Codex agent must
	// come back as Codex, not Claude Code.
	// Reincarnation is a relaunch, so the experimental auto-review guardian is
	// never re-engaged (autoReview=false) — it is an explicit fresh-spawn opt-in.
	// trustDir=false for the same reason: pre-trusting the cwd edits a config
	// tclaude does not own (~/.codex/config.toml or ~/.claude.json, depending on
	// the harness) and is only ever an explicit fresh-spawn opt-in.
	// A successor inherits the predecessor's recorded sandbox posture, not the
	// harness default — reincarnation must not weaken the sandbox.
	reincarnateSandbox := relaunch.Sandbox
	reincarnateSandboxImplementation := relaunch.activeSandboxImplementation()
	if relaunch.TemporaryHarnessBuiltinMode {
		effectiveSandbox = temporarySandboxLaunchSnapshot(relaunch.Harness, stableEffectiveSandbox)
	}
	// The successor inherits the posture, so it must also inherit the
	// obligation to verify it. Reincarnation is initiated by the agent itself,
	// with no human at a spawn dialog, which is exactly why it must not be the
	// path that skips a re-check of a harness sandbox tclaude cannot switch off.
	if fail := sandboxImplementationPostureFailure(
		relaunch.Harness, reincarnateSandboxImplementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := sandboxProfileCapabilityFailure(
		relaunch.Harness,
		reincarnateSandbox,
		effectiveSandbox,
		reincarnateSandboxImplementation,
	); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	// The successor inherits the predecessor's drive, so it inherits the
	// requirement that comes with it. Re-asked here rather than trusted from the
	// spawn that admitted the predecessor: the profile can have been narrowed
	// since, and a successor launched into a private network namespace would
	// come back with a channel that can never connect.
	if fail := copilotAPILoopbackFailure(
		relaunch.CopilotAPI, effectiveSandbox, reincarnateSandboxImplementation,
	); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if _, fail := planSandboxProfileAccessForLaunch(
		relaunch.Harness, reincarnateSandbox, effectiveSandbox,
		reincarnateSandboxImplementation,
		session.ModelTransportLaunchContext{Model: model, Cwd: cwd},
		false,
	); fail != nil {
		detail := fail.Msg
		if cleanupErr := cleanupUncommittedResumeSandboxPolicy(relaunchPolicy); cleanupErr != nil {
			detail += "; remove unused agent-owned directories: " + cleanupErr.Error()
		}
		writeError(w, fail.Status, fail.Kind, detail)
		return
	}
	// Reincarnation refreshes the target's policy through the same resolver as
	// resume. Persist it before launch so the actor's durable snapshot and the
	// successor's launch snapshot move together; every failure before identity
	// rotation restores the predecessor's previous policy and removes any newly
	// materialized private directories.
	persistedAgentID := ""
	if effectiveSandbox != nil && !relaunch.TemporaryHarnessBuiltinMode {
		agentID, err := db.AgentIDForConv(target)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "resolve target agent identity: "+err.Error())
			return
		}
		if agentID != "" {
			if err := db.SetAgentEffectiveSandboxConfig(agentID, effectiveSandbox); err != nil {
				writeError(w, http.StatusInternalServerError, "io", "record refreshed sandbox snapshot: "+err.Error())
				return
			}
			persistedAgentID = agentID
		}
	}
	rollbackSandbox := func(removeUnusedDirs bool) {
		if persistedAgentID != "" {
			var previous *sandboxpolicy.Snapshot
			if relaunchPolicy != nil {
				previous = relaunchPolicy.Previous
			}
			if err := db.SetAgentEffectiveSandboxConfig(persistedAgentID, previous); err != nil {
				slog.Warn("reincarnate: restore previous sandbox snapshot failed", "agent", persistedAgentID, "error", err)
			}
		}
		if !relaunch.TemporaryHarnessBuiltinMode && removeUnusedDirs && relaunchPolicy != nil && relaunchPolicy.Previous != nil && effectiveSandbox != nil {
			if _, err := removeSupersededMaterializedAgentDirectories(*effectiveSandbox, *relaunchPolicy.Previous); err != nil {
				slog.Warn("reincarnate: remove unused refreshed agent directories failed", "error", err)
			}
		}
	}
	approval, autoReview := relaunch.Approval, relaunch.AutoReview
	var fastModeAtLaunch *bool
	if relaunch.Harness == harness.CodexName {
		fastModeAtLaunch = codexFastModeAtLaunch(relaunch.FastMode, relaunch.CodexStateRoot)
	}
	spawnArgs := clcommon.SpawnArgs{
		AgentID:               persistedAgentID,
		EffectiveSandbox:      effectiveSandbox,
		Label:                 label,
		Cwd:                   cwd,
		Effort:                effort,
		Model:                 model,
		Harness:               relaunch.Harness,
		Sandbox:               reincarnateSandbox,
		SandboxImplementation: reincarnateSandboxImplementation,
		SandboxChosenBy:       relaunch.HarnessBuiltinModeSource,
		// The successor inherits the predecessor's recorded posture and forks
		// `session new` without -r. No operator control reaches this launch, so a
		// host that cannot provide a recorded boundary must disclose rather than
		// refuse — refusing would leave a context-exhausted agent no way out.
		SandboxContinuation:    true,
		Approval:               approval,
		ToolGovernance:         relaunch.ToolGovernance,
		AutoReview:             autoReview,
		AskUserQuestionTimeout: relaunch.AskUserQuestionTimeout,
		RemoteControl:          remoteControl,
		AutoMemory:             relaunch.AutoMemory,
		ContextFeatures:        relaunch.ContextFeatures,
		AutoCompactWindow:      relaunch.AutoCompactWindow,
		ContextWindowMax:       relaunch.ContextWindowMax,
		CopilotAPI:             relaunch.CopilotAPI,
		CodexAppServer:         relaunch.CodexAppServer,
		CodexStateRoot:         relaunch.CodexStateRoot,
		FastMode:               relaunch.FastMode,
	}

	// 2b. Compute the two generation titles (JOH-319). The living successor
	// keeps the plain base name; the retiring predecessor gets the `-x` archive
	// marker (`<prev>-x`, or `<prev>-x-<N>` on a repeat). Resolved BEFORE the
	// launch for two reasons: the launch-enrollment path below hands the
	// successor's name to the harness as a launch arg, and uniqueArchiveTitle's
	// scan of the titles in use must run while the predecessor is still the only
	// holder of the base name — `claude --name` claims it the moment the
	// successor's pane starts.
	//
	// FreshConvRowAt scans the parent's .jsonl when conv_index has no row for it
	// — required for back-to-back reincarnations where the parent itself was
	// just spawned and never indexed yet (otherwise prevTitle would be "" and
	// the successor would come up unnamed / the predecessor un-archived). A
	// non-CC harness (Codex) keeps its title in its own store (threads.title),
	// not the conv_index the CC path reads — source it through the harness
	// ConvStore so the carry survives.
	prevTitle := ""
	if t, ok := harnessNativeTitle(target); ok {
		prevTitle = t
	} else if row := agent.FreshConvRowAt(target, oldSess.Cwd); row != nil {
		prevTitle = agent.DisplayTitle(row)
	}
	// successorTitle is the stable base name the living generation keeps
	// (any legacy `-r-<N>` on prevTitle is stripped); retiredTitle /
	// retiredRename describe the archive rename of the outgoing pane.
	successorTitle := reincarnateBase(prevTitle)
	retiredTitle, retiredRename := retiredGenerationTitle(prevTitle)

	// 2c. Launch-enrollment path (TCL-731) — always used by Claude Code. The
	// agent.spawn_legacy_injection escape hatch applies only to ordinary spawn;
	// reincarnation cannot safely fall back because it has two ordered inputs
	// (the title and required handoff) that can collide in an unready pane.
	// The successor's conv-id can be PRESET, so its title and its first
	// turn ride in as LAUNCH ARGS (`claude --session-id/--name/[prompt]`) rather
	// than as two tmux send-keys streams into a pane whose input readiness the
	// daemon cannot observe. That is the whole bug: a pre-TUI tty buffers the
	// literal text but drops the Enter keypresses, so `/rename <title>` and the
	// handoff nudge replay as ONE merged line — the successor ends up titled
	// after its own handoff, and the handoff is never delivered as a turn (it
	// was consumed as rename argument text) yet is durably stamped delivered.
	//
	// Harnesses that cannot preset a conv-id (Codex generates it at first turn
	// and renames out-of-band through ConvStore) keep the inject-after-connect
	// flow in runReincarnatePostSpawn below, unchanged.
	successorHarness, _ := harness.Resolve(relaunch.Harness)
	launchEnroll := successorHarness.SupportsLaunchEnrollment()
	var preConvID string
	var handoffMsgID int64
	// handoffInlined records whether the launch prompt baked the whole handoff
	// inline rather than pointing at the inbox copy. When it did, the inbox copy
	// is inserted already delivered AND read — the agent has the text, so it must
	// never enter the nudge queue. Mirrors spawn's briefingInlined.
	var handoffInlined bool
	if launchEnroll {
		preConvID = convops.GenerateUUID()
		// Route the handoff through the first group the agent belongs to. Read
		// off the PREDECESSOR: every identity-bearing table is agent-keyed, so
		// the successor inherits exactly this set when RotateAgentConv links it
		// below — and unlike the successor conv, it already resolves here, before
		// the fork. group_id 0 — a solo agent with no groups — is a direct
		// message, the universal-inbox transport.
		var handoffGroupID int64
		if groups, err := db.ListGroupsForConv(target); err == nil && len(groups) > 0 {
			handoffGroupID = groups[0].ID
		}
		// Read the inline cap ONCE and thread it through both the bookkeeping
		// decision and the prompt build (spawn does the same): config.Load is
		// uncached, so two reads could disagree and leave a row born
		// delivered+read whose launch prompt only pointed at the inbox.
		inlineCap := spawnInlineMaxChars()
		// Same strictness as spawn: an empty body has nothing to inline.
		// decodeReincarnateBody already guarantees followUp != "", so this only
		// ever falls to the pointer form on an over-long handoff.
		handoffInlined = followUp != "" && spawnBriefingFitsLaunch(followUp, inlineCap)
		handoffMsgID = insertReincarnationHandoff(handoffGroupID, caller, preConvID, followUp, handoffInlined)
		spawnArgs.SessionID = preConvID
		// Match the spawn path's title gate: a base name that isn't a valid
		// rename title is not applied as the launch --name (claude records it as
		// the conversation title). RotateAgentConv still carries the display name
		// onto the actor, so the dashboard shows the intended name either way.
		if successorTitle != "" && isValidRenameTitle(successorTitle) {
			spawnArgs.Name = successorTitle
		} else if successorTitle != "" {
			slog.Warn("reincarnate: successor title not a valid rename title; skipping launch --name",
				"conv", preConvID, "title", successorTitle)
		}
		// The prompt names the successor by the title it was ACTUALLY launched
		// with (spawnArgs.Name), not the raw base name — a title the gate above
		// rejected must not be echoed back at the agent as its own identity.
		spawnArgs.InitialPrompt = buildReincarnationLaunchPrompt(spawnArgs.Name,
			reincarnationHandoffAuthor(caller, target), handoffMsgID, followUp, inlineCap)
	}
	// rollbackHandoff removes the pre-fork handoff row when the launch never
	// happened, so a failed reincarnation cannot strand an orphan inbox message
	// addressed to a conv-id that will never exist.
	rollbackHandoff := func() {
		if handoffMsgID <= 0 {
			return
		}
		if _, err := db.DeleteAgentMessagesByIDs([]int64{handoffMsgID}); err != nil {
			slog.Warn("reincarnate: rollback of pre-fork handoff message failed",
				"msg_id", handoffMsgID, "error", err)
		}
		handoffMsgID = 0
	}
	// A wrapper that dies AFTER the fork can't report through the return value
	// (the proofless launch path is fire-and-forget), so it signals here. Unlike
	// spawn, reincarnate is destructive — it archives and /exit-s the
	// predecessor — so mistaking a dead wrapper for a slow one costs the human a
	// live agent. Register before the launch, drain in the poll below.
	wrapperFailure := registerWrapperFailureSignal(label)
	defer unregisterWrapperFailureSignal(label)
	launchFailed := func(err error) {
		rollbackHandoff()
		rollbackSandbox(true)
		writeError(w, http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: "+err.Error())
	}
	if err := SpawnDetachedTclaudeNew(spawnArgs); err != nil {
		launchFailed(err)
		return
	}

	// 3. Wait for the successor to actually be UP.
	//
	// Two things are needed before the predecessor can be decommissioned: the
	// successor's conv-id, and its tmux session name (switchTmuxClients carries
	// the human's attached client onto it before the /exit, and a reincarnation
	// that cannot do that hands the human a dead terminal).
	//
	// Legacy path: poll for the conv-id, which the SessionStart hook writes once
	// the harness is running — so the id doubles as proof it booted.
	//
	// Launch-enrollment path: the conv-id was preset, so it is NOT proof of
	// anything, and neither is the session row. `tclaude session new` writes
	// that row BEFORE it creates the tmux session, and with --session-id the row
	// is born carrying the conv-id — so a successor whose harness dies on
	// startup (expired auth, a broken install, a failing MCP server) would look
	// identical to a healthy one, deterministically. Require the PANE to be
	// alive instead, and take the wrapper-failure signal as the other half:
	// together they cover a wrapper that never got as far as tmux and a harness
	// that exited immediately (its pane closes with it). A harness that dies
	// after this check still slips through — that residual is the same one the
	// hook-based gate always had.
	deadline := time.Now().Add(reincarnateSpawnTimeout)
	var newConv, newTmux string
	for time.Now().Before(deadline) {
		select {
		case werr := <-wrapperFailure:
			launchFailed(werr)
			return
		default:
		}
		s, err := db.LoadSession(label)
		if err == nil && s != nil {
			newTmux = s.TmuxSession
			if launchEnroll {
				if newTmux != "" && isConvOnline(preConvID) {
					// preConvID is authoritative: it is what `claude --session-id`
					// was told to use, so it IS the pane's conversation. The row's
					// own id should agree (the forked `session new` stamps it from
					// the same arg); a disagreement means the label resolved to a
					// row this launch did not write, which is worth a log even
					// though the launch arg still decides.
					if s.ConvID != "" && s.ConvID != preConvID {
						slog.Warn("reincarnate: successor session row carries an unexpected conv-id",
							"label", label, "row_conv", s.ConvID, "launch_conv", preConvID)
					}
					newConv = preConvID
					break
				}
			} else if s.ConvID != "" {
				newConv = s.ConvID
				// Without launch enrollment the harness minted this id, so this
				// is the first moment the successor's launch facts — its recorded
				// drive, its already allocated Copilot API port, and the channel
				// itself — have a conversation to be recorded against.
				// SpawnDetachedTclaudeNew already tried, but on this branch it was
				// handed no id to try with, so its attempt was a no-op.
				//
				// Unreachable today: Copilot is the only harness with the API drive
				// and it supports launch enrollment, so a Copilot reincarnation
				// always takes the branch above. Kept because a path that records
				// half of a launch is exactly the silent half-launch this drive must
				// not have, and going through the one seam means a later harness
				// arriving without launch enrollment cannot reintroduce it.
				//
				// Fresh: a successor is a new conversation, not a continuation of
				// the predecessor's.
				completeCopilotAPILaunch(s.ConvID, copilotAPILaunchFresh, spawnArgs)
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	// One last drain: a wrapper failure reported during the final poll interval
	// must not be read as the slow-pane case the timeout branch below handles.
	select {
	case werr := <-wrapperFailure:
		launchFailed(werr)
		return
	default:
	}
	if newConv == "" {
		tmuxToKill := newTmux
		if tmuxToKill == "" {
			tmuxToKill = label
		}
		if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxToKill)).Run(); err != nil {
			slog.Warn("reincarnate: timed-out successor kill failed", "session", tmuxToKill, "error", err)
		}
		rollbackHandoff()
		rollbackSandbox(true)
		missing := "conv-id"
		if launchEnroll {
			missing = "a live pane"
		}
		writeError(w, http.StatusGatewayTimeout, "timeout",
			"spawned session "+label+" but "+missing+" never materialised within "+
				reincarnateSpawnTimeout.String()+"; the timed-out successor was stopped and the target policy was restored")
		return
	}
	if relaunch.CodexAppServer && !awaitCodexAppServerLaunchReady(newConv, label) {
		stopFailedCodexAppServerLaunch(newConv, label, newTmux)
		rollbackHandoff()
		rollbackSandbox(true)
		writeError(w, http.StatusServiceUnavailable, "codex_app_server_unavailable",
			"the successor's explicitly selected Codex app-server did not become ready; "+
				"the failed successor was stopped and the predecessor remains active")
		return
	}
	// Tag the successor row's best-known remote-control state ON (JOH-261). The
	// --remote-control launch flag already armed the new pane's Remote Access;
	// this records tclaude's best-known state so the dashboard indicator + the
	// toggle direction start armed. The row exists (the poll above read it), so
	// the out-of-band UPDATE lands; keyed by the daemon-chosen label.
	if remoteControl {
		armRemoteControlOnNewRow(label)
	}

	// Audit trail: who triggered this reincarnation (self / cross-agent /
	// via-sudo). The actor model no longer retires a predecessor row to stamp
	// this on, so record it in the daemon log — the same forensic, surfaced
	// alongside the other lifecycle ops.
	granter := "system:reincarnate"
	if caller != target {
		granter = "system:reincarnate:by=" + auditedCallerWithSudoGrant(caller, perm, sudoGrantID)
	}

	// 4. Advance the actor old → new (db.RotateAgentConv): the agent_id never
	// moves — every identity-bearing table is agent-keyed since JOH-26 — so this
	// just links newConv as the fresh head generation, advances the live conv
	// pointer, records the succession edge and carries the display name. Shared
	// with Claude Code's /clear path (issue #192).
	slog.Info("reincarnate: advancing actor to successor conversation",
		"old", target, "new", newConv, "label", label, "granter", granter)
	if _, err := db.RotateAgentConv(target, newConv, "reincarnate"); err != nil {
		// The successor is already running with the refreshed directories, so
		// restore only the predecessor actor's durable snapshot. Removing paths
		// here would pull them out from under the orphan successor.
		rollbackSandbox(false)
		// db.RotateAgentConv is atomic and fail-closed: an error means NOTHING
		// committed (no generation link, no pointer advance, no succession edge),
		// including the case where the actor's pointer could not advance onto the
		// successor. Carrying on from here would decommission the old pane (step
		// 9: /exit + archive) while the new conv has no migrated identity,
		// stranding the agent. Abort the request instead and leave the old pane
		// alive with identity intact. The spawned successor stays around as an
		// orphan tclaude session reachable via `attach_cmd` for manual cleanup.
		slog.Error("reincarnate: actor rotation failed; aborting orchestration",
			"old", target, "new", newConv, "label", label, "error", err)
		writeError(w, http.StatusInternalServerError, "identity_migration",
			"failed to advance agent identity to successor conversation: "+err.Error())
		return
	}
	if relaunch.Harness == harness.CodexName {
		persistCodexFastModeAtLaunch(newConv, fastModeAtLaunch)
	}

	// 5. Carry any tmux clients attached to the old session over to
	// the new session BEFORE we /exit the old pane. Without this, the
	// human's terminal gets detached when CC dies and they have to
	// manually `tclaude session attach <label>`. Best-effort — if
	// nobody was attached or the switch fails, the attach_cmd in the
	// response is the fallback.
	switchedClients := switchTmuxClients(oldSess.TmuxSession, newTmux)

	// 6. Settle the handoff's inbox row.
	//
	// Launch-enrollment path: the row was inserted BEFORE the fork (its id is
	// named by the launch prompt) and the successor's very first turn already
	// carried it — inline, or as the read-it-from-your-inbox pointer. So it is
	// consumed at birth and must never enter the nudge queue: an inlined copy
	// was born delivered AND read, a pointer copy is stamped delivered here,
	// exactly mirroring spawn's markBriefingConsumed. Re-derive the actor
	// companions too: the row predates RotateAgentConv, so it landed with
	// to_agent ” and would drop out of the actor-keyed inbox at the NEXT
	// rotation without this.
	//
	// Post-connect path (Codex): queue the follow-up as an agent_messages row
	// BEFORE the post-spawn goroutine runs — the row is
	// written so the rename can land first and the flush delivery picks the
	// message up next. A solo (groupless) successor still gets a row: group_id 0
	// is a direct message, the universal-inbox transport. (decodeReincarnateBody
	// guarantees followUp != "".) Routed through the first group the migrated
	// agent now belongs to (post-migration, newConv is the member).
	var msgID int64
	if launchEnroll {
		msgID = handoffMsgID
		if msgID > 0 {
			if !handoffInlined {
				if err := db.MarkAgentMessageDelivered(msgID); err != nil {
					slog.Warn("reincarnate: failed to mark launch-delivered handoff",
						"conv", newConv, "msg_id", msgID, "error", err)
				}
			}
			if err := db.RederiveAgentMessageActorRefs(msgID); err != nil {
				slog.Warn("reincarnate: failed to re-derive handoff actor refs",
					"conv", newConv, "msg_id", msgID, "error", err)
			}
		}
		// The handoff itself needs no nudge, but OTHER head-following actor mail
		// may have been queued to this agent across the rotation. The legacy
		// path drains it as the tail of runReincarnatePostSpawn; without this
		// the launch-enrolled successor would wait for its own first `tclaude
		// agent` call or the periodic reaper to pick that mail up.
		//
		// Take waitForConvAlive's settle sleep first. The poll above only
		// established that the PANE exists — which is true the moment tmux
		// new-session returns, i.e. inside the very pre-TUI window this change
		// is about. Any nudge is still send-keys, so firing it here would land
		// buffered text and a dropped Enter, and flushQueue would count it
		// delivered regardless. This is the parity the legacy path got for free.
		goBackground(func() {
			if !waitForConvAlive(newConv) {
				slog.Warn("reincarnate: successor never came online; queued mail left for the next drain",
					"conv", newConv)
				return
			}
			enqueueDeliveryForConv(newConv)
		})
	} else {
		var handoffGroupID int64
		if groups, err := db.ListGroupsForConv(newConv); err == nil && len(groups) > 0 {
			handoffGroupID = groups[0].ID
		}
		if id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:  handoffGroupID,
			FromConv: caller,
			ToConv:   newConv,
			Subject:  db.ReincarnationHandoffSubject,
			Body:     followUp,
		}); err != nil {
			slog.Warn("reincarnate: insert handoff message failed", "error", err)
		} else {
			msgID = id
		}

		// 7. Post-spawn injection: wait for alive → /rename → flush the
		// handoff. Single goroutine so ordering is deterministic — without
		// this, the rename and the handoff nudge race and the user briefly
		// sees the wrong title in the new pane. Skipped entirely on the
		// launch-enrollment path, where both already rode in as launch args and
		// typing them again would double the greeting (TCL-731).
		goBackground(func() {
			runReincarnatePostSpawn(newConv, successorTitle)
		})
	}

	// 8. Archive-rename the retiring predecessor, then soft-stop it.
	//
	// Inject `/rename <prev>-x` (or `<prev>-x-<N>`) into the old pane,
	// writing a custom-title record to the .jsonl before /exit closes
	// the pane. The `-x` is the archive marker so the dead conv shows up
	// as archived in tmux pane titles + tools that read .jsonl directly
	// (e.g. `conv ls` hides it by default). The `-<N>` counter only
	// appears when an earlier retired generation already holds the bare
	// `-x` form — which now happens on every reincarnation, because
	// post-JOH-319 the living successor keeps its plain base name rather
	// than carrying an incrementing `-r-<N>`. The watch model /
	// FreshConvRow refresh picks it up on mtime. retiredGenerationTitle
	// returns ok=false when there is nothing to base a name on (empty
	// title) or the predecessor is already `-x`-marked; rename is skipped.
	//
	// On the legacy path, renaming the predecessor BEFORE the successor's
	// async base-name rename (that goroutine runs after wait-for-alive) is
	// also what keeps the base title unambiguous: the predecessor sheds
	// `<base>` for `<base>-x` here, well before the successor claims
	// `<base>`. The launch-enrollment path cannot order it that way — the
	// successor is named by `claude --name` at launch — so the two titles
	// can overlap for as long as this archive rename takes to land. That is
	// cosmetic: retiredTitle was resolved from the title set as it stood
	// BEFORE the launch (step 2b), so the successor holding `<base>` can
	// never push the predecessor onto a different archive name.
	//
	// The rename failing is non-fatal. The predecessor is now a past
	// GENERATION of the still-active actor (db.RotateAgentConv advanced
	// the actor's live pointer + recorded the succession edge), not a
	// standalone retired or archived entry. It is excluded from the active
	// roster (only the actor's current conv shows), the retired tray (the
	// actor is active) and the plain-conversations list (ListAgentConvIDs
	// covers every generation) — reachable via the succession edge / séance.
	if retiredRename {
		_ = deliverRename(target, retiredTitle)
	}
	// Stamp the durable archived flag on the predecessor's conv_index row
	// (JOH-320). `conv ls` and the watch view now decide visibility from
	// conv_index.archived_at alone — the `-x` rename above is a pure display
	// convention — so the retired generation must carry the column to stay
	// hidden by default. Unconditional: the predecessor is always superseded
	// here, even when retiredRename is false (an untitled predecessor gets no
	// display rename but is still archived). FreshConvRowAt above guaranteed a
	// conv_index row for a CC predecessor; sql.ErrNoRows (e.g. a Codex conv,
	// whose archived state lives in its own thread store) is the expected
	// no-op, not an error worth surfacing.
	if err := db.SetConvIndexArchived(target, true); err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("reincarnate: stamp archived_at on predecessor failed", "conv", short8(target), "error", err)
	}
	// Soft-stop the old pane via the harness's exit command. A harness
	// with no soft-exit command (Lifecycle.SoftExitCommand == "") is
	// left for a hard kill rather than typed a command it can't parse.
	if h := harnessForConv(target); h.SupportsSoftExit() {
		intentSet := setExitIntentBestEffort(oldSess, db.AgentExitActionReincarnate, relatedEventID)
		if !injectSoftExit(target, h.Life.SoftExitCommand(), "reincarnate-exit", intentSet) {
			clearFailedExitIntent(intentSet)
		}
	}
	if !relaunch.TemporaryHarnessBuiltinMode && relaunchPolicy != nil && relaunchPolicy.Previous != nil && effectiveSandbox != nil {
		scheduleReincarnationDirectoryCleanup(target, newConv, *relaunchPolicy.Previous)
	}

	resp := map[string]any{
		"old_conv":         target,
		"new_conv":         newConv,
		"new_title":        successorTitle,
		"retired_title":    retiredTitle,
		"label":            label,
		"tmux_session":     newTmux,
		"attach_cmd":       "tclaude session attach " + label,
		"migrated":         []string{},
		"switched_clients": switchedClients,
	}
	if caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	carry := ""
	switch switchedClients {
	case 0:
		carry = "no tmux client was attached, so the human will need to run attach_cmd manually"
	case 1:
		carry = "human's tmux client carried over to the new session"
	default:
		carry = fmt.Sprintf("%d tmux clients carried over to the new session", switchedClients)
	}
	// Name both ends of the handoff in the note, but only mention the
	// predecessor's archive title when one was actually applied (an
	// untitled predecessor is left as-is, retiredRename == false).
	keptAs := ""
	if retiredRename {
		keptAs = fmt.Sprintf(" (kept as %q)", retiredTitle)
	}
	renamedTo := "the base name"
	if successorTitle != "" {
		renamedTo = fmt.Sprintf("%q", successorTitle)
	}
	// On the launch-enrollment path spawnArgs.Name is the truth: a title the
	// rename charset gate rejected was never passed as --name, so the note must
	// not claim the successor came up under it. Only when there WAS a base name
	// to reject — an untitled predecessor yields no name for an unrelated
	// reason, and blaming the charset gate for it would be a false lead.
	if launchEnroll && successorTitle != "" && spawnArgs.Name == "" {
		renamedTo = "no launch name (the base name failed the rename charset gate)"
	}
	resp["follow_up"] = followUp
	// Describe how the successor actually got its name + first turn: baked into
	// the launch command (launch enrollment) or typed into the pane afterwards
	// (the post-connect path Codex requires).
	named, receives := "will be /renamed to", "then receive"
	if launchEnroll {
		named, receives = "launched named", "with"
	}
	if msgID > 0 {
		resp["message_id"] = msgID
		resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit%s; %s; new pane %s %s %s message #%d",
			short8(target), keptAs, carry, named, renamedTo, receives, msgID)
	} else {
		resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit%s; %s; new pane %s %s; WARNING: the handoff message failed to queue (see daemon logs)",
			short8(target), keptAs, carry, named, renamedTo)
	}
	writeJSON(w, http.StatusOK, resp)
}

// insertReincarnationHandoff writes the successor's handoff into its inbox
// BEFORE the fork, so the launch prompt can name the message id. Returns the
// row id, or 0 when the insert failed — the caller then carries the handoff
// inline-only, which is a degraded but working reincarnation rather than a
// failed one.
//
// inlined says the launch prompt carries the whole handoff verbatim: the inbox
// copy is then archival from birth, so delivered_at + read_at are stamped in the
// INSERT itself. A follow-up UPDATE would leave a window where the online or
// health flush can claim the still-unread row and inject a duplicate nudge —
// the same race spawn's briefingInlined path is written to avoid.
func insertReincarnationHandoff(groupID int64, fromConv, toConv, body string, inlined bool) int64 {
	m := &db.AgentMessage{
		GroupID:  groupID,
		FromConv: fromConv,
		ToConv:   toConv,
		Subject:  db.ReincarnationHandoffSubject,
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
		slog.Warn("reincarnate: insert handoff message failed", "conv", toConv, "error", err)
		return 0
	}
	return id
}

// reincarnationHandoffAuthor resolves who WROTE the handoff, for the successor's
// launch prompt. A self-reincarnation ("" here) needs no attribution — the
// prompt already frames the text as the agent's own previous generation. A
// cross-agent reincarnation names the manager that triggered it, resolved
// through the same display-name path the spawn welcome uses.
func reincarnationHandoffAuthor(caller, target string) string {
	if caller == "" || caller == target {
		return ""
	}
	return resolveSpawnerTitle(caller, "")
}

// buildReincarnationLaunchPrompt builds the positional launch prompt a
// reincarnated Claude Code successor submits as its FIRST turn (TCL-731). It is
// the reincarnation twin of buildSpawnLaunchPrompt: it rides in as a single
// shell-quoted argv positional, so — unlike the send-keys nudge it replaces —
// it can be multi-line and cannot be dropped, merged, or mistimed by a pane
// that is not reading input yet.
//
// The handoff body is inlined right after the [system: ...] orientation when it
// fits inlineMaxChars runes, so the successor acts on its first turn with no
// `tclaude agent inbox read` round-trip. A longer handoff keeps the inbox
// pointer form, where it is scrollable and doesn't balloon the launch command;
// both forms name the inbox copy by id.
//
// msgID <= 0 means the inbox copy could not be written. The prompt then never
// claims a message that doesn't exist, and it inlines the handoff REGARDLESS of
// length: a reincarnation whose successor gets no handoff comes up idle, which
// is the whole failure this path exists to prevent, and the follow-up is capped
// at agent.MaxInitialMessageBytes so it always fits in argv.
func buildReincarnationLaunchPrompt(title, handoffAuthor string, msgID int64, followUp string, inlineMaxChars int) string {
	body := strings.TrimSpace(followUp)
	welcome := "you are a fresh reincarnation"
	if title != "" {
		welcome += " of " + strconv.Quote(title)
	}
	welcome += ": a new instance that inherits the previous generation's identity" +
		" (groups, permissions, ownerships) but none of its context window."
	if handoffAuthor != "" {
		welcome += " The handoff was written by " + handoffAuthor + "."
	}
	welcome += " Use `tclaude agent` commands (whoami / --help / inbox ls) to introspect and coordinate."

	if body == "" {
		welcome += " Your predecessor left no handoff — run `tclaude agent inbox ls`," +
			" then continue its work from there."
		return "[system: " + welcome + "]"
	}
	fits := inlineMaxChars > 0 && utf8.RuneCountInString(body) <= inlineMaxChars
	if msgID > 0 && !fits {
		welcome += fmt.Sprintf(" Your predecessor's handoff is waiting in your inbox as message #%d"+
			" — read it with `tclaude agent inbox read %d`, then act on it.", msgID, msgID)
		return "[system: " + welcome + "]"
	}
	inboxNote := ""
	if msgID > 0 {
		inboxNote = fmt.Sprintf(" (also saved to your inbox as message #%d)", msgID)
	}
	welcome += " Your predecessor's handoff is below" + inboxNote + "; act on it."
	return "[system: " + welcome + "]\n\n" + body
}

// runReincarnatePostSpawn is the single goroutine that handles
// post-spawn injection in deterministic order: wait-for-alive →
// /rename → flush the handoff. Renaming first means the new pane's CC
// title shows the proper base name (JOH-319: the living successor keeps
// the plain `<base>` name) immediately, before any work output starts
// streaming.
//
// This is the post-connect path for harnesses that cannot preset a conv-id
// (currently Codex). A launch-enrolled successor never reaches it: both the
// title and the handoff are launch args there, precisely because this path
// cannot observe whether the pane is reading input yet — a pre-TUI tty buffers
// the literal text and drops the Enter keypresses, merging the two streams into
// one line (TCL-731). The settle gap below narrows that window; it does not
// close it.
//
// The handoff follow-up was already written as an agent_messages row
// before this goroutine fired (group_id 0 for a solo successor); flush
// delivers it through the normal nudge pipeline. Skips rename when
// newTitle == "" — the base name is empty only when the predecessor was
// itself untitled, in which case the successor derives a title from its
// first turn rather than being renamed to a blank.
func runReincarnatePostSpawn(newConv, newTitle string) {
	if !waitForConvAlive(newConv) {
		slog.Warn("reincarnate: new conv never came online; rename + handoff abandoned", "conv", newConv)
		return
	}
	if newTitle != "" {
		if !deliverRename(newConv, newTitle) {
			slog.Warn("reincarnate: rename delivery failed", "conv", newConv, "title", newTitle)
		}
		// Gap so the harness has time to process the rename
		// before the handoff message's nudge lands.
		time.Sleep(reincarnateReadyDelay)
	}
	// newConv is the agent's fresh head generation; route through the
	// per-agent dispatcher so head-following mail queued to the actor
	// (across the rotation) is delivered to it, not just exact-conv mail.
	enqueueDeliveryForConv(newConv)
}

// scheduleReincarnationDirectoryCleanup waits until the predecessor pane has
// actually stopped before deleting directories removed by the refreshed
// profile. It reloads the successor's latest snapshot at cleanup time so a
// subsequent profile change cannot make an old root live again underneath a
// stale cleanup decision.
func scheduleReincarnationDirectoryCleanup(oldConv, newConv string, previous sandboxpolicy.Snapshot) {
	goBackground(func() {
		if !waitForConvOffline(oldConv, retireWorktreeExitGrace) {
			slog.Warn("reincarnate: superseded agent-owned directories kept because predecessor did not exit within grace",
				"conv", oldConv, "grace", retireWorktreeExitGrace)
			return
		}
		current, err := db.AgentEffectiveSandboxConfigForConv(newConv)
		if err != nil || current == nil {
			slog.Warn("reincarnate: superseded agent-owned directories kept because successor policy could not be loaded",
				"conv", newConv, "error", err)
			return
		}
		if _, err := removeSupersededMaterializedAgentDirectories(previous, *current); err != nil {
			slog.Warn("reincarnate: remove superseded agent-owned directories failed", "error", err)
		}
	})
}

// switchTmuxClients moves tmux clients currently attached to oldTmux
// over to newTmux via `tmux switch-client -c <tty> -t <new>`. Returns
// the number of clients successfully switched. Best-effort: per-client
// failures are logged and skipped, since a stale client is harmless
// and the human can always fall back to the attach_cmd in the response.
//
// Run this BEFORE injecting /exit on the old pane — once /exit kills
// CC, the pane closes and any attached client is detached, defeating
// the carry-over.
func switchTmuxClients(oldTmux, newTmux string) int {
	return switchTmuxClientTTYs(tmuxClientTTYs(oldTmux), oldTmux, newTmux)
}

func tmuxClientTTYs(tmuxSession string) []string {
	out, err := clcommon.TmuxCommand("list-clients", "-t", clcommon.ExactTarget(tmuxSession), "-F", "#{client_tty}").Output()
	if err != nil {
		slog.Warn("tmux client handoff: list-clients failed; skipping client switch", "tmux", tmuxSession, "error", err)
		return nil
	}
	var clients []string
	for _, tty := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if tty != "" {
			clients = append(clients, tty)
		}
	}
	return clients
}

func switchTmuxClientTTYs(clients []string, oldTmux, newTmux string) int {
	n := 0
	for _, tty := range clients {
		if err := clcommon.TmuxCommand("switch-client", "-c", tty, "-t", clcommon.ExactTarget(newTmux)).Run(); err != nil {
			slog.Warn("tmux client handoff: switch-client failed", "tty", tty, "from", oldTmux, "to", newTmux, "error", err)
			continue
		}
		n++
	}
	return n
}

// short8 formats a conv-id for human output. Same shape as the
// `short` helper on the agent side; duplicated here so the daemon
// doesn't depend on the agent CLI package.
func short8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
