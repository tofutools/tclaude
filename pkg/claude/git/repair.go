package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/common"
)

type RepairParams struct {
	DryRun bool `long:"dry-run" help:"Show what would be repaired without making changes"`
	Debug  bool `long:"debug" help:"Print debug timing information"`
}

func RepairCmd() *cobra.Command {
	return boa.CmdT[RepairParams]{
		Use:   "repair",
		Short: "Repair sync directory by canonicalizing project paths",
		Long: `Repair the sync directory by merging equivalent project directories.

Uses the path mappings from ~/.claude/sync_config.json to identify
project directories that should be merged (e.g., different home paths
across machines).

This command:
1. Scans the sync directory for project directories
2. Groups them by canonical path (using sync_config.json)
3. Merges equivalent directories together
4. Removes the non-canonical duplicates`,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *RepairParams, cmd *cobra.Command, args []string) {
			if err := runRepair(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runRepair(params *RepairParams) error {
	if params.Debug {
		conv.DebugLog = true
	}

	syncDir := SyncDir()
	projectsDir := ProjectsDir()

	config, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load sync config: %w", err)
	}

	if len(config.Homes) == 0 && len(config.Dirs) == 0 {
		fmt.Printf("No path mappings configured in %s\n", ConfigPath())
		fmt.Printf("Nothing to repair.\n")
		return nil
	}

	// Get local home for path localization
	localHome, _ := os.UserHomeDir()

	fmt.Printf("Config: homes=%v, dirs=%d groups\n", config.Homes, len(config.Dirs))
	fmt.Printf("Local home: %s\n\n", localHome)

	// Repair local projects directory (localize paths to this machine)
	fmt.Printf("=== Repairing local projects (%s) ===\n\n", projectsDir)
	step1Start := time.Now()
	localCount, err := repairDirectory(projectsDir, config, params.DryRun, localHome)
	if err != nil {
		fmt.Printf("Warning: local repair failed: %v\n", err)
	}
	if params.Debug {
		fmt.Printf("[DEBUG] Local repair took %v\n", time.Since(step1Start))
	}

	// Repair sync directory (canonicalize paths)
	syncCount := 0
	if IsInitialized() {
		fmt.Printf("\n=== Repairing sync directory (%s) ===\n\n", syncDir)
		step2Start := time.Now()
		syncCount, err = repairDirectory(syncDir, config, params.DryRun, "")
		if err != nil {
			fmt.Printf("Warning: sync repair failed: %v\n", err)
		}
		if params.Debug {
			fmt.Printf("[DEBUG] Sync repair took %v\n", time.Since(step2Start))
		}
	}

	totalCount := localCount + syncCount

	// Step 3: Fix projectPath inconsistencies (sessions stored in wrong project dir)
	fmt.Printf("\n=== Fixing projectPath inconsistencies ===\n\n")
	step3Start := time.Now()
	localFixed, err := fixProjectPathInconsistencies(projectsDir, config, params.DryRun, localHome)
	if err != nil {
		fmt.Printf("Warning: projectPath fix failed for local: %v\n", err)
	}
	if params.Debug {
		fmt.Printf("[DEBUG] Local projectPath fix took %v\n", time.Since(step3Start))
	}

	syncFixed := 0
	if IsInitialized() {
		step4Start := time.Now()
		syncFixed, err = fixProjectPathInconsistencies(syncDir, config, params.DryRun, "")
		if err != nil {
			fmt.Printf("Warning: projectPath fix failed for sync: %v\n", err)
		}
		if params.Debug {
			fmt.Printf("[DEBUG] Sync projectPath fix took %v\n", time.Since(step4Start))
		}
	}

	totalFixed := localFixed + syncFixed
	if totalFixed > 0 {
		if params.DryRun {
			fmt.Printf("Would fix projectPath in %d session entries.\n", totalFixed)
		} else {
			fmt.Printf("Fixed projectPath in %d session entries.\n", totalFixed)
		}
	} else {
		fmt.Printf("No projectPath inconsistencies found.\n")
	}

	if totalCount == 0 && totalFixed == 0 {
		fmt.Printf("\nNo projects need repair.\n")
	} else if params.DryRun {
		fmt.Printf("\nWould repair %d project groups and fix %d paths. Run without --dry-run to apply.\n", totalCount, totalFixed)
	} else {
		fmt.Printf("\nRepaired %d project groups, fixed %d paths.\n", totalCount, totalFixed)
		if IsInitialized() {
			fmt.Printf("Run 'tclaude git sync' to commit and push the repairs.\n")
		}
	}

	return nil
}

// repairDirectory repairs a single directory by merging equivalent project dirs
// If localHome is non-empty, paths are localized to that home; otherwise they are canonicalized
func repairDirectory(dir string, config *SyncConfig, dryRun bool, localHome string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read directory: %w", err)
	}

	// Group projects by canonical name
	groups := make(map[string][]string) // canonical -> [original dirs]
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}

		canonical := config.CanonicalizeProjectDir(entry.Name())
		groups[canonical] = append(groups[canonical], entry.Name())
	}

	// Find groups that need merging
	// For local repair (localHome != ""), target is the localized form
	// For sync repair (localHome == ""), target is the canonical form
	mergeCount := 0
	for canonical, originals := range groups {
		// Determine the target directory name
		var target string
		if localHome != "" {
			target = config.LocalizeProjectDir(canonical, localHome)
		} else {
			target = canonical
		}

		needsMerge := len(originals) > 1 || (len(originals) == 1 && originals[0] != target)

		if !needsMerge {
			continue
		}

		mergeCount++
		fmt.Printf("Merge group: %s\n", target)
		for _, orig := range originals {
			if orig == target {
				fmt.Printf("  - %s (target)\n", orig)
			} else {
				fmt.Printf("  - %s -> merge into target\n", orig)
			}
		}

		if dryRun {
			fmt.Printf("  [dry-run] Would merge %d directories\n\n", len(originals))
			continue
		}

		// Perform the merge
		targetPath := filepath.Join(dir, target)

		// Ensure target directory exists
		if err := os.MkdirAll(targetPath, 0755); err != nil {
			return 0, fmt.Errorf("failed to create target directory: %w", err)
		}

		// Merge each non-target directory into target
		for _, orig := range originals {
			if orig == target {
				continue
			}

			origPath := filepath.Join(dir, orig)
			fmt.Printf("  Merging %s...\n", orig)

			if err := mergeProjectIntoCanonical(origPath, targetPath); err != nil {
				fmt.Printf("    Warning: merge failed: %v\n", err)
				continue
			}

			// Remove the original (now merged) directory
			if err := os.RemoveAll(origPath); err != nil {
				fmt.Printf("    Warning: failed to remove %s: %v\n", orig, err)
			}
		}
		fmt.Println()
	}

	// Update paths in all sessions-index.json files
	// If localHome is set, localize paths; otherwise canonicalize
	if !dryRun {
		pathsFixed, err := updateSessionPaths(dir, config, localHome)
		if err != nil {
			fmt.Printf("Warning: failed to update session paths: %v\n", err)
		} else if pathsFixed > 0 {
			if localHome != "" {
				fmt.Printf("Localized paths in %d session index files\n", pathsFixed)
			} else {
				fmt.Printf("Canonicalized paths in %d session index files\n", pathsFixed)
			}
		}
	}

	return mergeCount, nil
}

