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
		Long:        "Delete Claude Code conversations that have no user messages, remove stale index entries, and clean up dangling companion directories (subagents, etc.) with no corresponding .jsonl file.",
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

	// Find all empty conversations, missing-file index entries, and dangling directories
	var emptyConvs []emptyConversation
	var missingConvs []emptyConversation
	var danglingDirs []emptyConversation

	for _, projectPath := range projectPaths {
		empty := findEmptyConversations(projectPath)
		missing := findMissingFileEntries(projectPath)
		emptyConvs = append(emptyConvs, empty...)
		missingConvs = append(missingConvs, missing...)

		// Exclude dangling dirs already covered by empty/missing convs
		// (their companion dirs are cleaned up during conv deletion)
		coveredIDs := make(map[string]bool)
		for _, c := range empty {
			coveredIDs[c.SessionID] = true
		}
		for _, c := range missing {
			coveredIDs[c.SessionID] = true
		}
		for _, d := range findDanglingDirectories(projectPath) {
			if !coveredIDs[d.SessionID] {
				danglingDirs = append(danglingDirs, d)
			}
		}
	}

	if len(emptyConvs) == 0 && len(missingConvs) == 0 && len(danglingDirs) == 0 {
		fmt.Fprintf(stdout, "No empty, missing, or dangling conversations found\n")
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
	if len(danglingDirs) > 0 {
		fmt.Fprintf(stdout, "Found %d dangling directory/directories (no .jsonl file):\n\n", len(danglingDirs))
		for _, conv := range danglingDirs {
			fmt.Fprintf(stdout, "  %s/\n", conv.SessionID[:8])
		}
	}

	totalCount := len(emptyConvs) + len(missingConvs) + len(danglingDirs)

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

	// Combine empty and missing lists, group by project path for index updates
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

			// Delete companion directory if it exists (subagents, etc.)
			convDir := filepath.Join(projectPath, conv.SessionID)
			if info, err := os.Stat(convDir); err == nil && info.IsDir() {
				if err := os.RemoveAll(convDir); err != nil {
					fmt.Fprintf(stderr, "Error deleting companion directory for %s: %v\n", conv.SessionID[:8], err)
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

	// Delete dangling directories (UUID dirs with no corresponding .jsonl file)
	danglingDeleted := 0
	for _, conv := range danglingDirs {
		if err := os.RemoveAll(conv.FilePath); err != nil {
			fmt.Fprintf(stderr, "Error deleting dangling directory %s: %v\n", conv.SessionID[:8], err)
			continue
		}
		danglingDeleted++
	}

	if deleted > 0 {
		fmt.Fprintf(stdout, "Deleted %d conversation(s)\n", deleted)
	}
	if danglingDeleted > 0 {
		fmt.Fprintf(stdout, "Deleted %d dangling directory/directories\n", danglingDeleted)
	}
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

// findDanglingDirectories finds UUID-named directories without a corresponding .jsonl file
func findDanglingDirectories(projectPath string) []emptyConversation {
	var dangling []emptyConversation

	files, err := os.ReadDir(projectPath)
	if err != nil {
		return dangling
	}

	// Build set of existing .jsonl session IDs
	jsonlIDs := make(map[string]bool)
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".jsonl") {
			sessionID := strings.TrimSuffix(file.Name(), ".jsonl")
			if len(sessionID) == 36 {
				jsonlIDs[sessionID] = true
			}
		}
	}

	// Find directories with UUID names that lack a corresponding .jsonl file
	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		name := file.Name()
		if len(name) != 36 {
			continue
		}
		// Check it looks like a UUID (hyphens at expected positions)
		if name[8] != '-' || name[13] != '-' || name[18] != '-' || name[23] != '-' {
			continue
		}
		if !jsonlIDs[name] {
			dangling = append(dangling, emptyConversation{
				SessionID:   name,
				FilePath:    filepath.Join(projectPath, name),
				ProjectPath: projectPath,
			})
		}
	}

	return dangling
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
