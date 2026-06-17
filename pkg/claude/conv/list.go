package conv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
	"golang.org/x/term"
)

type ListParams struct {
	Dir             string `short:"C" long:"dir" optional:"true" help:"Directory to list conversations from (defaults to current directory)"`
	Global          bool   `short:"g" help:"List conversations from all projects"`
	SortBy          string `long:"sort-by" help:"Sort by: created, modified, messages, prompt, project" default:"modified"`
	Asc             bool   `long:"asc" help:"Sort ascending (default is descending)"`
	Long            bool   `short:"l" help:"Show detailed output"`
	Limit           int    `short:"n" help:"Limit number of results (0 = no limit)" default:"0"`
	JSON            bool   `long:"json" help:"Output as JSON"`
	Count           bool   `short:"c" long:"count" help:"Only output the count of conversations"`
	Since           string `long:"since" optional:"true" help:"Only include conversations modified after this time (e.g., 2024-01-15, 1h30m, 7d)"`
	Before          string `long:"before" optional:"true" help:"Only include conversations modified before this time (e.g., 2024-01-15, 1h30m, 7d)"`
	Watch           bool   `short:"w" long:"watch" help:"Interactive watch mode with search and session management"`
	Verbose         bool   `short:"v" long:"verbose" help:"Show debug info (stale scan stats, timing)"`
	Reindex         bool   `long:"reindex" help:"Force rescan all conversations from .jsonl files and update index"`
	ShowArchived    bool   `long:"show-archived" help:"Include archived convs whose title ends with the -x marker (default: hidden). Pairs with the groups archive concept."`
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
	SetDebugLog(params.Verbose)

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
		// List all Claude projects. A missing projects dir is not fatal —
		// there may still be other-harness (Codex) conversations.
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil && !os.IsNotExist(err) {
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

		// Merge every other registered harness (Codex, …), all dirs.
		allEntries = appendNonClaudeHarnessEntries(allEntries, "")
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

		// Canonicalize so the Claude project-dir encode and the harness cwd
		// filter (an exact-string match) agree on a relative or
		// trailing-slash --dir; otherwise Codex convs would silently drop.
		if abs, err := filepath.Abs(targetDir); err == nil {
			targetDir = abs
		}

		// Load Claude conversations if this dir has a project dir. A missing
		// one is no longer fatal — the directory may still have other-harness
		// (Codex) conversations, merged just below.
		projectPath := GetClaudeProjectPath(targetDir)
		if _, err := os.Stat(projectPath); err == nil {
			index, err := LoadSessionsIndexWithOptions(projectPath, loadOpts)
			if err != nil {
				fmt.Fprintf(stderr, "Error loading sessions index: %v\n", err)
				return 1
			}
			allEntries = index.Entries
		}

		// Merge every other registered harness (Codex, …) for this dir.
		allEntries = appendNonClaudeHarnessEntries(allEntries, targetDir)
	}

	// Filter by time if specified
	allEntries, err := FilterEntriesByTime(allEntries, params.Since, params.Before)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	// Hide archived convs by default — `-x`-suffixed CustomTitles are
	// reincarnated old instances kept on disk for history but rarely
	// what the user wants to see. Opt back in with --show-archived.
	// Same conceptual soft-delete as `groups archive` on the group
	// side; convs use the title-suffix marker today, with a planned
	// migration to a `conv_index.archived_at` column.
	if !params.ShowArchived {
		filtered := allEntries[:0]
		for _, e := range allEntries {
			if !e.IsArchived() {
				filtered = append(filtered, e)
			}
		}
		allEntries = filtered
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

	// Pull group memberships so we can render a "Groups" column when any
	// conv is in at least one group. Errors here are non-fatal — we just
	// drop the column.
	groupsByConv, _ := db.GroupNamesByConv()

	// Pull spawn-time agent names so a not-yet-renamed agent shows its
	// designated name rather than its raw first prompt (convDisplayTitle).
	// Best-effort: a nil map just skips the fallback.
	pendingByConv, _ := db.PendingNamesByConv()

	// Display using table
	RenderTable(stdout, allEntries, params.Global, params.Long, nil, groupsByConv, pendingByConv)

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

// RenderTable renders a table of conversation entries.
// matchCounts is optional - if provided, adds a "Matches" column.
// groupsByConv is optional - if any conv has at least one group,
// adds a "Groups" column. Pass nil or an empty map to skip.
// pendingByConv is optional - it supplies the spawn-time agent name used as a
// title fallback when a conv has no real custom title (see convDisplayTitle);
// pass nil to skip the fallback.
func RenderTable(stdout *os.File, entries []SessionEntry, showProject, long bool, matchCounts []int, groupsByConv map[string][]string, pendingByConv map[string]string) {
	showGroups := false
	for _, e := range entries {
		if len(groupsByConv[e.SessionID]) > 0 {
			showGroups = true
			break
		}
	}

	// Only surface the Harness column once a non-Claude-Code conv is in the
	// list, so a CC-only listing is unchanged. Empty / "claude" don't count.
	showHarness := false
	for _, e := range entries {
		if e.Harness != "" && e.Harness != harness.DefaultName {
			showHarness = true
			break
		}
	}

	t := table.NewWriter()
	t.SetOutputMirror(stdout)
	t.SetStyle(table.StyleLight)

	termWidth := getTerminalWidth()
	t.SetAllowedRowLength(termWidth)

	// Build header
	header := table.Row{"ID"}
	if showHarness {
		header = append(header, "Harness")
	}
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
	if showGroups {
		header = append(header, "Groups")
	}
	t.AppendHeader(header)

	// Calculate fixed column widths
	// ID=8, Modified=16, borders/padding ~20
	fixedWidth := 8 + 16 + 20
	if showHarness {
		fixedWidth += 9 // Harness column
	}
	if showProject {
		fixedWidth += 43 // Project column
	}
	if long {
		fixedWidth += 6 // Msgs column
	}
	if matchCounts != nil {
		fixedWidth += 8 // Matches column
	}
	if showGroups {
		fixedWidth += 20 // Groups column
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
		// Canonical "[title]: prompt" rendering — shared with conv ls -w
		// and the web dashboard via convindex.FormatConvTitle, with the
		// agent pending-name fallback (convDisplayTitle).
		displayText := truncatePrompt(convDisplayTitle(e, pendingByConv), promptWidth)
		modified := formatDate(e.Modified)

		row := table.Row{e.SessionID[:8]}
		if showHarness {
			row = append(row, harnessBadge(e.Harness))
		}
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
		if showGroups {
			row = append(row, strings.Join(groupsByConv[e.SessionID], ","))
		}
		t.AppendRow(row)
	}

	t.Render()
}

// convDisplayTitle renders a conv's canonical "[title]: prompt" string,
// falling back to the agent's spawn-time pending name when the conv has no
// real custom title. This mirrors the dashboard's title resolution
// (agent.FreshTitle: custom title → pending name → summary → first prompt),
// so a not-yet-renamed agent — e.g. a Codex agent whose out-of-band title
// write hasn't landed (JOH-216) — shows its designated name instead of its
// raw first prompt. pendingByConv may be nil; a conv absent from it falls
// through to the normal summary/first-prompt rendering unchanged.
func convDisplayTitle(e SessionEntry, pendingByConv map[string]string) string {
	custom := e.CustomTitle
	if custom == "" {
		custom = pendingByConv[e.SessionID]
	}
	return convindex.FormatConvTitle(custom, e.Summary, e.FirstPrompt)
}

// harnessBadge renders the harness label for the conv list. An empty
// harness — a CC conv indexed before the harness column existed — reads as
// the default "claude".
func harnessBadge(h string) string {
	if h == "" {
		return harness.DefaultName
	}
	return h
}

func cleanPrompt(prompt string) string {
	// Titles/prompts come from (now multi-harness) untrusted conversation
	// files and are printed straight into the user's terminal, so newlines
	// become spaces and every other control char — ESC/ANSI escapes above
	// all — is dropped rather than allowed to reach the terminal.
	var b strings.Builder
	b.Grow(len(prompt))
	for _, r := range prompt {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// Collapse multiple spaces
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return strings.TrimSpace(out)
}

func truncatePrompt(prompt string, maxLen int) string {
	prompt = cleanPrompt(prompt)
	// Count/cut on runes, not bytes, so a multi-byte char near the limit is
	// never split into invalid UTF-8 (maxLen also tracks display columns
	// more closely this way).
	r := []rune(prompt)
	if len(r) <= maxLen {
		return prompt
	}
	if maxLen < 1 {
		return "…"
	}
	return string(r[:maxLen-1]) + "…"
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
