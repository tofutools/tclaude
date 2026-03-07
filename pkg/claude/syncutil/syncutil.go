// Package syncutil provides shared utilities for Claude conversation sync.
// This package is designed to be imported by both cmd/claude/conv and cmd/claude/git
// without causing import cycles.
package syncutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SyncDir returns the path to the sync directory (~/.claude/projects_sync)
func SyncDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects_sync")
}

// ProjectsDir returns the path to the actual projects directory (~/.claude/projects)
func ProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// IsInitialized checks if the sync directory is a git repository
func IsInitialized() bool {
	gitDir := filepath.Join(SyncDir(), ".git")
	info, err := os.Stat(gitDir)
	return err == nil && info.IsDir()
}

// SyncConfig holds path mapping configuration for cross-machine sync
type SyncConfig struct {
	// Homes lists equivalent home directories across machines
	// The first entry is the canonical form
	// e.g., ["/home/gigur", "/Users/johkjo"]
	Homes []string `json:"homes"`

	// Dirs lists groups of equivalent directories
	// Each group's first entry is the canonical form
	// e.g., [["/home/gigur/git", "/Users/johkjo/git/personal"]]
	Dirs [][]string `json:"dirs"`
}

// ConfigPath returns the path to the sync config file
// Prefers sync dir (auto-synced) over local ~/.claude
func ConfigPath() string {
	// First check sync dir (shared across machines)
	syncPath := filepath.Join(SyncDir(), "sync_config.json")
	if _, err := os.Stat(syncPath); err == nil {
		return syncPath
	}

	// Fall back to local ~/.claude
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sync_config.json")
}

// LoadConfig loads the sync config, returning empty config if not found
func LoadConfig() (*SyncConfig, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &SyncConfig{}, nil
		}
		return nil, err
	}

	var config SyncConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// SaveConfig saves the sync config
func SaveConfig(config *SyncConfig) error {
	path := ConfigPath()
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ProjectDirToPath converts a Claude project dir name back to a path
// e.g., "-home-gigur-git-tofu" -> "/home/gigur/git/tofu"
func ProjectDirToPath(projectDir string) string {
	// Remove leading dash and convert dashes to path separators
	if strings.HasPrefix(projectDir, "-") {
		projectDir = projectDir[1:]
	}
	return "/" + strings.ReplaceAll(projectDir, "-", "/")
}

// PathToProjectDir converts a path to Claude project dir name
// e.g., "/home/gigur/git/tofu" -> "-home-gigur-git-tofu"
func PathToProjectDir(path string) string {
	// Normalize path separators and remove leading slash
	path = filepath.ToSlash(path)
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}
	return "-" + strings.ReplaceAll(path, "/", "-")
}

// CanonicalizeProjectDir converts a project dir to its canonical form
// by applying path mappings from the config
func (c *SyncConfig) CanonicalizeProjectDir(projectDir string) string {
	if c == nil {
		return projectDir
	}

	// Convert to path for easier manipulation
	path := ProjectDirToPath(projectDir)

	// Apply dirs mappings (check all entries in each group)
	path = c.applyDirsMapping(path)

	// Apply homes mappings
	path = c.applyHomesMapping(path)

	// Convert back to project dir
	return PathToProjectDir(path)
}

// applyDirsMapping applies directory mappings to a path
func (c *SyncConfig) applyDirsMapping(path string) string {
	for _, group := range c.Dirs {
		if len(group) < 2 {
			continue
		}
		canonical := group[0]
		for _, dir := range group[1:] {
			if strings.HasPrefix(path, dir) {
				return canonical + path[len(dir):]
			}
		}
	}
	return path
}

// applyHomesMapping applies home directory mappings to a path
func (c *SyncConfig) applyHomesMapping(path string) string {
	if len(c.Homes) < 2 {
		return path
	}
	canonical := c.Homes[0]
	for _, home := range c.Homes[1:] {
		if strings.HasPrefix(path, home) {
			return canonical + path[len(home):]
		}
	}
	return path
}