// updateSessionPaths updates projectPath in all sessions-index.json files
// If localHome is non-empty, paths are localized; otherwise they are canonicalized
func updateSessionPaths(dir string, config *SyncConfig, localHome string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}

	fixed := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}

		indexPath := filepath.Join(dir, entry.Name(), "sessions-index.json")
		wasFixed, err := updateIndexFile(indexPath, config, localHome)
		if err != nil {
			// Just warn, don't fail
			continue
		}
		if wasFixed {
			fixed++
		}
	}

	return fixed, nil
}

// loadIndexWithUnindexed reads sessions-index.json and adds entries for any .jsonl files
// that aren't already in the index. This ensures the repair/sync code picks up all conversations.
func loadIndexWithUnindexed(projectDir string) (*conv.SessionsIndex, error) {
	index, err := convops.LoadSessionsIndex(projectDir)
	if err != nil {
		return nil, err
	}

	indexed := make(map[string]bool, len(index.Entries))
	for _, e := range index.Entries {
		indexed[e.SessionID] = true
	}
	if files, err := os.ReadDir(projectDir); err == nil {
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			if indexed[id] {
				continue
			}
			filePath := filepath.Join(projectDir, f.Name())
			if entry := conv.ParseJSONLSessionPublic(filePath, id); entry != nil {
				index.Entries = append(index.Entries, *entry)
			}
		}
	}
	return index, nil
}

