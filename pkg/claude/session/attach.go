package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type AttachParams struct {
	ID    string `pos:"true" help:"Session ID to attach to"`
	Force bool   `short:"f" long:"force" help:"Attach even if session already has clients attached"`
}

func AttachCmd() *cobra.Command {
	return boa.CmdT[AttachParams]{
		Use:         "attach <id>",
		Short:       "Attach to a Claude Code session",
		Long:        "Attach to an existing Claude Code session. Use Ctrl+B D to detach.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *AttachParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return GetSessionCompletions(false), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *AttachParams, cmd *cobra.Command, args []string) {
			if err := runAttach(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runAttach(params *AttachParams) error {
	if params.ID == "" {
		return fmt.Errorf("session ID required")
	}

	// Extract just the ID from completion format
	sessionID := clcommon.ExtractIDFromCompletion(params.ID)

	// Find matching session
	state, err := findSession(sessionID)
	if err != nil {
		return err
	}

	// Check if session is alive
	if !IsTmuxSessionAlive(state.TmuxSession) {
		state.Status = StatusExited
		SaveSessionState(state)
		return fmt.Errorf("session %s has exited", state.ID)
	}

	// By default, don't attach if session already has clients (use --force to override)
	if !params.Force && IsTmuxSessionAttached(state.TmuxSession) {
		fmt.Printf("Session %s is already attached in another terminal\n", state.ID)
		// Try to focus the terminal window
		TryFocusAttachedSession(state.TmuxSession)
		return nil
	}

	fmt.Printf("Attaching to session %s... (Ctrl+B D to detach)\n", state.ID)
	return AttachToSession(state.ID, state.TmuxSession, params.Force)
}

// AttachToTmuxSession attaches to a tmux session, replacing the current process
// Returns exit code (0 = success) for use by other packages
func AttachToTmuxSession(tmuxSession string) int {
	if err := attachToSession(tmuxSession); err != nil {
		return 1
	}
	return 0
}

// AttachToSession attaches to a tmux session.
// Sets terminal title for window focus, then replaces process with tmux attach.
// If forceAttach is true, detaches other clients before attaching (-d flag).
func AttachToSession(sessionID, tmuxSession string, forceAttach bool) error {
	// Set TCLAUDE_SESSION_ID so focus functions can find our session
	os.Setenv("TCLAUDE_SESSION_ID", sessionID)

	// Set terminal title to include session ID (helps with window focus on WSL/Windows)
	setTerminalTitle(fmt.Sprintf("tclaude:%s", sessionID))

	// Attach to tmux session (replaces current process)
	return attachToSessionWithFlags(tmuxSession, forceAttach)
}

// setTerminalTitle sets the terminal window/tab title using escape sequences.
// This is used to identify our terminal window for focus operations.
func setTerminalTitle(title string) {
	// OSC 0 sets both icon and window title
	// Format: ESC ] 0 ; <title> BEL
	fmt.Printf("\033]0;%s\007", title)
}

// findSession finds a session by ID or prefix
func findSession(id string) (*SessionState, error) {
	states, err := ListSessionStates()
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	// Try exact match first
	for _, state := range states {
		if state.ID == id {
			return state, nil
		}
	}

	// Try prefix match
	var matches []*SessionState
	for _, state := range states {
		if strings.HasPrefix(state.ID, id) {
			matches = append(matches, state)
		}
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("no session found with ID %s", id)
	}
	if len(matches) > 1 {
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, fmt.Errorf("ambiguous ID %s, matches: %v", id, ids)
	}

	return matches[0], nil
}
