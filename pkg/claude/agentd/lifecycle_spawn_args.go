package agentd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

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

// bundleLaunchPreparationFailed clears only the pre-dispatch marker. Failures
// returned by Spawn itself remain uncertain: a child may already be running.
func bundleLaunchPreparationFailed(args clcommon.SpawnArgs, launchErr error) error {
	if err := db.ClearUnlaunchedFederationBundleLabel(args.AgentID, args.Label); err != nil {
		return errors.Join(launchErr, fmt.Errorf("clear undispatched bundle launch: %w", err))
	}
	return launchErr
}

// SpawnDetachedTclaudeNew is a thin facade over Spawn.SpawnNew.
// Tests substitute a behavior-accurate fake by assigning Spawn at
// setup; production keeps the LiveSpawner default. See clcommon.SpawnArgs
// for the per-field semantics.
func SpawnDetachedTclaudeNew(args clcommon.SpawnArgs) error {
	if err := applyClaudeSubscription(&args); err != nil {
		return err
	}
	if err := prepareCodexAppServerRuntime(&args); err != nil {
		return bundleLaunchPreparationFailed(args, err)
	}
	if err := prepareCopilotAPIPort(&args); err != nil {
		failPreparedCodexAppServerRuntime(args, err)
		return bundleLaunchPreparationFailed(args, err)
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
	if err := applyClaudeSubscription(&args); err != nil {
		return err
	}
	args.CodexAppServerExistingThread = kind == copilotAPILaunchResume
	if err := prepareCodexAppServerRuntime(&args); err != nil {
		return bundleLaunchPreparationFailed(args, err)
	}
	if err := prepareCopilotAPIPort(&args); err != nil {
		failPreparedCodexAppServerRuntime(args, err)
		return bundleLaunchPreparationFailed(args, err)
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
	if a.ManagedInstructionsRoot != "" {
		args = append(args, "--managed-instructions-root", a.ManagedInstructionsRoot)
	}
	if a.ModelProxy != "" {
		args = append(args, "--model-proxy", a.ModelProxy)
	}
	if a.ClaudeSubscription {
		args = append(args, "--claude-subscription")
	}
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
	if a.RouteHelperAgentID != "" {
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
	args = appendPeerMessagingFlag(args, a.PeerMessaging)
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

// appendPeerMessagingFlag adds `--peer-messaging` to a `tclaude session new`
// argv when the spawn opted back INTO Claude Code's own cross-session
// messaging. false omits it, which is the recommended posture and makes the
// forked `session new` inject the refusal. Bare boolean flag; the forked
// `session new` re-validates it against the harness (a non-Claude harness
// rejects an explicit opt-in).
func appendPeerMessagingFlag(args []string, peerMessaging bool) []string {
	if peerMessaging {
		args = append(args, "--peer-messaging")
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
	if a.ClaudeSubscription {
		args = append(args, "--claude-subscription")
	}
	if a.ManagedInstructionsRoot != "" {
		args = append(args, "--managed-instructions-root", a.ManagedInstructionsRoot)
	}
	if a.Label != "" {
		args = append(args, "--label", a.Label)
	}
	args = appendTrustDirFlag(args, a.TrustDir)
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
	if a.RouteHelperAgentID != "" {
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
	args = appendPeerMessagingFlag(args, a.PeerMessaging)
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
