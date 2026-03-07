package conv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type DeleteParams struct {
	ConvID string `pos:"true" help:"Conversation ID to delete"`
	Yes    bool   `short:"y" help:"Skip confirmation prompt"`
	Global bool   `short:"g" help:"Search for conversation across all projects"`
}

func DeleteCmd() *cobra.Command {
	return boa.CmdT[DeleteParams]{
		Use:         "delete",
		Aliases:     []string{"rm"},
		Short:       "Delete a Claude conversation",
		Long:        "Delete a Claude Code conversation from the current directory.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *DeleteParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				global, _ := cmd.Flags().GetBool("global")
				return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *DeleteParams, cmd *cobra.Command, args []string) {
			exitCode := RunDelete(params, os.Stdout, os.Stderr, os.Stdin)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunDelete(params *DeleteParams, stdout, stderr *os.File, stdin *os.File) int {
	// Extract just the ID from autocomplete format (e.g., "0459cd73_[myproject]_prompt..." -> "0459cd73")
	convID := clcommon.ExtractIDFromCompletion(params.ConvID)

	var entry *SessionEntry
	var projectPath string
	var index *SessionsIndex

	if params.Global {
		// Search all projects
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading projects directory: %v\n", err)
			return 1
		}

		for _, dirEntry := range entries {
			if !dirEntry.IsDir() {
				continue
			}
			projPath := projectsDir + "/" + dirEntry.Name()
			idx, err := LoadSessionsIndex(projPath)
			if err != nil {
				continue
			}
			if found, _ := FindSessionByID(idx, convID); found != nil {
				entry = found
				projectPath = projPath
				index = idx
				break
			}
		}
	} else {
		// Search current directory
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
			return 1
		}

		projectPath = GetClaudeProjectPath(cwd)
		var err2 error
		index, err2 = LoadSessionsIndex(projectPath)
		if err2 != nil {
			fmt.Fprintf(stderr, "Error loading sessions index: %v\n", err2)
			return 1
		}

		entry, _ = FindSessionByID(index, convID)
	}

	if entry == nil {
		fmt.Fprintf(stderr, "Conversation %s not found\n", convID)
		if !params.Global {
			fmt.Fprintf(stderr, "Hint: use -g to search all projects\n")
		}
		return 1
	}

	// Use the full session ID from the found entry
	fullID := entry.SessionID

	// Show what we're about to delete
	displayName := entry.DisplayTitle()
	if len(displayName) > 50 {
		displayName = displayName[:47] + "..."
	}
	fmt.Fprintf(stdout, "Conversation: %s\n", fullID[:8])
	fmt.Fprintf(stdout, "Project:      %s\n", entry.ProjectPath)
	fmt.Fprintf(stdout, "Title/Prompt: %s\n", displayName)
	fmt.Fprintf(stdout, "Messages:     %d\n", entry.MessageCount)

	// Confirm deletion
	if !params.Yes {
		fmt.Fprintf(stdout, "\nDelete this conversation? [y/N]: ")
		reader := bufio.NewReader(stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Fprintf(stdout, "Aborted.\n")
			return 0
		}
	}

	// Delete conversation file
	convFile := filepath.Join(projectPath, fullID+".jsonl")
	if err := os.Remove(convFile); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "Error deleting conversation file: %v\n", err)
		return 1
	}

	// Delete conversation directory if it exists
	convDir := filepath.Join(projectPath, fullID)
	if info, err := os.Stat(convDir); err == nil && info.IsDir() {
		if err := os.RemoveAll(convDir); err != nil {
			fmt.Fprintf(stderr, "Error deleting conversation directory: %v\n", err)
			return 1
		}
	}

	// Remove from index
	RemoveSessionByID(index, fullID)

	// Save index
	if err := SaveSessionsIndex(projectPath, index); err != nil {
		fmt.Fprintf(stderr, "Error saving sessions index: %v\n", err)
		return 1
	}

	// Add tombstone if sync is initialized
	if syncutil.IsInitialized() {
		if err := AddTombstoneForProject(projectPath, fullID); err != nil {
			fmt.Fprintf(stderr, "Warning: failed to add tombstone: %v\n", err)
			// Don't fail the deletion - tombstone is best-effort
		}
	}

	fmt.Fprintf(stdout, "Deleted conversation %s\n", fullID[:8])
	return 0
}

// AddTombstoneForProject adds a tombstone for a deleted session
// projectPath is the local project dir (e.g., ~/.claude/projects/-Users-alice-git-myproject)
func AddTombstoneForProject(projectPath, sessionID string) error {
	// Get the local project dir name (last component of path)
	localDirName := filepath.Base(projectPath)

	// Load config for path canonicalization
	config, err := syncutil.LoadConfig()
	if err != nil {
		return err
	}

	// Canonicalize the project dir name for sync
	canonicalDirName := config.CanonicalizeProjectDir(localDirName)

	// Add tombstone to the sync project dir
	syncProjectDir := filepath.Join(syncutil.SyncDir(), canonicalDirName)

	// Ensure sync project dir exists
	if err := os.MkdirAll(syncProjectDir, 0755); err != nil {
		return err
	}

	return syncutil.AddTombstone(syncProjectDir, sessionID)
}
