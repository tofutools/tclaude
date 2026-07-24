package agentd

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

var (
	processStoreRootMu       sync.RWMutex
	processStoreRootOverride string
)

func processStoreRoot() string {
	processStoreRootMu.RLock()
	override := processStoreRootOverride
	processStoreRootMu.RUnlock()
	if override != "" {
		return override
	}
	return store.DefaultRoot()
}

// removeLegacyProcessRuntimeData is intentionally narrower than the process
// root: template authoring remains filesystem-backed through the rewrite.
func removeLegacyProcessRuntimeData() error {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return err
	}
	if err := fs.RemoveLegacyRuntimeData(); err != nil {
		return fmt.Errorf("wipe obsolete process runtime: %w", err)
	}
	return nil
}

// wipeIncompatibleProcessRuns deletes run checkpoints whose stored schema
// version is not the engine's current one. Deliberately a wipe, not a
// migration: the process runtime is pre-graduation and flagged off, and
// TCL-622 fixed "next schema change is also a wipe" as policy. Only
// process_runs rows and their cascading evidence rows are affected.
func wipeIncompatibleProcessRuns() error {
	removed, err := db.DeleteProcessRunsWithoutCheckpointVersion(engine.CheckpointVersion)
	if err != nil {
		return fmt.Errorf("wipe incompatible process runs: %w", err)
	}
	if removed > 0 {
		slog.Info("process runtime: wiped runs with incompatible checkpoint schema",
			"count", removed, "version", engine.CheckpointVersion)
	}
	return nil
}

// SetProcessStoreRootForTest redirects the authoring store and P0 cleanup.
func SetProcessStoreRootForTest(root string) func() {
	processStoreRootMu.Lock()
	previous := processStoreRootOverride
	processStoreRootOverride = root
	processStoreRootMu.Unlock()
	return func() {
		processStoreRootMu.Lock()
		processStoreRootOverride = previous
		processStoreRootMu.Unlock()
	}
}
