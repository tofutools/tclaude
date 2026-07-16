package agentd

import (
	"strings"
	"testing"
)

// TestTerminalsCore_UnloadGuardTracksOpenPanes pins the accidental-tab-close
// guard in the shared Preact shell. Both the dashboard Terminals tab and the
// standalone pop-out mount this component, so it is armed only while a pane
// exists and disarmed for intentional auth recovery.
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
