package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/ratelimit"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

type NewParams struct {
	Dir string `short:"C" long:"dir" optional:"true" help:"Directory to start session in (defaults to current directory)"`
	// CwdWriteProof is an internal daemon-to-session capability. The harness
	// command checks its marker only after tmux has established the pane's cwd
	// inode, then removes it before exec. Hidden from normal CLI help.
	CwdWriteProof    string `long:"cwd-write-proof" optional:"true" help:"Internal: verify a daemon-issued cwd marker before launching the harness"`
	Resume           string `long:"resume" short:"r" optional:"true" help:"Resume an existing conversation by ID"`
	Global           bool   `short:"g" help:"Search for conversation across all projects (with --resume)"`
	Label            string `long:"label" optional:"true" help:"Custom label for the session"`
	Detached         bool   `long:"detached" short:"d" help:"Start detached (don't attach to session)"`
	WaitForRateLimit bool   `long:"wait-for-rate-limit" short:"w" help:"Wait for rate limit (5-hour and 7-day) to reset before starting session"`

	// Effort sets Claude Code's reasoning effort for the session via
	// `claude --effort <level>`. Empty (the default) omits the flag so
	// claude uses its own default; a non-empty value is normalised and
	// validated against clcommon.ValidEffortLevels in runNew.
	Effort string `long:"effort" optional:"true" help:"Claude reasoning effort: low|medium|high|xhigh|max. Unset = claude's own default (no flag passed)"`

	// Model picks the Claude model for the session via `claude --model
	// <alias>`. Empty (the default) omits the flag so claude uses its
	// own default; a non-empty value is normalised and validated
	// against clcommon.ValidModels in runNew.
	Model string `long:"model" optional:"true" help:"Claude model: fable|fable[1m]|opus|opus[1m]|sonnet|sonnet[1m]|haiku|opusplan, or a full model ID (e.g. claude-fable-5). Unset = claude's own default (no flag passed)"`

	// Harness selects the coding tool this session runs (default
	// "claude"). "codex" launches OpenAI Codex CLI in the tmux pane via
	// the codex Spawner. The chosen harness's ModelCatalog validates
	// --model/--effort and its Spawner builds the launch command, so the
	// rest of runNew stays harness-agnostic. The special value "shell"
	// (ShellHarnessName) is NOT a registered harness — it starts a plain,
	// ephemeral interactive shell instead (no conversation, no hooks, no
	// model/sandbox/approval), handled by runNewShell before any harness
	// resolution happens. See shell.go.
	Harness string `long:"harness" optional:"true" help:"Coding harness to launch: claude (default) | codex | shell (a plain, ephemeral shell — no conversation)"`

	// Shell is shorthand for --harness shell: it sets Harness to
	// ShellHarnessName in runNew before any harness resolution happens.
	// Mutually exclusive with an explicit --harness naming anything else.
	Shell bool `long:"shell" short:"s" help:"Start a plain interactive shell instead of a coding harness (shorthand for --harness shell)"`

	// Sandbox selects a harness's launch containment. On a direct `session new`
	// it is opt-in: unset emits no flag, so each harness uses its own config
	// (Codex's config.toml sandbox_mode; Claude Code's settings.json) — the human
	// running session new is the trust root, so tclaude doesn't override it. Pass
	// a value explicitly. Codex's modes are its native --sandbox enum; the
	// special value tclaude-agent (SandboxManagedProfile) is a shorthand
	// normalized to --permission-profile tclaude-agent. Claude Code has no
	// --sandbox flag, so its modes (inherit/on/off) are delivered as a
	// `claude --settings '<json>'` override; inherit normalizes to "" (omit). The
	// daemon spawn path (agentd / `agent spawn`) defaults Codex to the managed
	// profile (a spawned agent is the untrusted party) and Claude to inherit (no
	// override — its settings.json is the operator's chosen posture). See
	// JOH-192 / JOH-207.
	Sandbox string `long:"sandbox" optional:"true" help:"Launch containment (per-harness). Codex: tclaude-agent (managed profile = workspace-write + agentd socket) | workspace-write | read-only | danger-full-access. Claude Code: inherit | on (force OS sandbox on via --settings) | off. Unset = no override (each harness uses its own config)"`

	// AskUserQuestionTimeout is the per-session Claude Code AskUserQuestion
	// idle-timeout override (never|60s|5m|10m), delivered via `--settings`
	// alongside the sandbox block. inherit/unset omits it, so the agent uses the
	// operator's own settings.json value. A Claude-Code-only knob; a value for a
	// harness with no AskUserQuestion dialog (Codex) errors.
	AskUserQuestionTimeout string `long:"ask-user-question-timeout" optional:"true" help:"Claude Code AskUserQuestion idle-timeout override: inherit (use settings.json as-is) | never (wait for a human) | 60s | 5m | 10m (auto-continue with the default answer after the interval — keeps an unattended agent moving). Unset = inherit. Not applicable to Codex"`

	// PermissionProfile selects a tclaude-managed Codex permission profile to
	// run under, emitted as `codex -p <name>`. It is how the daemon keeps a
	// sandboxed Codex agent able to reach the agentd socket (JOH-207): the
	// daemon spawn path passes it (the tclaude-agent profile) in place of
	// --sandbox workspace-write, because Codex ignores permission profiles when
	// a --sandbox is present and only a profile can allowlist the socket. It is
	// mutually exclusive with --sandbox; the managed profile name is ensured on
	// disk before launch. Not applicable to Claude Code. Rarely set by hand.
	PermissionProfile string `long:"permission-profile" optional:"true" help:"Codex permission profile to run under (codex -p <name>); mutually exclusive with --sandbox. Not applicable to claude"`

	// Approval selects a harness's launch-time approval/permission posture.
	// Codex takes an --ask-for-approval policy; Claude Code has no approval flag
	// — its posture is the --permission-mode the spawner emits — but the value
	// rides through this same field (validated per-harness by claudeApproval /
	// codexApproval). On a direct `session new` it is opt-in: unset emits no flag,
	// so each harness uses its own config (the human running session new is the
	// trust root and can answer prompts — tclaude doesn't force a posture). The
	// daemon spawn path defaults it to each harness's safe value (Codex: never,
	// so a detached pane can't deadlock; Claude: inherit, no override). See
	// JOH-200.
	Approval string `long:"ask-for-approval" optional:"true" help:"Launch approval/permission posture (per-harness). Codex policy: untrusted|on-failure|on-request|never. Claude Code permission mode: inherit|plan|acceptEdits|default|auto|dontAsk|bypassPermissions. Unset = no override (each harness uses its own config)"`

	// AutoReview opts into the harness's guardian subagent (Codex's `-c
	// approvals_reviewer=auto_review`), which auto-decides approval prompts in
	// your place. Off by default (you review). Gated on the harness having an
	// approvals subsystem (Codex); set for claude is an error. Same per-spawn
	// opt-in on the direct `session new` path as on the daemon path —
	// experimental/undocumented upstream. See JOH-200 part 2.
	AutoReview bool `long:"auto-review" help:"EXPERIMENTAL: route Codex approval prompts to the guardian subagent (auto-decides in your place) instead of asking you. Off by default. Not applicable to claude"`

	// TrustDir opts into pre-trusting the launch cwd for Codex, so a
	// detached pane doesn't freeze on Codex's "do you trust this folder?"
	// onboarding modal (JOH-205). It writes [projects."<cwd>"] trust_level =
	// "trusted" into the user's ~/.codex/config.toml BEFORE launch — the
	// only mechanism Codex exposes (no per-invocation flag). OFF by default
	// and NEVER auto-defaulted on any path: editing the user's config.toml
	// is a side effect they must explicitly request (dashboard checkbox /
	// this flag). No-op for Claude Code (no dir-trust concept). The write is
	// atomic + idempotent (harness.EnsureCodexDirTrusted).
	TrustDir bool `long:"trust-dir" help:"Pre-trust the launch directory for Codex by writing [projects.\"<cwd>\"] trust_level=\"trusted\" into ~/.codex/config.toml, so a detached pane doesn't freeze on the trust-folder modal. Off by default; edits your Codex config, so opt-in only. Not applicable to claude"`

	// RemoteControl arms Claude Code's built-in Remote Access at launch
	// (`claude --remote-control`), so the session is reachable from
	// claude.ai/code + the Claude mobile app from its first turn (JOH-258). Off
	// by default. Gated on the harness having Remote Access (Claude Code); set
	// for Codex is an error. The daemon spawn path sets it from the dashboard /
	// `agent spawn` opt-in; a direct human `session new` may set it too. Needs
	// the operator logged into claude.ai (OAuth) for the session to actually
	// pair — outside tclaude's control.
	RemoteControl bool `long:"remote-control" help:"Start with Claude Code Remote Access ON (claude --remote-control), so the session is reachable from the Claude app. Off by default. Requires a claude.ai login to pair. Not applicable to codex"`

	// --join-group makes the new session auto-join an existing agent group
	// the moment its conv-id materialises. Routed through the daemon's
	// `groups.spawn` orchestration; not compatible with --resume / --label.
	JoinGroup string `long:"join-group" optional:"true" help:"Spawn the session and add it to an existing agent group (shorthand for agent spawn + foreground attach)"`
	Name      string `long:"name" optional:"true" help:"Display name for the session (claude --name; becomes its conversation title). With --join-group it is the new agent's name"`
	Role      string `long:"role" optional:"true" help:"Role tag for the new member in --join-group (e.g. 'tech-lead')"`
	Descr     string `long:"descr" optional:"true" help:"Description of the new member's purpose in --join-group"`

	// InitialPrompt is a first-turn prompt the harness submits itself at
	// launch (its own positional [PROMPT] arg) — used for a harness whose
	// conv-id is only knowable after the first turn (Codex). The daemon
	// spawn path sets it so a freshly-spawned Codex takes a turn on its own
	// and its conv-id materialises without a human typing the first message
	// (JOH-205); a direct human `session new` leaves it empty and types
	// their own first message. Ignored by a harness that reports its conv-id
	// at launch (Claude Code) and on a --resume. See codexSpawner.BuildCommand.
	InitialPrompt string `long:"initial-prompt" optional:"true" help:"First-turn prompt the harness submits itself at launch (its positional [prompt]). Daemon spawns set it automatically (Claude Code: the agent welcome; Codex: a conv-id seed)"`

	// SessionID pins the conversation id for a FRESH Claude Code launch
	// (`claude --session-id <uuid>`), so the conv-id is known before the pane
	// starts. The daemon's launch-enrollment spawn path sets it so the agent
	// can be enrolled + named via launch args instead of post-connect tmux
	// injection. A direct human launch may set it to choose a specific id.
	// Mutually exclusive with --resume; Claude-Code-only; must be a valid UUID.
	SessionID string `long:"session-id" optional:"true" help:"Use a specific conversation id (UUID) for a fresh Claude Code session (claude --session-id). Mutually exclusive with --resume"`
}

