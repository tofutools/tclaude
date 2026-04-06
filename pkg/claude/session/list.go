package session

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
	tbl "github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
	"golang.org/x/term"
)

type ListParams struct {
	JSON  bool     `long:"json" help:"Output as JSON"`
	All   bool     `short:"a" long:"all" help:"Include exited sessions"`
	Watch bool     `short:"w" long:"watch" help:"Interactive watch mode with auto-refresh"`
	Sort  string   `short:"s" long:"sort" optional:"true" help:"Sort by column" alts:"id,directory,status,age,updated"`
	Asc   bool     `long:"asc" help:"Sort ascending (default for id/directory/status)"`
	Desc  bool     `long:"desc" help:"Sort descending (default for updated)"`
	Show  []string `long:"show" optional:"true" help:"Only show these statuses" alts:"all,idle,working,awaiting_permission,awaiting_input,exited"`
	Hide  []string `long:"hide" optional:"true" help:"Hide these statuses" alts:"idle,working,awaiting_permission,awaiting_input,exited"`
}

func ListCmd() *cobra.Command {
	return boa.CmdT[ListParams]{
		Use:         "ls",
		Aliases:     []string{"list", "status"},
		Short:       "List Claude Code sessions",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *ListParams, cmd *cobra.Command, args []string) {
			if err := runList(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runList(params *ListParams) error {
	// Parse sort options
	sortState := parseSortParams(params.Sort, params.Asc, params.Desc)

	// Normalize status filters
	showFilter := normalizeStatusFilter(params.Show)
	hideFilter := normalizeStatusFilter(params.Hide)

	// Interactive watch mode
	if params.Watch {
		return RunWatchMode(params.All, sortState, showFilter, hideFilter)
	}

	states, err := ListSessionStates()
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(states) == 0 {
		fmt.Println("No sessions found")
		fmt.Println("\nStart a new session with: tclaude session new")
		return nil
	}

	// Refresh status and filter
	var filtered []*SessionState
	for _, state := range states {
		RefreshSessionStatus(state)
		if !params.All && state.Status == StatusExited {
			continue
		}
		if !matchesShowFilter(state.Status, showFilter) {
			continue
		}
		if matchesHideFilter(state.Status, hideFilter) {
			continue
		}
		filtered = append(filtered, state)
	}

	if len(filtered) == 0 {
		fmt.Println("No active sessions found")
		fmt.Println("\nStart a new session with: tclaude session new")
		return nil
	}

	// Apply sorting
	SortSessionsByKey(filtered, sortState.Key, sortState.Direction)

	if params.JSON {
		data, err := json.MarshalIndent(filtered, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	// Render table
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleLight)
	t.SetAllowedRowLength(getTermWidth())

	t.AppendHeader(table.Row{"", "ID", "Directory", "Status", "Age", "Updated"})

	for _, state := range filtered {
		colorFunc := getStatusColorFunc(state.Status)
		status := state.Status
		if state.StatusDetail != "" {
			status = status + ": " + state.StatusDetail
		}

		// Determine indicator: ⚡ = attached tmux, ▷ = detached tmux, ◉ = non-tmux/dead tmux
		indicator := "  "
		tmuxAlive := state.TmuxSession != "" && IsTmuxSessionAlive(state.TmuxSession)
		if !tmuxAlive {
			indicator = " ◉"
		} else if state.Attached > 0 {
			indicator = "⚡"
		} else {
			indicator = " ▷"
		}

		t.AppendRow(table.Row{
			colorFunc(indicator),
			colorFunc(state.ID),
			colorFunc(shortenPathForTable(state.Cwd, 40)),
			colorFunc(status),
			colorFunc(FormatDuration(time.Since(state.Created))),
			colorFunc(FormatDuration(time.Since(state.Updated))),
		})
	}

	t.Render()

	// Show hint for attaching
	fmt.Printf("\nAttach with: tclaude session attach <id>\n")

	return nil
}

func getTermWidth() int {
	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		return width
	}
	if width, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && width > 0 {
		return width
	}
	return 120
}

func shortenPathForTable(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	return "…" + path[len(path)-maxLen+1:]
}

func getStatusColorFunc(status string) func(a ...interface{}) string {
	switch status {
	case StatusIdle:
		return text.FgYellow.Sprint
	case StatusMainAgentIdle:
		return text.FgGreen.Sprint
	case StatusWorking:
		return text.FgGreen.Sprint
	case StatusAwaitingPermission, StatusAwaitingInput:
		return text.FgHiRed.Sprint
	case StatusExited:
		return text.FgHiBlack.Sprint
	default:
		return text.FgHiRed.Sprint // Unknown status = needs attention
	}
}

func parseSortParams(sortBy string, asc, desc bool) tbl.SortState {
	var key string
	switch sortBy {
	case "id":
		key = "id"
	case "directory", "dir":
		key = "project"
	case "status":
		key = "status"
	case "age", "created":
		key = "updated" // Map age/created to updated (age column was removed)
	case "updated", "time":
		key = "updated"
	default:
		key = ""
	}

	// Default direction depends on column
	// Time-based columns default to descending (most recent first)
	// Others default to ascending
	var dir tbl.SortDirection
	if asc {
		dir = tbl.SortAsc
	} else if desc {
		dir = tbl.SortDesc
	} else if key == "updated" {
		dir = tbl.SortDesc // Smart default for time
	} else {
		dir = tbl.SortAsc
	}

	return tbl.SortState{Key: key, Direction: dir}
}

// normalizeStatusFilter converts user-friendly names to internal status constants
func normalizeStatusFilter(show []string) []string {
	if len(show) == 0 {
		return nil
	}

	var result []string
	for _, s := range show {
		switch s {
		case "all":
			return nil // no filter
		case "idle":
			result = append(result, StatusIdle)
		case "working":
			result = append(result, StatusWorking, StatusMainAgentIdle)
		case "awaiting_permission", "permission":
			result = append(result, StatusAwaitingPermission)
		case "awaiting_input", "input":
			result = append(result, StatusAwaitingInput)
		case "attention":
			result = append(result, StatusAwaitingPermission, StatusAwaitingInput)
		case "exited":
			result = append(result, StatusExited)
		default:
			// Allow passing internal status names directly
			result = append(result, s)
		}
	}
	return result
}

// matchesShowFilter checks if a status should be shown (empty = show all)
func matchesShowFilter(status string, filter []string) bool {
	if len(filter) == 0 {
		return true // no filter = show all
	}
	for _, f := range filter {
		if f == status {
			return true
		}
	}
	return false
}

// matchesHideFilter checks if a status should be hidden (empty = hide none)
func matchesHideFilter(status string, filter []string) bool {
	if len(filter) == 0 {
		return false // no filter = hide nothing
	}
	for _, f := range filter {
		if f == status {
			return true
		}
	}
	return false
}
