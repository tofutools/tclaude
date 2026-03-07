package git

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type SyncParams struct {
	KeepLocal  bool `long:"keep-local" help:"On conflict, keep local version without asking"`
	KeepRemote bool `long:"keep-remote" help:"On conflict, keep remote version without asking"`
	DryRun     bool `long:"dry-run" help:"Show what would be synced without making changes"`
}

func SyncCmd() *cobra.Command {
	return boa.CmdT[SyncParams]{
		Use:         "sync",
		Short:       "Sync Claude conversations with remote",
		Long:        "Sync local Claude conversations with the remote git repository.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *SyncParams, cmd *cobra.Command, args []string) {
			if err := runSync(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runSync(params *SyncParams) error {
	syncDir := SyncDir()
	projectsDir := ProjectsDir()

	if !IsInitialized() {
		return fmt.Errorf("git sync not initialized. Run 'tclaude git init <repo-url>' first")
	}

	fmt.Printf("Syncing Claude conversations...\n\n")

	// Step 1: Pull remote changes into sync dir
	fmt.Printf("Step 1: Pulling remote changes...\n")
	if !params.DryRun {
		pullCmd := exec.Command("git", "pull", "--ff-only")
		pullCmd.Dir = syncDir
		pullCmd.Stdout = os.Stdout
		pullCmd.Stderr = os.Stderr
		if err := pullCmd.Run(); err != nil {
			// Might be first sync or diverged - try fetch instead
			fmt.Printf("  (pull failed, trying reset to remote)\n")
			fetchCmd := exec.Command("git", "fetch", "origin")
			fetchCmd.Dir = syncDir
			fetchCmd.Run()

			// Reset to remote if we have uncommitted local changes in sync dir
			// (this is safe because local source of truth is ~/.claude/projects)
			remoteBranch := getRemoteBranch(syncDir)
			if remoteBranch != "" {
				resetCmd := exec.Command("git", "reset", "--hard", "origin/"+remoteBranch)
				resetCmd.Dir = syncDir
				resetCmd.Run()
			}
		}
	}

	// Step 2: Merge local conversations into sync directory
	fmt.Printf("\nStep 2: Merging local conversations...\n")
	if err := mergeLocalToSync(projectsDir, syncDir, params); err != nil {
		return fmt.Errorf("failed to merge local changes: %w", err)
	}

	// Also update sync dirs that don't have local counterparts
	// This ensures unindexed files are included in the index
	if !params.DryRun {
		updateUnprocessedSyncDirs(projectsDir, syncDir)
	}

	// Clean up old tombstones (>30 days)
	if !params.DryRun {
		cleanupOldTombstonesInSync(syncDir)
	}

	// Step 3: Commit changes
	fmt.Printf("\nStep 3: Committing changes...\n")
	if !params.DryRun {
		// Add all changes
		addCmd := exec.Command("git", "add", "-A")
		addCmd.Dir = syncDir
		if output, err := addCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git add failed: %s\n%w", output, err)
		}

		// Check if there are changes to commit
		diffCmd := exec.Command("git", "diff", "--cached", "--quiet")
		diffCmd.Dir = syncDir
		if err := diffCmd.Run(); err != nil {
			// There are changes to commit
			hostname, _ := os.Hostname()
			commitMsg := fmt.Sprintf("sync from %s at %s", hostname, time.Now().Format(time.RFC3339))
			commitCmd := exec.Command("git", "commit", "-m", commitMsg)
			commitCmd.Dir = syncDir
			if output, err := commitCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git commit failed: %s\n%w", output, err)
			}
			fmt.Printf("  Committed changes\n")
		} else {
			fmt.Printf("  No changes to commit\n")
		}
	}

	// Step 4: Push to remote
	fmt.Printf("\nStep 4: Pushing to remote...\n")
	if !params.DryRun {
		pushCmd := exec.Command("git", "push", "-u", "origin", "HEAD")
		pushCmd.Dir = syncDir
		pushCmd.Stdout = os.Stdout
		pushCmd.Stderr = os.Stderr
		if err := pushCmd.Run(); err != nil {
			return fmt.Errorf("git push failed: %w", err)
		}
	}

	// Step 5: Copy merged results back to local
	fmt.Printf("\nStep 5: Updating local conversations...\n")
	if !params.DryRun {
		if err := copySyncToProjects(syncDir, projectsDir); err != nil {
			return fmt.Errorf("failed to update local projects: %w", err)
		}
	}

	fmt.Printf("\nSync complete!\n")
	return nil
}

// getRemoteBranch returns the remote branch name (main or master), or empty if none
func getRemoteBranch(syncDir string) string {
	// Try main first
	checkCmd := exec.Command("git", "rev-parse", "--verify", "origin/main")
	checkCmd.Dir = syncDir
	if err := checkCmd.Run(); err == nil {
		return "main"
	}
	// Try master
	checkCmd2 := exec.Command("git", "rev-parse", "--verify", "origin/master")
	checkCmd2.Dir = syncDir
	if err := checkCmd2.Run(); err == nil {
		return "master"
	}
	return ""
}

// mergeLocalToSync merges local conversations into the sync directory
// This is an intelligent merge: for sessions-index.json we merge entries,
// for conversation files we copy if newer or prompt on conflict
// Project directories are canonicalized using sync_config.json mappings
func mergeLocalToSync(projectsDir, syncDir string, params *SyncParams) error {
	// Load config for path canonicalization
	config, err := LoadConfig()
	if err != nil {
		fmt.Printf("  Warning: could not load sync config: %v\n", err)
		config = &SyncConfig{} // Continue with empty config
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No projects yet
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		localProject := filepath.Join(projectsDir, entry.Name())

		// Canonicalize the project dir name for sync
		canonicalName := config.CanonicalizeProjectDir(entry.Name())
		syncProject := filepath.Join(syncDir, canonicalName)

		if params.DryRun {
			if canonicalName != entry.Name() {
				fmt.Printf("  Would merge: %s -> %s\n", entry.Name(), canonicalName)
			} else {
				fmt.Printf("  Would merge: %s\n", entry.Name())
			}
			continue
		}

		// Create project directory in sync if needed
		if err := os.MkdirAll(syncProject, 0755); err != nil {
			return err
		}

		// Merge this project
		if err := mergeProject(localProject, syncProject, params); err != nil {
			fmt.Printf("  Warning: merge issue in %s: %v\n", entry.Name(), err)
		}
	}

	return nil
}

// copyProjectsToSync copies conversation files from projects to sync directory
func copyProjectsToSync(projectsDir, syncDir string, dryRun bool) error {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No projects yet
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		srcProject := filepath.Join(projectsDir, entry.Name())
		dstProject := filepath.Join(syncDir, entry.Name())

		if dryRun {
			fmt.Printf("  Would copy: %s\n", entry.Name())
			continue
		}

		// Create project directory in sync
		if err := os.MkdirAll(dstProject, 0755); err != nil {
			return err
		}

		// Copy all files (conversations and index)
		files, err := os.ReadDir(srcProject)
		if err != nil {
			continue
		}

		for _, f := range files {
			if f.IsDir() {
				// Copy subdirectories (conversation data dirs)
				if err := conv.CopyDir(filepath.Join(srcProject, f.Name()), filepath.Join(dstProject, f.Name())); err != nil {
					fmt.Printf("  Warning: failed to copy %s: %v\n", f.Name(), err)
				}
			} else {
				// Copy files
				if err := conv.CopyFile(filepath.Join(srcProject, f.Name()), filepath.Join(dstProject, f.Name())); err != nil {
					fmt.Printf("  Warning: failed to copy %s: %v\n", f.Name(), err)
				}
			}
		}
	}

	return nil
}

