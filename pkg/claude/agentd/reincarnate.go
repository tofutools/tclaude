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

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
//  2. Spawn a fresh tclaude session in the same cwd; poll for new
//     conv-id (mirrors handleGroupSpawn).
//  3. Migrate memberships / permissions / ownerships old → new.
//  4. Optionally enqueue follow-up as an agent_messages row addressed
//     to the new conv. Background goroutine waits for the new pane to
//     come online and runs flush() to deliver via the existing nudge
//     pipeline, including solo agents via direct group_id 0 mail.
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
	runReincarnationOrchestration(w, caller, caller, "", body)
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
	runReincarnationOrchestration(w, targetConv, caller, PermAgentReincarnate, body)
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
func runReincarnationOrchestration(w http.ResponseWriter, target, caller, perm string, body reincarnateBody) {
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
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session; can't reincarnate without a cwd to spawn into (try `tclaude agent groups resume` first if it's offline)")
		return
	}
	cwd, cwdErr := livePaneCwd(oldSess.TmuxSession)
	if cwdErr != nil {
		writeError(w, http.StatusInternalServerError, "io", cwdErr.Error())
		return
	}
	relaunchPolicy, policyErr := resolveResumeSandboxPolicy(target)
	if policyErr != nil {
		writeEffectiveSandboxLoadError(w, &effectiveSandboxChangedError{err: policyErr})
		return
	}
	var effectiveSandbox *sandboxpolicy.Snapshot
	if relaunchPolicy != nil {
		effectiveSandbox = relaunchPolicy.Snapshot
	}

	// 2. Spawn a fresh tclaude session in the same cwd, carrying the
	// predecessor's live model + reasoning effort (the JOH-36 follow-up:
	// the statusline hook persists model_id / effort_level on the
	// session row, so the launch-time flags are now reconstructible).
	// Fail-open: an agent whose statusbar never reported a model spawns
	// with no --model, claude's own default — the historical behaviour.
	effort, model := inheritedLaunchFlags(oldSess.ID)
	label := generateSpawnLabel()
	// Carry the predecessor's armed Remote Access to the successor (JOH-261):
	// a reincarnation is a directed handoff of the same identity, so an agent
	// the operator armed for phone access stays phone-reachable across it.
	// False (and so omitted) for an unarmed source or a Codex predecessor.
	remoteControl := remoteControlForRelaunch(target, oldSess.Harness)
	// Reincarnate under the same harness the predecessor ran on — a Codex
	// agent must come back as Codex, not Claude Code. oldSess.Harness is ""
	// for an untagged/claude row, which omits the flag (the default).
	// Reincarnation is a relaunch, so the experimental auto-review guardian is
	// never re-engaged (autoReview=false) — it is an explicit fresh-spawn opt-in.
	// trustDir=false for the same reason: pre-trusting the cwd edits the user's
	// ~/.codex/config.toml and is only ever an explicit fresh-spawn opt-in.
	reincarnateSandbox := sandboxForHarness(oldSess.Harness)
	if fail := sandboxProfileCapabilityFailure(oldSess.Harness, reincarnateSandbox, effectiveSandbox); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	// Reincarnation refreshes the target's policy through the same resolver as
	// resume. Persist it before launch so the actor's durable snapshot and the
	// successor's launch snapshot move together; every failure before identity
	// rotation restores the predecessor's previous policy and removes any newly
	// materialized private directories.
	persistedAgentID := ""
	if effectiveSandbox != nil {
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
		if removeUnusedDirs && relaunchPolicy != nil && relaunchPolicy.Previous != nil && effectiveSandbox != nil {
			if _, err := removeSupersededMaterializedAgentDirectories(*effectiveSandbox, *relaunchPolicy.Previous); err != nil {
				slog.Warn("reincarnate: remove unused refreshed agent directories failed", "error", err)
			}
		}
	}
	if err := SpawnDetachedTclaudeNew(clcommon.SpawnArgs{
		EffectiveSandbox:       effectiveSandbox,
		Label:                  label,
		Cwd:                    cwd,
		Effort:                 effort,
		Model:                  model,
		Harness:                oldSess.Harness,
		Sandbox:                reincarnateSandbox,
		Approval:               approvalForHarness(oldSess.Harness),
		AskUserQuestionTimeout: askTimeoutForRelaunch(target),
		RemoteControl:          remoteControl,
	}); err != nil {
		rollbackSandbox(true)
		writeError(w, http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: "+err.Error())
		return
	}

	// 3. Poll the sessions table for the new conv-id (the hook
	// callback writes it once CC starts inside tmux).
	deadline := time.Now().Add(reincarnateSpawnTimeout)
	var newConv, newTmux string
	for time.Now().Before(deadline) {
		s, err := db.LoadSession(label)
		if err == nil && s != nil {
			newTmux = s.TmuxSession
			if s.ConvID != "" {
				newConv = s.ConvID
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if newConv == "" {
		tmuxToKill := newTmux
		if tmuxToKill == "" {
			tmuxToKill = label
		}
		if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxToKill)).Run(); err != nil {
			slog.Warn("reincarnate: timed-out successor kill failed", "session", tmuxToKill, "error", err)
		}
		rollbackSandbox(true)
		writeError(w, http.StatusGatewayTimeout, "timeout",
			"spawned session "+label+" but conv-id never materialised within "+
				reincarnateSpawnTimeout.String()+"; the timed-out successor was stopped and the target policy was restored")
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
		granter = "system:reincarnate:by=" + auditedCaller(caller, perm)
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

	// 5. Carry any tmux clients attached to the old session over to
	// the new session BEFORE we /exit the old pane. Without this, the
	// human's terminal gets detached when CC dies and they have to
	// manually `tclaude session attach <label>`. Best-effort — if
	// nobody was attached or the switch fails, the attach_cmd in the
	// response is the fallback.
	switchedClients := switchTmuxClients(oldSess.TmuxSession, newTmux)

	// 6. Compute the two generation titles (JOH-319). The living
	// successor keeps the plain base name; the retiring predecessor gets
	// the `-x` archive marker (`<prev>-x`, or `<prev>-x-<N>` on a repeat).
	// Done here (before /exit on the old pane below) so the lookup of
	// prevTitle still resolves cleanly. FreshConvRowAt scans the parent's
	// .jsonl when conv_index has no row for it — required for back-to-back
	// reincarnations where the parent itself was just spawned and never
	// indexed yet (otherwise prevTitle would be "" and the successor would
	// come up unnamed / the predecessor un-archived).
	// A non-CC harness (Codex) keeps its title in its own store
	// (threads.title), not the conv_index the CC path reads — source it
	// through the harness ConvStore so the carry survives. CC falls through
	// to the existing FreshConvRowAt scan, unchanged.
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

	// 7. Queue the follow-up as an agent_messages row BEFORE the
	// post-spawn goroutine runs — the row is written so the rename can
	// land first and the flush delivery picks the message up next. A
	// solo (groupless) successor still gets a row: group_id 0 is a
	// direct message, the universal-inbox transport. (decodeReincarnate
	// FollowUp guarantees followUp != "".)
	// Route the handoff through the first group the migrated agent now
	// belongs to (post-migration, newConv is the member). group_id 0 —
	// a solo successor with no groups — is a direct message, the
	// universal-inbox transport.
	var handoffGroupID int64
	if groups, err := db.ListGroupsForConv(newConv); err == nil && len(groups) > 0 {
		handoffGroupID = groups[0].ID
	}
	var msgID int64
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

	// 8. Post-spawn injection: wait for alive → /rename → flush the
	// handoff. Single goroutine so ordering is deterministic — without
	// this, the rename and the handoff nudge race and the user briefly
	// sees the wrong title in the new pane.
	goBackground(func() {
		runReincarnatePostSpawn(newConv, successorTitle)
	})

	// 9. Archive-rename the retiring predecessor, then soft-stop it.
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
	// Renaming the predecessor BEFORE the successor's async base-name
	// rename (step 8 runs after wait-for-alive) is also what keeps the
	// base title unambiguous: the predecessor sheds `<base>` for
	// `<base>-x` here, well before the successor claims `<base>`.
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
		_ = injectSoftExit(target, h.Life.SoftExitCommand(), "reincarnate-exit")
	}
	if relaunchPolicy != nil && relaunchPolicy.Previous != nil && effectiveSandbox != nil {
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
	resp["follow_up"] = followUp
	if msgID > 0 {
		resp["message_id"] = msgID
		resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit%s; %s; new pane will be /renamed to %s then receive message #%d",
			short8(target), keptAs, carry, renamedTo, msgID)
	} else {
		resp["note"] = fmt.Sprintf("old %s soft-stopped via /exit%s; %s; new pane will be /renamed to %s; WARNING: the handoff message failed to queue (see daemon logs)",
			short8(target), keptAs, carry, renamedTo)
	}
	writeJSON(w, http.StatusOK, resp)
}

// runReincarnatePostSpawn is the single goroutine that handles
// post-spawn injection in deterministic order: wait-for-alive →
// /rename → flush the handoff. Renaming first means the new pane's CC
// title shows the proper base name (JOH-319: the living successor keeps
// the plain `<base>` name) immediately, before any work output starts
// streaming.
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
	out, err := clcommon.TmuxCommand("list-clients", "-t", clcommon.ExactTarget(oldTmux), "-F", "#{client_tty}").Output()
	if err != nil {
		slog.Warn("reincarnate: list-clients failed; skipping client switch", "tmux", oldTmux, "error", err)
		return 0
	}
	n := 0
	for _, tty := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if tty == "" {
			continue
		}
		if err := clcommon.TmuxCommand("switch-client", "-c", tty, "-t", clcommon.ExactTarget(newTmux)).Run(); err != nil {
			slog.Warn("reincarnate: switch-client failed", "tty", tty, "from", oldTmux, "to", newTmux, "error", err)
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
