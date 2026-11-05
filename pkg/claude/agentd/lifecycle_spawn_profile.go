package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// spawnParams is the fully-resolved, validated input to executeSpawn.
// handleGroupSpawn builds one from the decoded HTTP body; the
// group-template instantiator builds one per template agent spec.
// Every field is already validated by the time it reaches executeSpawn
// — cwd absolute and existing, worktree path resolved, initial_message
// length/charset-checked, reply-to resolved to a conv-id — so the
// shared core does no HTTP-shaped validation of its own.
type spawnParams struct {
	// onLaunchAttempt runs when a one-shot's harness is about to start, the point
	// past which a failure no longer means nothing ran.
	onLaunchAttempt func()
	// UsageProfileIDs are the usage profiles a human chose for this agent;
	// UsageSpawnProfile is the explicitly named spawn profile whose linked
	// usage profiles apply otherwise (see applySpawnUsageProfiles).
	UsageProfileIDs   []int64
	UsageSpawnProfile *db.SpawnProfile
	// spawnCopyCallerTiers are the profiles the caller itself resolved above
	// the default tiers (a template's inline profile, a named profile), and
	// spawnCopyDefaultTiers the group and global defaults applyDefaultProfile
	// consulted. explicitLaunch is what the request set explicitly. All three
	// become the agent's launch layers at first spawn (spawnLaunchLayers).
	// spawnCopyResolved marks that the default tiers were resolved for this
	// launch (spawnCopyDefaultTiers may then be empty, meaning none took part).
	// managedProfile is the named spawn profile of a launch the daemon
	// composes (a trigger or process worker): applyDefaultProfile resolves it
	// as the named tier, above the group and global default profiles.
	managedProfile        *db.SpawnProfile
	spawnCopyCallerTiers  []db.SpawnCopyTier
	spawnCopyDefaultTiers []db.SpawnCopyTier
	spawnCopyResolved     bool
	// defaultProfiles, when set, are the group and global default profiles
	// the caller already resolved from; applyDefaultProfile and the usage
	// binding use them instead of reading the defaults again, so one launch
	// sees one version of each.
	defaultProfiles *pinnedDefaultProfiles
	explicitLaunch  map[string]db.ExplicitLaunchValue
	// relaunchOverrides are the agent's per-agent relaunch-policy rows,
	// resolved at the first spawn (resolveRelaunchOverrides).
	relaunchOverrides []db.RelaunchPolicyOverride
	Arrival           *arrivalContext
	RemoteJob         *federationJobLaunch // trusted internal remote job boundary
	launchAuthority   func() error
	BundleHistory     *bundleHistoryLaunch // trusted internal bundle route only
	// NoLaunch enrolls the agent and installs its history without starting a
	// session: it lands offline and starts on resume. Trusted internal bundle
	// route only, and only with BundleHistory.
	NoLaunch bool
	// AgentID is a stable identity reserved before a pending harness conv-id
	// materialises. Empty on ordinary inline spawns, whose actor is allocated
	// together with the conv-id.
	AgentID string
	// routeHelperLaunchGeneration is daemon-owned intent, recovered from the
	// exact wrapper session row when the pending sweeper resumes enrollment.
	routeHelperLaunchGeneration string
	// EffectiveSandbox is the immutable additive capability snapshot resolved
	// at the trusted caller boundary. Daemon-managed launch paths pass a
	// versioned snapshot even when it is explicitly empty.
	EffectiveSandbox *sandboxpolicy.Snapshot
	Name             string
	// OneShotName is the caller's explicit name for a non-interactive spawn,
	// used as its tmux session name. Empty picks a generated one-shot-<id>.
	OneShotName string
	Role        string
	Descr       string
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
	ManagedPreviousConv         string
	ManagedInstructionResources []db.ManagedInstruction
	ManagedInstructionsRoot     string
	ManagedInstructionsBody     string
	Attachments                 []string
	Cwd                         string // resolved absolute directory
	WorktreePath                string // resolved absolute directory, or ""
	WorktreeBranch              string
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
	Model      string
	ModelProxy string
	// ClaudeSubscription is a peer operator's own Claude subscription token
	// for this worker, held in memory only (claude_subscription.go).
	ClaudeSubscription claudeSubscriptionHandoff
	// Harness is the resolved harness name to launch ("" or "claude" =
	// Claude Code, the default; "codex" = Codex CLI). It forwards to
	// `tclaude session new --harness <h>` and is validated at the spawn
	// boundary (handleGroupSpawn resolves it against the harness registry
	// before building the params).
	Harness string
	// SSHWorkaround is the already-resolved Git-over-SSH compatibility intent.
	// The effective sandbox gates activation for each process generation, while
	// this value is frozen into the durable relaunch profile at enrollment.
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
	// "tclaude-layer" for the tclaude-owned bubblewrap wrapper, or
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
	// bash/glob/grep/lsp/skill block. It forwards as --tools.
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
	// PeerMessaging keeps Claude Code's own cross-session messaging ON for the
	// new agent, forwarding `--peer-messaging` to `tclaude session new`. false
	// (the default, and what an unset profile resolves to) instead has the
	// launch inject the refusal, so agents coordinate through `tclaude agent
	// send` rather than a channel outside tclaude's groups, permissions and
	// audit trail. Gated at the spawn boundary (handleGroupSpawn →
	// harness.ResolvePeerMessaging); a harness with no cross-session messaging
	// system (Codex) rejects a true value.
	PeerMessaging bool
	// launchTogglesResolved says RemoteControl, AutoMemory, PeerMessaging,
	// GroupContext and ContextFeatures were already decided by a caller that
	// resolved them over its own tier stack (the direct spawn boundary, the
	// template deploy), so applyDefaultProfile leaves them alone. Unset,
	// applyDefaultProfile resolves them over the tiers it sees (TCL-1107) and
	// sets this, so the second pass a trigger worker or a deferred continuation
	// makes is a no-op.
	launchTogglesResolved bool
	// IncludeGroupContextDefault is what include_group_context falls back to on
	// the applyDefaultProfile path when no profile tier sets it: on for a
	// trigger or process worker, off (the zero value) for a scribe, matching
	// what each path did before any tier was consulted.
	IncludeGroupContextDefault bool
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
	// PiDrive drives a pi agent through tclaude's pi extension channel instead
	// of tmux send-keys (pi_drive.go). An explicit per-launch opt-in, pi only,
	// frozen into the relaunch profile so a resume keeps the drive.
	PiDrive bool
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

	// externalStewardGated records that handleGroupSpawn's checks for an external
	// steward ran. Every other way to executeSpawn refuses a principal.
	externalStewardGated bool
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
	// PermissionSources names the spawn profile each override came from
	// (launchIdentity.overrideSources), recorded as the row's birth_source.
	// A path that does not resolve identity (the pending-spawn sweeper, the
	// group-template instantiator) leaves it nil, so those rows record none.
	PermissionSources map[string]db.PermissionBirthSource
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
	// Set only by node admission: uncertain launches need durable reservations.
	nodeCapacityReserved bool
	// capacityQueueHead marks the launch of the capacity queue's head entry:
	// the one launch allowed past waiting spawns, and one that always reserves.
	capacityQueueHead bool
	// capacityPreAdmitted marks a launch whose capacity is already counted by a
	// durable reservation (a federation spawn request, an admitting teleport),
	// so it does not wait behind the local spawn queue.
	capacityPreAdmitted bool
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
	// privateOpenCodeStateDiscard transfers the deferred pass's fresh private
	// OpenCode state into executeSpawn, which removes it if the spawn fails
	// before a pane exists. Nil when this spawn did not create the state.
	privateOpenCodeStateDiscard func()
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

// spawnUnconfirmedKind is a launch that was started but not seen to come up.
const spawnUnconfirmedKind = "spawn_unconfirmed"

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
// pinnedDefaultProfiles are the group and global default spawn profiles one
// launch resolved from (either may be nil: none set).
type pinnedDefaultProfiles struct {
	group, global *db.SpawnProfile
}

// launchDefaultProfiles returns the group and global default profiles p's
// launch resolves from: the ones its caller pinned, else the current ones.
func launchDefaultProfiles(g *db.AgentGroup, p *spawnParams) (group, global *db.SpawnProfile) {
	if p.defaultProfiles != nil {
		return p.defaultProfiles.group, p.defaultProfiles.global
	}
	return groupDefaultProfile(g), globalDefaultProfile()
}

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
// Every spawn path reaches this through resolveRemoteControlLaunch (TCL-1107):
// the direct spawn, the template deploy, and — via applyDefaultProfile — the
// scribe, trigger and process workers.
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

// resolveCommonLaunchEnvironment applies the process-environment precedence:
// sandbox-profile values are merged later at process launch, while common
// values resolve as group < global profile < group profile < named/inline
// profile < explicit spawn. profileTiers arrives highest-first, so walk it in
// reverse before applying the explicit tier.
func resolveCommonLaunchEnvironment(g *db.AgentGroup, profileTiers []launchProfileTier, explicit []sandboxpolicy.EnvironmentEntry) ([]sandboxpolicy.EnvironmentEntry, error) {
	environment, _, err := resolveCommonLaunchEnvironmentParts(g, profileTiers, explicit)
	return environment, err
}

// resolveCommonLaunchEnvironmentParts also returns the immutable portion above
// the group tier. Relaunch uses that overlay with the source group's current
// environment, preserving the documented precedence while allowing group
// settings to take effect after stop/start.
func resolveCommonLaunchEnvironmentParts(g *db.AgentGroup, profileTiers []launchProfileTier, explicit []sandboxpolicy.EnvironmentEntry) ([]sandboxpolicy.EnvironmentEntry, []sandboxpolicy.EnvironmentEntry, error) {
	tiers := make([][]sandboxpolicy.EnvironmentEntry, 0, len(profileTiers)+1)
	for i := len(profileTiers) - 1; i >= 0; i-- {
		if profileTiers[i].profile != nil {
			tiers = append(tiers, profileTiers[i].profile.Environment)
		}
	}
	tiers = append(tiers, explicit)
	overrides, err := sandboxpolicy.MergeEnvironment(tiers...)
	if err != nil {
		return nil, nil, err
	}
	if g == nil {
		return overrides, overrides, nil
	}
	environment, err := sandboxpolicy.MergeEnvironment(g.Environment, overrides)
	return environment, overrides, err
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

// resolvePromptRefsLaunchField resolves the saved-prompt set the way role
// refs resolve: the highest tier that names any prompts wins as a whole. The
// group's own default prompts apply when neither the named profile nor the
// group's default profile names any, ahead of the global default profile —
// the same order the dashboard's spawn dialog pre-fills them in.
func resolvePromptRefsLaunchField(body agent.SpawnRequest, g *db.AgentGroup, tiers []launchProfileTier) []string {
	if body.PromptRefsSpecified() {
		return cleanRoleRefs(body.PromptRefs)
	}
	var groupPrompts []string
	if g != nil {
		groupPrompts = cleanRoleRefs(g.DefaultPrompts)
	}
	defaultTiers := 0
	for _, tier := range tiers {
		if tier.defaultTier {
			// The second ambient tier is the global default profile.
			if defaultTiers++; defaultTiers == 2 && len(groupPrompts) > 0 {
				return groupPrompts
			}
		}
		if tier.profile == nil {
			continue
		}
		if refs := cleanRoleRefs(tier.profile.PromptRefs); len(refs) > 0 {
			return refs
		}
	}
	return groupPrompts
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
	return field == "model_proxy" || field == modelField || field == effortField || field == contextWindowMaxField
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
	// The resolution may run more than once for one launch (a managed launch
	// resolves before its sandbox step, then executeSpawn runs it again), and
	// each pass appends its disclosures; keep each note once.
	defer func() { p.LaunchNotes = uniqueNotes(p.LaunchNotes) }()
	// The tiers are the group and global default profiles, beneath the named
	// profile a managed launch (a trigger or process worker) carries in
	// managedProfile. Each carries the provenance wording handleGroupSpawn
	// uses, keeping any skip note readable if it is surfaced on this path.
	groupProfile, globalProfile := launchDefaultProfiles(g, p)
	profiles := []struct {
		profile     *db.SpawnProfile
		source      func(string) string
		defaultTier bool
	}{
		{p.managedProfile, agent.ProvCLIProfileSource, false},
		{groupProfile, agent.ProvGroupProfileSource, true},
		{globalProfile, agent.ProvGlobalProfileSource, true},
	}
	// Captured BEFORE the tier loop can fill it: afterwards there is no way to
	// tell a harness the caller pinned from one a default profile supplied.
	harnessWasPinned := strings.TrimSpace(p.Harness) != ""
	// A caller that already resolved the default tiers (handleGroupSpawn)
	// recorded the profiles it actually launched with; keep those over this
	// later read.
	if !p.spawnCopyResolved {
		p.spawnCopyDefaultTiers = spawnCopyTiers(
			spawnCopyTier(db.SpawnCopyTierGroupDefault, groupProfile),
			spawnCopyTier(db.SpawnCopyTierGlobalDefault, globalProfile))
		p.spawnCopyResolved = true
		// The profile tiers' relaunch rows too (launch slice 11): a trigger,
		// process or template worker has no explicit rows of its own.
		if p.relaunchOverrides == nil {
			p.relaunchOverrides = resolveRelaunchOverrides(nil, []*db.SpawnProfile{p.managedProfile, groupProfile, globalProfile})
		}
	}
	tiers := make([]launchProfileTier, 0, len(profiles))
	for _, tier := range profiles {
		prof := tier.profile
		if prof != nil {
			if fail := profileSpawnFailure(prof, p.SpawnedByConv); fail != nil {
				return fail
			}
			tierSource := profileSource(prof, tier.source)
			tiers = append(tiers, launchProfileTier{
				profile: prof, source: tierSource, defaultTier: tier.defaultTier})
			if strings.TrimSpace(p.Harness) == "" {
				p.Harness = harnessOrDefault(prof.Harness)
				// Attributed where it is decided. The harness does not go through
				// resolveStringLaunchField (it is resolved first, to gate every other
				// field), so this is the only place that knows which tier supplied it.
				p.HarnessSource = tierSource
			}
		}
	}
	if p.EffectiveSandbox != nil && p.EffectiveSandbox.LaunchEnvironment == nil {
		environment, overrides, err := resolveCommonLaunchEnvironmentParts(g, tiers, nil)
		if err != nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_environment", err.Error()}
		}
		p.EffectiveSandbox.LaunchEnvironment = environment
		p.EffectiveSandbox.RefreshGroupEnvironment = true
		p.EffectiveSandbox.LaunchEnvironmentOverrides = overrides
		if g != nil {
			p.EffectiveSandbox.ResolutionGroupID = g.ID
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
	launchValues := launchValueResolution{Values: launchValueFields{
		ModelProxy: p.ModelProxy, Model: p.Model, Effort: p.Effort,
		Sandbox: p.HarnessBuiltinMode, Approval: p.ApprovalPolicy, Tools: p.ToolGovernance,
		AskUserQuestionTimeout: p.AskUserQuestionTimeout, AutoCompactWindow: p.AutoCompactWindow,
		ContextWindowMax: p.ContextWindowMax,
	}}
	if fail := resolveLaunchValueFields(h, tiers, &launchValues, launchValueOptions{}); fail != nil {
		return fail
	}
	resolvedValues := launchValues.Values
	p.ModelProxy, p.Model, p.Effort = resolvedValues.ModelProxy, resolvedValues.Model, resolvedValues.Effort
	p.HarnessBuiltinMode, p.ApprovalPolicy, p.ToolGovernance = resolvedValues.Sandbox, resolvedValues.Approval, resolvedValues.Tools
	p.AskUserQuestionTimeout, p.AutoCompactWindow = resolvedValues.AskUserQuestionTimeout, resolvedValues.AutoCompactWindow
	p.ContextWindowMax = resolvedValues.ContextWindowMax
	p.LaunchNotes = append(p.LaunchNotes, launchValues.Notes()...)
	p.ModelSource = preferResolvedSource(p.ModelSource, launchValues.Source[launchValueModel])
	p.EffortSource = preferResolvedSource(p.EffortSource, launchValues.Source[launchValueEffort])
	p.ContextWindowMaxSource = preferResolvedSource(p.ContextWindowMaxSource, launchValues.Source[launchValueContextWindowMax])
	p.HarnessBuiltinModeSource = preferResolvedSource(p.HarnessBuiltinModeSource, launchValues.Source[launchValueSandbox])
	if strings.TrimSpace(p.ProfileContext) == "" {
		p.ProfileContext, fieldNote = resolveProfileStartupContext(h.Name, tiers)
		noteLaunch()
	}
	p.SandboxImplementation, fieldSource, fieldNote, fail = resolveSandboxImplementationLaunchField(
		h, p.SandboxImplementation, tiers)
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
	if fail := withInstallDefaultRemedy(
		sandboxImplementationHostFailure(h.Name, p.SandboxImplementation), p.SandboxImplementationSource); fail != nil {
		return fail
	}
	if fail := withInstallDefaultRemedy(
		sandboxImplementationPostureFailure(h.Name, p.SandboxImplementation), p.SandboxImplementationSource); fail != nil {
		return fail
	}
	// The shared bool fields ride launchBoolFieldSpecs. An already-true value
	// counts as requested for the four opt-in toggles, as it always has here;
	// fast mode and the SSH workaround go by their Set bits alone.
	var launchBools launchBoolResolution
	launchBools.Value[launchBoolAutoReview], launchBools.Set[launchBoolAutoReview] = p.AutoReview, p.AutoReviewSet || p.AutoReview
	launchBools.Value[launchBoolTrustDir], launchBools.Set[launchBoolTrustDir] = p.TrustDir, p.TrustDirSet || p.TrustDir
	launchBools.Value[launchBoolCopilotAPI], launchBools.Set[launchBoolCopilotAPI] = p.CopilotAPI, p.CopilotAPISet || p.CopilotAPI
	launchBools.Value[launchBoolCodexAppServer], launchBools.Set[launchBoolCodexAppServer] = p.CodexAppServer, p.CodexAppServerSet || p.CodexAppServer
	launchBools.Value[launchBoolFastMode], launchBools.Set[launchBoolFastMode] = p.FastMode, p.FastModeSet
	launchBools.Value[launchBoolSSHWorkaround], launchBools.Set[launchBoolSSHWorkaround] = p.SSHWorkaround, p.SSHWorkaroundSet
	boolNotes, fail := resolveLaunchBoolFields(h, tiers, &launchBools,
		launchBoolAutoReview, launchBoolTrustDir, launchBoolCopilotAPI, launchBoolCodexAppServer,
		launchBoolFastMode, launchBoolSSHWorkaround)
	if fail != nil {
		return fail
	}
	p.LaunchNotes = append(p.LaunchNotes, boolNotes...)
	p.AutoReview, p.AutoReviewSet = launchBools.Value[launchBoolAutoReview], launchBools.Set[launchBoolAutoReview]
	p.TrustDir, p.TrustDirSet = launchBools.Value[launchBoolTrustDir], launchBools.Set[launchBoolTrustDir]
	p.CopilotAPI, p.CopilotAPISet = launchBools.Value[launchBoolCopilotAPI], launchBools.Set[launchBoolCopilotAPI]
	p.CopilotAPISource = preferResolvedSource(p.CopilotAPISource, launchBools.Source[launchBoolCopilotAPI])
	p.CodexAppServer, p.CodexAppServerSet = launchBools.Value[launchBoolCodexAppServer], launchBools.Set[launchBoolCodexAppServer]
	p.CodexAppServerSource = preferResolvedSource(p.CodexAppServerSource, launchBools.Source[launchBoolCodexAppServer])
	p.FastMode, p.FastModeSet = launchBools.Value[launchBoolFastMode], launchBools.Set[launchBoolFastMode]
	p.FastModeSource = preferResolvedSource(p.FastModeSource, launchBools.Source[launchBoolFastMode])
	p.SSHWorkaround, p.SSHWorkaroundSet = launchBools.Value[launchBoolSSHWorkaround], launchBools.Set[launchBoolSSHWorkaround]
	p.SSHWorkaroundSource = preferResolvedSource(p.SSHWorkaroundSource, launchBools.Source[launchBoolSSHWorkaround])
	if fail := applyDefaultLaunchToggles(g, h, tiers, p); fail != nil {
		return fail
	}
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

// applyDefaultLaunchToggles resolves remote_control, auto_memory,
// peer_messaging and include_group_context for a caller that reached
// executeSpawn without resolving them itself — a scribe, a trigger worker, a
// process worker — over the same resolver the direct spawn uses (TCL-1107):
// group remote-control policy > profile tiers > off, and for the group context
// profile tiers > the caller's IncludeGroupContextDefault. A value already
// true counts as requested, as it does for the other opt-in toggles here.
func applyDefaultLaunchToggles(g *db.AgentGroup, h *harness.Harness, tiers []launchProfileTier, p *spawnParams) *spawnFailure {
	if p.launchTogglesResolved {
		return nil
	}
	var groupPolicy *bool
	if g != nil {
		groupPolicy = g.RemoteControl
	}
	var explicitRC *bool
	if p.RemoteControl {
		explicitRC = &p.RemoteControl
	}
	remoteControl, note, fail := resolveRemoteControlLaunch(h, tiers, groupPolicy, explicitRC)
	if note != "" {
		p.LaunchNotes = append(p.LaunchNotes, note)
	}
	if fail != nil {
		return fail
	}
	var launchBools launchBoolResolution
	launchBools.Value[launchBoolAutoMemory], launchBools.Set[launchBoolAutoMemory] = p.AutoMemory, p.AutoMemory
	launchBools.Value[launchBoolPeerMessaging], launchBools.Set[launchBoolPeerMessaging] = p.PeerMessaging, p.PeerMessaging
	notes, fail := resolveLaunchBoolFields(h, tiers, &launchBools,
		launchBoolAutoMemory, launchBoolPeerMessaging, launchBoolIncludeGroupContext)
	p.LaunchNotes = append(p.LaunchNotes, notes...)
	if fail != nil {
		return fail
	}
	p.RemoteControl = remoteControl
	p.AutoMemory = launchBools.Value[launchBoolAutoMemory]
	p.PeerMessaging = launchBools.Value[launchBoolPeerMessaging]
	includeGroupContext := p.IncludeGroupContextDefault
	if launchBools.Set[launchBoolIncludeGroupContext] {
		includeGroupContext = launchBools.Value[launchBoolIncludeGroupContext]
		p.LaunchNotes = append(p.LaunchNotes, fmt.Sprintf("%s include_group_context = %v",
			launchBools.Source[launchBoolIncludeGroupContext], includeGroupContext))
	}
	p.GroupContext = groupStartupContext(g, includeGroupContext)
	if p.ContextFeatures == nil {
		features, cfNote, fail := resolveContextFeaturesLaunchField(nil, false, h, tiers)
		if cfNote != "" {
			p.LaunchNotes = append(p.LaunchNotes, cfNote)
		}
		if fail != nil {
			return fail
		}
		p.ContextFeatures = features
	}
	p.launchTogglesResolved = true
	return nil
}
