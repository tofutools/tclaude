package agentd

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const agentMessageAttachmentCleanupInterval = 10 * time.Minute

func startAgentMessageAttachmentCleanup(stop <-chan struct{}) {
	reconcileAgentMessageAttachments()
	go func() {
		ticker := time.NewTicker(agentMessageAttachmentCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reconcileAgentMessageAttachments()
			case <-stop:
				return
			}
		}
	}()
}

// reconcileAgentMessageAttachments repairs crash leftovers in both directions:
// missing bytes lose their stale metadata, and files no longer referenced by a
// message (including cascaded message deletion) are removed.
func reconcileAgentMessageAttachments() {
	attachments, err := db.ListAllAgentMessageAttachments()
	if err != nil {
		slog.Warn("agent message attachment cleanup: list failed", "error", err)
		return // fail closed: never delete bytes without an authoritative list
	}
	referenced := make(map[string]bool, len(attachments))
	for _, a := range attachments {
		clean := filepath.Clean(a.StoragePath)
		if info, err := os.Stat(clean); err != nil || !info.Mode().IsRegular() || info.Size() != a.SizeBytes {
			if err := db.DeleteAgentMessageAttachment(a.ID); err != nil {
				slog.Warn("agent message attachment cleanup: delete stale metadata failed", "error", err, "path", clean)
			}
			continue
		}
		referenced[clean] = true
	}
	_ = filepath.WalkDir(operatorMessageAttachmentsBase, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if !referenced[filepath.Clean(path)] {
			_ = os.Remove(path)
		}
		return nil
	})
}
