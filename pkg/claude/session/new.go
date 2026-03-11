package session

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/common"
)

type NewParams struct {
	Dir      string `short:"C" long:"dir" optional:"true" help:"Directory to start session in (defaults to current directory)"`
	Resume   string `long:"resume" short:"r" optional:"true" help:"Resume an existing conversation by ID"`
	Global   bool   `short:"g" help:"Search for conversation across all projects (with --resume)"`
	Label    string `long:"label" optional:"true" help:"Custom label for the session"`
	Detached bool   `long:"detached" short:"d" help:"Start detached (don't attach to session)"`
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
	cmd.RegisterFlagCompletionFunc("resume", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Check if -g flag is set (params may not be populated during completion)
		global, _ := cmd.Flags().GetBool("global")
		return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
	})

	return cmd
}

// RunNew is the exported entry point for running the new session command
func RunNew(params *NewParams) error {
	return runNew(params)
}

func runNew(params *NewParams) error {
	extraArgs := clcommon.ExtractClaudeExtraArgs()

	// Pass-through mode: --help, --version etc. — run claude directly, no tmux.
	if clcommon.ShouldRunClaudeDirect(extraArgs) {
		cmd := exec.Command("claude", extraArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
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

	// Build claude command with TCLAUDE_SESSION_ID env var
	claudeCmd := fmt.Sprintf("TCLAUDE_SESSION_ID=%s claude", sessionID)
	if fullConvID != "" {
		claudeCmd += " --resume " + fullConvID
	}
	if len(extraArgs) > 0 {
		quoted := make([]string, len(extraArgs))
		for i, a := range extraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		claudeCmd += " " + strings.Join(quoted, " ")
	}

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
	clcommon.TmuxCommand("set-option", "-t", tmuxSession, "set-titles", "on").Run()
	clcommon.TmuxCommand("set-option", "-t", tmuxSession, "set-titles-string", fmt.Sprintf("tclaude:%s", sessionID)).Run()

	// Get the PID of claude in the tmux session
	pid := ParsePIDFromTmux(tmuxSession)

	// Create session state (starts as idle, waiting for user input)
	state := &SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         pid,
		Cwd:         cwd,
		ConvID:      fullConvID,
		Status:      StatusIdle,
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