func NewCmd() *cobra.Command {
	cmd := boa.CmdT[NewParams]{
		Use:         "new",
		Short:       "Start a new Claude Code session",
		Long:        "Start a new Claude Code session in a tmux session. Attaches by default (Ctrl+B D to detach).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *NewParams, cmd *cobra.Command, args []string) {
			if err := runNew(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()

	// Allow arbitrary args so post-'--' args pass through to claude without cobra rejecting them.
	cmd.Args = cobra.ArbitraryArgs
	_ = cmd.Flags().MarkHidden("cwd-write-proof")

	// Register completion for --resume flag
	_ = cmd.RegisterFlagCompletionFunc("resume", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Check if -g flag is set (params may not be populated during completion)
		global, _ := cmd.Flags().GetBool("global")
		return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
	})

	RegisterJoinGroupCompletion(cmd)

	return cmd
}

// RegisterJoinGroupCompletion wires `--join-group` to suggest existing
// agent group names. Reads SQLite directly — completions fire on every
// <tab> keystroke, so they bypass the daemon (same convention as
// `tclaude agent groups …` completions). Exported so the top-level
// `tclaude` cobra cmd in pkg/claude/claude.go can register it too.
func RegisterJoinGroupCompletion(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("join-group", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		groups, err := db.ListAgentGroups()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		out := make([]string, 0, len(groups))
		for _, g := range groups {
			if strings.HasPrefix(g.Name, toComplete) {
				out = append(out, g.Name)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	})
}

// RunNew is the exported entry point for running the new session command
func RunNew(params *NewParams) error {
	return runNew(params)
}

// sandboxDescr returns the human-facing launch-containment descriptor for the
// session row / error messages: the permission-profile name when one is set
// (the JOH-207 path — `codex -p <name>`), otherwise the --sandbox mode (which
// may itself be ""). The two are mutually exclusive upstream, so at most one
// is non-empty.
func sandboxDescr(sandboxMode, permissionProfile string) string {
	if permissionProfile != "" {
		return permissionProfile
	}
	return sandboxMode
}

// JoinGroupHandler implements `--join-group`. Set by the agent package's
// init() to avoid a session→agent import cycle (agent already depends on
// session for AttachToSession). When nil, --join-group falls back to a
// clear error.
var JoinGroupHandler func(*NewParams) error

func runNew(params *NewParams) error {
	// "shell" is a sentinel, not a registered harness (see shell.go) — branch
	// before any harness resolution so a plain shell never touches the
	// coding-harness machinery below (model/effort validation, sandbox,
	// approval, hooks, --join-group, …). --shell is shorthand for
	// --harness shell; an explicit --harness naming anything else alongside
	// it is a conflicting request rather than something to silently resolve.
	params.Harness = strings.TrimSpace(params.Harness)
	if params.Shell {
		if params.Harness != "" && params.Harness != ShellHarnessName {
			return fmt.Errorf("--shell conflicts with --harness %s", params.Harness)
		}
		params.Harness = ShellHarnessName
	}
	if params.Harness == ShellHarnessName {
		return runNewShell(params)
	}

	// The spawn command, model/effort validation and resume form all come
	// from the selected harness behind the seam (pkg/claude/harness).
	// --harness picks it (default "claude"); an unknown value errors here
	// rather than silently launching Claude Code.
	h, err := harness.Resolve(params.Harness)
	if err != nil {
		return err
	}

	// Normalise + validate --effort up front so a typo errors cleanly
	// here (and, on the daemon spawn path, surfaces as the forked
	// `tclaude session new`'s non-zero exit) rather than being forwarded
	// to claude. Empty stays empty → the flag is omitted entirely. The
	// cleaned value is written back so the --join-group handler sees the
	// normalised level too.
	effort, err := h.Models.ValidateEffort(params.Effort)
	if err != nil {
		return err
	}
	params.Effort = effort

	// Same treatment for --model: normalise + validate up front, empty
	// stays empty → the flag is omitted entirely.
	model, err := h.Models.ValidateModel(params.Model)
	if err != nil {
		return err
	}
	params.Model = model

	// --session-id pins a fresh conversation id for Claude Code
	// (`claude --session-id`). It is mutually exclusive with --resume (a
	// resume already has an id) and only the default harness accepts a preset
	// conv-id — Codex generates its own at first turn. Validate the shape here
	// so a malformed id fails cleanly rather than at `claude` launch.
	params.SessionID = strings.TrimSpace(params.SessionID)
	if params.SessionID != "" {
		if params.Resume != "" {
			return fmt.Errorf("--session-id cannot be combined with --resume")
		}
		if h.Name != harness.DefaultName {
			return fmt.Errorf("--session-id is only supported for the %q harness", harness.DefaultName)
		}
		if !clcommon.IsValidUUID(params.SessionID) {
			return fmt.Errorf("--session-id must be a valid UUID, got %q", params.SessionID)
		}
	}
	params.CwdWriteProof = strings.TrimSpace(params.CwdWriteProof)
	if params.CwdWriteProof != "" && !isValidSpawnCwdProofToken(params.CwdWriteProof) {
		return fmt.Errorf("invalid internal cwd write proof")
	}

	// Validate --sandbox up front WITHOUT defaulting it: a direct
	// `tclaude session new` is the human's own session, and the human is the
	// trust root — tclaude must not silently override their config.toml
	// sandbox_mode, so we emit a sandbox flag only when they pass it explicitly.
	// (The daemon spawn path is where the secure default belongs — an
	// agentd-spawned agent is the untrusted party — and it threads the resolved
	// mode in explicitly.) For Claude Code, ValidateSandboxMode normalizes its
	// inherit/blank to "" (no `--settings` override); the spawner turns on/off
	// into the override. The cwd-safety check needs the resolved cwd, so it
	// happens later.
	sandboxMode, err := harness.ValidateSandboxMode(h, params.Sandbox)
	if err != nil {
		return err
	}
	params.Sandbox = sandboxMode

	// Normalize the managed-profile pseudo-mode. SandboxManagedProfile is the
	// dashboard/daemon way of selecting `codex -p tclaude-agent` through the one
	// sandbox dropdown, but it is the profile name, not a real Codex --sandbox
	// value — so on the direct CLI translate `--sandbox tclaude-agent` into
	// --permission-profile here, converging both paths on the profile rather than
	// emitting a bogus literal `--sandbox tclaude-agent` Codex would reject. (The
	// daemon never reaches this: appendSandboxArgs already passes
	// --permission-profile.) A conflicting explicit --permission-profile is a real
	// error; an equal one is harmless.
	if h.Name == harness.CodexName && sandboxMode == harness.SandboxManagedProfile {
		if up := strings.TrimSpace(params.PermissionProfile); up != "" && up != harness.CodexAgentProfile {
			return fmt.Errorf("--sandbox %s selects the managed %s profile and conflicts with --permission-profile %s",
				harness.SandboxManagedProfile, harness.CodexAgentProfile, up)
		}
		params.PermissionProfile = harness.CodexAgentProfile
		sandboxMode = ""
		params.Sandbox = ""
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
		return err
	}
	params.PermissionProfile = profile
	if profile != "" {
		if sandboxMode != "" {
			return fmt.Errorf("--permission-profile and --sandbox are mutually exclusive: " +
				"Codex ignores a permission profile when --sandbox is set")
		}
		if h.Name != harness.CodexName {
			return fmt.Errorf("--permission-profile is a Codex launch option; harness %q has no permission profiles", h.Name)
		}
	}

	// Validate --ask-for-approval up front WITHOUT defaulting it, for the same
	// trust-root reason as --sandbox above: a direct `tclaude session new` is
	// the human's own session and they can attach to answer prompts, so tclaude
	// emits the approval/permission flag only when they pass it explicitly. The
	// daemon spawn path is where each harness's safe default belongs (its pane
	// is unattended) and it threads the resolved value in explicitly. The value
	// is validated per-harness (Codex's --ask-for-approval policy vs Claude
	// Code's --permission-mode); for Claude, inherit/blank normalizes to "" (no
	// --permission-mode override). The spawner emits the harness-appropriate flag.
	approvalPolicy, err := harness.ValidateApprovalPolicy(h, params.Approval)
	if err != nil {
		return err
	}
	params.Approval = approvalPolicy

	// Gate --auto-review the same way: it is allowed only for a harness with an
	// approvals subsystem (Codex), so setting it for Claude Code errors here.
	// There is no non-false default to apply (it is off unless explicitly opted
	// into), so ResolveAutoReview serves both this direct path and the daemon
	// path. See JOH-200 part 2.
	autoReview, err := harness.ResolveAutoReview(h, params.AutoReview)
	if err != nil {
		return err
	}
	params.AutoReview = autoReview

	// Gate --trust-dir the same way: pre-trusting the launch cwd is a
	// Codex-only concept (Claude Code has no "trust this folder?" modal), and
	// unlike the flags above it edits the user's ~/.codex/config.toml, so it
	// is strictly opt-in and never defaulted on any path. Setting it for
	// another harness errors here; the actual write happens just before
	// launch, once cwd is resolved.
	if _, err := harness.ResolveTrustDir(h, params.TrustDir); err != nil {
		return err
	}

	// Gate --remote-control: arming built-in Remote Access at launch is a
	// Claude Code feature (the --remote-control flag), so setting it for a
	// harness without Remote Access (Codex) errors here. Off unless opted into,
	// so one ResolveRemoteControl serves both this direct path and the daemon
	// path. See JOH-258.
	remoteControl, err := harness.ResolveRemoteControl(h, params.RemoteControl)
	if err != nil {
		return err
	}
	params.RemoteControl = remoteControl

	// Validate --ask-user-question-timeout: a Claude-Code-only settings.json
	// override (never|60s|5m|10m) delivered via `--settings`, so a value for a
	// harness with no AskUserQuestion dialog (Codex) errors here. There is no
	// forced default (inherit/blank normalizes to "" = no override — enabling
	// auto-continue is an explicit opt-in), so one ResolveAskTimeoutMode serves
	// both this direct path and the daemon path.
	askTimeout, err := harness.ResolveAskTimeoutMode(h, params.AskUserQuestionTimeout)
	if err != nil {
		return err
	}
	params.AskUserQuestionTimeout = askTimeout

	if params.JoinGroup != "" {
		if JoinGroupHandler == nil {
			return fmt.Errorf("--join-group is not wired up in this binary")
		}
		return JoinGroupHandler(params)
	}
	extraArgs := clcommon.ExtractClaudeExtraArgs()

	// Pass-through mode: --help, --version etc. — run the harness binary
	// directly, no tmux.
	if clcommon.ShouldRunClaudeDirect(extraArgs) {
		cmd := exec.Command(h.Spawn.Binary(), extraArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Self-guard: a Claude Code instance must not directly launch
	// another Claude Code session. Placed after the --join-group and
	// pass-through branches on purpose: --join-group delegates to the
	// agentd daemon (gated there by the `groups.spawn` permission), and
	// pass-through only prints `claude --help`/`--version`. Daemon-forked
	// spawns are unaffected — see GuardAgainstNestedSpawn.
	if err := GuardAgainstNestedSpawn(); err != nil {
		return err
	}

	// Check tmux is installed
	if err := CheckTmuxInstalled(); err != nil {
		return err
	}

	// Check if hooks are installed (warn if not)
	EnsureHooksInstalled(false, os.Stdout, os.Stderr)

	// Sync the configured Claude Code transcript-retention override
	// (claude_cleanup_period_days) into ~/.claude/settings.json. No-op unless
	// set; logs and continues on failure.
	_ = EnsureClaudeCleanupPeriod()

	// Determine working directory
	cwd, err := resolveSessionDir(params.Dir)
	if err != nil {
		return err
	}

	// Set up signal handling for clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Bridge sigCh into a context so WaitForRateLimit can be interrupted by Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Extract just the ID from autocomplete format (e.g., "0459cd73_[title]_prompt..." -> "0459cd73")
	shortID := clcommon.ExtractIDFromCompletion(params.Resume)

	// Resolve to full UUID and get project path, via the harness's
	// conversation source (CC's cwd-indexed resolver, or another harness's
	// ConvStore — e.g. Codex's rollout/state DB).
	var fullConvID string
	var convProjectPath string
	if shortID != "" {
		var err error
		fullConvID, convProjectPath, err = resolveResumeConv(h, shortID, params.Global, cwd)
		if err != nil {
			return err
		}
		// Use conversation's project directory instead of cwd
		if convProjectPath != "" {
			cwd = convProjectPath
		}
	}

	// The session row's primary key carries the FULL identity — never a
	// truncation. On resume that's the conversation's full UUID (already in
	// sessions.conv_id); collapsing it to 8 chars made two conversations
	// sharing a hex prefix collide on the PK (SaveSession's ON CONFLICT(id)
	// silently overwrote the row → wrong-session reattach + conflated
	// notify_state / session_cost_daily). See JOH-248.
	// Priority: explicit label > resumed conv UUID > random synthetic.
	sessionID := GenerateSessionID()
	if fullConvID != "" {
		sessionID = fullConvID
	}
	if params.Label != "" {
		sessionID = params.Label
	}

	// When resuming, reserve the conversation before launching. This both
	// rejects an already-live conv AND serializes against a concurrent resume:
	// two `session new -r` of the same conv that both pass a bare read-guard
	// would otherwise both run `claude --resume` on the same .jsonl (interleaved
	// appends → corruption), because the disambiguated tmux name no longer makes
	// the second `new-session` clean-fail. Keyed on conv_id, it catches the live
	// session whatever its PK shape (full-UUID / synthetic / old convID[:8]).
	// The lock is held (defer) until the session row is written and runNew
	// returns; the OS frees it if this process dies. The PK guard below still
	// backstops a reused --label. See JOH-332.
	if fullConvID != "" {
		release, reject := ReserveConvForLaunch(fullConvID)
		if reject != nil {
			return reject
		}
		defer release()
	}

	// The session PK is now final (priority above: label > resumed conv UUID >
	// random synthetic). Reject if a LIVE session already owns it. The tmux
	// name is disambiguated below (UniqueTmuxSessionName), so without this
	// guard SaveSessionState's ON CONFLICT(id) would silently overwrite that
	// live session's row. Before JOH-248 the PK and tmux name were identical,
	// so the duplicate `new-session` clean-failed; this restores that now that
	// the names diverge. A row owned only by a DEAD session is fine to reuse.
	// (This PK guard primarily backstops a reused --label; the resumed-conv
	// case is covered by the conv_id guard above. See JOH-332.)
	owner, err := liveOwnerConflict(sessionID, params.Label)
	if err != nil {
		return err
	}
	if owner != nil {
		return fmt.Errorf("session %s already exists for this conversation; attach with: tclaude session attach %s", owner.TmuxSession, owner.TmuxSession)
	}

	// The tmux session name is the short, human-facing handle (tmux status
	// line, `session ls`, attach target). Render it short here while the PK
	// stays full, and keep it unique among live tmux sessions.
	tmuxSession := UniqueTmuxSessionName(TmuxNameBase(sessionID, params.Label, cwd))

	if params.WaitForRateLimit {
		if ratelimit.WaitForRateLimit(ctx, os.Stdout, sessionID, cwd) {
			return fmt.Errorf("interrupted")
		}
	}

	// Build claude command with all environment variables forwarded
	additionalEnv := map[string]string{
		"TCLAUDE_SESSION_ID": sessionID,
	}
	// Pin managed Codex sessions to agentd's canonical state-free socket. That
	// socket lives outside the profile's denied ~/.tclaude private-state tree.
	if err := ApplyAgentSocketEnv(h.Name, params.Sandbox, params.PermissionProfile, additionalEnv); err != nil {
		return err
	}
	// Keep Claude Code's interactive "Resume from summary" chooser from blocking
	// this detached pane (the daemon forks `tclaude session new -r` here, and a
	// tmux-driven flow can't answer a TUI it didn't expect). No-op for non-Claude
	// harnesses. See ApplyClaudeResumeEnv.
	ApplyClaudeResumeEnv(h, additionalEnv)
	envExports := clcommon.BuildEnvExports(additionalEnv)

	// Sandbox cwd-safety guard: a writable sandbox (Codex workspace-write)
	// confines writes to the cwd subtree, so a cwd at/above $HOME would make
	// ~/.tclaude / ~/.codex / ~/.claude writable and defeat the protection.
	// Refuse that rather than spawn an agent with a false sense of
	// containment. No-op for harnesses/modes that don't write outside cwd.
	// The managed tclaude-agent profile extends :workspace (same cwd-subtree
	// writability), so guard it exactly as workspace-write.
	guardMode := sandboxMode
	if params.PermissionProfile == harness.CodexAgentProfile {
		guardMode = harness.SandboxWorkspaceWrite
	}
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(guardMode, cwd, home) {
		return fmt.Errorf("refusing to launch a %s agent in %q under workspace-write containment (%s): "+
			"that cwd contains your tclaude/Codex/Claude state dirs, which the sandbox would make writable "+
			"(defeating it). Run the agent from a project subdirectory, or use sandbox %s to opt out of the sandbox",
			h.Name, cwd, sandboxDescr(sandboxMode, params.PermissionProfile), harness.SandboxDangerFull)
	}

	// Ensure the managed profile file exists before launch (self-healing —
	// works even if `tclaude setup` was never run). This lives after cwd
	// resolution so the profile can add a narrow write grant for the launch
	// repo's Git common dir, which linked worktrees need for `git commit`.
	// Only the tclaude-owned profile is auto-created; any other name must
	// already be defined by the user's own config.
	if params.PermissionProfile == harness.CodexAgentProfile {
		if _, eerr := harness.EnsureCodexAgentProfileForCwd(cwd); eerr != nil {
			return fmt.Errorf("ensure codex permission profile %q: %w", params.PermissionProfile, eerr)
		}
	}

	// Pre-trust the launch dir for Codex when the operator opted in
	// (--trust-dir), BEFORE the pane starts: Codex reads ~/.codex/config.toml
	// at startup, so the [projects."<cwd>"] trust entry must already be there
	// or the agent freezes on the trust-folder modal (JOH-205). Opt-in only
	// (the early gate guarantees the harness is Codex); atomic + idempotent.
	//
	// Best-effort: pre-trust is an optimisation over the focus-button fallback
	// — if it fails (an FS error, or a config shape the editor refuses to touch
	// rather than corrupt), the agent still launches and the operator can clear
	// the trust-folder modal on the pending pane via the dashboard focus button
	// (Part A). So warn and continue rather than fail the spawn.
	if params.TrustDir && h.Name == harness.CodexName {
		if err := harness.EnsureCodexDirTrusted(cwd); err != nil {
			slog.Warn("could not pre-trust the launch dir for codex; the trust-folder modal may appear — clear it via the dashboard focus button",
				"cwd", cwd, "err", err)
		}
	}

	// A preset --session-id makes the conv-id known before launch (fresh CC
	// only). Stamp it on the session row now — the SessionStart hook reports
	// the same id — so the daemon's launch-enrollment path sees the conv-id
	// immediately rather than polling for the hook. A resume already resolved
	// fullConvID above; an unset id leaves the row's conv-id for the hook.
	rowConvID := fullConvID
	if params.SessionID != "" && fullConvID == "" {
		rowConvID = params.SessionID
	}

	harnessCmd := h.Spawn.BuildCommand(harness.SpawnSpec{
		EnvExports:             envExports,
		ResumeID:               fullConvID,
		SessionID:              params.SessionID,
		Name:                   params.Name,
		Effort:                 effort,
		Model:                  model,
		ExtraArgs:              extraArgs,
		SandboxMode:            sandboxMode,
		AskUserQuestionTimeout: askTimeout,
		PermissionProfile:      params.PermissionProfile,
		ApprovalPolicy:         approvalPolicy,
		AutoReview:             autoReview,
		RemoteControl:          remoteControl,
		InitialPrompt:          params.InitialPrompt,
	})
	if params.CwdWriteProof != "" {
		harnessCmd = guardHarnessCommandWithCwdProof(harnessCmd, params.CwdWriteProof)
	}

	// Create the detached tmux session running the harness command.
	if err := launchDetachedTmuxSession(tmuxSession, cwd, harnessCmd); err != nil {
		return err
	}

	applyTmuxWindowTitle(tmuxSession, sessionID)

	// Enable tmux mouse-wheel scrollback for this session when the harness
	// relies on tmux for history (Codex CLI). Scoped to this session only so
	// Claude Code panes — which own their scrollback — are untouched (JOH-213).
	ConfigureTmuxScrollback(tmuxSession, h)

	// Configure keybindings for session navigation (idempotent)
	ConfigureTmuxKeybindings()

	// Get the PID of claude in the tmux session
	pid := ParsePIDFromTmux(tmuxSession)

	// Create session state (starts as idle, waiting for user input).
	// Tag it with the harness it was spawned under so the tag is set on
	// the row's first write rather than relying on the DB default —
	// today always "claude"; the same line carries "codex" once
	// --harness selects a different harness.
	state := &SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         pid,
		Cwd:         cwd,
		ConvID:      rowConvID,
		Status:      StatusIdle,
		Harness:     h.Name,
		// Record the resolved launch sandbox descriptor so the dashboard can
		// badge it (JOH-162): the --sandbox mode, or — when the agent runs
		// under a managed permission profile (codex -p <name>, the JOH-207
		// path) — the profile name. "" for a harness with no launch sandbox
		// flag (Claude Code). Stored verbatim, never coalesced; this is the
		// only write of the column, so it can't be re-derived later.
		SandboxMode: sandboxDescr(sandboxMode, params.PermissionProfile),
		// Record the resolved AskUserQuestion idle-timeout so a relaunch (resume /
		// clone / reincarnate) can PRESERVE it — inherit/5m/never carried across
		// the handoff instead of reverting to global settings.json (schema v97).
		// "" for an un-chosen ask-timeout or a non-Claude harness. Stored verbatim.
		AskUserQuestionTimeout: askTimeout,
		Created:                time.Now(),
		Updated:                time.Now(),
	}

	if err := SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	if h.Name == harness.CodexName {
		if err := db.UpdateSessionModel(sessionID, model); err != nil {
			slog.Warn("failed to seed Codex session model", "session_id", sessionID, "error", err)
		}
		if err := db.UpdateSessionModelID(sessionID, model); err != nil {
			slog.Warn("failed to seed Codex session model id", "session_id", sessionID, "error", err)
		}
		if err := db.UpdateSessionEffort(sessionID, effort); err != nil {
			slog.Warn("failed to seed Codex session effort", "session_id", sessionID, "error", err)
		}
	}

	return announceAndAttach(fmt.Sprintf("Created session %s", tmuxSession), sessionID, tmuxSession, cwd, params.Detached)
}

// The CC launch-command builder lives behind the harness seam now —
// see claudeSpawner.BuildCommand in pkg/claude/harness/claude.go.

// resolveSessionDir resolves the --dir/-C value to an absolute working
// directory: the current directory when unset, else dir made absolute
// against the current directory. Shared by runNew and runNewShell.
func resolveSessionDir(dir string) (string, error) {
	cwd := dir
	if cwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}
		return cwd, nil
	}
	if cwd[0] != '/' {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}
		cwd = filepath.Join(wd, cwd)
	}
	return cwd, nil
}