// updateIndexFile updates projectPath fields in a sessions-index.json
// If localHome is non-empty, paths are localized; otherwise they are canonicalized
// Also rebuilds the index to include all .jsonl files (prevents unindexed files from adding old paths)
// Filters out tombstoned sessions to respect deletions
// Returns true if the file was modified
func updateIndexFile(indexPath string, config *SyncConfig, localHome string) (bool, error) {
	projectDir := filepath.Dir(indexPath)

	// Load tombstones to filter out deleted sessions
	tombstones, _ := syncutil.LoadTombstones(projectDir)
	tombstonedIDs := tombstones.TombstonedSessionIDs()

	// Read sessions-index.json and include unindexed .jsonl files
	index, err := loadIndexWithUnindexed(projectDir)
	if err != nil {
		return false, err
	}

	// Filter out tombstoned sessions and update paths
	filtered := make([]conv.SessionEntry, 0, len(index.Entries))
	for i := range index.Entries {
		// Skip tombstoned sessions
		if tombstonedIDs[index.Entries[i].SessionID] {
			continue
		}

		// Update projectPath
		origPath := index.Entries[i].ProjectPath
		if origPath != "" {
			if localHome != "" {
				index.Entries[i].ProjectPath = config.LocalizePath(origPath, localHome)
			} else {
				index.Entries[i].ProjectPath = canonicalizePath(origPath, config)
			}
		}

		// Update fullPath
		origFull := index.Entries[i].FullPath
		if origFull != "" {
			if localHome != "" {
				index.Entries[i].FullPath = config.LocalizePath(origFull, localHome)
			} else {
				index.Entries[i].FullPath = canonicalizePath(origFull, config)
			}
		}

		filtered = append(filtered, index.Entries[i])
	}
	index.Entries = filtered

	// Always write the complete index to prevent unindexed files from adding old paths back
	newData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return false, err
	}

	// Check if content changed
	oldData, _ := os.ReadFile(indexPath)
	if string(oldData) == string(newData) {
		return false, nil
	}

	return true, os.WriteFile(indexPath, newData, 0644)
}

// canonicalizePath converts a filesystem path to its canonical form
// It also canonicalizes embedded project directory names in fullPath-style paths
func canonicalizePath(path string, config *SyncConfig) string {
	// Apply dirs mappings first
	for _, group := range config.Dirs {
		if len(group) < 2 {
			continue
		}
		canonical := group[0]
		for _, dir := range group[1:] {
			if strings.HasPrefix(path, dir) {
				path = canonical + path[len(dir):]
				break
			}
		}
	}

	// Apply homes mappings
	if len(config.Homes) >= 2 {
		canonical := config.Homes[0]
		for _, home := range config.Homes[1:] {
			if strings.HasPrefix(path, home) {
				path = canonical + path[len(home):]
				break
			}
		}
	}

	// Also canonicalize embedded project directory names (e.g., in fullPath)
	// Look for patterns like /.claude/projects/-path-style-dir/
	path = canonicalizeEmbeddedProjectDir(path, config)

	return path
}

// canonicalizeEmbeddedProjectDir finds and canonicalizes project dir names embedded in paths
// e.g., /home/gigur/.claude/projects/-Users-alice-git-myproject/session.jsonl
// becomes /home/gigur/.claude/projects//home/alice/git/myproject/session.jsonl
func canonicalizeEmbeddedProjectDir(path string, config *SyncConfig) string {
	// Look for .claude/projects/ pattern
	projectsMarker := ".claude/projects/"
	idx := strings.Index(path, projectsMarker)
	if idx == -1 {
		return path
	}

	// Find the project dir name (starts after projects/, ends at next /)
	start := idx + len(projectsMarker)
	end := strings.Index(path[start:], "/")
	if end == -1 {
		// Project dir is the last component
		projectDir := path[start:]
		canonicalDir := config.CanonicalizeProjectDir(projectDir)
		if projectDir != canonicalDir {
			return path[:start] + canonicalDir
		}
		return path
	}

	// Extract, canonicalize, and replace
	projectDir := path[start : start+end]
	canonicalDir := config.CanonicalizeProjectDir(projectDir)
	if projectDir != canonicalDir {
		return path[:start] + canonicalDir + path[start+end:]
	}
	return path
}

