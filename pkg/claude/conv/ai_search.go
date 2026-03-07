package conv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type AISearchParams struct {
	Query  string `pos:"true" help:"Natural language search query"`
	Global bool   `short:"g" help:"Search across all projects"`
	Long   bool   `short:"l" help:"Show detailed output"`
	Limit  int    `short:"n" help:"Limit number of results (0 = no limit)" default:"5"`
	Since  string `long:"since" optional:"true" help:"Only include conversations modified after this time (e.g., 2024-01-15, 1h30m, 7d)"`
	Before string `long:"before" optional:"true" help:"Only include conversations modified before this time (e.g., 2024-01-15, 1h30m, 7d)"`
}

func AISearchCmd() *cobra.Command {
	return boa.CmdT[AISearchParams]{
		Use:         "ai-search",
		Aliases:     []string{"ai", "ask"},
		Short:       "Search conversations using Claude AI",
		Long:        "Search conversations using natural language by delegating to Claude Code.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *AISearchParams, cmd *cobra.Command, args []string) {
			exitCode := RunAISearch(params, os.Stdout, os.Stderr)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

// AIResponse is the expected response structure from Claude
type AIResponse struct {
	IDs []string `json:"ids"`
}

func RunAISearch(params *AISearchParams, stdout, stderr *os.File) int {
	// First, gather all conversations
	var allEntries []SessionEntry

	if params.Global {
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
			index, err := LoadSessionsIndex(projectPath)
			if err != nil {
				continue
			}
			allEntries = append(allEntries, index.Entries...)
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
			return 1
		}

		projectPath := GetClaudeProjectPath(cwd)
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			fmt.Fprintf(stderr, "No Claude conversations found for %s\n", cwd)
			return 1
		}

		index, err := LoadSessionsIndex(projectPath)
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

	if len(allEntries) == 0 {
		fmt.Fprintf(stdout, "No conversations found\n")
		return 0
	}

	// Sort by modified date descending
	sort.Slice(allEntries, func(i, j int) bool {
		return allEntries[i].Modified > allEntries[j].Modified
	})

	// Build a compact JSON representation for Claude
	// Include all searchable fields so Claude can match against any of them
	type CompactEntry struct {
		ID      string `json:"id"`
		Title   string `json:"title,omitempty"`   // Legacy custom title
		Summary string `json:"summary,omitempty"` // AI-generated summary
		Prompt  string `json:"prompt"`            // First user prompt
		Project string `json:"project"`
		Msgs    int    `json:"msgs"`
	}

	compactEntries := make([]CompactEntry, len(allEntries))
	for i, e := range allEntries {
		prompt := e.FirstPrompt
		if len(prompt) > 200 {
			prompt = prompt[:197] + "..."
		}
		project := e.ProjectPath
		if parts := strings.Split(project, string(filepath.Separator)); len(parts) > 0 {
			project = parts[len(parts)-1]
		}
		compactEntries[i] = CompactEntry{
			ID:      e.SessionID[:8],
			Title:   e.CustomTitle,
			Summary: e.Summary,
			Prompt:  prompt,
			Project: project,
			Msgs:    e.MessageCount,
		}
	}

	listJSON, err := json.Marshal(compactEntries)
	if err != nil {
		fmt.Fprintf(stderr, "Error marshaling conversation list: %v\n", err)
		return 1
	}

	// Build the prompt for Claude
	prompt := fmt.Sprintf(`Here is a list of conversations in JSON format:

%s

Please find the conversation IDs that best match this query: "%s"

Return ONLY a JSON object in this exact format, with no other text:
{"ids": ["<id1>", "<id2>", ...]}

Return up to %d matching IDs, ordered by relevance. If no conversations match, return {"ids": []}.`, string(listJSON), params.Query, params.Limit)

	// Run claude -p
	fmt.Fprintf(stderr, "Asking Claude to find matching conversations...\n")
	cmd := exec.Command("claude", "-p", prompt)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "Error running claude: %v\n", err)
		if errBuf.Len() > 0 {
			fmt.Fprintf(stderr, "Claude stderr: %s\n", errBuf.String())
		}
		return 1
	}

	// Parse the response
	responseText := strings.TrimSpace(outBuf.String())

	// Try to extract JSON from the response (Claude might add some explanation)
	jsonStart := strings.Index(responseText, "{")
	jsonEnd := strings.LastIndex(responseText, "}")
	if jsonStart == -1 || jsonEnd == -1 || jsonEnd < jsonStart {
		fmt.Fprintf(stderr, "Could not parse Claude's response as JSON:\n%s\n", responseText)
		return 1
	}

	jsonStr := responseText[jsonStart : jsonEnd+1]
	var response AIResponse
	if err := json.Unmarshal([]byte(jsonStr), &response); err != nil {
		fmt.Fprintf(stderr, "Error parsing Claude's response: %v\n", err)
		fmt.Fprintf(stderr, "Response was: %s\n", responseText)
		return 1
	}

	if len(response.IDs) == 0 {
		fmt.Fprintf(stdout, "No matching conversations found\n")
		return 0
	}

	// Find the matching entries
	var matchedEntries []SessionEntry
	for _, id := range response.IDs {
		for _, e := range allEntries {
			if strings.HasPrefix(e.SessionID, id) {
				matchedEntries = append(matchedEntries, e)
				break
			}
		}
	}

	if len(matchedEntries) == 0 {
		fmt.Fprintf(stdout, "No matching conversations found\n")
		return 0
	}

	// Display results
	fmt.Fprintf(stdout, "\nFound %d matching conversation(s):\n\n", len(matchedEntries))
	RenderTable(stdout, matchedEntries, params.Global, params.Long, nil)

	return 0
}