// copySyncToProjects copies merged results back to projects directory
// Uses sync_config.json to map canonical paths to local equivalents
// Also localizes paths inside sessions-index.json to use local machine paths
// Deletes local files for tombstoned sessions
func copySyncToProjects(syncDir, projectsDir string) error {
	// Load config for path mapping
	config, err := LoadConfig()
	if err != nil {
		fmt.Printf("  Warning: could not load sync config: %v\n", err)
		config = &SyncConfig{}
	}

	// Get local home for path localization
	localHome, _ := os.UserHomeDir()

	entries, err := os.ReadDir(syncDir)
	if err != nil {
		return err
	}

	// Build set of existing local project dirs for quick lookup
	localDirs := make(map[string]bool)
	if localEntries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range localEntries {
			if e.IsDir() {
				localDirs[e.Name()] = true
			}
		}
	}

	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}

		srcProject := filepath.Join(syncDir, entry.Name())

		// Find the local equivalent project dir
		localName := findLocalEquivalent(entry.Name(), localDirs, config)
		dstProject := filepath.Join(projectsDir, localName)

		// Load tombstones for this project
		tombstones, _ := syncutil.LoadTombstones(srcProject)
		tombstonedIDs := tombstones.TombstonedSessionIDs()

		// Delete local files for tombstoned sessions
		if len(tombstonedIDs) > 0 {
			deleteTombstonedFiles(dstProject, tombstonedIDs)
		}

		// Create project directory
		if err := os.MkdirAll(dstProject, 0755); err != nil {
			return err
		}

		// Copy all files, with special handling for sessions-index.json
		files, err := os.ReadDir(srcProject)
		if err != nil {
			continue
		}

		for _, f := range files {
			srcPath := filepath.Join(srcProject, f.Name())
			dstPath := filepath.Join(dstProject, f.Name())

			// Skip tombstoned session files/dirs
			if isTombstonedFile(f.Name(), tombstonedIDs) {
				continue
			}

			if f.IsDir() {
				if err := conv.CopyDir(srcPath, dstPath); err != nil {
					fmt.Printf("  Warning: failed to copy %s: %v\n", f.Name(), err)
				}
			} else if f.Name() == "sessions-index.json" {
				// Special handling: localize paths in sessions-index.json
				if err := copyAndLocalizeIndex(srcPath, dstPath, config, localHome); err != nil {
					fmt.Printf("  Warning: failed to copy %s: %v\n", f.Name(), err)
				}
			} else {
				if err := conv.CopyFile(srcPath, dstPath); err != nil {
					fmt.Printf("  Warning: failed to copy %s: %v\n", f.Name(), err)
				}
			}
		}
	}

	return nil
}

