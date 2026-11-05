package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

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
	if db.IsExternalStewardConv(p.SpawnedByConv) && !p.externalStewardGated {
		return nil, &spawnFailure{Status: http.StatusForbidden, Kind: externalStewardCeilingCode,
			Msg: "an external steward starts workers only through its pinned agent.spawn grant"}
	}
	// Cleanup lifetimes. Only the pre-fork undos (the managed server's cgroup
	// and runtime, the pre-fork enrollment) ride the launchundo stack below. The
	// deferred cleanups registered along the way each have their own lifetime,
	// and their registration order is load-bearing, so they stay as flag-gated
	// defers rather than joining the stack:
	//   - until the fork, on failure: imported history, and the private
	//     OpenCode state discard (registered first so it runs last, after the
	//     conversation is undone); a failure confirmed after the fork still
	//     runs both, through launchFailed;
	//   - until the fork, on any return that did not launch, a successful
	//     offline landing included: the private attachment root and the
	//     agent-directory tree;
	//   - past the fork, until a successful return: the route-helper credential
	//     and a synchronous launch's pending reservation;
	//   - on every return: the managed-instructions overlay (kept when the
	//     launch succeeded or started) and the dir write-proof markers (handed
	//     to the deferred server continuation when there is one).
	privateAttachmentsLaunched := false
	// Registered first: a deferred launch hands its state over before the
	// checks below, and the conversation is undone while its store still exists.
	discardPrivateOpenCodeState := p.privateOpenCodeStateDiscard
	defer func() {
		if failure != nil && !privateAttachmentsLaunched && discardPrivateOpenCodeState != nil {
			discardPrivateOpenCodeState()
		}
	}()
	if p.launchAuthority != nil {
		if err := p.launchAuthority(); err != nil {
			return nil, &spawnFailure{Status: 403, Kind: "teleport_revoked", Msg: err.Error()}
		}
	}
	defer db.NotifyStatusChanged()
	defer func() {
		// Every spawn surface gets its usage profiles here, so a trigger or
		// process worker launched through a spawn profile carries its budget
		// just like a dashboard spawn.
		if outcome != nil {
			groupProfile, globalProfile := launchDefaultProfiles(g, &p)
			applySpawnUsageProfiles(outcome.AgentID, outcome.ConvID, p.UsageProfileIDs,
				p.UsageSpawnProfile, groupProfile, globalProfile)
		}
	}()
	// A usage limit's block_spawn step refuses every fresh spawn it covers,
	// whichever surface started it (dashboard, CLI, template, trigger, waves).
	if failure := checkUsageSpawnBlock(g); failure != nil {
		return nil, failure
	}
	releaseCapacity, capacityFailure := acquireNodeLaunch(&p)
	if capacityFailure != nil {
		return nil, capacityFailure
	}
	defer releaseCapacity()
	timing := config.StartupTiming("spawn", "name", p.Name, "harness", p.Harness, "async", p.Async)
	defer func() {
		timing("return", "failed", failure != nil)
	}()
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
	if fail := normalizeSpawnLaunchPosture(g, &p, groupName); fail != nil {
		return nil, fail
	}
	activeSSHWorkaround, fail := settleSpawnTrust(&p)
	if fail != nil {
		return nil, fail
	}

	// Resolve the harness once for the rest of the spawn. A
	// server-authoritative harness needs its daemon-owned endpoint and
	// server-issued conversation id before enrollment or the pane fork; an
	// empty/unknown --harness yields a nil descriptor and every Supports*/
	// Uses* predicate degrades gracefully.
	spawnHarness, _ := harness.Resolve(p.Harness)

	// An import that lands offline needs history it can resume.
	if p.NoLaunch && p.BundleHistory == nil {
		return nil, &spawnFailure{http.StatusBadRequest, "no_start",
			"only an import whose conversation lands can land offline"}
	}

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
	if p.Async && !p.NoLaunch && spawnHarness.UsesAuthoritativeServer() && p.pendingSpawnLabel == "" {
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
			*p.EffectiveSandbox, label, activeSSHWorkaround)
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
		ManagedInstructionsRoot:    p.ManagedInstructionsRoot,
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
		ModelProxy:                 p.ModelProxy,
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
		PeerMessaging:              p.PeerMessaging,
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
		if routeHelperGeneration != "" && !routeHelperCommitted {
			revokeRouteHelperCredentials(routeHelperConvID, routeHelperGeneration)
		}
	}()

	cleanupManaged, managedFail := prepareManagedInstructions(g, &p)
	if managedFail != nil {
		return nil, managedFail
	}
	spawnArgs.AgentID = p.AgentID
	spawnArgs.ManagedInstructionsRoot = p.ManagedInstructionsRoot
	managedStarted := false
	defer func() { cleanupManaged(failure == nil || managedStarted) }()
	var importedID string
	importedCleanup := func() {}
	importedLaunched := false
	defer func() {
		if failure != nil && !importedLaunched {
			importedCleanup()
		}
	}()
	// undo holds what the launch creates from here to the fork (the managed
	// server's cgroup and runtime, the pre-fork enrollment). Each step is
	// registered once, when its state exists; a return before the fork commits
	// undoes them newest first. Registered after the defers above so it runs
	// before them, as the per-return unwinding it replaces did.
	undo := &launchundo.Undo{}
	defer undo.UnwindUnlessCommitted()
	// installHistory takes the harness because the managed-server step swaps
	// in the agent's own before installing into it.
	installHistory := func(spawnHarness *harness.Harness) *spawnFailure {
		if p.BundleHistory == nil || importedID != "" {
			return nil
		}
		if !spawnHarness.SupportsHistoryTransfer() || spawnHarness.History.Format() != p.BundleHistory.Format {
			return &spawnFailure{http.StatusBadRequest, "history", "resolved harness cannot resume this history format"}
		}
		var err error
		importedID, importedCleanup, err = importBundleHistory(spawnHarness, p.BundleHistory, p.Cwd)
		if err != nil {
			importedCleanup = func() {}
			return &spawnFailure{http.StatusBadRequest, "history", "install imported history: " + err.Error()}
		}
		return nil
	}
	var openCodeLaunch *openCodeLaunch
	if spawnHarness.UsesAuthoritativeServer() {
		var fail *spawnFailure
		openCodeLaunch, spawnHarness, fail = prepareManagedServer(g, &p, &spawnArgs, spawnHarness, label, undo,
			&discardPrivateOpenCodeState, installHistory, &importedID)
		if fail != nil {
			return nil, fail
		}
	}

	launchEnroll, routeEnabled, fail := resolveSpawnLaunchSeam(g, &p, spawnHarness, layeredLaunch, groupName, installHistory, &importedID)
	if fail != nil {
		return nil, fail
	}
	preConvID, preMsgID, briefingInlined, fail := enrollOrSeedSpawn(g, &p, &spawnArgs, spawnHarness,
		groupName, importedID, openCodeLaunch, undo, launchEnroll)
	if fail != nil {
		return nil, fail
	}
	if p.NoLaunch {
		return landSpawnWithoutLaunch(&p, spawnHarness, preConvID, undo)
	}
	routeGeneration, fail := stageRouteHelper(g, &p, &spawnArgs, spawnHarness, label, preConvID,
		routeEnabled, layeredLaunch, launchEnroll)
	if fail != nil {
		return nil, fail
	}
	routeHelperConvID, routeHelperGeneration = preConvID, routeGeneration

	// Final dir write-proof re-assertion, as late as possible before the fork:
	// confirm every proof-verified dir is still exactly the canonical path it
	// was verified as (unchanged and not turned into a symlink). This shrinks
	// the verify→launch TOCTOU window to the microscopic gap between this check
	// and the child inheriting the cwd — a swap performed after verification is
	// caught here rather than launched into. Only proof-verified spawns carry
	// DirWriteProofDirs, so human / exempt / no-override spawns are untouched.
	if fail := reassertDirWriteProof(p.DirWriteProofDirs); fail != nil {
		return nil, fail
	}

	// A launch that may answer before its pane or conv-id exists reserves its
	// stable identity first; see reserveLaunch.
	reservedPending, pendingHeld, fail := reserveLaunch(g, &p, label, preConvID, routeHelperGeneration, launchEnroll)
	if fail != nil {
		return nil, fail
	}

	if reservedPending && !p.Async {
		defer func() {
			// A synchronous caller reports failure, so a reservation must not
			// later enroll a worker that its template/process may already retry.
			if failure != nil {
				deletePendingSpawnRow(label)
			}
		}()
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
		managedStarted = false // confirmed failed launch relinquishes its overlay
		importedCleanup()
		importedCleanup = func() {}
		privateAttachmentCleanup()
		privateAttachmentCleanup = func() {}
		if pendingHeld {
			if deleteErr := db.DeletePendingSpawn(label); deleteErr != nil {
				slog.Warn("spawn: failed to remove reservation after launch failure",
					"label", label, "error", deleteErr)
			}
		}
		// The enrollment ran before the fork and the managed server may be up;
		// a confirmed failure undoes them even after the fork has committed.
		undo.Unwind()
		// A confirmed failure can come after the fork, when the deferred check
		// no longer runs.
		if discardPrivateOpenCodeState != nil {
			discardPrivateOpenCodeState()
			discardPrivateOpenCodeState = nil
		}
		federationSpawnFailed(p.AgentID, "spawn", err.Error())
		return nil, &spawnFailure{http.StatusInternalServerError, "spawn",
			"failed to launch tclaude session new: " + err.Error()}
	}

	wrapperFailure := registerWrapperFailureSignal(label)
	defer unregisterWrapperFailureSignal(label)
	// Bound every row/pane observation below to this fork. A random spawn label
	// is deliberately short and can collide with durable predecessor state;
	// recording the boundary before the child starts lets non-preset harnesses
	// reject such a row, while launch enrollment has the stronger conv-id proof.
	if err := db.SetFederationSpawnLaunchLabel(p.AgentID, label); err != nil {
		return launchFailed(err)
	}
	if err := db.SetFederationBundleLaunchLabel(p.AgentID, label); err != nil {
		return launchFailed(err)
	}
	timing("launch_prepared", "label", label)
	launchedAt := time.Now()
	rememberHTTPProxyLaunchGroup(label, g, p.PermissionOverrides)
	launch, err := prepareLaunchCommand(&p, &spawnArgs, importedID, preConvID)
	if err != nil {
		return launchFailed(err)
	}
	if err := launch(spawnArgs); err != nil {
		return launchFailed(err)
	}
	// Forked: the launched agent owns what undo holds. Only a confirmed
	// failure (launchFailed) still undoes it.
	undo.Commit()
	importedLaunched = true
	managedStarted = true
	timing("session_wrapper_dispatched", "label", label)
	agentDirectoriesLaunched = true
	privateAttachmentsLaunched = true
	if openCodeLaunch != nil {
		if err := sendOpenCodePromptForSpawn(openCodeLaunch, p.Cwd,
			spawnArgs.InitialPrompt, p.Model, p.Effort); err != nil {
			return launchFailed(err)
		}
	}

	// Auto-focus opens the new agent's pane once it is up; see spawnFocus.
	var tmuxSession string
	focus := &spawnFocus{label: label, auto: p.AutoFocus, web: p.AutoFocusWeb}

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
	pollBudget, backgroundBackfillBudget := spawnPollBudgets(p, spawnHarness, timeout)
	polled := pollSpawnedSession(spawnPoll{
		label: label, launchEnroll: launchEnroll, preConvID: preConvID, launchedAt: launchedAt,
		deadline: launchedAt.Add(pollBudget), pendingHeld: pendingHeld, harness: spawnHarness,
		args: spawnArgs, cwd: p.Cwd, wrapperFailure: wrapperFailure, focus: focus, timing: timing,
	})
	if polled.failure != nil {
		return launchFailed(polled.failure)
	}
	convID := polled.convID
	tmuxSession = polled.tmuxSession

	timing("conversation_poll_finished", "label", label, "conv", convID, "budget_ms", pollBudget.Milliseconds())

	// The launch settles one of four ways; see spawnSettle. Each ending says
	// whether the route-helper credential stays with the launch, and the
	// revoke defer above keys on that.
	settle := spawnSettle{g: g, p: p, label: label, preConvID: preConvID, preMsgID: preMsgID,
		briefingInlined: briefingInlined, launchEnroll: launchEnroll, launchedAt: launchedAt,
		pendingHeld: pendingHeld, harness: spawnHarness, args: spawnArgs, focus: focus,
		pollBudget: pollBudget, backgroundBackfillBudget: backgroundBackfillBudget}
	var keepRouteHelper bool
	switch {
	case launchEnroll:
		// Final wrapper-failure check: distinguish a reported wrapper failure
		// from a pane that is merely too slow to confirm. A wrapper that fails
		// after this point is out of signal reach; the unconfirmed path below
		// preserves its enrollment so the operator can inspect or retire it.
		select {
		case werr := <-wrapperFailure:
			return launchFailed(werr)
		default:
		}
		outcome, failure, keepRouteHelper = settle.enrolledLaunch(tmuxSession)
	case convID != "":
		outcome, failure, keepRouteHelper = settle.discoveredConv(convID, tmuxSession)
	case p.Async:
		outcome, failure, keepRouteHelper = settle.pendingAsync(tmuxSession)
	default:
		failure = settle.timedOut()
	}
	if keepRouteHelper {
		routeHelperCommitted = true
	}
	return outcome, failure
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

