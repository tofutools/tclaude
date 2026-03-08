package git

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type StatusParams struct{}

func StatusCmd() *cobra.Command {
	return boa.CmdT[StatusParams]{
		Use:         "status",
		Short:       "Show git sync status",
		Long:        "Show the status of the Claude conversation sync repository, including pending changes, deletions, and remote sync state.",
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

	// Show last sync time (last commit)
	logCmd := exec.Command("git", "log", "-1", "--format=%cr (%ci)", "--date=local")
	logCmd.Dir = syncDir
	logOutput, err := logCmd.Output()
	if err == nil && len(logOutput) > 0 {
		fmt.Printf("Last sync: %s\n", strings.TrimSpace(string(logOutput)))
	}

	// Show ahead/behind remote
	showAheadBehind(syncDir)

	// Show pending tombstones (deletions)
	titles := loadConversationTitles(projectsDir)
	showPendingTombstones(syncDir, titles)

	// Copy local changes to sync dir so we can see pending changes
	if err := copyProjectsToSync(projectsDir, syncDir, false); err != nil {
		return fmt.Errorf("failed to copy local changes: %w", err)
	}

	// Show pending conversation changes
	showPendingChanges(syncDir, titles)

	return nil
}

// showAheadBehind shows how many commits we are ahead/behind the remote
func showAheadBehind(syncDir string) {
	remoteBranch := getRemoteBranch(syncDir)
	if remoteBranch == "" {
		return
	}

	revList := exec.Command("git", "rev-list", "--left-right", "--count", "HEAD...origin/"+remoteBranch)
	revList.Dir = syncDir
	output, err := revList.Output()
	if err != nil {
		return
	}

	parts := strings.Fields(strings.TrimSpace(string(output)))
	if len(parts) != 2 {
		return
	}

	ahead := parts[0]
	behind := parts[1]

	if ahead == "0" && behind == "0" {
		fmt.Printf("Remote:    up to date\n")
	} else {
		var status []string
		if ahead != "0" {
			status = append(status, ahead+" ahead")
		}
		if behind != "0" {
			status = append(status, behind+" behind")
		}
		fmt.Printf("Remote:    %s\n", strings.Join(status, ", "))
	}
}

type tombstoneInfo struct {
	sessionID string
	deletedAt string
	deletedBy string
	project   string
}

// showPendingTombstones shows only tombstones not yet committed/pushed in the sync dir
func showPendingTombstones(syncDir string, titles map[string]string) {
	tombstones := findPendingTombstones(syncDir)
	if len(tombstones) == 0 {
		return
	}

	fmt.Printf("\nPending deletions (%d):\n", len(tombstones))
	for _, t := range tombstones {
		title := titles[t.sessionID]
		if title == "" {
			title = t.sessionID[:8]
		} else {
			title = truncateTitle(title, 50)
		}
		// Show deletion time as just the date portion
		deletedDate := t.deletedAt
		if idx := strings.IndexByte(deletedDate, 'T'); idx > 0 {
			deletedDate = deletedDate[:idx]
		}
		fmt.Printf("  %s  (deleted %s by %s)\n", title, deletedDate, t.deletedBy)
	}
}

// findPendingTombstones finds tombstones in the sync dir that haven't been committed yet
func findPendingTombstones(syncDir string) []tombstoneInfo {
	var pending []tombstoneInfo

	// Find deletions.json files with uncommitted changes
	modifiedCmd := exec.Command("git", "diff", "--name-only", "--", "*/"+syncutil.DeletionsFile)
	modifiedCmd.Dir = syncDir
	modifiedOutput, _ := modifiedCmd.Output()

	// Also find untracked (new) deletions.json files
	untrackedCmd := exec.Command("git", "ls-files", "--others", "--exclude-standard", "--", "*/"+syncutil.DeletionsFile)
	untrackedCmd.Dir = syncDir
	untrackedOutput, _ := untrackedCmd.Output()

	// Combine both lists
	var changedPaths []string
	isUntracked := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(modifiedOutput)), "\n") {
		if line != "" {
			changedPaths = append(changedPaths, line)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(untrackedOutput)), "\n") {
		if line != "" {
			changedPaths = append(changedPaths, line)
			isUntracked[line] = true
		}
	}

	for _, relPath := range changedPaths {
		absPath := filepath.Join(syncDir, relPath)
		projectName := filepath.Dir(relPath)

		// Load current working copy
		current, err := syncutil.LoadTombstones(filepath.Dir(absPath))
		if err != nil || current == nil {
			continue
		}

		// For untracked files, all tombstones are new
		committedIDs := make(map[string]bool)
		if !isUntracked[relPath] {
			// Load committed version via git show
			committedIDs = loadCommittedTombstoneIDs(syncDir, relPath)
		}

		// Find tombstones not in the committed version
		for _, t := range current.Entries {
			if !committedIDs[t.SessionID] {
				pending = append(pending, tombstoneInfo{
					sessionID: t.SessionID,
					deletedAt: t.DeletedAt,
					deletedBy: t.DeletedBy,
					project:   projectName,
				})
			}
		}
	}

	return pending
}

// loadCommittedTombstoneIDs returns the set of session IDs from the committed version of a deletions.json
func loadCommittedTombstoneIDs(syncDir, relPath string) map[string]bool {
	ids := make(map[string]bool)

	showCmd := exec.Command("git", "show", "HEAD:"+relPath)
	showCmd.Dir = syncDir
	output, err := showCmd.Output()
	if err != nil {
		return ids
	}

	var deletions syncutil.Deletions
	if err := json.Unmarshal(output, &deletions); err != nil {
		return ids
	}

	for _, t := range deletions.Entries {
		ids[t.SessionID] = true
	}
	return ids
}

// showPendingChanges shows conversations with uncommitted changes in the sync dir
func showPendingChanges(syncDir string, titles map[string]string) {
	statusCmd := exec.Command("git", "status", "--short")
	statusCmd.Dir = syncDir
	output, err := statusCmd.Output()
	if err != nil {
		return
	}

	if len(output) == 0 {
		fmt.Printf("\nNo pending conversation changes to sync.\n")
		return
	}

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

		// Skip deletions.json — those are shown in the tombstones section
		if strings.HasSuffix(path, "/"+syncutil.DeletionsFile) {
			continue
		}

		uuid := extractConversationUUID(path)
		if uuid != "" {
			conversations[uuid]++
		} else {
			otherChanges++
		}
	}

	if len(conversations) > 0 {
		fmt.Printf("\nModified conversations (%d):\n", len(conversations))
		for uuid, count := range conversations {
			title := titles[uuid]
			if title == "" {
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
