package conv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/common"
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

	// If not found in index, try to find matching .jsonl files on disk
	var fullID string
	inIndex := entry != nil

	if entry != nil {
		fullID = entry.SessionID
	} else {
		if params.Global {
			projectsDir := ClaudeProjectsDir()
			entries, err := os.ReadDir(projectsDir)
			if err == nil {
				for _, dirEntry := range entries {
					if !dirEntry.IsDir() {
						continue
					}
					projPath := projectsDir + "/" + dirEntry.Name()
					if found := findJSONLByPrefix(projPath, convID); found != "" {
						fullID = found
						projectPath = projPath
						idx, _ := LoadSessionsIndex(projPath)
						if idx != nil {
							index = idx
						}
						break
					}
				}
			}
		} else {
			if projectPath != "" {
				fullID = findJSONLByPrefix(projectPath, convID)
			}
		}

		if fullID == "" {
			fmt.Fprintf(stderr, "Conversation %s not found\n", convID)
			if !params.Global {
				fmt.Fprintf(stderr, "Hint: use -g to search all projects\n")
			}
			return 1
		}
	}

	// Show what we're about to delete
	if entry != nil {
		displayName := entry.DisplayTitle()
		if len(displayName) > 50 {
			displayName = displayName[:47] + "..."
		}
		fmt.Fprintf(stdout, "Conversation: %s\n", fullID[:8])
		fmt.Fprintf(stdout, "Project:      %s\n", entry.ProjectPath)
		fmt.Fprintf(stdout, "Title/Prompt: %s\n", displayName)
		fmt.Fprintf(stdout, "Messages:     %d\n", entry.MessageCount)
	} else {
		fmt.Fprintf(stdout, "Conversation: %s (not in index)\n", fullID[:8])
		fmt.Fprintf(stdout, "Project:      %s\n", filepath.Base(projectPath))
	}

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

	// Delegate to the comprehensive cleanup. Handles filesystem + DB
	// union purge + session-env + sync tombstone in one place.
	if _, err := DeleteConvByID(fullID); err != nil {
		fmt.Fprintf(stderr, "Error deleting conversation: %v\n", err)
		return 1
	}
	// Avoid unused-variable noise on `inIndex` (kept for the display
	// branches above; the actual delete is identity-agnostic).
	_ = inIndex
	_ = index

	fmt.Fprintf(stdout, "Deleted conversation %s\n", fullID[:8])
	return 0
}

// findJSONLByPrefix scans a project directory for a .jsonl file matching the given ID prefix.
// Returns the full session ID (without .jsonl extension) if exactly one match is found, empty string otherwise.
func findJSONLByPrefix(projectPath, idPrefix string) string {
	entries, err := os.ReadDir(projectPath)
	if err != nil {
		return ""
	}
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if id == idPrefix || strings.HasPrefix(id, idPrefix) {
			matches = append(matches, id)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// DeleteConvByID is the single source-of-truth cleanup for a
// conversation — whether it's a free conv, an agent's conv, or an
// orphan with no conv_index row. Every "delete a conversation"
// surface (`tclaude conv rm`, daemon `/v1/agent/.../delete`, dashboard
// `DELETE /api/agents/...`) delegates here.
//
// Comprehensive cleanup:
//
//   - filesystem: removes the .jsonl + any sibling conv directory;
//     walks ~/.claude/projects/* to find the file when conv_index
//     doesn't know where it lives (orphan path).
//   - DB: invokes db.DeleteAgentByConvID, which purges every row
//     referencing this conv-id across conv_index, sessions, and every
//     agent_* table (group_members, group_owners, permissions,
//     messages, cron_jobs, conv_succession, embeddings, …).
//   - legacy artifacts: removes the sessions-index.json entry and the
//     ~/.claude/session-env/<convID> env file the hook callback uses.
//   - sync: writes a tombstone when syncutil is initialised.
//
// What it does NOT do: kill an alive tmux session. That's the
// caller's policy (force vs refuse). Callers must stop the tmux
// session first if they want a live agent dead.
//
// Idempotent: orphans, unknown conv-ids, and double-calls all return
// (zero-counts, nil) with whatever work CAN be done. Errors are returned
// only for genuine I/O failures, not for "thing was already gone".
//
// Returns the per-table DB-row-removal counts from db.DeleteAgentByConvID
// so callers (the daemon's /v1/agent/.../delete response in particular)
// can surface them to the user.
func DeleteConvByID(convID string) (db.AgentDeletionCounts, error) {
	var counts db.AgentDeletionCounts
	if convID == "" {
		return counts, fmt.Errorf("convID is required")
	}

	// 1. Locate the .jsonl. Try conv_index first (fast, knows the
	//    project path). For orphans where conv_index is gone, fall
	//    back to scanning every project dir on disk — the file may
	//    still exist even if the cache row vanished.
	var fullPath string
	var projectPath string
	fullID := convID
	if row, _ := db.GetConvIndex(convID); row != nil {
		fullID = row.ConvID
		fullPath = row.FullPath
		if fullPath != "" {
			projectPath = filepath.Dir(fullPath)
		}
	}
	if fullPath == "" {
		// Orphan path: walk the projects dir looking for a matching .jsonl.
		if home, err := os.UserHomeDir(); err == nil {
			projectsDir := filepath.Join(home, ".claude", "projects")
			if entries, err := os.ReadDir(projectsDir); err == nil {
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					candidate := filepath.Join(projectsDir, e.Name(), fullID+".jsonl")
					if _, err := os.Stat(candidate); err == nil {
						fullPath = candidate
						projectPath = filepath.Join(projectsDir, e.Name())
						break
					}
				}
			}
		}
	}

	// 2. Remove the .jsonl + any sibling conv dir.
	if fullPath != "" {
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return counts, fmt.Errorf("remove jsonl: %w", err)
		}
	}
	if projectPath != "" {
		convDir := filepath.Join(projectPath, fullID)
		if info, err := os.Stat(convDir); err == nil && info.IsDir() {
			if err := os.RemoveAll(convDir); err != nil {
				return counts, fmt.Errorf("remove conv dir: %w", err)
			}
		}
	}

	// 3. Comprehensive DB purge — drops conv_index, sessions, and
	//    every agent_* row referencing this conv-id.
	c, err := db.DeleteAgentByConvID(fullID)
	if err != nil {
		return counts, fmt.Errorf("db purge: %w", err)
	}
	counts = c

	// 4. Drop the ~/.claude/session-env/<convID> env file written by
	//    the spawn flow. Best-effort.
	if home, err := os.UserHomeDir(); err == nil {
		envFile := filepath.Join(home, ".claude", "session-env", fullID)
		_ = os.Remove(envFile)
	}

	// 5. Write sync tombstone so other machines see the delete.
	//    Legacy: kept until the git-sync subcommand is removed.
	if syncutil.IsInitialized() && projectPath != "" {
		_ = AddTombstoneForProject(projectPath, fullID)
	}
	return counts, nil
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