// deleteTombstonedFiles deletes local files for tombstoned sessions
func deleteTombstonedFiles(projectDir string, tombstonedIDs map[string]bool) {
	for sessionID := range tombstonedIDs {
		// Delete .jsonl file
		jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
		if err := os.Remove(jsonlPath); err == nil {
			fmt.Printf("  Deleted tombstoned: %s.jsonl\n", sessionID[:8])
		}

		// Delete session directory if it exists
		dirPath := filepath.Join(projectDir, sessionID)
		if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
			if err := os.RemoveAll(dirPath); err == nil {
				fmt.Printf("  Deleted tombstoned: %s/\n", sessionID[:8])
			}
		}
	}
}

// isTombstonedFile checks if a file/dir name corresponds to a tombstoned session
func isTombstonedFile(name string, tombstonedIDs map[string]bool) bool {
	// Check for .jsonl files
	if strings.HasSuffix(name, ".jsonl") {
		sessionID := strings.TrimSuffix(name, ".jsonl")
		return tombstonedIDs[sessionID]
	}
	// Check for session directories (UUID format)
	return tombstonedIDs[name]
}

// copyAndLocalizeIndex copies a sessions-index.json while localizing paths
// It also scans for unindexed .jsonl files in the source directory
func copyAndLocalizeIndex(srcPath, dstPath string, config *SyncConfig, localHome string) error {
	srcDir := filepath.Dir(srcPath)

	// Load index using conv package - this includes unindexed sessions
	index, err := conv.LoadSessionsIndex(srcDir)
	if err != nil {
		return err
	}

	// Localize paths in entries
	for i := range index.Entries {
		if index.Entries[i].ProjectPath != "" {
			index.Entries[i].ProjectPath = config.LocalizePath(index.Entries[i].ProjectPath, localHome)
		}
		if index.Entries[i].FullPath != "" {
			index.Entries[i].FullPath = config.LocalizePath(index.Entries[i].FullPath, localHome)
		}
	}

	// Sort by sessionId for stable ordering
	sort.Slice(index.Entries, func(i, j int) bool {
		return index.Entries[i].SessionID < index.Entries[j].SessionID
	})

	// Write localized index
	newData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(dstPath, newData, 0644)
}

