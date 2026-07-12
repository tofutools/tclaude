package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_HarnessLineWired guards the per-agent harness/model
// line — "CC · O4.8 1M" under the row's dot/focus/cog cluster — plus
// its appearance in the status-dot tooltip. The pieces span three files
// (helpers.js builds + exports it, render.js wires it into the member
// cell, dashboard.css styles it); a rename in one silently breaks the
// feature in the browser, and the repo has no JS test runner, so this
// asserts on the embedded concatenation at `go test ./...`.
//
// The model itself comes from state.model — surfaced by the dashboard
// snapshot from the sessions.model column the statusline hook records.
func TestDashboardHTML_HarnessLineWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// helpers.js: the builders are defined, read state.model, and are
	// exported for render.js.
	must("function harnessLine(m)", "harnessLine helper is defined")
	must("function harnessModel(m)", "harnessModel helper (tooltip text) is defined")
	must("m.state.model", "the line reads the model off the agent's state")
	must("agentStatusDot, harnessLine,", "harnessLine is exported from helpers.js")

	// render.js: wired into the member control cell — same column as the
	// dot/actions, NOT a new <td>.
	must("${agentStatusDot(m)}${actions}</div>${harnessLine(m)}", "harnessLine renders in the agent-ctl cell")

	// The always-visible label is shortModel()-compressed; the FULL name
	// stays in the tooltip (the title attr / the status-dot tip).
	must("function shortModel(", "shortModel compressor is defined")
	must("${esc(shortModel(model))}", "the visible chip uses the shortened model")
	must("Model: ${model}", "harnessLine's tooltip keeps the FULL model name")

	// The reasoning-effort level (JOH-37) trails the model — "CC · O4.8 1M
	// high" — read off state.effort_level, with its own styled span and a
	// tooltip clause. Omitted when absent so models without effort support
	// stay at "CC · O4.8 1M".
	must("m.state.effort_level", "harnessLine reads the effort level off the agent's state")
	must("harness-effort", "the effort token has its own span")
	must("Effort: ${effort}", "harnessLine's tooltip names the effort level when present")

	// Status-dot tooltip surfaces the harness+model on hover (the brief's
	// second ask), using the full model via harnessModel.
	must("running on ${hm}", "the status-dot tooltip appends the harness/model")

	// CSS: the line and its prefix/separator are styled (no chip/box — one
	// continuous string).
	must(".agent-harness", "harness line has a style rule")
	must(".harness-sep", "the middot separator is styled")
	must(".harness-effort", "the effort token is styled")

	// The harness is now a per-agent value (state.harness), not a frontend
	// constant: a label map keyed by the tag drives the chip, and the line
	// reads the tag off the agent's state (JOH-162).
	must("const HARNESS_LABELS = {", "per-harness label map replaces the CC constant")
	must("claude: { short: 'CC', long: 'Claude Code' }", "claude keeps its CC label")
	must("codex: { short: 'Codex', long: 'Codex CLI' }", "codex has its own label")
	must("m.state.harness", "harnessLine reads the harness tag off the agent's state")
}

// TestDashboardHTML_HarnessBadgeAndSandboxWired guards the JOH-162 per-agent
// surfaces: a non-default harness (Codex) is badged even before a model is
// known, the launch-sandbox chip renders from state.sandbox_mode, and the
// rename affordance is gated on the harness's deliverable-rename capability.
// All three span helpers.js (builders) + render.js (wiring) + dashboard.css
// (styles); the repo has no JS test runner, so this asserts on the embedded
// concatenation.
func TestDashboardHTML_HarnessBadgeAndSandboxWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// helpers.js: a non-default harness is flagged even with no model yet,
	// so a mixed group is legible before the first tick. A default-harness
	// (Claude Code) no-model row stays clean UNLESS Remote Access is armed,
	// in which case the bare 📱 indicator still earns a minimal line.
	must("function isDefaultHarness(name)", "default-harness predicate defined")
	must("if (isDefaultHarness(harness)) {", "no-model Claude Code rows stay clean; Codex still badges")
	must("return remoteEl ? `<div class=\"agent-harness\">${remoteEl}</div>` : '';", "an armed remote indicator still shows on a pre-tick CC row")

	// helpers.js: the sandbox badge builder reads state.sandbox_mode, is
	// exported, and special-cases the full-access (sandbox-off) mode.
	must("function sandboxBadge(m)", "sandboxBadge helper is defined")
	must("m.state.sandbox_mode", "sandboxBadge reads the launch sandbox off the agent's state")
	must("danger-full-access", "the full-access (sandbox-off) mode is special-cased")
	must("harnessLine, sandboxBadge,", "sandboxBadge is exported from helpers.js")

	// render.js: the sandbox chip renders in the agent control cell, next
	// to the harness line.
	must("${harnessLine(m)}${sandboxBadge(m)}", "sandboxBadge renders beside the harness line")

	// render.js: the rename affordance is gated on the harness capability —
	// a non-renameable harness gets a fixed (non-editable) name.
	must("function harnessCanRename(snapshot, name)", "rename-capability lookup is defined")
	must("function renameNameCell(m, state, idPair = '')", "the name cell switches on rename capability")
	must("harnessCanRename(lastSnapshot, state.harness)", "the name cell gates rename on the agent's harness")
	must("rowname-fixed", "a non-renameable harness gets a fixed-name span")

	// CSS: the sandbox chip + its danger variant + the fixed-name tweak are
	// styled.
	must(".sandbox-badge", "sandbox chip has a style rule")
	must(".sandbox-badge.sandbox-danger", "the full-access sandbox chip is styled distinctly")
	must(".rowname-text.rowname-fixed", "the non-renameable name drops the click-to-edit affordance")
}

