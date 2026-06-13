package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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
	Dir              string `short:"C" long:"dir" optional:"true" help:"Directory to start session in (defaults to current directory)"`
	Resume           string `long:"resume" short:"r" optional:"true" help:"Resume an existing conversation by ID"`
	Global           bool   `short:"g" help:"Search for conversation across all projects (with --resume)"`
	Label            string `long:"label" optional:"true" help:"Custom label for the session"`
	Detached         bool   `long:"detached" short:"d" help:"Start detached (don't attach to session)"`
	Compact          int    `long:"compact" optional:"true" help:"Auto-compact at this context usage percentage (overrides config)"`
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
	// rest of runNew stays harness-agnostic.
	Harness string `long:"harness" optional:"true" help:"Coding harness to launch: claude (default) | codex"`

	// Sandbox selects a harness's launch-time OS-sandbox mode (Codex's
	// --sandbox). On a direct `session new` it is opt-in: unset emits no
	// flag, so Codex uses the user's own config.toml sandbox_mode (the human
	// running session new is the trust root — tclaude doesn't override their
	// config). Pass a value to sandbox explicitly. The daemon spawn path
	// (agentd / `agent spawn`) defaults it to workspace-write instead, since
	// a spawned agent is the untrusted party. Not applicable to Claude Code
	// (settings.json-driven), which errors if it is set. See JOH-192.
	Sandbox string `long:"sandbox" optional:"true" help:"Codex OS-sandbox mode: read-only|workspace-write|danger-full-access. Unset = no flag (Codex uses your config.toml). Not applicable to claude"`

	// Approval selects a harness's launch-time approval policy (Codex's
	// --ask-for-approval). On a direct `session new` it is opt-in: unset emits
	// no flag, so Codex uses the user's own config.toml (the human running
	// session new is the trust root and can attach to answer prompts — tclaude
	// doesn't force a policy on them). The daemon spawn path (agentd / `agent
	// spawn`) defaults it to the non-escalating `never` instead, since its pane
	// is detached/unattended and would otherwise deadlock; that resolved value
	// arrives here as an explicit --ask-for-approval. Not applicable to Claude
	// Code (settings.json-driven), which errors if it is set. See JOH-200.
	Approval string `long:"ask-for-approval" optional:"true" help:"Codex approval policy: untrusted|on-failure|on-request|never. Unset = no flag (Codex uses your config.toml). Not applicable to claude"`

	// --join-group makes the new session auto-join an existing agent group
	// the moment its conv-id materialises. Routed through the daemon's
	// `groups.spawn` orchestration; not compatible with --resume / --label.
	JoinGroup string `long:"join-group" optional:"true" help:"Spawn the session and add it to an existing agent group (shorthand for agent spawn + foreground attach)"`
	Name      string `long:"name" optional:"true" help:"Name for the new agent in --join-group (e.g. 'reviewer'); becomes its conversation title"`
	Role      string `long:"role" optional:"true" help:"Role tag for the new member in --join-group (e.g. 'tech-lead')"`
	Descr     string `long:"descr" optional:"true" help:"Description of the new member's purpose in --join-group"`
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

// JoinGroupHandler implements `--join-group`. Set by the agent package's
// init() to avoid a session→agent import cycle (agent already depends on
// session for AttachToSession). When nil, --join-group falls back to a
// clear error.
var JoinGroupHandler func(*NewParams) error

