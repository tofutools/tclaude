package conv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type SearchParams struct {
	Pattern       string `pos:"true" help:"Search pattern (regex supported)"`
	Dir           string `pos:"true" optional:"true" help:"Directory to search in (defaults to current directory)"`
	Global        bool   `short:"g" help:"Search across all projects"`
	Content       bool   `short:"-" long:"content" help:"Search full conversation content (slow, default searches only titles/prompts)"`
	Long          bool   `short:"l" help:"Show detailed output"`
	Context       int    `short:"C" help:"Lines of context around matches" default:"0"`
	CaseSensitive bool   `short:"s" help:"Case sensitive search (default is insensitive)"`
	SortBy        string `long:"sort-by" help:"Sort by: created, modified, messages, prompt, project, matches" default:"modified"`
	Asc           bool   `long:"asc" help:"Sort ascending (default is descending)"`
	Limit         int    `short:"n" help:"Limit number of results (0 = no limit)" default:"0"`
	JSON          bool   `long:"json" help:"Output as JSON"`
	Count         bool   `short:"c" long:"count" help:"Only output the count of matching conversations"`
	Since         string `long:"since" optional:"true" help:"Only include conversations modified after this time (e.g., 2024-01-15, 1h30m, 7d)"`
	Before        string `long:"before" optional:"true" help:"Only include conversations modified before this time (e.g., 2024-01-15, 1h30m, 7d)"`
}

type SearchResult struct {
	Entry   SessionEntry
	Matches []MatchLine
}

type MatchLine struct {
	LineNum int
	Content string
}

