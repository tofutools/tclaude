package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_SlopHotkey pins the global keyboard shortcut that
// toggles slop mode (Ctrl/⌘ + Alt/⌥ + Shift + S). Like the other slop
// guards (TestDashboardHTML_SlopFx / _SlopExtras) the feature is purely
// client-side, so we string-search the embedded source rather than
// running the JS — a dropped bootstrap call or a refactored modifier
// check would otherwise kill the shortcut silently in the browser.
func TestDashboardHTML_SlopHotkey(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Public surface + bootstrap wiring. slop.js owns the toggle, so it
	// owns the hotkey; dashboard.js must install it once at startup.
	must("export function bindSlopHotkey", "slop.js exports the hotkey binder")
	must("bindSlopHotkey();", "dashboard.js installs the hotkey at bootstrap")

	// The shortcut's identity: physical S key (layout-independent),
	// Shift + Alt, and either Ctrl or Cmd. Pin each clause so a refactor
	// can't quietly drop a modifier and widen the accident surface.
	must("e.code !== 'KeyS'", "matches the physical S key, not the layout-dependent e.key")
	must("!e.shiftKey || !e.altKey", "requires both Shift and Alt")
	must("!e.ctrlKey && !e.metaKey", "requires Ctrl (Win/Linux) or Cmd (macOS)")

	// Holding the keys must not strobe the toggle, and the shortcut must
	// actually flip slop mode.
	must("if (e.repeat) return;", "auto-repeat is ignored so a held key doesn't strobe the toggle")
	must("toggleSlop();", "the hotkey flips slop mode")
}
