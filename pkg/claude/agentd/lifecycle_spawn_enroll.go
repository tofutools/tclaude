package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/startupctx"
)

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
	timing := config.StartupTiming("spawn_backfill", "label", label, "conv", convID)
	defer timing("return")
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
			return
		}
		attachRouteHelperConversation(p.AgentID, convID, p.routeHelperLaunchGeneration)
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
	contextBody := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments, p.ManagedInstructionsBody)
	welcomeInSeed := h.NeedsSpawnSeed() && spawnBriefingFitsLaunch(contextBody, spawnInlineMaxChars())
	briefingInlined := contextBody != "" && welcomeInSeed
	// actorCreated is deliberately dropped here: this path runs post-connect
	// against a LIVE conversation, so even a partially-failed enrollment must
	// never delete the real, running agent's identity.
	spawnContextMsgID, _, fail := enrollSpawnedConv(g, p, convID, briefingInlined)
	if fail != nil {
		return fail
	}
	attachRouteHelperConversation(p.AgentID, convID, p.routeHelperLaunchGeneration)
	helperActive := hasRouteHelperForConversation(p.AgentID, convID)
	if runtime.GOOS == "linux" && g != nil && !helperActive {
		enabled, err := db.IsAgentGroupRouteEnabled(g.ID, PermRoutesPublish, PermRoutesConsume)
		if err == nil && enabled {
			slog.Warn("group route helper is unavailable; relaunch the agent (pending helper enrollment expires after 15 minutes)", "conv", convID, "group", g.Name)
		}
	}
	if hostPublishAllowed(convID) && !helperActive {
		warnHostRouteHelperSkipped(convID, "spawn", "this launch has no eligible pane-local helper; use tclaude’s pane sandbox with private networking and relaunch")
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

	returnHome := false
	if presence, err := db.GetAgentFederationPresence(agentID); err != nil {
		return 0, actorCreated, &spawnFailure{500, "identity", err.Error()}
	} else if presence != nil {
		returnHome = presence.State == "here" && presence.HomeInstance == presence.CurrentInstance && presence.ArrivalOffer != ""
	}
	if returnHome {
		p.PermissionOverrides = nil
		p.IsOwner = false
	}
	// Membership is optional for process-owned v1 agents. Ordinary spawn
	// callers still pass a group and retain the existing fatal membership
	// contract.
	if g != nil && !returnHome {
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
	// The launch layers are taken once, at first spawn. The agent row may
	// already exist (a reserved pending spawn, or the session-start hook
	// created it), so the write is "only if none yet", not "only if this call
	// created the row". Best-effort like the spawn config: an agent without
	// layers relaunches from its stored launch shape, as an agent spawned
	// before they existed does.
	if agentID != "" {
		if _, err := db.SetAgentLaunchLayersIfUnset(agentID, spawnLaunchLayers(p, time.Now())); err != nil {
			slog.Warn("spawn: failed to record launch layers", "agent", agentID, "error", err)
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
		var source *db.PermissionBirthSource
		if src, ok := p.PermissionSources[slug]; ok {
			source = &src
		}
		if err := db.SetAgentBirthPermissionOverride(convID, slug, override.Effect, override.Scope, permissionGranter, source); err != nil {
			slog.Warn("spawn: failed to apply birth permission override",
				"conv", convID, "slug", slug, "effect", override.Effect, "scope", override.Scope, "error", err)
		}
	}

	if db.IsExternalStewardConv(p.SpawnedByConv) {
		if err := writeExternalStewardChildDenies(convID, p.SpawnedByConv); err != nil {
			return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "external_steward", err.Error()}
		}
	}

	// Node-profile defaults are mandatory, unlike ordinary best-effort birth
	// overrides. Persist them before the launch path can start the first turn.
	workerDefaults, workerErr := db.GetFederationWorkerDefaults(agentID)
	if workerErr != nil {
		return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "worker_defaults", workerErr.Error()}
	}
	if !returnHome && workerDefaults != nil && len(workerDefaults.Permissions) > 0 {
		provenance := fmt.Sprintf("peer:%s node-profile:%s id:%s revision:%d", workerDefaults.Peer, workerDefaults.ProfileName, workerDefaults.ProfileID, workerDefaults.Revision)
		for _, slug := range db.SortedOverrideSlugs(workerDefaults.Permissions) {
			override := workerDefaults.Permissions[slug]
			if err := db.SetAgentPermissionOverrideWithScope(convID, slug, override.Effect, override.Scope, provenance); err != nil {
				return 0, actorCreated, &spawnFailure{http.StatusInternalServerError, "worker_defaults", err.Error()}
			}
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
	spawnContext := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments, p.ManagedInstructionsBody)
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
	recordStartupSnapshot(agentID, g, p, spawnContextMsgID)
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
	if restored, err := db.RollbackFederationArrival(convID); err != nil {
		slog.Warn("spawn: stable arrival rollback failed; retaining actor for inspection", "conv", convID, "error", err)
		return
	} else if restored {
		return
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
		if _, err := db.DeleteUnlaunchedAgentByConvID(convID); err != nil {
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
	timing := config.StartupTiming("spawn_post_init", "conv", convID)
	defer timing("return")
	if !waitForConvPaneAlive(convID) {
		slog.Warn("spawn: new conv never came online; post-init injection abandoned",
			"conv", convID)
		return
	}
	if pickAliveSession(convID) == nil {
		slog.Warn("spawn: no alive tmux session for post-init injection", "conv", convID)
		return
	}
	timing("pane_alive")
	h := harnessForConv(convID)
	codexSelected, piSelected, ok := postInitDriveSelection("spawn", convID, h)
	if !ok {
		return
	}

	// Codex's seed-delivered welcome and out-of-band title update need no
	// keystrokes. Its app-server path has an explicit control-readiness wait.
	// Preserve the historical settling delay for other paths, including a
	// legacy Codex welcome that still needs to be typed into the pane.
	skipKeystrokeDelay := piSelected || h.Name == harness.CodexName && (welcomeInSeed || codexSelected)
	if !skipKeystrokeDelay {
		timing("keystroke_delay_begin", "delay_ms", reincarnateReadyDelay.Milliseconds())
		time.Sleep(reincarnateReadyDelay)
		timing("keystroke_delay_complete")
	} else {
		timing("keystroke_delay_skipped")
	}

	if !awaitPostInitDrive("spawn", convID, h, codexSelected, piSelected) {
		return
	}

	timing("control_ready")

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
		if !deliverRenameWhenReady(convID, name) {
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
		case piSelected:
			err = piDriveMessage(convID, welcome, "followUp")
		default:
			err = injectTextWhenReady(convID, welcome)
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

// recordStartupSnapshot stores the spawn-time facts the post-compaction
// re-injection needs (see startupctx.ComposeReinjection). Best-effort: a
// failure only means a later compaction re-injects less, so it is logged
// rather than failing a spawn that has already happened.
func recordStartupSnapshot(agentID string, g *db.AgentGroup, p spawnParams, briefMsgID int64) {
	if agentID == "" {
		return
	}
	spawnedByAgent := p.SpawnedByAgent
	if spawnedByAgent == "" && p.SpawnedByConv != "" {
		spawnedByAgent, _ = db.AgentIDForConv(p.SpawnedByConv)
	}
	// The spawn opted out of the group context exactly when the group had one
	// and it is not what the brief starts with (the mission, role and process
	// blocks that follow are delivered either way). Deriving it keeps every
	// spawn path (including a replayed pending spawn) correct without threading
	// another field.
	standing := strings.TrimSpace(g.GetDefaultContext())
	includeGroupContext := standing == "" || strings.HasPrefix(strings.TrimSpace(p.GroupContext), standing)
	if err := db.UpsertAgentStartupSnapshot(db.AgentStartupSnapshot{
		AgentID:             agentID,
		SpawnGroupID:        spawnGroupID(g),
		SpawnedByAgent:      spawnedByAgent,
		IncludeGroupContext: includeGroupContext,
		ProfileContext:      strings.TrimSpace(p.ProfileContext),
		WorktreePath:        p.WorktreePath,
		WorktreeBranch:      p.WorktreeBranch,
		BriefMessageID:      briefMsgID,
	}); err != nil {
		slog.Warn("spawn: failed to record startup snapshot", "agent", agentID, "error", err)
	}
}

// buildSpawnContextBody assembles the startup briefing body for a
// freshly-spawned agent's inbox. See startupctx.ContextBody, which owns the
// rendering so the post-compaction re-injection cannot drift from it.
func buildSpawnContextBody(groupName, groupContext, profileContext, initialMessage string, attachments []string, managed ...string) string {
	body := startupctx.ContextBody(groupName, groupContext, profileContext, initialMessage, attachments)
	for _, section := range managed {
		if section != "" {
			if body != "" {
				body += "\n\n---\n\n"
			}
			body += section
		}
	}
	return body
}

// buildSpawnAttachmentsSection renders the briefing's "Attached files" block.
func buildSpawnAttachmentsSection(attachments []string) string {
	return startupctx.AttachmentsSection(attachments)
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
	return startupctx.IdentityPrefix(attribution, name, role, descr, groupName, worktreePath, worktreeBranch)
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
