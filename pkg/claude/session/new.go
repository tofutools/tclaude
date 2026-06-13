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
	// `tclaude session new` launches a Claude Code session, so the spawn
	// command, model/effort validation and resume form all come from the
	// claude harness behind the seam (pkg/claude/harness). When a
	// `--harness` flag lands, this resolves the requested harness instead.
	h := harness.Default()

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

	// Resolve to full UUID and get project path
	var fullConvID string
	var convProjectPath string
	if shortID != "" {
		convInfo := clcommon.ResolveConvID(shortID, params.Global, cwd)
		if convInfo != nil {
			fullConvID = convInfo.SessionID
			convProjectPath = convInfo.ProjectPath
		} else {
			if params.Global {
				return fmt.Errorf("conversation %s not found", shortID)
			}
			return fmt.Errorf("conversation %s not found in current project (use -g to search all projects)", shortID)
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

	claudeCmd := h.Spawn.BuildCommand(harness.SpawnSpec{
		EnvExports: envExports,
		ResumeID:   fullConvID,
		Effort:     effort,
		Model:      model,
		ExtraArgs:  extraArgs,
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