func runNew(params *NewParams) error {
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

	// Validate --sandbox up front WITHOUT defaulting it: a direct
	// `tclaude session new` is the human's own session, and the human is the
	// trust root — tclaude must not silently override their config.toml
	// sandbox_mode, so we emit --sandbox only when they pass it explicitly.
	// (The daemon spawn path is where the workspace-write default belongs —
	// an agentd-spawned agent is the untrusted party — and it threads the
	// resolved mode in as an explicit --sandbox.) An explicit mode for a
	// harness without a launch sandbox flag (Claude Code) errors here. The
	// cwd-safety check needs the resolved cwd, so it happens later.
	sandboxMode, err := harness.ValidateSandboxMode(h, params.Sandbox)
	if err != nil {
		return err
	}
	params.Sandbox = sandboxMode

	// Validate --ask-for-approval up front WITHOUT defaulting it, for the same
	// trust-root reason as --sandbox above: a direct `tclaude session new` is
	// the human's own session and they can attach to answer prompts, so tclaude
	// emits --ask-for-approval only when they pass it explicitly. The daemon
	// spawn path is where the non-escalating `never` default belongs (its pane
	// is unattended) and it threads the resolved policy in as an explicit flag.
	// An explicit policy for a harness without a launch approval flag (Claude
	// Code) errors here.
	approvalPolicy, err := harness.ValidateApprovalPolicy(h, params.Approval)
	if err != nil {
		return err
	}
	params.Approval = approvalPolicy

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

	// Determine working directory
	cwd := params.Dir
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	// Make path absolute
	if cwd[0] != '/' {
		wd, _ := os.Getwd()
		cwd = wd + "/" + cwd
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

	// Generate session ID (use short prefix for our tracking)
	// Priority: explicit label > conv ID prefix (when resuming) > random
	sessionID := GenerateSessionID()
	if shortID != "" {
		// Use conv ID prefix as session ID for easy association
		sessionID = shortID
		if len(sessionID) > 8 {
			sessionID = sessionID[:8]
		}
	}
	if params.Label != "" {
		sessionID = params.Label
	}
	tmuxSession := sessionID

	if params.WaitForRateLimit {
		if ratelimit.WaitForRateLimit(ctx, os.Stdout, sessionID, cwd) {
			return fmt.Errorf("interrupted")
		}
	}

	// Build claude command with all environment variables forwarded
	additionalEnv := map[string]string{
		"TCLAUDE_SESSION_ID": sessionID,
	}
	if params.Compact > 0 {
		additionalEnv["TCLAUDE_AUTO_COMPACT"] = fmt.Sprintf("%d", params.Compact)
	}
	envExports := clcommon.BuildEnvExports(additionalEnv)

	// Sandbox cwd-safety guard: a writable sandbox (Codex workspace-write)
	// confines writes to the cwd subtree, so a cwd at/above $HOME would make
	// ~/.tclaude / ~/.codex / ~/.claude writable and defeat the protection.
	// Refuse that rather than spawn an agent with a false sense of
	// containment. No-op for harnesses/modes that don't write outside cwd.
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(sandboxMode, cwd, home) {
		return fmt.Errorf("refusing to launch a %s agent in %q under --sandbox %s: "+
			"that cwd contains your tclaude/Codex/Claude state dirs, which the sandbox would make writable "+
			"(defeating it). Run the agent from a project subdirectory, or pass --sandbox %s to opt out of the sandbox",
			h.Name, cwd, sandboxMode, harness.SandboxDangerFull)
	}

	claudeCmd := h.Spawn.BuildCommand(harness.SpawnSpec{
		EnvExports:     envExports,
		ResumeID:       fullConvID,
		Effort:         effort,
		Model:          model,
		ExtraArgs:      extraArgs,
		SandboxMode:    sandboxMode,
		ApprovalPolicy: approvalPolicy,
	})

	// Create tmux session with claude
	// Use tmux new-session -d to create detached
	// We use sh -c to set the environment variable
	tmuxArgs := []string{
		"new-session",
		"-d",              // detached
		"-s", tmuxSession, // session name
		"-c", cwd, // working directory
		"sh", "-c", claudeCmd,
	}

	tmuxCmd := clcommon.TmuxCommand(tmuxArgs...)
	tmuxCmd.Stdout = os.Stdout
	tmuxCmd.Stderr = os.Stderr

	if err := tmuxCmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Configure tmux to set window title with our session ID
	// This ensures the title persists and is visible for window focus
	_ = clcommon.TmuxCommand("set-option", "-t", tmuxSession, "set-titles", "on").Run()
	_ = clcommon.TmuxCommand("set-option", "-t", tmuxSession, "set-titles-string", fmt.Sprintf("tclaude:%s", sessionID)).Run()

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
		ConvID:      fullConvID,
		Status:      StatusIdle,
		Harness:     h.Name,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	if err := SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	fmt.Printf("Created session %s\n", sessionID)
	fmt.Printf("  Directory: %s\n", cwd)

	if params.Detached {
		fmt.Printf("\nAttach with: tclaude session attach %s\n", sessionID)
		return nil
	}

	fmt.Println("\nAttaching... (Ctrl+B D to detach)")
	return AttachToSession(sessionID, tmuxSession, false)
}

// The CC launch-command builder lives behind the harness seam now —
// see claudeSpawner.BuildCommand in pkg/claude/harness/claude.go.

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