// spawnSettle settles a forked launch once the poll is over: an enrolled
// launch confirms its pane, a discovered conversation finishes enrollment, an
// async launch goes pending, and a synchronous one times out. Each ending
// returns the outcome or failure and whether the route-helper credential stays
// with the launch.
type spawnSettle struct {
	g                                    *db.AgentGroup
	p                                    spawnParams
	label, preConvID                     string
	preMsgID                             int64
	briefingInlined, launchEnroll        bool
	launchedAt                           time.Time
	pendingHeld                          bool
	harness                              *harness.Harness
	args                                 clcommon.SpawnArgs
	focus                                *spawnFocus
	pollBudget, backgroundBackfillBudget time.Duration
}

// Launch-enrollment path: the conv-id was PRESET and enrollment ran before
// the fork. Return it only after tmux has proved a live pane. A slow or
// missing pane remains enrolled because it may still materialise; rolling
// that state back could strand a live, named, greeted, group-less pane. But
// it is reported as unconfirmed, never as a successful attachable spawn.
//
// An unconfirmed pane keeps its route-helper credential although it is
// reported as a failure: the enrollment is preserved because the child may
// still come up, and that child needs its routes. Revoking it would strand a
// live agent without them.
func (st spawnSettle) enrolledLaunch(tmuxSession string) (*spawnOutcome, *spawnFailure, bool) {
	p, label, preConvID, preMsgID, briefingInlined := st.p, st.label, st.preConvID, st.preMsgID, st.briefingInlined
	launchEnroll, launchedAt, pendingHeld := st.launchEnroll, st.launchedAt, st.pendingHeld
	spawnHarness, spawnArgs, focus := st.harness, st.args, st.focus
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
			s.TmuxSession != "" {
			if session.IsTmuxSessionAlive(s.TmuxSession) ||
				(spawnHarness.UsesCommandInput() && spawnArgs.InitialPrompt != "" &&
					s.Status == session.StatusExited && s.ConvID == preConvID) {
				tmuxSession = s.TmuxSession
			}
		}
	}
	if tmuxSession == "" {
		// The child may merely be slow, so preserve its pre-fork enrollment,
		// briefing, launch-owned directories, and route credentials. But an
		// unobserved pane is not a successful spawn: returning a conv-id here
		// makes fast terminal clients attach to a session that may never exist.
		// Reported unconfirmed, yet the route-helper credential stays: true.
		markBriefingConsumed(preConvID, preMsgID, briefingInlined)
		return nil, &spawnFailure{http.StatusGatewayTimeout, spawnUnconfirmedKind,
			"launch enrollment was preserved, but no live tmux pane appeared before the startup deadline"}, true
	}
	focus.open(tmuxSession)
	markBriefingConsumed(preConvID, preMsgID, briefingInlined)
	// Deferred server-authoritative continuation: atomically replace the
	// pending reservation with the (already-made) actor binding, so the
	// dashboard's Pending row promotes into the enrolled agent. A claim
	// miss is benign — the sweeper saw the session row first and cleared
	// the reservation against the same enrollment; a claim error leaves
	// the row for the sweeper's idempotent already-enrolled path.
	if pendingHeld {
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
	return launchedOutcome(p, label, preConvID, outcomeTmux, focus.mode), nil, true
}

