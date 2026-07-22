package agentd

import (
	"fmt"
	"sync"

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
