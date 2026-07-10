package agentd

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// migrateStateIntoDataDir relocates pre-split daemon-owned state that lives
// directly under ~/.tclaude into ~/.tclaude/data, so the data/ sandbox deny
// covers it as one subtree.
//
// It covers ONLY the files the daemon itself writes (operator_token, debug.log,
// config.json, processes/). Two state files that ANY tclaude invocation opens —
// the SQLite database group and output.log — self-heal earlier in their own
// open paths (db.Open and pkg/common's log writer), so a CLI run before the
// daemon restarts still relocates them instead of creating fresh copies at the
// new path. scribe/ is deliberately NOT moved: it is an agent-facing, cwd-bound
// workdir that must stay writable and reachable, hence outside the denied data/.
//
// It is idempotent — an entry already at the new path (or absent at the old
// one) is a no-op — and serve.go calls it once per startup AFTER
// prepareSocketPath has rejected an already-running daemon, so exactly one
// process ever migrates.
func migrateStateIntoDataDir() error {
	root := config.ConfigDir()
	dataDir := config.DataDir()
	if root == "" || dataDir == "" {
		return nil // no home dir; nothing to migrate
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	for _, name := range []string{
		"operator_token", "debug.log", "config.json", "plugins.json",
		"processes", "remote-access", "exports",
	} {
		if err := relocateStateEntry(filepath.Join(root, name), filepath.Join(dataDir, name), name); err != nil {
			return err
		}
	}
	return nil
}

// relocateStateEntry moves oldPath to newPath (a file or directory) unless
// newPath already exists (idempotent) or oldPath is absent (nothing to move).
func relocateStateEntry(oldPath, newPath, name string) error {
	if _, err := os.Lstat(newPath); err == nil {
		return nil // already at new location
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", newPath, err)
	}
	if _, err := os.Lstat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return nil // nothing at old location
		}
		return fmt.Errorf("stat %s: %w", oldPath, err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("move %s -> %s: %w", oldPath, newPath, err)
	}
	slog.Info("relocated daemon state into data dir", "name", name, "from", oldPath, "to", newPath)
	return nil
}
