package session

import (
	"fmt"
	"os"
	"sort"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type GotoParams struct {
	Direction string `pos:"true" help:"Direction: next or prev"`
}

func GotoCmd() *cobra.Command {
	return boa.CmdT[GotoParams]{
		Use:         "goto <next|prev>",
		Short:       "Focus the next or previous session's terminal window",
		Long:        "Cycles through alive, attached sessions and focuses the terminal window of the next or previous one relative to the current session (identified by $TCLAUDE_SESSION_ID).",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *GotoParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return []string{"next", "prev"}, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *GotoParams, cmd *cobra.Command, args []string) {
			if err := runGoto(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runGoto(params *GotoParams) error {
	if params.Direction != "next" && params.Direction != "prev" {
		return fmt.Errorf("direction must be 'next' or 'prev', got %q", params.Direction)
	}

	currentID := os.Getenv("TCLAUDE_SESSION_ID")
	if currentID == "" {
		return fmt.Errorf("not inside a tclaude session ($TCLAUDE_SESSION_ID not set)")
	}

	// Get alive sessions that have a terminal attached
	states, err := ListSessionStates()
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	var alive []*SessionState
	for _, s := range states {
		if s.TmuxSession != "" && IsTmuxSessionAlive(s.TmuxSession) && IsTmuxSessionAttached(s.TmuxSession) {
			alive = append(alive, s)
		}
	}

	if len(alive) < 2 {
		return fmt.Errorf("no other attached sessions to switch to")
	}

	// Sort by ID for stable ordering
	sort.Slice(alive, func(i, j int) bool {
		return alive[i].ID < alive[j].ID
	})

	// Find current session index
	currentIdx := -1
	for i, s := range alive {
		if s.ID == currentID {
			currentIdx = i
			break
		}
	}

	if currentIdx == -1 {
		return fmt.Errorf("current session %s not found among attached sessions", currentID)
	}

	// Pick next or prev (wrapping around)
	var targetIdx int
	if params.Direction == "next" {
		targetIdx = (currentIdx + 1) % len(alive)
	} else {
		targetIdx = (currentIdx - 1 + len(alive)) % len(alive)
	}

	target := alive[targetIdx]
	TryFocusAttachedSession(target.TmuxSession)
	return nil
}