// Conv-id resolved within the poll: finish enrollment inline (Codex, or CC
// with the legacy-injection revert flag) and inject the rename + welcome.
func (st spawnSettle) discoveredConv(convID, tmuxSession string) (*spawnOutcome, *spawnFailure, bool) {
	g, p, label, pendingHeld, focus := st.g, st.p, st.label, st.pendingHeld, st.focus
	if p.CodexAppServer && !awaitCodexAppServerLaunchReady(convID, label) {
		stopFailedCodexAppServerLaunch(convID, label, label)
		return nil, &spawnFailure{http.StatusServiceUnavailable, "codex_app_server_unavailable",
			"Codex app-server was explicitly selected but the verified control handle did not become ready"}, false
	}
	if pendingHeld {
		claimed, err := db.ClaimPendingSpawnAndBindAgent(label, convID, p.AgentID, "spawn")
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "identity",
				"failed to bind reserved agent " + p.AgentID + " to spawned conv " + convID + ": " + err.Error()}, false
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
					"pending spawn " + label + " was canceled or conflicted: " + detail}, false
			}
			return launchedOutcome(p, label, convID, tmuxSession, focus.mode), nil, true
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
			return launchedOutcome(p, label, convID, tmuxSession, focus.mode), nil, true
		}
		return nil, fail, false
	}
	return launchedOutcome(p, label, convID, tmuxSession, focus.mode), nil, true
}