func SearchCmd() *cobra.Command {
	return boa.CmdT[SearchParams]{
		Use:         "search",
		Aliases:     []string{"grep"},
		Short:       "Search within Claude conversations",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *SearchParams, cmd *cobra.Command, args []string) {
			exitCode := RunSearch(params, os.Stdout, os.Stderr)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunSearch(params *SearchParams, stdout, stderr *os.File) int {
	// Compile regex (case insensitive by default)
	pattern := params.Pattern
	if !params.CaseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		fmt.Fprintf(stderr, "Invalid pattern: %v\n", err)
		return 1
	}

	var projectPaths []string

	if params.Global {
		// Search all projects
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading projects directory: %v\n", err)
			return 1
		}

		for _, entry := range entries {
			if entry.IsDir() {
				projectPaths = append(projectPaths, filepath.Join(projectsDir, entry.Name()))
			}
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
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			fmt.Fprintf(stderr, "No Claude conversations found for %s\n", targetDir)
			return 1
		}
		projectPaths = []string{projectPath}
	}

	var results []SearchResult

	for _, projectPath := range projectPaths {
		index, err := LoadSessionsIndex(projectPath)
		if err != nil {
			continue
		}

		// Filter by time if specified
		entries, err := FilterEntriesByTime(index.Entries, params.Since, params.Before)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}

		for _, entry := range entries {
			// Search metadata fields (CustomTitle, Summary, FirstPrompt)
			metadataMatches := searchMetadata(entry, re)

			var contentMatches []MatchLine
			if params.Content {
				// Search full conversation content (slow)
				contentMatches = searchConversation(entry.FullPath, re)
			}

			allMatches := append(metadataMatches, contentMatches...)
			if len(allMatches) > 0 {
				results = append(results, SearchResult{
					Entry:   entry,
					Matches: allMatches,
				})
			}
		}
	}

	// Count output (before limit is applied)
	if params.Count {
		fmt.Fprintf(stdout, "%d\n", len(results))
		return 0
	}

	if len(results) == 0 {
		fmt.Fprintf(stdout, "No matches found\n")
		return 0
	}

	// Sort results
	sortSearchResults(results, params.SortBy, params.Asc)

	// Apply limit
	if params.Limit > 0 && params.Limit < len(results) {
		results = results[:params.Limit]
	}

	// Extract entries and match counts for table rendering
	entries := make([]SessionEntry, len(results))
	matchCounts := make([]int, len(results))
	for i, r := range results {
		entries[i] = r.Entry
		matchCounts[i] = len(r.Matches)
	}

	// JSON output
	if params.JSON {
		type JSONResult struct {
			SessionEntry
			MatchCount int         `json:"matchCount"`
			Matches    []MatchLine `json:"matches,omitempty"`
		}
		jsonResults := make([]JSONResult, len(results))
		for i, r := range results {
			jsonResults[i] = JSONResult{
				SessionEntry: r.Entry,
				MatchCount:   len(r.Matches),
				Matches:      r.Matches,
			}
		}
		data, err := json.MarshalIndent(jsonResults, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "Error marshaling JSON: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	// Display results using shared table renderer
	RenderTable(stdout, entries, params.Global, params.Long, matchCounts)

	// Show context if requested
	if params.Long || params.Context > 0 {
		fmt.Fprintln(stdout)
		for _, r := range results {
			fmt.Fprintf(stdout, "%s:\n", r.Entry.SessionID[:8])
			maxMatches := 5
			for j, m := range r.Matches {
				if j >= maxMatches {
					fmt.Fprintf(stdout, "  ... and %d more matches\n", len(r.Matches)-maxMatches)
					break
				}
				fmt.Fprintf(stdout, "  L%d: %s\n", m.LineNum, m.Content)
			}
		}
	}

	fmt.Fprintf(stdout, "\n%d conversation(s) with matches\n", len(results))
	return 0
}

func searchMetadata(entry SessionEntry, re *regexp.Regexp) []MatchLine {
	var matches []MatchLine

	// Search CustomTitle
	if entry.CustomTitle != "" && re.MatchString(entry.CustomTitle) {
		matches = append(matches, MatchLine{
			LineNum: 0,
			Content: "[title] " + truncatePrompt(entry.CustomTitle, 70),
		})
	}

	// Search Summary
	if entry.Summary != "" && re.MatchString(entry.Summary) {
		matches = append(matches, MatchLine{
			LineNum: 0,
			Content: "[summary] " + truncatePrompt(entry.Summary, 70),
		})
	}

	// Search FirstPrompt
	if entry.FirstPrompt != "" && re.MatchString(entry.FirstPrompt) {
		matches = append(matches, MatchLine{
			LineNum: 0,
			Content: "[prompt] " + truncatePrompt(entry.FirstPrompt, 70),
		})
	}

	return matches
}

func searchConversation(filePath string, re *regexp.Regexp) []MatchLine {
	file, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer file.Close()

	var matches []MatchLine
	var lines []string

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	for i, line := range lines {
		if re.MatchString(line) {
			// Extract a readable portion around the match
			content := extractMatchContext(line, re)
			matches = append(matches, MatchLine{
				LineNum: i + 1,
				Content: content,
			})
		}
	}

	return matches
}

func extractMatchContext(line string, re *regexp.Regexp) string {
	// Try to extract content from JSON - look for "content" or "text" fields
	// This is a simplified extraction - jsonl lines contain message content

	// Find the match location
	loc := re.FindStringIndex(line)
	if loc == nil {
		return truncatePrompt(line, 80)
	}

	// Extract context around the match
	start := loc[0] - 40
	if start < 0 {
		start = 0
	}
	end := loc[1] + 40
	if end > len(line) {
		end = len(line)
	}

	excerpt := line[start:end]

	// Clean up JSON artifacts
	excerpt = strings.ReplaceAll(excerpt, "\\n", " ")
	excerpt = strings.ReplaceAll(excerpt, "\\t", " ")
	excerpt = strings.ReplaceAll(excerpt, "\\\"", "\"")

	// Collapse whitespace
	for strings.Contains(excerpt, "  ") {
		excerpt = strings.ReplaceAll(excerpt, "  ", " ")
	}

	prefix := ""
	suffix := ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(line) {
		suffix = "…"
	}

	return prefix + strings.TrimSpace(excerpt) + suffix
}

func sortSearchResults(results []SearchResult, sortBy string, asc bool) {
	sort.Slice(results, func(i, j int) bool {
		var less bool
		switch sortBy {
		case "created":
			less = results[i].Entry.Created < results[j].Entry.Created
		case "messages":
			less = results[i].Entry.MessageCount < results[j].Entry.MessageCount
		case "prompt":
			less = strings.ToLower(results[i].Entry.FirstPrompt) < strings.ToLower(results[j].Entry.FirstPrompt)
		case "project":
			less = results[i].Entry.ProjectPath < results[j].Entry.ProjectPath
		case "matches":
			less = len(results[i].Matches) < len(results[j].Matches)
		default: // modified
			less = results[i].Entry.Modified < results[j].Entry.Modified
		}
		if asc {
			return less
		}
		return !less
	})
}
