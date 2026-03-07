package session

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type FocusParams struct {
	ID string `pos:"true" help:"Session ID to focus"`
}

func FocusCmd() *cobra.Command {
	return boa.CmdT[FocusParams]{
		Use:         "focus <id>",
		Short:       "Focus a Claude Code session's terminal window",
		Long:        "Attempts to focus the terminal window for a session. If not found, opens a new window and attaches.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *FocusParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return GetSessionCompletions(false), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *FocusParams, cmd *cobra.Command, args []string) {
			if err := runFocus(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runFocus(params *FocusParams) error {
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

	// Try to focus the window
	fmt.Printf("Focusing session %s...\n", state.ID)

	// Set the session ID for other functions that may need it
	os.Setenv("TCLAUDE_SESSION_ID", state.ID)

	// Try to focus the terminal running this session
	TryFocusAttachedSession(state.TmuxSession)

	return nil
}
