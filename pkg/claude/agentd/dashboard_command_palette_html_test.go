package agentd

import (
	"strings"
	"testing"
)

// The dashboard's Ctrl/Cmd-K command palette ("spotlight", palette.js)
// is a keyboard launcher that searches across operations the dashboard
// already has and runs the picked one — it adds no new daemon
// behaviour, it only composes existing endpoints + modals. Like the
// other dashboard render guards, this test pins the wiring across the
// HTML / CSS / JS by string-searching the embedded source rather than
// running the JS, so a rename in one file that silently breaks the
// feature in the browser fails at `go test ./...`.
func TestDashboardHTML_CommandPalette(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Markup: the overlay rides on .modal-overlay (shared backdrop +
	// auto-refresh suspend), with its search input, results list and the
	// discoverable header button.
	must(`id="command-palette-modal"`, "the palette overlay exists")
	must(`class="modal-overlay palette-overlay"`,
		"the palette is a .modal-overlay so it suspends the 2s refresh while open")
	must(`id="palette-input"`, "the palette has a search input")
	must(`id="palette-list"`, "the palette has a results list")
	must(`id="command-palette-btn"`, "a header button opens the palette for discoverability")

	// CSS: the box + the keyboard-selection highlight.
	must(".palette-box {", "the palette box has a CSS rule")
	must(".palette-item.selected", "the keyboard-selected command is highlighted")

	// JS: the module is defined, exported, and called from boot.
	must("export function bindCommandPalette(", "palette.js exports its binder")
	must("bindCommandPalette();", "dashboard.js boot wires the palette")

	// The Ctrl/Cmd-K trigger: a modifier + the "k" key, claimed with
	// preventDefault. Pressing it again toggles closed.
	must(`(e.key || '').toLowerCase() !== 'k'`, "the trigger is the K key")
	must("e.ctrlKey || e.metaKey", "the trigger requires Ctrl or Cmd")

	// The commands DELEGATE to operations that already exist — bulk
	// window ops, per-agent jump/hide, the window-subset modal, and
	// spawn. This is what keeps the palette a thin surface.
	must("'/api/agent-windows'", "bulk focus/unfocus reuses /api/agent-windows")
	must("/api/jump/", "per-agent focus reuses /api/jump")
	must("/api/hide/", "per-agent hide reuses /api/hide")
	must("openWindowModal('all', null)", "the subset picker reuses the window modal")
	must("openAgentSpawnModal({})", "the spawn command reuses the spawn modal")

	// The theme toggle (regular ↔ slop) reuses slop.js's toggleSlop,
	// which had to be exported for the palette to reach it.
	must("export function toggleSlop(", "slop.js exports its theme toggle")
	must("run: () => toggleSlop(),", "the palette offers a Switch theme command")
	must("'Switch to slop theme'", "the toggle is labelled by its destination theme")

	// Group fold: collapse / expand the Groups-tab <details> listing.
	must("Collapse all groups", "the palette can collapse every group")
	must("Expand all groups", "the palette can expand every group")
	must("function setGroupOpen(", "per-group collapse/expand toggles the group's <details>")
	must("data-group-key=", "fold targets the group's <details data-group-key>")

	// Consistent presentation: every detach command reads "Hide", every
	// raise command reads "Focus" (no stray "Unfocus" in a label).
	must("label: 'Hide all windows'", "the bulk detach command reads Hide, not Unfocus")

	// Ranking + synonyms live in the pure, unit-tested scorer module.
	must("./palette-score.js", "palette imports the pure ranking module")
	must("rankCommands(commands", "rendering ranks via the shared scorer")
	must("export const SYNONYMS", "the scorer defines a synonym map")
	must("hide: ['unfocus']", "hide is a synonym for unfocus")
	must("show: ['focus']", "show is a synonym for focus")

	// Accessibility + focus hygiene: the combobox input points its
	// aria-activedescendant at the keyboard-selected option (each option
	// carries a stable id), and closing returns focus to the trigger.
	must("aria-activedescendant", "the combobox announces the active option to screen readers")
	must("palette-opt-", "options carry stable ids for aria-activedescendant")
	must("lastFocus = document.activeElement", "the trigger element is captured for focus restore")

	// Keyboard model: ↑/↓ move, Enter runs, Esc closes.
	must("case 'ArrowDown':", "ArrowDown moves the selection")
	must("case 'ArrowUp':", "ArrowUp moves the selection")
	must("case 'Enter':", "Enter runs the selected command")
	must("case 'Escape':", "Escape closes the palette")
}
