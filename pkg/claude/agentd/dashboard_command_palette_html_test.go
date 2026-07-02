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

	// Spawn: the plain command reuses the spawn modal, DEFAULTING (not
	// pinning) the picker to the group the operator last interacted with
	// (folded / spawned / palette-touched), and there is one PINNED "Spawn
	// agent in <group>…" per group. Both reuse openAgentSpawnModal; the
	// group memory lives in the shared last-group module.
	must("./last-group.js", "the palette imports the last-interacted-group memory")
	must("const lastGroup = lastInteractedGroup();",
		"the plain spawn reads the last-interacted group")
	must("openAgentSpawnModal(lastGroupLive ? { defaultGroup: lastGroup } : {})",
		"the plain spawn defaults the picker to the last group (still changeable)")
	must("label: wiz(`Spawn agent in ${g.name}…`, `Summon a familiar into ${g.name}…`)",
		"the palette offers a pinned spawn per group (plain label + arcane wizard label)")
	must("openAgentSpawnModal({ groupName: g.name })",
		"each per-group spawn pins its group")

	// Spawn profiles: the palette can open the profiles-management overlay —
	// the same list the Groups cog's "⧉ profiles…" entry opens — reusing
	// openProfilesManageModal (exported from modal-profiles.js just for this).
	// The command adds only a keyboard entry point, no new behaviour. The
	// presented label is a wiz(plain, arcane) pair — "Edit profiles…" plainly,
	// "Edit familiar patterns…" under body.wizard (profiles re-letter to
	// patterns) — pinned in one needle so dropping either copy fails CI.
	must("./modal-profiles.js", "the palette imports the profiles manage overlay")
	must("wiz('Edit profiles…', 'Edit familiar patterns…')",
		"the palette offers the spawn-profiles manager (plain + arcane label)")
	must("run: () => openProfilesManageModal(),",
		"the profiles command reuses the existing manage overlay")

	// The "last interacted group" memory MUST be stamped from the genuine
	// group-title click/keyboard fold (bindGroupTitleToggle), NOT from the
	// `toggle` event — the 2s re-render re-fires `toggle` on every open
	// <details>, so recording there would let whatever group is open last
	// win with no operator intent. Pin the record call on the title-toggle
	// path so a refactor that moves it onto the spurious `toggle` handler
	// (bindDetailsPersistence) is conspicuous. (A string guard can't prove
	// the negative; this pins the intended placement.)
	must("recordGroupInteraction(details.getAttribute('data-group-key'))",
		"genuine fold/unfold records the last-interacted group on the click path")

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
	// raise command reads "Focus" (no stray "Unfocus" in a label). In 🧙 mode
	// the presented label re-skins to "Veil all familiars" via the wiz() helper
	// — both copies are pinned in the one needle. The plain word stays the
	// keyword vocabulary, so old search terms keep working (TestDashboardHTML_
	// WizardCommandPaletteSynonyms covers the arcane→plain synonym bridge).
	must("wiz('Hide all windows', 'Veil all familiars')", "the bulk detach command reads Hide (plain) / Veil (wizard), not Unfocus")

	// Power control: stop / start agent PROCESSES (distinct from the
	// window ops, which only detach/raise terminals). Each tier delegates
	// to the same calls the dashboard's Shutdown / Power-on buttons and
	// per-row status dots make — global + per-group reuse
	// shutdownScope/powerOnScope (the /api/shutdown + /api/power-on
	// endpoints), per-agent reuses stopAgentReq/resumeAgentReq behind the
	// 3-way shutdownConfirm. Every variant is gated on a live count or the
	// agent's current state so the palette never offers a no-op or the
	// wrong verb.
	must("shutdownScope, powerOnScope, shutdownConfirm, stopAgentReq, resumeAgentReq",
		"palette imports the existing power-control ops from refresh.js")
	// Each presented label is a wiz(plain, arcane) pair — the plain wording in
	// the default/slop theme, the 🧙 Slumber/Awaken wording under body.wizard —
	// pinned in one needle so dropping either copy fails CI. The plain verbs
	// stay in the keywords so old search terms keep working.
	// Global all-agents.
	must("wiz('Shut down all agents', 'Slumber all familiars')", "the palette offers a global shutdown (plain + arcane label)")
	must("shutdownScope('all', null)", "global shutdown reuses shutdownScope")
	must("wiz('Power on all agents', 'Awaken all familiars')", "the palette offers a global power-on (plain + arcane label)")
	must("powerOnScope('all', null)", "global power-on reuses powerOnScope")
	// Per-group batch.
	must("wiz(`Shut down group: ${g.name}`, `Slumber coven: ${g.name}`)", "the palette offers a per-group shutdown (plain + arcane label)")
	must("shutdownScope('group', g.name)", "per-group shutdown reuses shutdownScope")
	must("wiz(`Power on group: ${g.name}`, `Awaken coven: ${g.name}`)", "the palette offers a per-group power-on (plain + arcane label)")
	must("powerOnScope('group', g.name)", "per-group power-on reuses powerOnScope")
	// Per-agent, state-gated.
	must("wiz(`Stop agent: ${label}`, `Slumber familiar: ${label}`)", "the palette offers a per-agent stop (plain + arcane label)")
	must("stopAgentInteractive(sel, label)",
		"per-agent stop reuses the 3-way shutdownConfirm then stopAgentReq")
	must("wiz(`Resume agent: ${label}`, `Awaken familiar: ${label}`)", "the palette offers a per-agent resume (plain + arcane label)")
	must("resumeAgentReq(sel, label)", "per-agent resume reuses resumeAgentReq")

	// Retire: the palette can demote agents back to plain conversations.
	// A per-agent "Retire agent: <name>" reuses the same confirm + flags
	// as the per-row ⚙ Retire button (retireAgentInteractive), and a
	// per-group "Retire idle/offline agents in <group>" opens a PREVIEW
	// modal (openRetirePreview) that lists precisely the matching members,
	// lets the human opt agents out, and POSTs the EXPLICIT ticked conv-id
	// list to /api/groups/{name}/retire — so the BE retires exactly what
	// the human previewed, not a cohort it re-derived. Listed only when a
	// live match count is non-zero so the palette never offers a no-op.
	must("wiz(`Retire agent: ${label}`, `Banish familiar: ${label}`)", "the palette offers a per-agent retire (plain + arcane Banish label)")
	must("retireAgentInteractive(a.conv_id, label)",
		"per-agent retire reuses the shared confirm + POST flow")
	must("for (const status of ['idle', 'offline'])",
		"the bulk retire offers the idle and offline cohorts")
	must("wiz(`Retire ${status} agents in ${g.name}`, `Banish ${status} familiars in ${g.name}`)",
		"the palette offers a per-group bulk retire by status (plain + arcane Banish label)")
	must("countGroupMembersByStatus(g.name, status)",
		"the bulk retire command is gated on a live match count (no no-op)")
	must("openRetirePreview(g.name, status)",
		"per-group bulk retire opens the preview modal")

	// Ranking + synonyms live in the pure, unit-tested scorer module.
	must("./palette-score.js", "palette imports the pure ranking module")
	must("rankCommands(commands", "rendering ranks via the shared scorer")
	must("export const SYNONYMS", "the scorer defines a synonym map")
	must("hide: ['unfocus', 'veil']", "hide bridges to unfocus (plain) and veil (wizard)")
	must("show: ['focus', 'reveal']", "show bridges to focus (plain) and reveal (wizard)")

	// Accessibility + focus hygiene: the combobox input points its
	// aria-activedescendant at the keyboard-selected option (each option
	// carries a stable id), and closing returns focus to the trigger.
	must("aria-activedescendant", "the combobox announces the active option to screen readers")
	must("palette-opt-", "options carry stable ids for aria-activedescendant")
	must("lastFocus = document.activeElement", "the trigger element is captured for focus restore")

	// Keyboard model: ↑/↓ move one row (wrapping), PageUp/PageDown jump a
	// viewport-worth (clamping at the ends), Enter runs, Esc closes.
	must("case 'ArrowDown':", "ArrowDown moves the selection")
	must("case 'ArrowUp':", "ArrowUp moves the selection")
	must("case 'PageDown':", "PageDown jumps the selection down a page")
	must("case 'PageUp':", "PageUp jumps the selection up a page")
	must("case 'Enter':", "Enter runs the selected command")
	must("case 'Escape':", "Escape closes the palette")
	must("function movePage(", "the page jump clamps at the ends (distinct from the wrapping move)")
	must("function pageSize(", "the page jump is a measured viewport-worth of rows")
	// The footer advertises the page-jump keys alongside the ↑/↓ nav hint.
	must("<kbd>PgUp</kbd><kbd>PgDn</kbd> jump", "the palette footer documents PageUp/PageDown")
}
