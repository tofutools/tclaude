package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

// RelocateLegacyState moves pre-split non-database state into the private
// data directory. It runs from the common CLI pre-run path as well as daemon
// startup so a CLI command cannot create an empty destination before the
// daemon gets a chance to move the operator's real state.
func RelocateLegacyState() error {
	if privateConfigIntentionallyInaccessible() || common.PreSplitAgentdReachable() {
		return nil
	}
	root := ConfigDir()
	dataDir := DataDir()
	if root == "" || dataDir == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		slog.Warn("failed to create data dir for legacy-state relocation; continuing on the active layout",
			"path", dataDir, "error", err)
		return nil
	}
	names := []string{
		"operator_token", "debug.log", "config.json", "plugins.json",
		"output.log", "processes", "remote-access", "exports",
		"claude-sessions.migrated", "notify-state.migrated",
	}
	// Rotated logs are just as sensitive as the active log and otherwise stay
	// readable at the tclaude root after the sandbox deny narrows to data/.
	if rotated, err := filepath.Glob(filepath.Join(root, "output.log.*")); err == nil {
		for _, path := range rotated {
			names = append(names, filepath.Base(path))
		}
	}
	for _, name := range names {
		oldPath := filepath.Join(root, name)
		newPath := filepath.Join(dataDir, name)
		if err := relocateLegacyStateEntry(oldPath, newPath, strings.HasPrefix(name, "output.log")); err != nil {
			// Mixed-version processes may recreate legacy entries after the first
			// migration. Never brick every CLI command from PersistentPreRunE:
			// TclaudeStatePath keeps readers on the only existing copy, or prefers
			// data/ when both exist. Preserve non-log conflicts for the operator.
			slog.Warn("failed to relocate legacy state; continuing on the active layout",
				"name", name, "from", oldPath, "to", newPath, "error", err)
		}
	}
	return nil
}

func relocateLegacyStateEntry(oldPath, newPath string, quarantineConflict bool) error {
	if _, err := os.Lstat(newPath); err == nil {
		if _, oldErr := os.Lstat(oldPath); oldErr == nil {
			if quarantineConflict {
				quarantinePath := fmt.Sprintf("%s.legacy-%s-%d", newPath,
					time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid())
				if err := os.Rename(oldPath, quarantinePath); err != nil {
					if os.IsNotExist(err) {
						return nil // a racing invocation already moved it
					}
					return fmt.Errorf("quarantine recreated legacy log %s -> %s: %w", oldPath, quarantinePath, err)
				}
				slog.Warn("quarantined recreated legacy log; canonical data log remains active",
					"from", oldPath, "quarantine", quarantinePath, "active", newPath)
				return nil
			}
			slog.Warn("legacy and data state both exist; preferring data and preserving legacy entry for operator recovery",
				"legacy", oldPath, "active", newPath)
			return nil
		} else if !os.IsNotExist(oldErr) {
			return fmt.Errorf("stat %s: %w", oldPath, oldErr)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", newPath, err)
	}
	if _, err := os.Lstat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", oldPath, err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		if os.IsNotExist(err) {
			// Another migrator can win between Lstat and Rename.
			if _, destErr := os.Lstat(newPath); destErr == nil {
				return nil
			}
		}
		return fmt.Errorf("move %s -> %s: %w", oldPath, newPath, err)
	}
	slog.Info("relocated legacy state into data dir", "from", oldPath, "to", newPath)
	return nil
}
