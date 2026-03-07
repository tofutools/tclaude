package conv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type PruneEmptyParams struct {
	Dir    string `pos:"true" optional:"true" help:"Directory to prune (defaults to current directory)"`
	Global bool   `short:"g" help:"Prune across all projects"`
	Yes    bool   `short:"y" help:"Skip confirmation prompt"`
	DryRun bool   `short:"n" long:"dry-run" help:"Show what would be deleted without deleting"`
}

func PruneEmptyCmd() *cobra.Command {
	return boa.CmdT[PruneEmptyParams]{
		Use:         "prune-empty",
		Short:       "Delete empty conversations with no user messages",
		Long:        "Delete Claude Code conversations that have no user messages. Only considers .jsonl files, not conversation directories.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *PruneEmptyParams, cmd *cobra.Command, args []string) {
			exitCode := RunPruneEmpty(params, os.Stdout, os.Stderr, os.Stdin)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

// emptyConversation represents a conversation to be pruned
type emptyConversation struct {
	SessionID   string
	FilePath    string
	ProjectPath string
	IsIndexed   bool
}

func RunPruneEmpty(params *PruneEmptyParams, stdout, stderr *os.File, stdin *os.File) int {
	var projectPaths []string

	if params.Global {
		// All projects
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

	// Find all empty conversations and missing-file index entries
	var emptyConvs []emptyConversation
	var missingConvs []emptyConversation

	for _, projectPath := range projectPaths {
		emptyConvs = append(emptyConvs, findEmptyConversations(projectPath)...)
		missingConvs = append(missingConvs, findMissingFileEntries(projectPath)...)
	}

	if len(emptyConvs) == 0 && len(missingConvs) == 0 {
		fmt.Fprintf(stdout, "No empty or missing conversations found\n")
		return 0
	}

	// Show what we found
	if len(emptyConvs) > 0 {
		fmt.Fprintf(stdout, "Found %d empty conversation(s) (no user messages):\n\n", len(emptyConvs))
		for _, conv := range emptyConvs {
			indexedStr := ""
			if conv.IsIndexed {
				indexedStr = " (indexed)"
			}
			fmt.Fprintf(stdout, "  %s%s\n", conv.SessionID[:8], indexedStr)
		}
	}
	if len(missingConvs) > 0 {
		fmt.Fprintf(stdout, "Found %d index-only entry/entries (file missing on disk):\n\n", len(missingConvs))
		for _, conv := range missingConvs {
			fmt.Fprintf(stdout, "  %s\n", conv.SessionID[:8])
		}
	}

	totalCount := len(emptyConvs) + len(missingConvs)

	if params.DryRun {
		fmt.Fprintf(stdout, "\nDry run - no changes made\n")
		return 0
	}

	// Confirm deletion
	if !params.Yes {
		fmt.Fprintf(stdout, "\nDelete these %d conversation(s)? [y/N]: ", totalCount)
		reader := bufio.NewReader(stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Fprintf(stdout, "Aborted.\n")
			return 0
		}
	}

	// Combine both lists and group by project path for index updates
	allConvs := append(emptyConvs, missingConvs...)
	byProject := make(map[string][]emptyConversation)
	for _, conv := range allConvs {
		byProject[conv.ProjectPath] = append(byProject[conv.ProjectPath], conv)
	}

	// Delete conversations
	deleted := 0
	syncInitialized := syncutil.IsInitialized()

	for projectPath, convs := range byProject {
		// Load index for this project (to remove indexed entries)
		index, _ := loadSessionsIndexOnly(projectPath)

		for _, conv := range convs {
			// Delete the .jsonl file (may not exist for missing-file entries)
			if err := os.Remove(conv.FilePath); err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(stderr, "Error deleting %s: %v\n", conv.SessionID[:8], err)
					continue
				}
			}

			// Remove from index if present
			if index != nil && conv.IsIndexed {
				RemoveSessionByID(index, conv.SessionID)
			}

			// Add tombstone if sync is initialized
			if syncInitialized {
				if err := AddTombstoneForProject(projectPath, conv.SessionID); err != nil {
					fmt.Fprintf(stderr, "Warning: failed to add tombstone for %s: %v\n", conv.SessionID[:8], err)
					// Don't fail - tombstone is best-effort
				}
			}

			deleted++
		}

		// Save updated index
		if index != nil {
			if err := SaveSessionsIndex(projectPath, index); err != nil {
				fmt.Fprintf(stderr, "Error saving index for %s: %v\n", projectPath, err)
			}
		}
	}

	fmt.Fprintf(stdout, "Deleted %d conversation(s)\n", deleted)
	return 0
}

// loadSessionsIndexOnly loads just the sessions-index.json without scanning for unindexed files
func loadSessionsIndexOnly(projectPath string) (*SessionsIndex, error) {
	indexPath := filepath.Join(projectPath, "sessions-index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &SessionsIndex{Version: 1, Entries: []SessionEntry{}}, nil
		}
		return nil, fmt.Errorf("failed to read sessions index: %w", err)
	}

	var index SessionsIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to parse sessions index: %w", err)
	}

	return &index, nil
}

// findEmptyConversations finds all .jsonl files with no user messages
func findEmptyConversations(projectPath string) []emptyConversation {
	var empty []emptyConversation

	// Load index to check which sessions are indexed
	index, _ := loadSessionsIndexOnly(projectPath)
	indexedIDs := make(map[string]bool)
	if index != nil {
		for _, e := range index.Entries {
			indexedIDs[e.SessionID] = true
		}
	}

	// Scan for .jsonl files
	files, err := os.ReadDir(projectPath)
	if err != nil {
		return empty
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}

		// Extract session ID from filename
		sessionID := strings.TrimSuffix(file.Name(), ".jsonl")
		if len(sessionID) != 36 { // UUID length
			continue
		}

		filePath := filepath.Join(projectPath, file.Name())

		// Check if it has any user messages
		if !hasUserMessages(filePath) {
			empty = append(empty, emptyConversation{
				SessionID:   sessionID,
				FilePath:    filePath,
				ProjectPath: projectPath,
				IsIndexed:   indexedIDs[sessionID],
			})
		}
	}

	return empty
}

// findMissingFileEntries finds index entries whose .jsonl file doesn't exist on disk
func findMissingFileEntries(projectPath string) []emptyConversation {
	var missing []emptyConversation

	index, err := loadSessionsIndexOnly(projectPath)
	if err != nil || index == nil {
		return missing
	}

	for _, e := range index.Entries {
		filePath := filepath.Join(projectPath, e.SessionID+".jsonl")
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			missing = append(missing, emptyConversation{
				SessionID:   e.SessionID,
				FilePath:    filePath,
				ProjectPath: projectPath,
				IsIndexed:   true,
			})
		}
	}

	return missing
}

// hasUserMessages checks if a .jsonl file contains any user messages
func hasUserMessages(filePath string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return true // Assume not empty on error to be safe
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg jsonlMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		// Found a user message
		if msg.Type == "user" && msg.Message.Role == "user" {
			return true
		}
	}

	return false
}
