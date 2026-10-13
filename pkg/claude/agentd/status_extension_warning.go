package agentd

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const statusExtensionStartupTimeout = 30 * time.Second

func statusExtensionWarning(name string, created, lastHook, now time.Time) string {
	h, ok := harness.Get(name)
	if !ok || h.StatusExtension == nil || created.IsZero() || !lastHook.Before(created) || now.Sub(created) < statusExtensionStartupTimeout {
		return ""
	}
	return fmt.Sprintf("%s reported no status within 30s of launch; check that its tclaude extension loaded, the extension API is compatible, and the configured hook executable is reachable", h.Name)
}

func (r *sessionReaper) warnMissingStatusExtension(st *session.SessionState, now time.Time) {
	warning := statusExtensionWarning(st.Harness, st.Created, st.LastHook, now)
	if warning == "" {
		delete(r.statusExtensionWarned, st.ID)
		return
	}
	if r.statusExtensionWarned == nil {
		r.statusExtensionWarned = map[string]time.Time{}
	}
	if r.statusExtensionWarned[st.ID].Equal(st.Created) {
		return
	}
	r.statusExtensionWarned[st.ID] = st.Created
	slog.Warn("harness status extension did not report after launch", "session", st.ID, "harness", st.Harness, "detail", warning)
}
