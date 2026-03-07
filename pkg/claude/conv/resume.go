package conv

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type ResumeParams struct {
	ConvID   string `pos:"true" help:"Conversation ID to resume (can be short prefix)"`
	Global   bool   `short:"g" help:"Search for conversation across all projects"`
	Detached bool   `short:"d" long:"detached" help:"Start detached (don't attach to session)"`
	NoTmux   bool   `long:"no-tmux" help:"Run without tmux session management (old behavior)"`
}

func ResumeCmd() *cobra.Command {
	return boa.CmdT[ResumeParams]{
		Use:         "resume",
		Short:       "Resume a Claude Code conversation",
		Long:        "Resume a Claude Code conversation by ID. Finds the conversation, changes to its project directory, and launches claude --resume.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *ResumeParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			// Check if -g flag is set (p.Global may not be populated during completion)
			global, _ := cmd.Flags().GetBool("global")
			return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *ResumeParams, cmd *cobra.Command, args []string) {
			exitCode := RunResume(params, os.Stdout, os.Stderr)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunResume(params *ResumeParams, stdout, stderr *os.File) int {
	// Extract just the ID from autocomplete format (e.g., "0459cd73_[myproject]_prompt..." -> "0459cd73")
	convID := clcommon.ExtractIDFromCompletion(params.ConvID)

	// Get current directory for local search
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
		return 1
	}

	// Resolve conversation ID to full info
	convInfo := clcommon.ResolveConvID(convID, params.Global, cwd)
	if convInfo == nil {
		fmt.Fprintf(stderr, "Conversation %s not found\n", convID)
		if !params.Global {
			fmt.Fprintf(stderr, "Hint: use -g to search all projects\n")
		}
		return 1
	}

	projectPath := convInfo.ProjectPath

	// Show what we're doing
	displayName := convInfo.DisplayTitle
	if displayName == "" {
		displayName = convInfo.FirstPrompt
	}
	if len(displayName) > 50 {
		displayName = displayName[:47] + "..."
	}

	// Use session management by default (unless --no-tmux)
	if !params.NoTmux {
		return runResumeWithSession(convInfo, projectPath, displayName, !params.Detached, stdout, stderr)
	}

	fmt.Fprintf(stdout, "Resuming [%s] in %s\n\n", displayName, projectPath)

	// Run claude --resume as a subprocess with connected I/O
	claudeArgs := append([]string{"--resume", convInfo.SessionID}, clcommon.ExtractClaudeExtraArgs()...)
	cmd := exec.Command("claude", claudeArgs...)
	cmd.Dir = projectPath
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "Error running claude: %v\n", err)
		return 1
	}

	return 0
}

func runResumeWithSession(convInfo *clcommon.ConvInfo, projectPath, displayName string, attach bool, stdout, stderr *os.File) int {
	// Check tmux is installed
	if err := session.CheckTmuxInstalled(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Check if hooks are installed (warn if not)
	session.EnsureHooksInstalled(false, stdout, stderr)

	// Use conv ID prefix as session ID
	sessionID := convInfo.SessionID
	if len(sessionID) > 8 {
		sessionID = sessionID[:8]
	}

	// Check if session already exists
	existing, _ := session.LoadSessionState(sessionID)
	if existing != nil && session.IsTmuxSessionAlive(existing.TmuxSession) {
		fmt.Fprintf(stderr, "Session %s already exists for this conversation\n", sessionID)
		fmt.Fprintf(stderr, "Attach with: tclaude session attach %s\n", sessionID)
		return 1
	}

	tmuxSession := "tclaude-" + sessionID

	// Build claude command with TCLAUDE_SESSION_ID env var
	claudeCmd := fmt.Sprintf("TCLAUDE_SESSION_ID=%s claude --resume %s", sessionID, convInfo.SessionID)
	if extraArgs := clcommon.ExtractClaudeExtraArgs(); len(extraArgs) > 0 {
		quoted := make([]string, len(extraArgs))
		for i, a := range extraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		claudeCmd += " " + strings.Join(quoted, " ")
	}

	// Create tmux session
	tmuxArgs := []string{
		"new-session",
		"-d",
		"-s", tmuxSession,
		"-c", projectPath,
		"sh", "-c", claudeCmd,
	}

	tmuxCmd := exec.Command("tmux", tmuxArgs...)
	if err := tmuxCmd.Run(); err != nil {
		fmt.Fprintf(stderr, "Failed to create tmux session: %v\n", err)
		return 1
	}

	// Get PID and save state (starts as idle, waiting for user input)
	pid := session.ParsePIDFromTmux(tmuxSession)
	state := &session.SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         pid,
		Cwd:         projectPath,
		ConvID:      convInfo.SessionID,
		Status:      session.StatusIdle,
		Created:     time.Now(),
	}

	if err := session.SaveSessionState(state); err != nil {
		fmt.Fprintf(stderr, "Failed to save session state: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Resuming [%s] in session %s\n", displayName, sessionID)
	fmt.Fprintf(stdout, "  Directory: %s\n", projectPath)

	if attach {
		fmt.Fprintf(stdout, "\nAttaching... (Ctrl+B D to detach)\n")
		return session.AttachToTmuxSession(tmuxSession)
	}

	fmt.Fprintf(stdout, "\nAttach with: tclaude session attach %s\n", sessionID)
	return 0
}