// findLocalEquivalent finds the local project dir name for a canonical sync dir
// Returns the canonical name if no local equivalent is found
func findLocalEquivalent(canonicalName string, localDirs map[string]bool, config *SyncConfig) string {
	// If canonical already exists locally, use it
	if localDirs[canonicalName] {
		return canonicalName
	}

	// Find all equivalent project dirs and check if any exist locally
	equivalents := config.FindEquivalentProjectDirs(canonicalName)
	for _, eq := range equivalents {
		if localDirs[eq] {
			return eq
		}
	}

	// No local equivalent found, use canonical
	return canonicalName
}

// mergeProject merges a source project into destination project
// Used for merging local conversations into sync directory
func mergeProject(srcProject, dstProject string, params *SyncParams) error {
	// Ensure dst project exists
	os.MkdirAll(dstProject, 0755)

	// Merge tombstones first (so we know what to skip)
	// Load local tombstones (if any exist from previous syncs)
	localTombstones, _ := syncutil.LoadTombstones(srcProject)
	// Load sync tombstones
	syncTombstones, _ := syncutil.LoadTombstones(dstProject)

	// Merge local tombstones into sync
	if syncutil.MergeTombstones(localTombstones, syncTombstones) {
		if err := syncutil.SaveTombstones(dstProject, syncTombstones); err != nil {
			fmt.Printf("    Warning: failed to save merged tombstones: %v\n", err)
		}
	}

	// Get set of tombstoned session IDs
	tombstonedIDs := syncTombstones.TombstonedSessionIDs()

	// Delete tombstoned files from sync directory
	if len(tombstonedIDs) > 0 {
		deleteTombstonedFiles(dstProject, tombstonedIDs)
	}

	// Merge sessions-index.json
	if err := mergeSessionsIndex(srcProject, dstProject); err != nil {
		fmt.Printf("    Warning: index merge issue: %v\n", err)
	}

	// Merge conversation files from source
	srcFiles, _ := os.ReadDir(srcProject)
	for _, f := range srcFiles {
		if f.Name() == "sessions-index.json" || f.Name() == syncutil.DeletionsFile {
			continue // Already handled
		}

		// Skip tombstoned session files
		if isTombstonedFile(f.Name(), tombstonedIDs) {
			continue
		}

		srcPath := filepath.Join(srcProject, f.Name())
		dstPath := filepath.Join(dstProject, f.Name())

		if f.IsDir() {
			// Copy directory (subagents, etc)
			if err := conv.CopyDir(srcPath, dstPath); err != nil {
				fmt.Printf("    Warning: failed to copy dir %s: %v\n", f.Name(), err)
			}
			continue
		}

		// Check if file exists in destination
		if _, err := os.Stat(dstPath); os.IsNotExist(err) {
			// Source only - copy it
			conv.CopyFile(srcPath, dstPath)
			continue
		}

		// Both exist - check if different
		if filesEqual(srcPath, dstPath) {
			continue // Same content
		}

		// Different content - for now, source (local) wins
		// This is safe because we pulled remote first, so sync has remote state
		// and local changes should overwrite
		if strings.HasSuffix(f.Name(), ".jsonl") {
			if err := handleConversationConflict(srcPath, dstPath, f.Name(), params); err != nil {
				fmt.Printf("    Warning: conflict resolution failed for %s: %v\n", f.Name(), err)
			}
		}
	}

	return nil
}

