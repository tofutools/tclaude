package agentd

import (
	"strings"
	"testing"
)

// TestTerminalsCore_UnloadGuardTracksOpenPanes pins the accidental-tab-close
// guard in the shared multiplexer. Both the dashboard Terminals tab and the
// standalone pop-out page mount this core, so the listener must live here and
// must be armed only while at least one pane exists. Keeping it off for an
// empty mux avoids prompting during ordinary dashboard navigation and avoids
// unnecessarily disabling Firefox's back/forward cache.
func TestTerminalsCore_UnloadGuardTracksOpenPanes(t *testing.T) {
	src := readDashboardJS(t, "terminal-shell-island.js")
	for _, needle := range []string{
		"const confirmUnload = (event) =>",
		"event.preventDefault()",
		"event.returnValue = true",
		"if (!hasPanes) return undefined",
		"window.addEventListener('beforeunload', confirmUnload)",
		"window.removeEventListener('beforeunload', confirmUnload)",
		"window.addEventListener('tclaude:auth-expired', disarmForAuth)",
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("terminal-shell-island.js missing %q — open terminals must guard accidental page unloads", needle)
		}
	}
}