// Conv-id did not materialise within the poll. An Async (dashboard) spawn
// records its full enrollment intent in pending_spawns and returns a
// PENDING outcome (empty conv-id) — the operator can already see + focus
// the pane (auto-focus fired above as soon as it came up) to clear the
// gate, and the sweeper back-fills the enrollment once the conv-id
// appears. Restart-safe: the row carries everything finishSpawnEnrollment
// needs.
func (st spawnSettle) pendingAsync(tmuxSession string) (*spawnOutcome, *spawnFailure, bool) {
	g, p, label, launchedAt, focus := st.g, st.p, st.label, st.launchedAt, st.focus
	spawnHarness, pollBudget, backgroundBackfillBudget := st.harness, st.pollBudget, st.backgroundBackfillBudget
	focus.open(tmuxSession) // belt-and-suspenders: open the pane even if it came up slow
	ps, err := db.GetPendingSpawn(label)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io",
			"failed to verify pending spawn reservation " + label + ": " + err.Error()}, false
	}
	if ps == nil || ps.AgentID != p.AgentID {
		return nil, &spawnFailure{http.StatusConflict, "identity",
			"pending spawn " + label + " was canceled before its response"}, false
	}
	slog.Info("spawn: conv-id not yet materialised; recorded pending spawn",
		"label", label, "group", g.Name, "harness", p.Harness)
	if spawnHarness.NeedsSpawnSeed() && backgroundBackfillBudget > pollBudget {
		goBackground(func() {
			backfillPendingSpawnInline(g, p, label, spawnHarness, launchedAt, backgroundBackfillBudget)
		})
	}
	return launchedOutcome(p, label, "", tmuxSession, focus.mode), nil, true
}

// Synchronous (template or process-performer) path: the caller needs the
// conv-id now, so a timeout is a hard failure — unchanged from before inc2.
func (st spawnSettle) timedOut() *spawnFailure {
	label, pollBudget := st.label, st.pollBudget
	return &spawnFailure{http.StatusGatewayTimeout, "timeout",
		"spawned session " + label + " but conv-id never materialised within " + pollBudget.String() +
			" — the session may still come up; check `tclaude session attach " + label + "`"}
}