// mergeProjectIntoCanonical merges a source project into the canonical project
func mergeProjectIntoCanonical(srcPath, dstPath string) error {
	// Merge sessions-index.json
	srcIndex := filepath.Join(srcPath, "sessions-index.json")
	dstIndex := filepath.Join(dstPath, "sessions-index.json")

	if _, err := os.Stat(srcIndex); err == nil {
		if err := mergeSessionsIndexFiles(srcIndex, dstIndex); err != nil {
			fmt.Printf("    Warning: index merge issue: %v\n", err)
		}
	}

	// Copy all other files/directories
	entries, err := os.ReadDir(srcPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.Name() == "sessions-index.json" {
			continue // Already handled
		}

		srcFile := filepath.Join(srcPath, entry.Name())
		dstFile := filepath.Join(dstPath, entry.Name())

		// Check if destination exists
		if _, err := os.Stat(dstFile); err == nil {
			// Destination exists - skip (prefer existing canonical version)
			continue
		}

		// Copy to canonical location
		if entry.IsDir() {
			if err := conv.CopyDir(srcFile, dstFile); err != nil {
				fmt.Printf("    Warning: failed to copy dir %s: %v\n", entry.Name(), err)
			}
		} else {
			if err := conv.CopyFile(srcFile, dstFile); err != nil {
				fmt.Printf("    Warning: failed to copy file %s: %v\n", entry.Name(), err)
			}
		}
	}

	return nil
}

// mergeSessionsIndexFiles merges src sessions-index.json into dst
func mergeSessionsIndexFiles(srcPath, dstPath string) error {
	// Use the existing merge function with project dirs
	srcDir := filepath.Dir(srcPath)
	dstDir := filepath.Dir(dstPath)
	return mergeSessionsIndex(srcDir, dstDir)
}

// fixProjectPathInconsistencies scans all sessions-index.json files and fixes entries
// where the projectPath doesn't match the project directory where the file is stored.
// This can happen when conversations are moved or synced incorrectly.
// Returns the number of entries fixed.
func fixProjectPathInconsistencies(dir string, config *SyncConfig, dryRun bool, localHome string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	totalFixed := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}

		projectDirPath := filepath.Join(dir, entry.Name())
		indexPath := filepath.Join(projectDirPath, "sessions-index.json")

		fixed, err := fixProjectPathInIndex(indexPath, entry.Name(), config, dryRun, localHome)
		if err != nil {
			// Just warn, don't fail the whole operation
			continue
		}
		totalFixed += fixed
	}

	return totalFixed, nil
}

// fixProjectPathInIndex fixes projectPath inconsistencies in a single sessions-index.json
// projectDirName is the name of the project directory (e.g., "-home-gigur-git-forge")
func fixProjectPathInIndex(indexPath, projectDirName string, config *SyncConfig, dryRun bool, localHome string) (int, error) {
	projectDir := filepath.Dir(indexPath)

	// Read sessions-index.json directly (repair operates on JSON files, not DB-backed scan)
	index, err := convops.LoadSessionsIndex(projectDir)
	if err != nil {
		return 0, err
	}

	// The expected project path derived from the directory name
	// e.g., "-home-gigur-git-forge" -> "/home/gigur/git/forge"
	expectedProjectPath := ProjectDirToPath(projectDirName)
	expectedCanonical := config.CanonicalizeProjectDir(projectDirName)

	fixed := 0
	for i := range index.Entries {
		origPath := index.Entries[i].ProjectPath
		if origPath == "" {
			continue
		}

		// Check if the projectPath's canonical form matches the directory's canonical form
		entryProjectDir := PathToProjectDir(origPath)
		entryCanonical := config.CanonicalizeProjectDir(entryProjectDir)

		if entryCanonical != expectedCanonical {
			// The projectPath is wrong - it doesn't match the directory where the file lives
			fixed++
			if dryRun {
				sessionIDPrefix := index.Entries[i].SessionID
				if len(sessionIDPrefix) > 8 {
					sessionIDPrefix = sessionIDPrefix[:8]
				}
				fmt.Printf("  Would fix: %s\n", sessionIDPrefix)
				fmt.Printf("    projectPath: %s -> %s\n", origPath, expectedProjectPath)
			} else {
				// Apply the correct path (localized or canonical as appropriate)
				if localHome != "" {
					index.Entries[i].ProjectPath = config.LocalizePath(expectedProjectPath, localHome)
				} else {
					index.Entries[i].ProjectPath = canonicalizePath(expectedProjectPath, config)
				}
			}
		}
	}

	if fixed > 0 && !dryRun {
		// Write the updated index
		newData, err := json.MarshalIndent(index, "", "  ")
		if err != nil {
			return fixed, err
		}
		if err := os.WriteFile(indexPath, newData, 0644); err != nil {
			return fixed, err
		}
		fmt.Printf("  Fixed %d entries in %s\n", fixed, projectDirName)
	}

	return fixed, nil
}