// launchDetachedTmuxSession creates the detached tmux session that hosts a
// launch — `tmux new-session -d -s <name> -c <cwd> sh -c <cmd>` — wiring the
// child's stdout/stderr through. cmd is the harness command (claudeSpawner's
// BuildCommand output) or the plain shell's exec line. Shared by runNew and
// runNewShell.
func launchDetachedTmuxSession(tmuxSession, cwd, cmd string) error {
	// Use tmux new-session -d to create detached; sh -c carries the env exports.
	tmuxCmd := clcommon.TmuxCommand("new-session", "-d", "-s", tmuxSession, "-c", cwd, "sh", "-c", cmd)
	tmuxCmd.Stdout = os.Stdout
	tmuxCmd.Stderr = os.Stderr
	if err := tmuxCmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}
	return nil
}

// isValidSpawnCwdProofToken accepts the daemon's base64url token alphabet and
// a conservative length bound before the value enters a sh -c command. The
// daemon-generated token is currently 75 characters; the larger bound leaves
// room for a wire-compatible format revision.
func isValidSpawnCwdProofToken(proof string) bool {
	if len(proof) == 0 || len(proof) > 128 {
		return false
	}
	for _, r := range proof {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// guardHarnessCommandWithCwdProof prefixes the harness command with a marker
// check performed by the shell tmux starts *after* tmux has chdir'd the pane.
// At that point the shell's cwd is an inode, not a path lookup: even if the
// spawning agent renames the directory and replaces the original pathname,
// this process and the exec'd harness remain in the verified directory object.
// A pre-launch pathname swap instead lands the shell in a directory without
// the unpredictable marker, so the harness never starts.
func guardHarnessCommandWithCwdProof(harnessCmd, proof string) string {
	marker := clcommon.SpawnCwdProofPrefix + proof
	quoted := clcommon.ShellQuoteArg(marker)
	return "tclaude_cwd_proof=" + quoted + "; " +
		"if [ ! -f \"$tclaude_cwd_proof\" ] || [ -L \"$tclaude_cwd_proof\" ] || [ -s \"$tclaude_cwd_proof\" ]; then " +
		"echo 'tclaude: spawn cwd write proof missing or invalid; refusing harness launch' >&2; exit 126; fi; " +
		"rm -f -- \"$tclaude_cwd_proof\" || exit 126; " + harnessCmd
}

// applyTmuxWindowTitle sets this session's tmux window title to
// `tclaude:<sessionID>` when config focus.window_title is enabled (default on),
// so the title persists and drives title-based window focus/tiling. An explicit
// false leaves the terminal's own title alone. Best-effort. Shared by both
// launch paths.
func applyTmuxWindowTitle(tmuxSession, sessionID string) {
	if !windowTitleEnabledFn() {
		return
	}
	_ = clcommon.TmuxCommand("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":", "set-titles", "on").Run()
	_ = clcommon.TmuxCommand("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":", "set-titles-string", fmt.Sprintf("tclaude:%s", sessionID)).Run()
}

// announceAndAttach prints the created-session summary and then either reports
// the detach hint (detached launches) or attaches to the pane. createdLine is
// the harness-specific "Created …" headline (a coding session vs a shell
// session). Shared launch tail of runNew and runNewShell.
func announceAndAttach(createdLine, sessionID, tmuxSession, cwd string, detached bool) error {
	fmt.Println(createdLine)
	fmt.Printf("  Directory: %s\n", cwd)

	if detached {
		fmt.Printf("\nAttach with: tclaude session attach %s\n", tmuxSession)
		return nil
	}

	fmt.Println("\nAttaching... (Ctrl+B D to detach)")
	return AttachToSession(sessionID, tmuxSession, false)
}

// resolveResumeConv resolves a --resume id prefix to a full conversation
// id + its project path, using the harness's own conversation source:
// Claude Code keeps the established cwd-indexed resolver
// (clcommon.ResolveConvID, unchanged); any other harness resolves through
// its ConvStore (Codex reads its rollout files + state DB, not
// ~/.claude/projects). A conversation that doesn't resolve is an error;
// `global` widens the search beyond the current project.
func resolveResumeConv(h *harness.Harness, shortID string, global bool, cwd string) (fullConvID, projectPath string, err error) {
	if h.Name == harness.DefaultName {
		convInfo := clcommon.ResolveConvID(shortID, global, cwd)
		if convInfo == nil {
			return "", "", resumeNotFoundErr(shortID, global)
		}
		return convInfo.SessionID, convInfo.ProjectPath, nil
	}
	if !h.SupportsConvs() {
		return "", "", fmt.Errorf("harness %q cannot resolve a conversation to resume", h.Name)
	}
	ref, err := h.Convs.Resolve(shortID, cwd, global)
	if err != nil {
		return "", "", err
	}
	if ref == nil {
		return "", "", resumeNotFoundErr(shortID, global)
	}
	return ref.ConvID, ref.ProjectPath, nil
}

func resumeNotFoundErr(shortID string, global bool) error {
	if global {
		return fmt.Errorf("conversation %s not found", shortID)
	}
	return fmt.Errorf("conversation %s not found in current project (use -g to search all projects)", shortID)
}