// spawnPollBudgets is how long executeSpawn polls for the launched session
// (see pollSpawnedSession), and how long a pending seed-needing launch keeps
// back-filling in the background once the response has gone out.
func spawnPollBudgets(p spawnParams, spawnHarness *harness.Harness, timeout time.Duration) (pollBudget, backgroundBackfillBudget time.Duration) {
	pollBudget = timeout
	if p.Async && asyncSpawnInlineGrace < pollBudget {
		pollBudget = asyncSpawnInlineGrace
	}
	backgroundBackfillBudget = pollBudget
	if p.Async && spawnHarness.NeedsSpawnSeed() && codexAsyncSpawnResponseGrace > 0 && codexAsyncSpawnResponseGrace < pollBudget {
		pollBudget = codexAsyncSpawnResponseGrace
	}
	return pollBudget, backgroundBackfillBudget
}

// spawnPoll is what pollSpawnedSession watches: the forked launch's session
// row, its pane, and its wrapper-failure signal.
type spawnPoll struct {
	label, preConvID     string
	launchEnroll         bool
	launchedAt, deadline time.Time
	pendingHeld          bool
	harness              *harness.Harness
	args                 clcommon.SpawnArgs
	cwd                  string
	wrapperFailure       chan error
	focus                *spawnFocus
	timing               func(string, ...any)
}

// spawnPollResult is the session the poll found: its conv-id once one
// materialised, and its live tmux pane. A non-nil failure is a confirmed
// launch failure (a dead wrapper, or a pane that exited during startup).
type spawnPollResult struct {
	convID, tmuxSession string
	failure             error
}