// mergeSessionsIndex intelligently merges src sessions-index.json into dst
// src = user's local projects, dst = sync dir (which has remote state after pull)
// Canonicalizes paths in src entries before merging to avoid overwriting canonical paths
// Filters out tombstoned sessions to prevent deleted conversations from reappearing
func mergeSessionsIndex(srcProject, dstProject string) error {
	// Load config for path canonicalization
	config, _ := LoadConfig()

	dstIndexPath := filepath.Join(dstProject, "sessions-index.json")

	// Load tombstones from sync dir - these indicate deleted sessions
	tombstones, _ := syncutil.LoadTombstones(dstProject)
	tombstonedIDs := tombstones.TombstonedSessionIDs()

	// Load src (local) index - include unindexed sessions
	srcIndex, err := conv.LoadSessionsIndex(srcProject)
	if err != nil {
		srcIndex = &conv.SessionsIndex{Version: 1}
	}

	// Load dst (sync/remote) index - also include unindexed sessions
	// (there may be .jsonl files that exist in sync but aren't in the index)
	dstIndex, err := conv.LoadSessionsIndex(dstProject)
	if err != nil {
		dstIndex = &conv.SessionsIndex{Version: 1}
	}

	// Merge: union of entries, prefer newer modified timestamp
	// Skip tombstoned sessions
	merged := make(map[string]conv.SessionEntry)

	// Add all dst (remote) entries first, canonicalizing paths
	// (paths should already be canonical, but canonicalize to ensure consistency)
	for _, e := range dstIndex.Entries {
		if tombstonedIDs[e.SessionID] {
			continue // Session was deleted, don't include
		}
		if config != nil {
			e.ProjectPath = canonicalizePath(e.ProjectPath, config)
			e.FullPath = canonicalizePath(e.FullPath, config)
		}
		merged[e.SessionID] = e
	}

	// Merge src (local) entries - canonicalize paths before merging
	for _, e := range srcIndex.Entries {
		if tombstonedIDs[e.SessionID] {
			continue // Session was deleted, don't include
		}

		// Canonicalize paths in the entry
		if config != nil {
			e.ProjectPath = canonicalizePath(e.ProjectPath, config)
			e.FullPath = canonicalizePath(e.FullPath, config)
		}

		if existing, ok := merged[e.SessionID]; ok {
			// Both have this entry - keep the one with newer Modified timestamp
			existingTime, _ := time.Parse(time.RFC3339, existing.Modified)
			srcTime, _ := time.Parse(time.RFC3339, e.Modified)
			if srcTime.After(existingTime) {
				merged[e.SessionID] = e
			}
		} else {
			// Local only - add it
			merged[e.SessionID] = e
		}
	}

	// Build result
	dstIndex.Entries = make([]conv.SessionEntry, 0, len(merged))
	for _, e := range merged {
		dstIndex.Entries = append(dstIndex.Entries, e)
	}

	// Sort by sessionId for stable ordering (prevents spurious diffs from map iteration randomness)
	sort.Slice(dstIndex.Entries, func(i, j int) bool {
		return dstIndex.Entries[i].SessionID < dstIndex.Entries[j].SessionID
	})

	// Save merged index to dst
	data, err := json.MarshalIndent(dstIndex, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(dstIndexPath, data, 0644)
}

// handleConversationConflict handles a conflict in a conversation file
// srcPath = user's local file, dstPath = sync dir file (has remote state)
func handleConversationConflict(srcPath, dstPath, filename string, params *SyncParams) error {
	if params.KeepLocal {
		fmt.Printf("    Conflict in %s: keeping local (--keep-local)\n", filename)
		return conv.CopyFile(srcPath, dstPath)
	}

	if params.KeepRemote {
		fmt.Printf("    Conflict in %s: keeping remote (--keep-remote)\n", filename)
		return nil // dst already has remote state
	}

	// Read both files
	localData, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	remoteData, err := os.ReadFile(dstPath)
	if err != nil {
		return err
	}

	localLines := countLinesBytes(localData)
	remoteLines := countLinesBytes(remoteData)

	// Auto-resolve: if one is a prefix of the other, the longer one wins
	// This handles the normal case where messages are only appended
	if len(localData) > len(remoteData) && hasPrefix(localData, remoteData) {
		fmt.Printf("    Auto-resolved %s: local extends remote (%d > %d messages)\n", filename, localLines, remoteLines)
		return conv.CopyFile(srcPath, dstPath)
	}
	if len(remoteData) > len(localData) && hasPrefix(remoteData, localData) {
		fmt.Printf("    Auto-resolved %s: remote extends local (%d > %d messages)\n", filename, remoteLines, localLines)
		return nil // dst already has remote state
	}

	// Content diverged - ask user
	fmt.Printf("\n    Conflict: %s\n", filename)
	fmt.Printf("      Local:  %d messages\n", localLines)
	fmt.Printf("      Remote: %d messages\n", remoteLines)
	fmt.Printf("      (content has diverged, not a simple append)\n")
	fmt.Printf("    Keep which version? [l]ocal / [r]emote / [s]kip: ")

	reader := bufio.NewReader(os.Stdin)
	response, _ := reader.ReadString('\n')
	response = strings.TrimSpace(strings.ToLower(response))

	switch response {
	case "l", "local":
		fmt.Printf("    Keeping local version\n")
		return conv.CopyFile(srcPath, dstPath)
	case "r", "remote":
		fmt.Printf("    Keeping remote version\n")
		return nil // dst already has remote state
	default:
		fmt.Printf("    Skipping (keeping remote)\n")
		return nil
	}
}

// hasPrefix checks if data starts with prefix
func hasPrefix(data, prefix []byte) bool {
	if len(prefix) > len(data) {
		return false
	}
	for i := range prefix {
		if data[i] != prefix[i] {
			return false
		}
	}
	return true
}

// countLinesBytes counts newlines in a byte slice
func countLinesBytes(data []byte) int {
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

// filesEqual checks if two files have the same content
func filesEqual(path1, path2 string) bool {
	data1, err1 := os.ReadFile(path1)
	data2, err2 := os.ReadFile(path2)
	if err1 != nil || err2 != nil {
		return false
	}
	if len(data1) != len(data2) {
		return false
	}
	for i := range data1 {
		if data1[i] != data2[i] {
			return false
		}
	}
	return true
}

// cleanupOldTombstonesInSync removes tombstones older than 30 days from all projects in sync dir
func cleanupOldTombstonesInSync(syncDir string) {
	entries, err := os.ReadDir(syncDir)
	if err != nil {
		return
	}

	totalCleaned := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}

		projectDir := filepath.Join(syncDir, entry.Name())
		tombstones, err := syncutil.LoadTombstones(projectDir)
		if err != nil || len(tombstones.Entries) == 0 {
			continue
		}

		cleaned := syncutil.CleanupOldTombstones(tombstones, syncutil.TombstoneMaxAge)
		if cleaned > 0 {
			if err := syncutil.SaveTombstones(projectDir, tombstones); err == nil {
				totalCleaned += cleaned
			}
		}
	}

	if totalCleaned > 0 {
		fmt.Printf("  Cleaned up %d old tombstones\n", totalCleaned)
	}
}

