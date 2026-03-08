package conv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type ListParams struct {
	Dir    string `short:"C" long:"dir" optional:"true" help:"Directory to list conversations from (defaults to current directory)"`
	Global bool   `short:"g" help:"List conversations from all projects"`
	SortBy string `long:"sort-by" help:"Sort by: created, modified, messages, prompt, project" default:"modified"`
	Asc    bool   `long:"asc" help:"Sort ascending (default is descending)"`
	Long   bool   `short:"l" help:"Show detailed output"`
	Limit  int    `short:"n" help:"Limit number of results (0 = no limit)" default:"0"`
	JSON   bool   `long:"json" help:"Output as JSON"`
	Count  bool   `short:"c" long:"count" help:"Only output the count of conversations"`
	Since  string `long:"since" optional:"true" help:"Only include conversations modified after this time (e.g., 2024-01-15, 1h30m, 7d)"`
	Before string `long:"before" optional:"true" help:"Only include conversations modified before this time (e.g., 2024-01-15, 1h30m, 7d)"`
	Watch   bool `short:"w" long:"watch" help:"Interactive watch mode with search and session management"`
	Verbose bool `short:"v" long:"verbose" help:"Show debug info (stale scan stats, timing)"`
	Reindex bool `long:"reindex" help:"Force rescan all conversations from .jsonl files and update index"`
}

