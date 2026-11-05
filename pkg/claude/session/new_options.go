package session

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// newLaunchOptions is what resolveNewLaunch decides from a launch's params
// before runNew creates anything: the harness and every validated, resolved
// launch option the rest of the launch reads.
type newLaunchOptions struct {
	// h: the harness that launches the session.
	h      *harness.Harness
	effort string
	model  string
	// harnessBuiltinMode: the harness's own sandbox mode, after the implementation decision.
	harnessBuiltinMode    string
	sandboxImplementation sandboxpolicy.Implementation
	// outerLayer: tclaude's own sandbox layer wraps the pane.
	outerLayer bool
	// unconfined: the launch omits OS confinement.
	unconfined bool
	// effectiveHarnessBuiltinMode: harnessBuiltinMode as the Codex managed profile sees it.
	effectiveHarnessBuiltinMode string
	networkAccess               sandboxpolicy.NetworkAccess
	approvalPolicy              string
	// recordedApprovalPolicy: the approval policy the session row records.
	recordedApprovalPolicy string
	autoReview             bool
	remoteControl          bool
	autoMemory             bool
	askTimeout             string
	contextFeatures        map[string]string
	autoCompactWindow      string
}

// resolveNewLaunch validates params and resolves the launch options from
// them, normalizing params in place. It creates nothing, so any error leaves
// no state behind.
func resolveNewLaunch(params *NewParams, launchSandbox *sandboxpolicy.Snapshot) (*newLaunchOptions, error) {
	// The spawn command, model/effort validation and resume form all come
	// from the selected harness behind the seam (pkg/claude/harness).
	// --harness picks it (default "claude"); an unknown value errors here
	// rather than silently launching Claude Code, as does an experimental
	// harness the operator has not opted into.
	h, err := harness.ResolveSpawnable(params.Harness)
	if err != nil {
		return nil, err
	}

	// Normalise + validate --effort up front so a typo errors cleanly
	// here (and, on the daemon spawn path, surfaces as the forked
	// `tclaude session new`'s non-zero exit) rather than being forwarded
	// to claude. Empty stays empty → the flag is omitted entirely. The
	// cleaned value is written back so the --join-group handler sees the
	// normalised level too.
	effort, err := h.Models.ValidateEffort(params.Effort)
	if err != nil {
		return nil, err
	}
	params.Effort = effort

	// Same treatment for --model: normalise + validate up front, empty
	// stays empty → the flag is omitted entirely.
	model, err := h.Models.ValidateModel(params.Model)
	if err != nil {
		return nil, err
	}
	params.Model = model
	if params.ModelProxy == "off" {
		params.ModelProxy = ""
	}
	if params.ClaudeSubscription && (h.Name != harness.DefaultName || params.ModelProxy != "") {
		return nil, errors.New("a Claude subscription launch runs Claude Code without a model gateway")
	}
	if params.ModelProxy != "" {
		if !h.SupportsModelProxy() {
			return nil, errors.New(h.ModelProxyRefusal())
		}
		if h.Name == harness.CopilotName && (strings.TrimSpace(params.Model) == "" || params.Model == "auto") {
			return nil, errors.New("copilot model gateway requires an explicit --model; automatic model selection is unavailable offline")
		}
		if !strings.Contains(params.ModelProxy, "@") {
			return nil, fmt.Errorf("--model-proxy must be <name>@<trusted peer>")
		}
	}

	// --session-id pins a fresh conversation id for a harness that declares a
	// preset shape (Harness.PresetSessionID): a UUID for Claude Code, Copilot
	// CLI and Gemini CLI, an agentd-minted ses_ id for OpenCode. Codex
	// generates its own at first turn and refuses one.
	params.SessionID = strings.TrimSpace(params.SessionID)
	if err := validatePresetSessionID(h, params.SessionID, params.Resume, params.ManagedLaunch); err != nil {
		return nil, err
	}
	// A harness that declares MintFreshSessionID (Gemini CLI) would otherwise
	// have nothing report the id it mints for itself, and the session row would
	// stay conversation-less for good — unresumable and invisible to every
	// conv-keyed surface. Its `--session-id` accepts a caller-chosen id, so a
	// fresh launch that was not handed one gets one here and is known from
	// the first moment, exactly like a launch-enrolled daemon spawn.
	if h.MintsFreshLaunchSessionID() && params.SessionID == "" && params.Resume == "" {
		params.SessionID = convops.GenerateUUID()
	}
	params.CwdWriteProof = strings.TrimSpace(params.CwdWriteProof)
	if params.CwdWriteProof != "" && !isValidSpawnCwdProofToken(params.CwdWriteProof) {
		return nil, fmt.Errorf("invalid internal cwd write proof")
	}
	params.DirWriteProof = strings.TrimSpace(params.DirWriteProof)
	if params.DirWriteProof != "" && !isValidSpawnCwdProofToken(params.DirWriteProof) {
		return nil, fmt.Errorf("invalid internal dir write proof")
	}
	if params.CwdWriteProof != "" && params.DirWriteProof != "" && params.CwdWriteProof != params.DirWriteProof {
		return nil, fmt.Errorf("internal cwd and dir write proofs must use the same token")
	}
	if err := validateCodexGitCommonDirPin(params); err != nil {
		return nil, err
	}
	if err := validateGitWorktreeWriteDirPins(params); err != nil {
		return nil, err
	}

	// Validate --sandbox up front WITHOUT defaulting it: a FRESH
	// `tclaude session new` is the human's own session, and the human is the
	// trust root — tclaude must not silently override their config.toml
	// sandbox_mode, so we emit a sandbox flag only when they pass it explicitly
	// or their own global default spawn profile supplies it
	// (applyGlobalDefaultLaunchProfile).
	// (The daemon spawn path is where the secure default belongs — an
	// agentd-spawned agent is the untrusted party — and it threads the resolved
	// mode in explicitly.) On a --resume params.Sandbox may already carry the
	// conversation's RECORDED mode, filled by applyRecordedLaunchPosture before
	// runNew: a relaunch replays the containment decision that was made rather
	// than re-deriving a new one, and erasing it would be permanent (TCL-730).
	// That is a replay, not a default — an explicit flag still wins.
	// For Claude Code, ValidateHarnessBuiltinMode normalizes its
	// inherit/blank to "" (no `--settings` override); the spawner turns on/off
	// into the override. The cwd-safety check needs the resolved cwd, so it
	// happens later.
	harnessBuiltinMode, err := harness.ValidateHarnessBuiltinMode(h, params.Sandbox)
	if err != nil {
		return nil, err
	}
	params.Sandbox = harnessBuiltinMode

	sandboxImplementation, err := sandboxpolicy.NormalizeImplementation(params.SandboxImpl)
	if err != nil {
		return nil, err
	}
	// A set harness-builtin on a fresh launch (or an explicit override on a
	// resume) is a new applicability decision and must be truthful. A flagless
	// resume may carry the exact same spelling from a legacy/default record;
	// that pure replay is deliberately grandfathered because historical rows
	// never persisted setness and OpenCode gains no privilege from the spelling.
	freshSandboxImplementationResolution :=
		strings.TrimSpace(params.Resume) == "" || params.sandboxImplExplicit
	if err := validateSandboxImplementationDecision(
		h, params.SandboxImpl, sandboxImplementation, freshSandboxImplementationResolution,
	); err != nil {
		return nil, err
	}
	params.SandboxImpl = string(sandboxImplementation)
	outerLayer := sandboxImplementation.UsesTclaudeLayer()
	requestedHarnessBuiltinMode := harnessBuiltinMode
	harnessBuiltinMode, err = harness.ResolveSandboxImplementationMode(
		h, harnessBuiltinMode, sandboxImplementation)
	if err != nil {
		return nil, err
	}
	params.Sandbox = harnessBuiltinMode
	if outerLayer {
		// The single-wall implementation replaces the harness's own permission
		// profile with tclaude's outer wall; ResolveSandboxImplementationMode
		// above has already forced the matching harness-native mode. Clearing
		// the profile HERE — rather than after the Codex managed-profile
		// normalization below, where this used to live — keeps that
		// normalization and the --permission-profile/--sandbox mutual-exclusion
		// check from judging a profile this launch does not use.
		//
		// Clearing it is not the same as ignoring it: a profile flag the outer
		// wall discards is still a flag the operator typed, and a combination
		// that was a loud error before must stay one. Validate what they asked
		// for against the mode they asked for, THEN discard it.
		if err := validateReplacedPermissionProfile(
			h, requestedHarnessBuiltinMode, params.PermissionProfile,
		); err != nil {
			return nil, err
		}
		params.PermissionProfile = ""
	}
	if outerLayer {
		if err := ValidateTclaudeLayerHarness(h.Name); err != nil {
			return nil, err
		}
	}

	// Normalize the managed-profile pseudo-mode. SandboxManagedProfile is the
	// dashboard/daemon way of selecting `codex -p tclaude-agent` through the one
	// sandbox dropdown, but it is the profile name, not a real Codex --sandbox
	// value — so on the direct CLI translate `--sandbox tclaude-agent` into
	// --permission-profile here, converging both paths on the profile rather than
	// emitting a bogus literal `--sandbox tclaude-agent` Codex would reject. (The
	// daemon never reaches this: appendSandboxArgs already passes
	// --permission-profile.) A conflicting explicit --permission-profile is a real
	// error; an equal one is harmless.
	if h.Name == harness.CodexName && harnessBuiltinMode == harness.SandboxManagedProfile {
		if up := strings.TrimSpace(params.PermissionProfile); up != "" && up != harness.CodexAgentProfile {
			return nil, fmt.Errorf("--sandbox %s selects the managed %s profile and conflicts with --permission-profile %s",
				harness.SandboxManagedProfile, harness.CodexAgentProfile, up)
		}
		params.PermissionProfile = harness.CodexAgentProfile
		harnessBuiltinMode = ""
		params.Sandbox = ""
	}
	// Mount paths are enforceable only where tclaude owns a mount namespace.
	// This gate runs before every harness-specific one because the alternative
	// is not a weaker policy but a WRONG one: the host path would be exposed and
	// the authored sandbox path would be empty.
	if err := sandboxpolicy.ValidateMountPathSupport(
		sandboxSnapshotActiveFilesystem(launchSandbox), sandboxImplementation, runtime.GOOS,
	); err != nil {
		return nil, err
	}
	if err := sandboxpolicy.ValidateProtectedAncestorWriteSupport(
		sandboxSnapshotActiveFilesystem(launchSandbox), sandboxImplementation, runtime.GOOS,
	); err != nil {
		return nil, err
	}
	// Same shape, same reason, for a rule naming a single file: the directory
	// lists below cannot carry one, so an implementation that enforces through
	// them would launch with the rule silently absent.
	if err := sandboxpolicy.ValidateFileGrantSupport(
		sandboxSnapshotActiveFilesystem(launchSandbox), sandboxImplementation, runtime.GOOS,
	); err != nil {
		return nil, err
	}
	// And the same for a temporary filesystem, which no path-filter or
	// directory-list sandbox can conjure at all.
	if err := sandboxpolicy.ValidateTmpfsSupport(
		sandboxSnapshotTmpfs(launchSandbox), sandboxImplementation, runtime.GOOS,
	); err != nil {
		return nil, err
	}
	// An implementation that confines nothing is not asked whether the harness
	// can represent a rule: it has stood every access boundary down on purpose,
	// so the rules below are inert rather than unsupported. Refusing here would
	// make the implementation unreachable for any operator whose global or
	// group profile already carries such a rule, while the chain itself must
	// keep resolving because resource_limits travel in it. This mirrors the
	// daemon seam's arm in sandboxProfileCapabilityFailure; mount paths are
	// still refused above, where an empty authored path is a real failure.
	unconfined := sandboxImplementation.OmitsOSConfinement()
	if launchSandbox != nil {
		if err := harness.ValidateExplicitFilesystemRoot(
			h, sandboxImplementation, launchSandbox.Effective.FilesystemRoot,
			runtime.GOOS,
		); err != nil {
			return nil, err
		}
	}
	codexManagedProfile := h.Name == harness.CodexName && params.PermissionProfile == harness.CodexAgentProfile
	effectiveHarnessBuiltinMode := harnessBuiltinMode
	if codexManagedProfile {
		// The managed profile is selected via -p rather than --sandbox, so
		// harnessBuiltinMode was cleared above; restore the logical mode for the gates.
		effectiveHarnessBuiltinMode = harness.SandboxManagedProfile
	}
	if err := ValidateHarnessSandboxRepresentation(
		h, harnessBuiltinMode, codexManagedProfile, launchSandbox, outerLayer, unconfined,
	); err != nil {
		return nil, err
	}
	networkAccess := sandboxSnapshotNetworkAccess(launchSandbox)
	hasNewAccessAxes := launchSandbox != nil &&
		(launchSandbox.Effective.Network != nil ||
			launchSandbox.Effective.UnixSockets != nil ||
			launchSandbox.Effective.FilesystemRoot == sandboxpolicy.FilesystemRootSeparate)
	if outerLayer && !hasNewAccessAxes {
		effective := sandboxpolicy.EffectiveProfile{NetworkAccess: networkAccess}
		if launchSandbox != nil {
			effective = launchSandbox.Effective
		}
		if _, err := ValidateTclaudeLayerNetwork(
			h,
			effective,
			harness.ResolvedModelTransport{Model: model},
		); err != nil {
			return nil, err
		}
	}

	// Validate --permission-profile: a Codex-only knob (codex -p <name>) that
	// is mutually exclusive with --sandbox. The daemon spawn path passes the
	// managed tclaude-agent profile here IN PLACE OF --sandbox workspace-write
	// so a sandboxed agent can still reach the agentd socket (JOH-207) — Codex
	// ignores a permission profile whenever a --sandbox is present, and only a
	// profile can allowlist that one Unix socket. The name is charset-validated
	// (it becomes a launch arg / filename / TOML key).
	profile, err := harness.ValidateCodexProfileName(params.PermissionProfile)
	if err != nil {
		return nil, err
	}
	params.PermissionProfile = profile
	if profile != "" {
		if harnessBuiltinMode != "" {
			return nil, fmt.Errorf("--permission-profile and --sandbox are mutually exclusive: " +
				"Codex ignores a permission profile when --sandbox is set")
		}
		if h.Name != harness.CodexName {
			return nil, fmt.Errorf("--permission-profile is a Codex launch option; harness %q has no permission profiles", h.Name)
		}
	}

	// Validate --ask-for-approval up front WITHOUT defaulting it, for the same
	// trust-root reason as --sandbox above: a FRESH `tclaude session new` is
	// the human's own session and they can attach to answer prompts, so tclaude
	// emits the approval/permission flag only when they pass it explicitly. The
	// daemon spawn path is where each harness's safe default belongs (its pane
	// is unattended) and it threads the resolved value in explicitly. A --resume
	// may arrive with the recorded posture already on params, on the same replay
	// terms as --sandbox above. The value
	// is validated per-harness (Codex's --ask-for-approval policy vs Claude
	// Code's --permission-mode); for Claude, inherit/blank normalizes to "" (no
	// --permission-mode override). The spawner emits the harness-appropriate flag.
	approvalPolicy, err := harness.ValidateApprovalPolicy(h, params.Approval)
	if err != nil {
		return nil, err
	}
	recordedApprovalPolicy := approvalPolicy
	if h.Name == harness.DefaultName && recordedApprovalPolicy == "" {
		// No Claude flag means the inherit posture exactly. Persist the sentinel
		// even though command emission still omits it, so approval-lineage checks
		// can distinguish a known inherit launch from a legacy unknown row.
		recordedApprovalPolicy = harness.ClaudePermissionInherit
	}
	if h.Name == harness.CopilotName && recordedApprovalPolicy == "" {
		// Identical reasoning for Copilot: emitting no permission flags IS the
		// `inherit` token, so persisting the sentinel is a faithful record
		// rather than an inference. Without it, every human-started Copilot
		// session would look like a legacy unreconstructable row to the spawn
		// approval guard, and a human's own session could not spawn even the
		// baseline children its posture provably permits.
		recordedApprovalPolicy = harness.CopilotApprovalInherit
	}
	if h.Name == harness.GeminiName && recordedApprovalPolicy == "" {
		// And for Gemini: no --approval-mode IS the inherit posture.
		recordedApprovalPolicy = harness.GeminiApprovalInherit
	}
	if h.Name == harness.PiName && recordedApprovalPolicy == "" {
		// And for pi: no tool restriction IS its native auto posture.
		recordedApprovalPolicy = harness.PiApprovalAuto
	}
	if err := RequireConfinementForApproval(h, recordedApprovalPolicy, outerLayer,
		params.AllowUnenforcedSandbox, launchIsSandboxContinuation(params)); err != nil {
		return nil, err
	}
	params.Approval = approvalPolicy

	toolGovernance, err := harness.ValidateToolGovernance(h, params.ToolGovernance)
	if err != nil {
		return nil, err
	}
	params.ToolGovernance = toolGovernance

	// Gate --auto-review the same way: it is allowed only for a harness with an
	// approvals subsystem (Codex), so setting it for Claude Code errors here.
	// There is no non-false default to apply (it is off unless explicitly opted
	// into), so ResolveAutoReview serves both this direct path and the daemon
	// path. See JOH-200 part 2.
	autoReview, err := harness.ResolveAutoReview(h, params.AutoReview)
	if err != nil {
		return nil, err
	}
	params.AutoReview = autoReview

	// Gate --trust-dir the same way: pre-trusting the launch cwd means something
	// only for a harness with a directory-trust dialog tclaude can pre-seed
	// (Claude Code and Codex), and unlike the flags above it edits a config file
	// tclaude does not own, so it is strictly opt-in and never defaulted on any
	// path. Setting it for another harness errors here; the actual write happens
	// just before launch, once cwd is resolved.
	if _, err := harness.ResolveTrustDir(h, params.TrustDir); err != nil {
		return nil, err
	}

	// Gate --remote-control: arming built-in Remote Access at launch is a
	// Claude Code feature (the --remote-control flag), so setting it for a
	// harness without Remote Access (Codex) errors here. Off unless opted into
	// on a fresh launch — or unless a --resume carried the conversation's
	// recorded posture onto params, which reproduces reachability the agent
	// already had rather than granting new. One ResolveRemoteControl serves this
	// direct path and the daemon path. See JOH-258.
	remoteControl, err := harness.ResolveRemoteControl(h, params.RemoteControl)
	if err != nil {
		return nil, err
	}
	params.RemoteControl = remoteControl

	// Gate --auto-memory the same way: opting back into auto memory only means
	// something for a harness that has an auto-memory system (Claude Code), so
	// requesting it for Codex errors here rather than silently doing nothing.
	// The default (off) is valid for every harness — it is simply not injected
	// for one that has no such switch. See ApplyAutoMemoryEnv.
	autoMemory, err := harness.ResolveAutoMemory(h, &params.AutoMemory)
	if err != nil {
		return nil, err
	}
	params.AutoMemory = autoMemory

	// Gate --peer-messaging the same way: opting back into Claude Code's
	// cross-session messaging only means something for a harness that has it
	// (Claude Code), so requesting it for Codex errors here rather than
	// silently doing nothing. The default (off) is valid for every harness — it
	// is simply not injected for one with no such system.
	peerMessaging, err := harness.ResolvePeerMessaging(h, &params.PeerMessaging)
	if err != nil {
		return nil, err
	}
	params.PeerMessaging = peerMessaging

	// Validate --ask-user-question-timeout: a Claude-Code-only settings.json
	// override (never|60s|5m|10m) delivered via `--settings`, so a value for a
	// harness with no AskUserQuestion dialog (Codex) errors here. There is no
	// forced default (inherit/blank normalizes to "" = no override — enabling
	// auto-continue is an explicit opt-in), so one ResolveAskTimeoutMode serves
	// both this direct path and the daemon path.
	askTimeout, err := harness.ResolveAskTimeoutMode(h, params.AskUserQuestionTimeout)
	if err != nil {
		return nil, err
	}
	params.AskUserQuestionTimeout = askTimeout

	// Gate --context-features the same way: parse the CLI spelling, then resolve
	// it against the harness so an unknown slug, a bad state, or a trim requested
	// for a harness with no steerable startup context (Codex) errors here rather
	// than silently doing nothing. Unset stays nil, which injects nothing.
	requestedFeatures, err := harness.ParseContextFeatures(params.ContextFeatures)
	if err != nil {
		return nil, err
	}
	contextFeatures, err := harness.ResolveContextFeatures(h, requestedFeatures)
	if err != nil {
		return nil, err
	}
	params.ContextFeatures = harness.FormatContextFeatures(contextFeatures)

	// Gate --auto-compact-window the same way, with one extra job: the resolve
	// also NORMALIZES the operator's spelling ("450k" → "450000"), and params is
	// written back so everything downstream — the env injection, the recorded
	// session row, the durable relaunch profile — agrees on one canonical form.
	autoCompactWindow, err := harness.ResolveAutoCompactWindow(h, params.AutoCompactWindow)
	if err != nil {
		return nil, err
	}
	params.AutoCompactWindow = autoCompactWindow
	contextWindowMax, err := harness.ResolveCopilotContextWindow(h, params.ContextWindowMax)
	if err != nil {
		return nil, err
	}
	params.ContextWindowMax = contextWindowMax
	fastModeWasSpecified := strings.TrimSpace(params.FastMode) != ""
	fastMode, err := harness.ResolveFastModeFlag(h, params.FastMode)
	if err != nil {
		return nil, err
	}
	params.FastMode = fastMode
	if params.JoinGroup != "" && fastModeWasSpecified && params.FastMode == harness.FastModeInherit {
		// RunJoinGroup must distinguish explicit inherit from an omitted flag so
		// it can suppress group/global/profile fast-mode defaults on the wire.
		params.FastMode = "inherit"
	}
	// Opt-in only on this surface: the flag's absence leaves the posture false,
	// so a launch that never asked for it keeps the send-keys path exactly as it
	// was. A request for a harness with no API-backed mode is refused here.
	copilotAPI, err := harness.ResolveCopilotAPI(h, &params.CopilotAPI)
	if err != nil {
		return nil, err
	}
	params.CopilotAPI = copilotAPI
	// Whether anything can actually PROVIDE the drive this launch resolved. Placed
	// before the port resolution rather than after it, because a launch that is
	// about to be refused or dropped must not allocate a loopback port first —
	// binding and closing 127.0.0.1:0 for a drive nobody is going to run is
	// exactly the pointless exposure this gate exists to prevent (TCL-1084).
	//
	// Every consequence of that decision — the drive params runs with, the drop
	// marker the posture write reads, and clearing a port the drive no longer needs
	// — lives inside the applier rather than here, so they are testable without a
	// pane and cannot be separated from each other by a tidy-up.
	if err := applyCopilotAPIDriveDecision(params, h, os.Stderr); err != nil {
		return nil, err
	}
	copilotAPIPort, err := ResolveCopilotAPIPort(params.CopilotAPI, params.CopilotAPIPort)
	if err != nil {
		return nil, err
	}
	params.CopilotAPIPort = copilotAPIPort
	codexAppServer, err := harness.ResolveCodexAppServer(h, &params.CodexAppServer)
	if err != nil {
		return nil, err
	}
	params.CodexAppServer = codexAppServer
	if params.CodexAppServer {
		if !params.ManagedLaunch || strings.TrimSpace(params.CodexAppServerSocket) == "" ||
			strings.TrimSpace(params.CodexAppServerURL) == "" ||
			strings.TrimSpace(params.CodexAppServerTokenSHA256) == "" ||
			strings.TrimSpace(params.CodexAppServerTokenHandoff) == "" ||
			strings.TrimSpace(params.CodexAppServerPIDFile) == "" ||
			strings.TrimSpace(params.CodexAppServerLogFile) == "" ||
			strings.TrimSpace(params.CodexAppServerGeneration) == "" {
			return nil, fmt.Errorf("--codex-app-server requires an agentd-managed launch with private runtime paths")
		}
	} else if params.CodexAppServerSocket != "" || params.CodexAppServerPIDFile != "" ||
		params.CodexAppServerURL != "" || params.CodexAppServerTokenSHA256 != "" ||
		params.CodexAppServerTokenHandoff != "" ||
		params.CodexAppServerLogFile != "" || params.CodexAppServerGeneration != "" {
		return nil, fmt.Errorf("codex app-server runtime paths require --codex-app-server")
	}
	return &newLaunchOptions{
		h:                           h,
		effort:                      effort,
		model:                       model,
		harnessBuiltinMode:          harnessBuiltinMode,
		sandboxImplementation:       sandboxImplementation,
		outerLayer:                  outerLayer,
		unconfined:                  unconfined,
		effectiveHarnessBuiltinMode: effectiveHarnessBuiltinMode,
		networkAccess:               networkAccess,
		approvalPolicy:              approvalPolicy,
		recordedApprovalPolicy:      recordedApprovalPolicy,
		autoReview:                  autoReview,
		remoteControl:               remoteControl,
		autoMemory:                  autoMemory,
		askTimeout:                  askTimeout,
		contextFeatures:             contextFeatures,
		autoCompactWindow:           autoCompactWindow,
	}, nil
}