// TestDashboardHTML_SpawnHarnessMenusWired guards the JOH-162 spawn dialog:
// a harness selector that reshapes the Model + Sandbox menus per harness,
// driven off the snapshot's harness catalog, with the chosen harness +
// sandbox forwarded in the spawn POST body. Spans dashboard.html (the new
// rows) + modal-spawn.js (the logic); asserted on the embedded source
// since the repo has no JS test runner.
func TestDashboardHTML_SpawnHarnessMenusWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the harness selector, the catalog Model row, its fallback
	// free-text row, and the sandbox selector row exist.
	must(`id="agent-spawn-harness"`, "spawn dialog has a harness selector")
	must(`class="spawn-inline-fields"`, "spawn dialog compacts Model and Effort onto one row")
	must(`id="agent-spawn-model-claude-row"`, "the catalog model row is identifiable for toggling")
	must(`id="agent-spawn-model-codex"`, "spawn dialog has a no-suggestions fallback model input")
	must(`id="agent-spawn-effort" aria-label="Effort"`, "compact Effort select keeps an accessible label")
	must(`id="agent-spawn-sandbox"`, "spawn dialog has a sandbox selector")
	must(`#agent-spawn-modal .spawn-inline-fields`, "spawn dialog has scoped CSS for the compact launch row")

	// modal-spawn.js: the selector is populated from the catalog, the rows
	// reshape per harness, and the active model control is read on submit.
	must("function populateSpawnHarnessSelect()", "harness selector is populated from the catalog")
	must("lastSnapshot.harnesses", "the dialog reads the snapshot harness catalog")
	must("function applySpawnHarness(harnessName)", "the dialog reshapes per harness")
	must("function activeSpawnModelEl()", "submit reads whichever Model control is active")
	must("populateModelSelect($('#agent-spawn-model'), h.models)", "the Model dropdown is rebuilt from the selected harness catalog")

	// modal-spawn.js: the spawn POST body always carries the dropdown's explicit
	// harness selection (including Claude) and sandbox.
	must("if (harness) body.harness = harness", "selected harness is always sent in the spawn body")
	must("body.sandbox = sandbox", "the chosen sandbox is sent in the spawn body")

	// AskUserQuestion idle-timeout (Claude-Code-only) — the row + selector exist,
	// reshape per harness off the catalog's can_ask_timeout gate, and the chosen
	// value is forwarded in the spawn body. Pins the JS/HTML so a JS-stale
	// worktree (embedded assets) trips here rather than silently at integration.
	must(`id="agent-spawn-ask-timeout"`, "spawn dialog has an AskUserQuestion-timeout selector")
	must("can: 'can_ask_timeout', modes: 'ask_timeout_modes'", "the timeout row gates on the harness catalog's can_ask_timeout (via the shared SPAWN_LAUNCH_SETTINGS table)")
	must("body.ask_user_question_timeout = askTimeout", "the chosen AskUserQuestion timeout is sent in the spawn body")
	// modal-profiles.js: the profile editor edits + persists the same field.
	must(`id="profile-editor-ask-timeout"`, "profile editor has an AskUserQuestion-timeout selector")
	must("body.ask_user_question_timeout = draft.ask_user_question_timeout", "the profile editor persists the AskUserQuestion timeout")

	// modal-spawn.js: the Effort menu is rebuilt per harness from the
	// catalog's effort_levels (single source of truth — the static HTML
	// options are only a pre-snapshot fallback), so a harness with its own
	// reasoning scale needs no dashboard edit.
	must("function populateSpawnEffortSelect(", "the effort menu is rebuilt per harness")
	must("h.effort_levels", "the effort menu reads the harness's effort levels from the catalog")
}

// TestDashboardHTML_ShortModelRules pins the shortModel() compression
// rules. The transform lives entirely in helpers.js JS and the repo has
// no JS test runner, so — as with the context-meter formula — this guards
// the *rules* by asserting their source substrings survive, and the
// contract is documented here for the manual-verification trail:
//
//	"Opus 4.8 (1M context)" → "O4.8 1M"   (initial+version, size token)
//	"Opus 4.8"              → "O4.8"       (initial glued to version)
//	"Sonnet 4.6"            → "S4.6"
//	"Haiku 4.5"             → "H4.5"
//
// A regression in any rule (dropping the parenthetical peel, the size
// extraction, or the no-space initial+version glue) trips one of these.
func TestDashboardHTML_ShortModelRules(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// Rule 1: peel a trailing "(…)" parenthetical and pull a size token.
	must(`main.match(/\(([^)]*)\)\s*$/)`, "peels the trailing parenthetical")
	must(`paren[1].match(/\d+\s*[KMBkmb]/)`, "extracts the window size token (1M / 200K)")

	// Rule 2: family initial glued onto the version with no space.
	must("parts[0].charAt(0).toUpperCase() + parts.slice(1).join(' ')", "initial + version, no space")

	// Rule 2b: the size token is appended after a space when present.
	must("size ? `${core} ${size}` : core", "size token joins with a space, else core alone")
}