// pollSpawnedSession polls the sessions table until the launch's conv-id
// materialises or the deadline passes. It confirms the pane is alive before
// counting it, opens auto-focus and arms remote control once it is, marks a
// held reservation launched, discovers a harness's conv-id from its store
// when no hook reports one, and detects a launch that died.
func pollSpawnedSession(w spawnPoll) spawnPollResult {
	label, launchEnroll, preConvID, launchedAt, deadline := w.label, w.launchEnroll, w.preConvID, w.launchedAt, w.deadline
	pendingHeld, spawnHarness, spawnArgs := w.pendingHeld, w.harness, w.args
	wrapperFailure, focus, timing := w.wrapperFailure, w.focus, w.timing
	var tmuxSession string
	var convID string
	paneObserved := false
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
			return spawnPollResult{failure: werr}
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
				// A one-shot shell is expected to make its pane disappear when
				// the command completes. Once the authenticated exit callback has
				// recorded that terminal state, the preset conversation proves the
				// launch happened; command exit (including non-zero) is completion,
				// not a harness-startup failure.
				if spawnHarness.UsesCommandInput() && spawnArgs.InitialPrompt != "" &&
					s.Status == session.StatusExited && s.ConvID != "" {
					tmuxSession = s.TmuxSession
					convID = s.ConvID
					break
				}
				// The shell callback may still be racing this observation. Do not
				// classify its retained, cleanly-finished pane as a failed harness
				// startup; wait for the authenticated terminal-state write above.
				if spawnHarness.UsesCommandInput() && spawnArgs.InitialPrompt != "" {
					sleepSpawnPoll(deadline)
					continue
				}
				// A retained dead pane is definitive startup-failure evidence. Its
				// callback also copies the bounded error tail into the Logs tab before
				// cleanup; fail the spawn response instead of enrolling an offline
				// actor and opening an attach command that can only close.
				if s.TmuxSession != "" {
					if detail, paneID, inspected := startupCorpseExitDetail(s.TmuxSession); inspected {
						// Before launchFailed, which rolls back the enrollment
						// and takes the session row with it.
						logStartupCorpseOutput(label, s.TmuxSession, paneID, detail)
						return spawnPollResult{failure: fmt.Errorf("managed pane exited during startup (%s); see the Logs tab for its output", detail)}
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
					return spawnPollResult{failure: fmt.Errorf("managed pane exited during startup (%s); see the Logs tab for its output", detail)}
				}
				// The authenticated pane callback may already have recorded and
				// cleaned the corpse before this poll observed it.
				if s.Status == session.StatusExited {
					return spawnPollResult{failure: errors.New("managed pane exited during startup; see the Logs tab for its output")}
				}
				sleepSpawnPoll(deadline)
				continue
			}
			if !paneObserved {
				timing("pane_observed", "label", label)
				paneObserved = true
			}
			tmuxSession = s.TmuxSession
			focus.open(tmuxSession) // pane is up — open it now, conv-id or not
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
			if id := discoverSpawnedConvID(spawnHarness, w.cwd, launchedAt, label); id != "" {
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
	return spawnPollResult{convID: convID, tmuxSession: tmuxSession}
}

// reserveLaunch reserves the launch's pending row when it may answer before
// its pane or conv-id exists: capacity-limited launches and async harnesses
// without launch enrollment can return before either materialises. Reserve
// and persist the stable actor identity BEFORE the
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
//
// It returns whether this call reserved the row and whether any reservation
// for the label exists.
func reserveLaunch(g *db.AgentGroup, p *spawnParams, label, preConvID, routeHelperGeneration string,
	launchEnroll bool) (reservedPending, pendingHeld bool, fail *spawnFailure) {
	reservedPending = (p.Async && !launchEnroll || p.nodeCapacityReserved ||
		g != nil && preConvID == "" && routeHelperGeneration != "") && p.pendingSpawnLabel == ""
	pendingHeld = reservedPending || p.pendingSpawnLabel != ""
	if reservedPending {
		if g == nil {
			return false, false, &spawnFailure{http.StatusInternalServerError, "spawn", "ungrouped asynchronous spawn is not supported"}
		}
		if p.AgentID == "" {
			p.AgentID = db.NewAgentID()
		}
		if err := db.InsertPendingSpawn(pendingSpawnFromParams(g, *p, label)); err != nil {
			return false, false, &spawnFailure{http.StatusInternalServerError, "io",
				"failed to reserve pending spawn " + label + ": " + err.Error()}
		}
	}
	return reservedPending, pendingHeld, nil
}

// stageRouteHelper decides whether the launch carries a route helper and, when
// it does, mints its credential. Group routes come in already resolved
// (routeEnabled); host routes are staged here when the harness and sandbox
// allow them. The helper's identity, credential, generation, group set and
// proxy mode land in spawnArgs. It returns the credential's generation (empty
// when no helper rides the launch) for executeSpawn's revoke-on-failure defer.
func stageRouteHelper(g *db.AgentGroup, p *spawnParams, spawnArgs *clcommon.SpawnArgs,
	spawnHarness *harness.Harness, label, preConvID string,
	routeEnabled, layeredLaunch, launchEnroll bool) (generation string, fail *spawnFailure) {
	groupRoutesEnabled := routeEnabled
	hostRoutesEnabled := false
	// A grouped late-enrolled launch cannot resolve its effective grants yet.
	// Stage host discovery speculatively; all publication remains permission-gated.
	// Group membership supplies the durable identity reservation before launch.
	if (preConvID != "" && hostPublishAllowed(preConvID)) ||
		(runtime.GOOS == "linux" && preConvID == "" && g != nil) {
		reason := hostRouteHelperIneligibleReason(spawnHarness.Name, spawnArgs.SandboxImplementation, spawnArgs.EffectiveSandbox)
		if reason == "" && !layeredLaunch {
			reason = "the agent is not launched in tclaude’s pane sandbox"
		}
		if reason == "" && !launchEnroll && runtime.GOOS != "linux" {
			reason = "the launch has no pre-enrolled conversation identity"
		}
		hostRoutesEnabled = reason == ""
		if !hostRoutesEnabled && preConvID != "" {
			warnHostRouteHelperSkipped(preConvID, "spawn", reason)
		}
	}
	if hostRoutesEnabled {
		routeEnabled = true
		if runtime.GOOS == "darwin" {
			spawnArgs.DarwinRouteCapable = true
		}
	}
	if routeEnabled {
		var agentID string
		var agentErr error
		if preConvID != "" {
			agentID, agentErr = db.AgentIDForConv(preConvID)
		} else {
			if p.AgentID == "" {
				p.AgentID = db.NewAgentID()
			}
			agentID = p.AgentID
		}
		if agentErr != nil || strings.TrimSpace(agentID) == "" {
			return "", &spawnFailure{http.StatusInternalServerError, "route_authority", "could not resolve route helper identity"}
		}
		var routeCredential, routeGeneration string
		var credentialErr error
		if preConvID == "" {
			routeCredential, routeGeneration, credentialErr = stageRouteHelperCredential(agentID, label)
		} else {
			routeCredential, routeGeneration, credentialErr = mintRouteHelperCredential(agentID, preConvID)
		}
		if credentialErr != nil {
			return "", &spawnFailure{http.StatusInternalServerError, "route_authority", "could not mint route helper credential: " + credentialErr.Error()}
		}
		spawnArgs.RouteHelperAgentID = agentID
		spawnArgs.RouteHelperConvID = preConvID
		spawnArgs.RouteHelperLaunchGeneration = routeGeneration
		spawnArgs.RouteHelperCredential = routeCredential
		if g != nil && groupRoutesEnabled {
			spawnArgs.RouteHelperGroupIDs = []int64{g.ID}
		}
		if hostRoutesEnabled {
			spawnArgs.RouteHelperGroupIDs = append(spawnArgs.RouteHelperGroupIDs, 0)
		}
		if spawnArgs.DarwinRouteCapable {
			spawnArgs.DarwinRouteAgentID = agentID
		}
		spawnArgs.RouteHelperProxyOnly = runtime.GOOS == "darwin"
		p.routeHelperLaunchGeneration = routeGeneration
		generation = routeGeneration
	}
	return generation, nil
}

// enrollBeforeLaunch enrolls a launch-enrollment spawn before its fork: it
// presets the conversation id (an imported history's, the managed server's,
// or a fresh one), enrolls the agent with its inbox briefing (born consumed
// when it fits inline), registers the enrollment's rollback on undo, and
// fills the launch args: session id, Darwin route identity, --name and the
// launch prompt. It returns the conversation id, the briefing's message id and
// whether the briefing rode the launch prompt inline.
func enrollBeforeLaunch(g *db.AgentGroup, p *spawnParams, spawnArgs *clcommon.SpawnArgs,
	spawnHarness *harness.Harness, groupName, importedID string, openCodeLaunch *openCodeLaunch,
	undo *launchundo.Undo) (preConvID string, preMsgID int64, briefingInlined bool, fail *spawnFailure) {
	if importedID != "" {
		preConvID = importedID
	} else if openCodeLaunch != nil {
		preConvID = openCodeLaunch.ConvID
	} else {
		preConvID = convops.GenerateUUID()
	}
	// Decide the briefing's launch state before inserting its inbox copy.
	// An inlined copy must be born delivered + read in the same INSERT;
	// inserting it unread and fixing it up after launch leaves a window where
	// the online-message flush can claim and inject a redundant nudge.
	spawnContextBody := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments, p.ManagedInstructionsBody)
	inlineCap := spawnInlineMaxChars()
	// An offline landing has no launch prompt to inline into: its briefing
	// stays queued and is delivered when the agent first comes online.
	briefingInlined = !p.NoLaunch && !spawnHarness.UsesCommandInput() && spawnContextBody != "" && spawnBriefingFitsLaunch(spawnContextBody, inlineCap)
	mid, actorCreated, fail := enrollSpawnedConv(g, *p, preConvID, briefingInlined)
	if fail != nil {
		// Enrollment can fail with partial state already committed (the
		// actor row, membership, task-ref). Unwind it: the conv-id was
		// preset and its pane will never start, so anything left behind is
		// a ghost the operator would have to clear by hand.
		rollbackSpawnEnrollment(g, preConvID, mid, actorCreated)
		return "", 0, false, fail
	}
	enrolledConvID := preConvID
	undo.Add(func() { rollbackSpawnEnrollment(g, enrolledConvID, mid, actorCreated) })
	spawnArgs.SessionID = preConvID
	if p.DarwinRouteCapable {
		resolvedAgentID, resolveErr := db.AgentIDForConv(preConvID)
		if resolveErr != nil {
			return "", 0, false, &spawnFailure{http.StatusInternalServerError, "darwin_route_launch",
				"resolve Darwin route agent identity: " + resolveErr.Error()}
		}
		if p.DarwinRouteAgentID != "" && p.DarwinRouteAgentID != resolvedAgentID {
			return "", 0, false, &spawnFailure{http.StatusConflict, "darwin_route_launch",
				"darwin route agent identity does not match conversation owner"}
		}
		p.DarwinRouteAgentID = resolvedAgentID
		if p.DarwinRouteAgentID == "" {
			return "", 0, false, &spawnFailure{http.StatusConflict, "darwin_route_launch",
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
	if spawnHarness.UsesCommandInput() {
		// For the shell pseudo-harness the brief is executable command text,
		// not an agent welcome. Empty means an interactive shell.
		spawnArgs.InitialPrompt = p.InitialMessage
	} else {
		spawnArgs.InitialPrompt = buildSpawnLaunchPrompt(p.Name, p.Role, p.Descr, groupName,
			mid, p.InitialMessage != "", spawnContextBody, p.WorktreePath, p.WorktreeBranch,
			resolveSpawnerTitle(p.SpawnedByConv, p.SpawnedByAgent), inlineCap)
	}
	return preConvID, mid, briefingInlined, nil
}

// prepareManagedServer readies a server-authoritative harness (OpenCode)
// before enrollment, since its server issues the conversation id the agent
// enrolls under. It claims private server state for a tclaude-layer launch
// (wrapping *discardPrivateOpenCodeState so the server never outlives it),
// installs imported history into the server's store, and starts the server in
// its resource cgroup, degrading to no cgroup when the host allows it. An
// offline landing installs history but starts no server. The started server
// lands in spawnArgs, and its stop is registered on undo. It returns the
// launch (nil when none started) and the harness, which history installation
// may swap for the agent's own.
func prepareManagedServer(g *db.AgentGroup, p *spawnParams, spawnArgs *clcommon.SpawnArgs,
	spawnHarness *harness.Harness, label string, undo *launchundo.Undo,
	discardPrivateOpenCodeState *func(), installHistory func(*harness.Harness) *spawnFailure,
	importedID *string) (*openCodeLaunch, *harness.Harness, *spawnFailure) {
	var openCodeLaunch *openCodeLaunch
	rememberHTTPProxyLaunchGroup(label, g, p.PermissionOverrides)
	resolvedCwd, err := resolveOpenCodeLaunchCwd(p.Cwd)
	if err != nil {
		return nil, spawnHarness, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	p.Cwd = resolvedCwd
	spawnArgs.Cwd = resolvedCwd
	permissionJSON, err := openCodePermissionJSONForLaunch(
		p.Cwd, p.HarnessBuiltinMode, p.ApprovalPolicy, p.ToolGovernance, p.EffectiveSandbox)
	if err != nil {
		return nil, spawnHarness, &spawnFailure{http.StatusUnprocessableEntity, "invalid_opencode_permission_policy",
			"could not build OpenCode access-control policy: " + err.Error()}
	}
	implementation, implementationErr := sandboxpolicy.NormalizeImplementation(
		p.SandboxImplementation)
	if implementationErr != nil {
		return nil, spawnHarness, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_network", implementationErr.Error()}
	}
	if implementation == sandboxpolicy.ImplementationTclaudeLayer {
		if p.AgentID == "" {
			p.AgentID = db.NewAgentID()
		}
		discard, allocationErr := claimPrivateOpenCodeState(p.AgentID)
		if allocationErr != nil {
			return nil, spawnHarness, &spawnFailure{http.StatusInternalServerError, "spawn",
				"failed to allocate private OpenCode state: " + allocationErr.Error()}
		}
		if discard != nil {
			*discardPrivateOpenCodeState = discard
		}
		if discardState := *discardPrivateOpenCodeState; discardState != nil {
			// Not every failure below stops the server, and it must not
			// outlive the state it serves from.
			*discardPrivateOpenCodeState = func() {
				if openCodeLaunch != nil {
					_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
				}
				discardState()
			}
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
		return nil, spawnHarness, &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network", err.Error()}
	}
	resourceCgroupDir, resourceCleanup, resourceErr := prepareManagedServerResourceCgroup(
		label, p.EffectiveSandbox, p.SandboxImplementation, p.AllowUnenforcedSandbox, false)
	if resourceErr != nil {
		return nil, spawnHarness, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_resource_limits", resourceErr.Error()}
	}
	resourceStep := undo.Add(resourceCleanup)

	if p.BundleHistory != nil {
		if err = prepareOpenCodeTclaudeLayerState(sandboxSpec); err != nil {
			return nil, spawnHarness, &spawnFailure{http.StatusInternalServerError, "history", err.Error()}
		}
		spawnHarness, err = openCodeHistoryForAgent(spawnHarness, p.AgentID)
		if err != nil {
			return nil, spawnHarness, &spawnFailure{http.StatusInternalServerError, "history", err.Error()}
		}
		if fail := installHistory(spawnHarness); fail != nil {
			return nil, spawnHarness, fail
		}
	}
	// An offline landing's conversation is in its store now; its server
	// starts at resume.
	if p.NoLaunch {
		resourceStep.Run()
	} else {
		openCodeLaunch, err = startOpenCodeRuntimeForSpawn(
			label, p.Cwd, p.Name, *importedID, permissionJSON,
			p.SandboxImplementation, sandboxSpec, resourceCgroupDir)
		if err != nil && resourceCgroupDir != "" &&
			errors.Is(err, errOpenCodeResourceCgroup) &&
			degradeManagedServerResourceCgroup(
				p.EffectiveSandbox, p.SandboxImplementation, p.AllowUnenforcedSandbox, false, err) {
			resourceStep.Run()
			resourceCgroupDir = ""
			openCodeLaunch, err = startOpenCodeRuntimeForSpawn(
				label, p.Cwd, p.Name, *importedID, permissionJSON,
				p.SandboxImplementation, sandboxSpec, "")
		}
		if err != nil {
			return nil, spawnHarness, &spawnFailure{http.StatusInternalServerError, "spawn",
				"failed to start managed OpenCode server: " + err.Error()}
		}
		// The running server owns its cgroup now; stopping it is the undo.
		resourceStep.Release()
		undo.Add(func() { _ = stopOpenCodeRuntime(openCodeLaunch.SessionID) })
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
	return openCodeLaunch, spawnHarness, nil
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
	if p.AgentID == "" {
		p.AgentID = db.NewAgentID()
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(p.SandboxImplementation)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_network", err.Error()}
	}
	var discardPrivateState func()
	defer func() {
		if discardPrivateState != nil {
			discardPrivateState()
		}
	}()
	if implementation == sandboxpolicy.ImplementationTclaudeLayer {
		if discardPrivateState, err = claimPrivateOpenCodeState(p.AgentID); err != nil {
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
	if err := db.SetFederationSpawnLaunchLabel(p.AgentID, label); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if err := db.SetFederationBundleLaunchLabel(p.AgentID, label); err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
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
	continuation.privateOpenCodeStateDiscard, discardPrivateState = discardPrivateState, nil
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
	deferredSpawnsRunning.Store(label, struct{}{})
	goBackground(func() {
		defer deferredSpawnsRunning.Delete(label)
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
	federationSpawnFailed(p.AgentID, fail.Kind, fail.Msg)
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
		CapacityReserved:    p.nodeCapacityReserved,
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
	sshWorkaround := p.SSHWorkaround
	pending.SSHWorkaround = &sshWorkaround
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
			if id := discoverSpawnedConvID(h, p.Cwd, launchedAt, label); id != "" {
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
