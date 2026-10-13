package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// piWalledAuthSyncInterval bounds how long a login a walled pi refreshed
// stays only in the walled copy. A provider that rotates refresh tokens
// leaves the host's own token stale meanwhile, so an unsandboxed pi started
// in that window has to log in again.
const piWalledAuthSyncInterval = 30 * time.Second

// startPiWalledAuthSync carries refreshed OAuth logins between pi's agent
// directory and tclaude's walled copy of it (harness.SyncPiWalledAuth).
func startPiWalledAuthSync(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(piWalledAuthSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := harness.SyncPiWalledAuth(); err != nil {
					slog.Warn("pi walled auth sync failed", "error", err, "module", "agentd")
				}
			case <-stop:
				return
			}
		}
	}()
}
