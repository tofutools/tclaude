package session

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
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

// tmuxClient is a lightweight representation of an attached tmux client.
type tmuxClient struct {
	session string
	tty     string
}

func runGoto(params *GotoParams) error {
	if params.Direction != "next" && params.Direction != "prev" {
		return fmt.Errorf("direction must be 'next' or 'prev', got %q", params.Direction)
	}

	currentID := os.Getenv("TCLAUDE_SESSION_ID")
	if currentID == "" {
		return fmt.Errorf("not inside a tclaude session ($TCLAUDE_SESSION_ID not set)")
	}

	// Single tmux call to get all attached clients with their session and TTY
	cmd := clcommon.TmuxCommand("list-clients", "-F", "#{session_name} #{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list tmux clients: %w", err)
	}

	var clients []tmuxClient
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			clients = append(clients, tmuxClient{session: parts[0], tty: parts[1]})
		}
	}

	if len(clients) < 2 {
		return nil
	}

	// Sort by session name for stable ordering
	sort.Slice(clients, func(i, j int) bool {
		return clients[i].session < clients[j].session
	})

	// Find current session index
	currentIdx := -1
	for i, c := range clients {
		if c.session == currentID {
			currentIdx = i
			break
		}
	}

	if currentIdx == -1 {
		return fmt.Errorf("current session %s not found among attached clients", currentID)
	}

	// Pick next or prev (wrapping around)
	var targetIdx int
	if params.Direction == "next" {
		targetIdx = (currentIdx + 1) % len(clients)
	} else {
		targetIdx = (currentIdx - 1 + len(clients)) % len(clients)
	}

	target := clients[targetIdx]
	focusTTY(target.tty)
	return nil
}
