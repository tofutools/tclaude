package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// piWalledAuthSyncInterval bounds how long a login a walled pi refreshed
// stays only in the walled copy. A provider that rotates refresh tokens
// leaves the host's own token stale meanwhile, so an unsandboxed pi started
// in that window has to log in again.
const piWalledAuthSyncInterval = 30 * time.Second

// piConversationHasSession reports whether a conversation still has a
// session that has not exited, so its walled agent directory is kept. A
// lookup that fails keeps it.
func piConversationHasSession(conversation string) bool {
	rows, err := db.FindSessionsByConvID(conversation)
	if err != nil {
		return true
	}
	for _, row := range rows {
		if row.Status != session.StatusExited {
			return true
		}
	}
	return false
}

// startPiWalledAuthSync carries refreshed OAuth logins between pi's agent
// directory and tclaude's walled copy of it (harness.SyncPiWalledAuth).
func startPiWalledAuthSync(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(piWalledAuthSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := harness.SyncPiWalledAuth(piConversationHasSession); err != nil {
					slog.Warn("pi walled auth sync failed", "error", err, "module", "agentd")
				}
			case <-stop:
				return
			}
		}
	}()
}