// FindEquivalentProjectDirs returns all project dir names that map to the same canonical form
func (c *SyncConfig) FindEquivalentProjectDirs(projectDir string) []string {
	if c == nil {
		return []string{projectDir}
	}

	canonical := c.CanonicalizeProjectDir(projectDir)
	result := []string{canonical}

	// Generate all possible variants by applying reverse mappings
	canonicalPath := ProjectDirToPath(canonical)

	// For each home variant
	for _, home := range c.Homes {
		if len(c.Homes) > 0 && strings.HasPrefix(canonicalPath, c.Homes[0]) {
			variant := home + canonicalPath[len(c.Homes[0]):]
			variantDir := PathToProjectDir(variant)
			if variantDir != canonical && !contains(result, variantDir) {
				result = append(result, variantDir)
			}
		}
	}

	// For each dirs variant
	for _, group := range c.Dirs {
		if len(group) < 2 {
			continue
		}
		if strings.HasPrefix(canonicalPath, group[0]) {
			for _, dir := range group[1:] {
				variant := dir + canonicalPath[len(group[0]):]
				variantDir := PathToProjectDir(variant)
				if !contains(result, variantDir) {
					result = append(result, variantDir)
				}
			}
		}
	}

	return result
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// LocalizePath converts a path to the local machine's equivalent path
// It first canonicalizes the path (in case it's from another machine),
// then converts to local form if not on the canonical machine
func (c *SyncConfig) LocalizePath(path, localHome string) string {
	if c == nil || len(c.Homes) == 0 {
		return path
	}

	canonical := c.Homes[0]

	// First, canonicalize the path (handles case where path is from another machine)
	path = c.canonicalizePath(path)

	// Find which home prefix applies to this machine
	var localPrefix string
	for _, home := range c.Homes {
		if home == localHome || strings.HasPrefix(localHome, home) {
			localPrefix = home
			break
		}
	}

	// If we're on the canonical machine, path is already correct
	if localPrefix == "" || localPrefix == canonical {
		// Still need to canonicalize embedded project dirs
		return c.canonicalizeEmbeddedProjectDir(path)
	}

	// Convert from canonical to local
	// Check dirs mappings first (more specific)
	for _, group := range c.Dirs {
		if len(group) < 2 {
			continue
		}
		canonicalDir := group[0]
		if strings.HasPrefix(path, canonicalDir) {
			for _, dir := range group[1:] {
				if strings.HasPrefix(dir, localPrefix) {
					path = dir + path[len(canonicalDir):]
					break
				}
			}
		}
	}

	// Apply homes mapping
	if strings.HasPrefix(path, canonical) {
		path = localPrefix + path[len(canonical):]
	}

	// Also localize embedded project directory names
	path = c.localizeEmbeddedProjectDir(path, localHome)

	return path
}

// canonicalizePath converts any path to canonical form
func (c *SyncConfig) canonicalizePath(path string) string {
	// Apply dirs mappings first
	for _, group := range c.Dirs {
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
	if len(c.Homes) >= 2 {
		canonical := c.Homes[0]
		for _, home := range c.Homes[1:] {
			if strings.HasPrefix(path, home) {
				path = canonical + path[len(home):]
				break
			}
		}
	}

	return path
}

// canonicalizeEmbeddedProjectDir canonicalizes project dir names embedded in paths
func (c *SyncConfig) canonicalizeEmbeddedProjectDir(path string) string {
	projectsMarker := ".claude/projects/"
	idx := strings.Index(path, projectsMarker)
	if idx == -1 {
		return path
	}

	start := idx + len(projectsMarker)
	end := strings.Index(path[start:], "/")
	if end == -1 {
		projectDir := path[start:]
		canonicalDir := c.CanonicalizeProjectDir(projectDir)
		if projectDir != canonicalDir {
			return path[:start] + canonicalDir
		}
		return path
	}

	projectDir := path[start : start+end]
	canonicalDir := c.CanonicalizeProjectDir(projectDir)
	if projectDir != canonicalDir {
		return path[:start] + canonicalDir + path[start+end:]
	}
	return path
}

// localizeEmbeddedProjectDir finds and localizes project dir names embedded in paths
func (c *SyncConfig) localizeEmbeddedProjectDir(path, localHome string) string {
	projectsMarker := ".claude/projects/"
	idx := strings.Index(path, projectsMarker)
	if idx == -1 {
		return path
	}

	start := idx + len(projectsMarker)
	end := strings.Index(path[start:], "/")
	if end == -1 {
		projectDir := path[start:]
		localDir := c.LocalizeProjectDir(projectDir, localHome)
		if projectDir != localDir {
			return path[:start] + localDir
		}
		return path
	}

	projectDir := path[start : start+end]
	localDir := c.LocalizeProjectDir(projectDir, localHome)
	if projectDir != localDir {
		return path[:start] + localDir + path[start+end:]
	}
	return path
}

// LocalizeProjectDir converts a project dir to the local machine's form
func (c *SyncConfig) LocalizeProjectDir(projectDir, localHome string) string {
	if c == nil || len(c.Homes) == 0 {
		return projectDir
	}

	path := ProjectDirToPath(projectDir)
	localPath := c.LocalizePath(path, localHome)
	return PathToProjectDir(localPath)
}

// Tombstone constants
const (
	// TombstoneMaxAge is the maximum age of tombstones before they are pruned
	TombstoneMaxAge = 30 * 24 * time.Hour // 30 days
)

// Tombstone represents a deleted session
type Tombstone struct {
	SessionID string `json:"sessionId"`
	DeletedAt string `json:"deletedAt"`
	DeletedBy string `json:"deletedBy"`
}

// Deletions holds the tombstone data for a project
type Deletions struct {
	Version int         `json:"version"`
	Entries []Tombstone `json:"entries"`
}

// DeletionsFile is the filename for tombstone data
const DeletionsFile = "deletions.json"

// LoadTombstones loads the deletions file from a project directory
func LoadTombstones(projectDir string) (*Deletions, error) {
	path := filepath.Join(projectDir, DeletionsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Deletions{Version: 1}, nil
		}
		return nil, err
	}

	var deletions Deletions
	if err := json.Unmarshal(data, &deletions); err != nil {
		return nil, err
	}
	return &deletions, nil
}

// SaveTombstones saves the deletions file to a project directory
func SaveTombstones(projectDir string, deletions *Deletions) error {
	path := filepath.Join(projectDir, DeletionsFile)
	data, err := json.MarshalIndent(deletions, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// AddTombstone adds a tombstone for a deleted session
func AddTombstone(projectDir, sessionID string) error {
	deletions, err := LoadTombstones(projectDir)
	if err != nil {
		return err
	}

	// Check if tombstone already exists
	for _, t := range deletions.Entries {
		if t.SessionID == sessionID {
			return nil // Already tombstoned
		}
	}

	hostname, _ := os.Hostname()
	tombstone := Tombstone{
		SessionID: sessionID,
		DeletedAt: time.Now().UTC().Format(time.RFC3339),
		DeletedBy: hostname,
	}

	deletions.Entries = append(deletions.Entries, tombstone)
	return SaveTombstones(projectDir, deletions)
}

// MergeTombstones merges src tombstones into dst
// Returns true if dst was modified
func MergeTombstones(src, dst *Deletions) bool {
	if src == nil || len(src.Entries) == 0 {
		return false
	}

	modified := false
	existing := make(map[string]int)

	for i, t := range dst.Entries {
		existing[t.SessionID] = i
	}

	for _, srcEntry := range src.Entries {
		if idx, ok := existing[srcEntry.SessionID]; ok {
			srcTime, _ := time.Parse(time.RFC3339, srcEntry.DeletedAt)
			dstTime, _ := time.Parse(time.RFC3339, dst.Entries[idx].DeletedAt)
			if srcTime.Before(dstTime) {
				dst.Entries[idx] = srcEntry
				modified = true
			}
		} else {
			dst.Entries = append(dst.Entries, srcEntry)
			existing[srcEntry.SessionID] = len(dst.Entries) - 1
			modified = true
		}
	}

	return modified
}

// CleanupOldTombstones removes tombstones older than maxAge
func CleanupOldTombstones(deletions *Deletions, maxAge time.Duration) int {
	if deletions == nil || len(deletions.Entries) == 0 {
		return 0
	}

	cutoff := time.Now().UTC().Add(-maxAge)
	kept := make([]Tombstone, 0, len(deletions.Entries))
	removed := 0

	for _, t := range deletions.Entries {
		deletedAt, err := time.Parse(time.RFC3339, t.DeletedAt)
		if err != nil {
			kept = append(kept, t)
			continue
		}

		if deletedAt.After(cutoff) {
			kept = append(kept, t)
		} else {
			removed++
		}
	}

	deletions.Entries = kept
	return removed
}

// HasTombstone checks if a session has a tombstone
func (d *Deletions) HasTombstone(sessionID string) bool {
	if d == nil {
		return false
	}
	for _, t := range d.Entries {
		if t.SessionID == sessionID {
			return true
		}
	}
	return false
}

// TombstonedSessionIDs returns a set of all tombstoned session IDs
func (d *Deletions) TombstonedSessionIDs() map[string]bool {
	result := make(map[string]bool)
	if d == nil {
		return result
	}
	for _, t := range d.Entries {
		result[t.SessionID] = true
	}
	return result
}