// ValidateHarnessSandboxRepresentation refuses a launch whose sandbox profile
// carries a rule the harness's OWN sandbox cannot represent: filesystem rules
// per harness and mode, a reopen beneath a deny the harness would discard, and
// legacy network_access rules outside the Codex managed sandbox or OpenCode's
// web-tool access control. It asks nothing of a launch behind tclaude's sandbox
// or one that confines nothing. `session new` and the conv resume paths both
// call it, so a resume admits exactly the postures a spawn does.
//
// codexManagedProfile is whether a Codex launch runs under the managed
// tclaude-agent permission profile, selected via -p in place of --sandbox.
func ValidateHarnessSandboxRepresentation(
	h *harness.Harness,
	harnessBuiltinMode string,
	codexManagedProfile bool,
	launchSandbox *sandboxpolicy.Snapshot,
	outerLayer, unconfined bool,
) error {
	if !outerLayer && !unconfined && len(sandboxSnapshotActiveFilesystem(launchSandbox)) > 0 &&
		h.Name == harness.CodexName && !codexManagedProfile {
		return fmt.Errorf("unsupported_sandbox_profile_filesystem: codex filesystem rules require sandbox %s", harness.SandboxManagedProfile)
	}
	if !outerLayer && !unconfined && len(sandboxSnapshotDirs(launchSandbox, sandboxpolicy.AccessDeny)) > 0 &&
		h.Name == harness.DefaultName && harnessBuiltinMode != harness.ClaudeSandboxOn {
		return fmt.Errorf("unsupported_sandbox_profile_filesystem: Claude filesystem deny rules require sandbox %s", harness.ClaudeSandboxOn)
	}
	if !outerLayer && !unconfined && len(sandboxSnapshotActiveFilesystem(launchSandbox)) > 0 &&
		h.Name == harness.GeminiName {
		return fmt.Errorf("unsupported_sandbox_profile_filesystem: Gemini CLI's own sandbox cannot represent sandbox-profile filesystem rules; use --sandbox-impl tclaude-layer")
	}
	if !outerLayer && !unconfined && len(sandboxSnapshotActiveFilesystem(launchSandbox)) > 0 &&
		h.Name == harness.OpenCodeName && harnessBuiltinMode != harness.OpenCodeSandboxAccessControl {
		return fmt.Errorf("unsupported_sandbox_profile_filesystem: OpenCode filesystem rules require soft access-control mode %s", harness.OpenCodeSandboxAccessControl)
	}
	// Capability gates. Both refuse loudly rather than launching with a weaker
	// policy than the operator asked for: a reopen beneath a deny that the
	// harness silently discards, or an acknowledged protected grant it quietly
	// dropped, would both be worse than an error, because the operator would
	// trust isolation that is not there. This is the EARLY gate over the
	// authored profile; the rendered rules are gated again once the launch
	// contract has added its own reopens.
	effectiveHarnessBuiltinMode := harnessBuiltinMode
	if codexManagedProfile {
		// The managed profile is selected via -p rather than --sandbox, so
		// harnessBuiltinMode was cleared above; restore the logical mode for the gates.
		effectiveHarnessBuiltinMode = harness.SandboxManagedProfile
	}
	launchFilesystem := sandboxSnapshotActiveFilesystem(launchSandbox)
	if !outerLayer && !unconfined {
		if err := harness.ValidateSandboxReopenUnderDeny(h.Name, effectiveHarnessBuiltinMode, launchFilesystem); err != nil {
			return err
		}
	}
	networkAccess := sandboxSnapshotNetworkAccess(launchSandbox)
	hasNewAccessAxes := launchSandbox != nil &&
		(launchSandbox.Effective.Network != nil ||
			launchSandbox.Effective.UnixSockets != nil ||
			launchSandbox.Effective.FilesystemRoot == sandboxpolicy.FilesystemRootSeparate)
	if networkAccess != sandboxpolicy.NetworkAccessInherit && !hasNewAccessAxes && !outerLayer && !unconfined {
		switch h.Name {
		case harness.CodexName:
			if !codexManagedProfile {
				return fmt.Errorf("unsupported_sandbox_profile_network: codex network rules require sandbox %s", harness.SandboxManagedProfile)
			}
			if err := harness.ValidateCodexAgentNetworkAccess(networkAccess); err != nil {
				return fmt.Errorf("unsupported_sandbox_profile_network: %w", err)
			}
		case harness.OpenCodeName:
			if harnessBuiltinMode != harness.OpenCodeSandboxAccessControl {
				return fmt.Errorf("unsupported_sandbox_profile_network: OpenCode web-tool network rules require soft access-control mode %s", harness.OpenCodeSandboxAccessControl)
			}
		default:
			return fmt.Errorf("unsupported_sandbox_profile_network: network policies are supported by the Codex managed sandbox or as OpenCode web-tool access control")
		}
	}
	return nil
}
