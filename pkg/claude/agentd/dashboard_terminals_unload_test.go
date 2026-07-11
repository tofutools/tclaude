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
	src := readDashboardJS(t, "terminals-core.js")
	for _, needle := range []string{
		"function confirmTerminalUnload(e)",
		"e.preventDefault()",
		"e.returnValue = true",
		"function updateUnloadGuard(n)",
		"const shouldArm = n > 0",
		"window[shouldArm ? 'addEventListener' : 'removeEventListener']('beforeunload', confirmTerminalUnload)",
		"updateUnloadGuard(n)",
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("terminals-core.js missing %q — open terminals must guard accidental page unloads", needle)
		}
	}
}
