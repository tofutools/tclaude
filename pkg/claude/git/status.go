package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type StatusParams struct{}

func StatusCmd() *cobra.Command {
	return boa.CmdT[StatusParams]{
		Use:         "status",
		Short:       "Show git sync status",
		Long:        "Show the status of the Claude conversation sync repository.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *StatusParams, cmd *cobra.Command, args []string) {
			if err := runStatus(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runStatus(_ *StatusParams) error {
	syncDir := SyncDir()
	projectsDir := ProjectsDir()

	if !IsInitialized() {
		fmt.Printf("Git sync not initialized.\n")
		fmt.Printf("Run 'tclaude git init <repo-url>' to set up sync.\n")
		return nil
	}

	fmt.Printf("Sync directory: %s\n\n", syncDir)

	// Show git remote
	remoteCmd := exec.Command("git", "remote", "-v")
	remoteCmd.Dir = syncDir
	remoteCmd.Stdout = os.Stdout
	remoteCmd.Stderr = os.Stderr
	remoteCmd.Run()

	fmt.Println()

	// Copy local changes to sync dir so we can see pending changes
	if err := copyProjectsToSync(projectsDir, syncDir, false); err != nil {
		return fmt.Errorf("failed to copy local changes: %w", err)
	}

	// Show pending changes
	statusCmd := exec.Command("git", "status", "--short")
	statusCmd.Dir = syncDir
	output, err := statusCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get git status: %w", err)
	}

	if len(output) == 0 {
		fmt.Printf("No pending changes to sync.\n")
	} else {
		// Build a map of UUID -> title from sessions indices
		titles := loadConversationTitles(projectsDir)

		// Parse and group changes by conversation UUID
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		conversations := make(map[string]int) // uuid -> file count
		otherChanges := 0

		for _, line := range lines {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			path := parts[len(parts)-1]

			// Extract UUID from paths like:
			// - project/uuid.jsonl
			// - project/uuid/subagents/foo.jsonl
			uuid := extractConversationUUID(path)
			if uuid != "" {
				conversations[uuid]++
			} else {
				otherChanges++
			}
		}

		if len(conversations) > 0 {
			fmt.Printf("Conversations with pending changes (%d):\n", len(conversations))
			for uuid, count := range conversations {
				title := titles[uuid]
				if title == "" {
					// Show short UUID when no title available
					title = uuid[:8]
				} else {
					title = truncateTitle(title, 60)
				}
				if count == 1 {
					fmt.Printf("  %s\n", title)
				} else {
					fmt.Printf("  %s (%d files)\n", title, count)
				}
			}
		}

		if otherChanges > 0 {
			fmt.Printf("\nOther changes: %d files\n", otherChanges)
		}
	}

	// Show last sync time (last commit)
	logCmd := exec.Command("git", "log", "-1", "--format=%cr (%ci)", "--date=local")
	logCmd.Dir = syncDir
	logOutput, err := logCmd.Output()
	if err == nil && len(logOutput) > 0 {
		fmt.Printf("\nLast sync: %s", logOutput)
	}

	return nil
}

// extractConversationUUID extracts the conversation UUID from a path
// Handles paths like:
// - project/uuid.jsonl
// - project/uuid/subagents/foo.jsonl
func extractConversationUUID(path string) string {
	parts := strings.Split(path, "/")
	for _, part := range parts {
		// Check for uuid.jsonl
		name := strings.TrimSuffix(part, ".jsonl")
		if uuidRegex.MatchString(name) {
			return name
		}
		// Check for uuid directory
		base := filepath.Base(part)
		if uuidRegex.MatchString(base) {
			return base
		}
	}
	return ""
}

// loadConversationTitles loads all conversation titles from sessions indices
func loadConversationTitles(projectsDir string) map[string]string {
	titles := make(map[string]string)

	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return titles
	}

	for _, project := range projects {
		if !project.IsDir() {
			continue
		}

		projectPath := filepath.Join(projectsDir, project.Name())
		index, err := conv.LoadSessionsIndex(projectPath)
		if err != nil {
			continue
		}

		for _, entry := range index.Entries {
			title := entry.DisplayTitle()
			if title != "" {
				titles[entry.SessionID] = title
			}
		}
	}

	return titles
}

// truncateTitle truncates a title to a maximum length
func truncateTitle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