// updateUnprocessedSyncDirs updates sessions-index.json for sync directories
// that don't have local counterparts. This ensures unindexed .jsonl files
// are included in the index before committing.
func updateUnprocessedSyncDirs(projectsDir, syncDir string) {
	config, _ := LoadConfig()

	// Build set of local project dirs (canonical names)
	localDirs := make(map[string]bool)
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				canonicalName := config.CanonicalizeProjectDir(e.Name())
				localDirs[canonicalName] = true
			}
		}
	}

	// Process sync dirs that don't have local counterparts
	syncEntries, err := os.ReadDir(syncDir)
	if err != nil {
		return
	}

	for _, entry := range syncEntries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}

		// Skip if this sync dir has a local counterpart
		if localDirs[entry.Name()] {
			continue
		}

		projectDir := filepath.Join(syncDir, entry.Name())

		// Load index (includes unindexed sessions)
		index, err := conv.LoadSessionsIndex(projectDir)
		if err != nil {
			continue
		}

		// Canonicalize paths
		if config != nil {
			for i := range index.Entries {
				index.Entries[i].ProjectPath = canonicalizePath(index.Entries[i].ProjectPath, config)
				index.Entries[i].FullPath = canonicalizePath(index.Entries[i].FullPath, config)
			}
		}

		// Sort by sessionId for stable ordering
		sort.Slice(index.Entries, func(i, j int) bool {
			return index.Entries[i].SessionID < index.Entries[j].SessionID
		})

		// Save updated index
		indexPath := filepath.Join(projectDir, "sessions-index.json")
		data, err := json.MarshalIndent(index, "", "  ")
		if err != nil {
			continue
		}
		os.WriteFile(indexPath, data, 0644)
	}
}