func ListCmd() *cobra.Command {
	return boa.CmdT[ListParams]{
		Use:         "list",
		Aliases:     []string{"ls"},
		Short:       "List Claude conversations in a directory",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *ListParams, cmd *cobra.Command, args []string) {
			exitCode := RunList(params, os.Stdout, os.Stderr)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunList(params *ListParams, stdout, stderr *os.File) int {
	DebugLog = params.Verbose

	// Watch mode
	if params.Watch {
		if err := RunConvWatchMode(params.Global, params.Since, params.Before); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}

	var allEntries []SessionEntry
	loadOpts := LoadSessionsIndexOptions{ForceRescan: params.Reindex}

	if params.Global {
		// List all projects
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading projects directory: %v\n", err)
			return 1
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			projectPath := filepath.Join(projectsDir, entry.Name())
			index, err := LoadSessionsIndexWithOptions(projectPath, loadOpts)
			if err != nil {
				continue // Skip projects with errors
			}
			allEntries = append(allEntries, index.Entries...)
		}
	} else {
		// Single directory
		targetDir := params.Dir
		if targetDir == "" {
			var err error
			targetDir, err = os.Getwd()
			if err != nil {
				fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
				return 1
			}
		}

		projectPath := GetClaudeProjectPath(targetDir)

		// Check if project directory exists
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			fmt.Fprintf(stderr, "No Claude conversations found for %s\n", targetDir)
			return 1
		}

		// Load index
		index, err := LoadSessionsIndexWithOptions(projectPath, loadOpts)
		if err != nil {
			fmt.Fprintf(stderr, "Error loading sessions index: %v\n", err)
			return 1
		}

		allEntries = index.Entries
	}

	// Filter by time if specified
	allEntries, err := FilterEntriesByTime(allEntries, params.Since, params.Before)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	// Count output (before limit is applied)
	if params.Count {
		fmt.Fprintf(stdout, "%d\n", len(allEntries))
		return 0
	}

	if len(allEntries) == 0 {
		fmt.Fprintf(stdout, "No conversations found\n")
		return 0
	}

	// Sort entries
	sortEntries(allEntries, params.SortBy, params.Asc)

	// Apply limit
	if params.Limit > 0 && params.Limit < len(allEntries) {
		allEntries = allEntries[:params.Limit]
	}

	// JSON output
	if params.JSON {
		data, err := json.MarshalIndent(allEntries, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "Error marshaling JSON: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	// Display using table
	RenderTable(stdout, allEntries, params.Global, params.Long, nil)

	return 0
}

func sortEntries(entries []SessionEntry, sortBy string, asc bool) {
	sort.Slice(entries, func(i, j int) bool {
		var less bool
		switch sortBy {
		case "created":
			less = entries[i].Created < entries[j].Created
		case "messages":
			less = entries[i].MessageCount < entries[j].MessageCount
		case "prompt":
			less = strings.ToLower(entries[i].FirstPrompt) < strings.ToLower(entries[j].FirstPrompt)
		case "project":
			less = entries[i].ProjectPath < entries[j].ProjectPath
		default: // modified
			less = entries[i].Modified < entries[j].Modified
		}
		if asc {
			return less
		}
		return !less
	})
}

// getTerminalWidth returns the terminal width, or a default if unavailable
func getTerminalWidth() int {
	// Try stdout first
	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		return width
	}
	// Try stderr (remains a TTY when stdout is piped)
	if width, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && width > 0 {
		return width
	}
	return 120 // default
}

// RenderTable renders a table of conversation entries
// matchCounts is optional - if provided, adds a "Matches" column
func RenderTable(stdout *os.File, entries []SessionEntry, showProject, long bool, matchCounts []int) {
	t := table.NewWriter()
	t.SetOutputMirror(stdout)
	t.SetStyle(table.StyleLight)

	termWidth := getTerminalWidth()
	t.SetAllowedRowLength(termWidth)

	// Build header
	header := table.Row{"ID"}
	if showProject {
		header = append(header, "Project")
	}
	header = append(header, "Prompt/Title")
	if long {
		header = append(header, "Msgs")
	}
	header = append(header, "Modified")
	if matchCounts != nil {
		header = append(header, "Matches")
	}
	t.AppendHeader(header)

	// Calculate fixed column widths
	// ID=8, Modified=16, borders/padding ~20
	fixedWidth := 8 + 16 + 20
	if showProject {
		fixedWidth += 43 // Project column
	}
	if long {
		fixedWidth += 6 // Msgs column
	}
	if matchCounts != nil {
		fixedWidth += 8 // Matches column
	}

	// Prompt/Title gets remaining space
	promptWidth := termWidth - fixedWidth
	if promptWidth < 30 {
		promptWidth = 30
	}
	if promptWidth > 100 {
		promptWidth = 100
	}

	// Add rows
	for i, e := range entries {
		// Format: [title]: prompt, or just prompt if no title/summary
		var titleStr string
		if e.HasTitle() {
			titleStr = e.DisplayTitle()
		}
		displayText := truncatePrompt(convindex.FormatTitleAndPrompt(titleStr, e.FirstPrompt), promptWidth)
		modified := formatDate(e.Modified)

		row := table.Row{e.SessionID[:8]}
		if showProject {
			row = append(row, shortenPath(e.ProjectPath, 41))
		}
		row = append(row, displayText)
		if long {
			row = append(row, e.MessageCount)
		}
		row = append(row, modified)
		if matchCounts != nil {
			row = append(row, matchCounts[i])
		}
		t.AppendRow(row)
	}

	t.Render()
}

func cleanPrompt(prompt string) string {
	// Replace newlines with spaces
	prompt = strings.ReplaceAll(prompt, "\n", " ")
	prompt = strings.ReplaceAll(prompt, "\r", "")
	// Collapse multiple spaces
	for strings.Contains(prompt, "  ") {
		prompt = strings.ReplaceAll(prompt, "  ", " ")
	}
	return strings.TrimSpace(prompt)
}

func truncatePrompt(prompt string, maxLen int) string {
	prompt = cleanPrompt(prompt)
	if len(prompt) <= maxLen {
		return prompt
	}
	return prompt[:maxLen-1] + "…"
}

func formatDate(isoDate string) string {
	if len(isoDate) < 10 {
		return isoDate
	}
	t, err := time.Parse(time.RFC3339, isoDate)
	if err != nil {
		return isoDate[:10] // fallback to first 10 chars
	}
	return t.Local().Format("2006-01-02 15:04")
}

func shortenPath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	// Try to show last part of path
	parts := strings.Split(path, string(filepath.Separator))
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if len(last) <= maxLen-3 {
			return "…" + string(filepath.Separator) + last
		}
	}
	return "…" + path[len(path)-maxLen+1:]
}
