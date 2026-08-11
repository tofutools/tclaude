package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_HarnessLineWired guards the per-agent harness/model
// line — "[Claude mark] · Opus 4.8" under the row's dot/focus/cog cluster — plus
// its appearance in the status-dot tooltip. The pieces span three files
// (groups-member-table.js owns and wires it into the member
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

	// The native components are defined and read state.model.
	must("function HarnessLine({ member, snapshot })", "HarnessLine component is defined")
	must("function AgentStatusDot({ member })", "status-dot component is defined")
	must("state.model", "the line reads the model off the agent's state")

	// MemberCell wires it into the member control cell — same column as the
	// dot/actions, NOT a new <td>.
	must("<${HarnessLine} member=${member} snapshot=${snapshot} /></td>", "HarnessLine renders in the agent-ctl cell")

	// The always-visible model is displayModel()-normalised; the harness uses a
	// compact product mark while both FULL names stay in tooltips/accessibility
	// labels (the title attrs / the status-dot tip).
	must("function displayModel(", "displayModel normaliser is defined")
	must("${displayModel(model, harness)}", "the visible chip uses the harness-aware normalised model")
	must("'Last used model' : 'Model'", "harnessLine's tooltip keeps the FULL model name and labels offline values as historical")
	must("import { HarnessMark } from './harness-mark.js'", "the member table imports the shared harness-mark component")
	must("<${HarnessMark} name=${harness} shortLabel=${labels.short} longLabel=${labels.long} tooltip=${drive ? title : labels.long} />",
		"the harness line renders the mark with full-name and fallback labels")
	must("const PRODUCT_MARKS = new Set(['claude', 'codex', 'copilot', 'opencode'])",
		"the known product-mark set is explicit")
	must(`role="img" aria-label=${longLabel} title=${tooltip}`,
		"each mark keeps its full harness name for assistive tech and accepts the general tooltip on hover")
	must(`class="harness-name" title=${tooltip}>${shortLabel}</span>`,
		"an unknown future harness keeps a visible text fallback and accepts the general tooltip")

	// The reasoning-effort level (JOH-37) trails the model — "CC · Opus 4.8
	// hi" — read off state.effort_level, with its own styled span and a
	// full-name tooltip. Omitted when absent so models without effort support
	// stay at "CC · Opus 4.8".
	must("state.effort_level", "HarnessLine reads the effort level off the agent's state")
	must("const EFFORT_LABELS = {", "effort levels have compact display labels")
	must("low: 'lw'", "low effort is displayed as lw")
	must("medium: 'md'", "medium effort is displayed as md")
	must("high: 'hi'", "high effort is displayed as hi")
	must("xhigh: 'xi'", "xhigh effort is displayed as xi")
	must("max: 'mx'", "max effort is displayed as mx")
	must("function shortEffort(effort)", "the compact effort display has a named transform")
	must("harness-effort", "the effort token has its own span")
	must("title=${effort}", "the effort token keeps the full value in its tooltip")
	must("${shortEffort(effort)}", "the effort token renders the compact value")
	must("'Last used effort' : 'Effort'", "harnessLine's tooltip names live effort and labels offline effort as historical")

	// Status-dot tooltip surfaces the harness+model on hover (the brief's
	// second ask), using the full model via harnessModel.
	must("online ? 'running on' : 'last used'", "the status-dot tooltip distinguishes live from last-used harness/model metadata")

	// CSS: the line and its prefix/separator are styled (no chip/box — one
	// continuous string).
	must(".agent-harness", "harness line has a style rule")
	must(".harness-sep", "the middot separator is styled")
	must(".harness-effort", "the effort token is styled")
	must("runtime-meta-offline", "offline runtime metadata remains rendered with a dimmed treatment")
	must("offline ? 'Last used harness' : 'Harness'", "offline metadata is labelled as last-used in its tooltip")
	must(`role="note" aria-label=${title}`, "last-used metadata is exposed without relying on pointer-only tooltips")

	// The harness is now a per-agent value (state.harness), not a frontend
	// constant: a label map keyed by the tag drives the mark/tooltips, and the line
	// reads the tag off the agent's state (JOH-162).
	must("const HARNESS_LABELS = {", "per-harness label map replaces the CC constant")
	must("claude: { short: 'CC', long: 'Claude Code' }", "claude keeps its CC label")
	must("codex: { short: 'Codex', long: 'Codex CLI' }", "codex has its own label")
	must("opencode: { short: 'OC', long: 'OpenCode' }", "OpenCode uses the compact OC label")
	must("copilot: { short: 'COP', long: 'GitHub Copilot CLI' }", "Copilot uses the compact COP label")
	must("if (harness === 'opencode')", "OpenCode model display removes its provider prefix")
	must("state.harness", "HarnessLine reads the harness tag off the agent's state")

	// API-backed Copilot and app-server-backed Codex launches keep their drive
	// and health details in this same general tooltip. They do not add "api" or
	// "app" tokens to the visible harness/model line.
	must("function driveTooltip(state)", "drive metadata has a dedicated tooltip formatter")
	must("if (drive) title += ` — ${drive}`", "drive metadata is appended to the general harness tooltip")
	must("Drive: Copilot embedded JSON-RPC API", "Copilot API drive is identified in the tooltip")
	must("Drive: Codex app-server ready", "Codex app-server drive is identified in the tooltip")
	must("tabindex=${drive ? '0' : null}", "drive-bearing general tooltips are keyboard and touch focusable")
	must("data-full-metadata=${drive ? title : null}", "focus disclosure carries the same complete tooltip text")
	must(".agent-harness[data-full-metadata]:focus::after", "focused drive metadata reveals its tooltip without a pointer")
	if strings.Contains(dashboardAssets, "class=${'harness-drive'") {
		t.Error("API/app-server drive metadata must not regain a visible indicator")
	}
	if strings.Contains(dashboardAssets, ".agent-harness .harness-drive") {
		t.Error("removed API/app-server indicators must not retain dashboard styling")
	}

	// The selected treatment gives every known mark the same fixed geometry and
	// muted currentColor. No per-harness color rule should creep into the row.
	must(".agent-harness .harness-mark {", "known harness marks have a shared style rule")
	must("display: inline-flex; width: 14px; height: 14px;", "marks occupy one equal-width slot")
	must("fill: currentColor", "all product silhouettes inherit the shared greyscale treatment")
	if strings.Contains(dashboardAssets, ".harness-mark[data-harness-mark=") {
		t.Error("harness marks must not regain per-product color overrides")
	}
}

func TestDashboardHarnessMarkNoticesEmbedded(t *testing.T) {
	tests := map[string]string{
		"vendor/harness-marks/README.md":                 "@lobehub/icons-static-svg",
		"vendor/harness-marks/LICENSE-LobeHub.txt":       "Copyright (c) 2023 LobeHub",
		"vendor/harness-marks/LICENSE-GitHub-Primer.txt": "Copyright (c) 2026 GitHub Inc.",
		"vendor/harness-marks/LICENSE-OpenCode.txt":      "Copyright (c) 2025 opencode",
	}
	for path, notice := range tests {
		body := string(mustReadFS(dashboardAssetsFS, path))
		if !strings.Contains(body, notice) {
			t.Errorf("embedded harness-mark notice %s missing %q", path, notice)
		}
	}
}

// TestDashboardHTML_HarnessBadgeAndSandboxWired guards the JOH-162 per-agent
// surfaces: a non-default harness (Codex) is badged even before a model is
// known, the launch-sandbox chip renders from state.sandbox_mode, and the
// rename affordance is gated on the harness's deliverable-rename capability.
// All three span groups-member-table.js + helpers.js capabilities + dashboard.css
// (styles); the repo has no JS test runner, so this asserts on the embedded
// concatenation.
func TestDashboardHTML_HarnessBadgeAndSandboxWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// HarnessLine flags a non-default harness even with no model yet,
	// so a mixed group is legible before the first tick. A default-harness
	// (Claude Code) no-model row stays clean UNLESS Remote Access is armed,
	// in which case the bare 📱 indicator still earns a minimal line.
	must("if (!harness || harness === 'claude')", "no-model Claude Code rows stay clean; Codex still badges")
	must("const indicated = (member.online && state.remote_control)",
		"an armed remote still earns a minimal line on a pre-tick CC row")
	must("|| !!sandboxIndicator(member)",
		"so does a recorded sandbox verdict, which is known before the first tick")
	// TCL-761: and so does a run of refused brokered callbacks — that agent
	// never GETS a model (the status line is what stamps one, and it is
	// exactly what is being refused), so the pre-tick branch is the only one
	// it ever renders through. Behaviour: jstest/broker-refusal-badge.test.mjs.
	must("|| Number(state.broker_refusals || 0) > 0;",
		"a starved agent must be badged on the branch it is stuck on")
	must("return indicated ? html`<div class=\"agent-harness\">${sandbox}${remote}${refused}</div>` : null;",
		"the pre-tick line carries every armed indicator")

	// The sandbox badge component reads state.sandbox_mode and special-cases
	// the full-access (sandbox-off) mode.
	must("export function SandboxBadge({ member, showDetails = false })", "SandboxBadge component is defined")
	must("function sandboxIndicator(member)", "the sandbox posture decision is separable from its rendering")
	must("member.state?.sandbox_mode", "the sandbox indicator reads the launch sandbox off the agent's state")
	must("danger-full-access", "the full-access (sandbox-off) mode is special-cased")

	// The mode is only the launch request. Where the row carries a resolved
	// verdict (Claude Code, whose `inherit` default defers to settings.json),
	// the badge describes whether the agent is actually confined. Behaviour is
	// covered by jstest/sandbox-badge.test.mjs; these pin the production wiring.
	must("function osSandboxBadge(mode, state, unverified, implementation)", "the recorded-verdict badge decision is defined")
	must("member.state?.os_sandbox_state", "SandboxBadge reads the recorded OS-sandbox verdict off the agent's state")
	// A verdict tclaude could not prove must not render as a plain padlock.
	must("member.state?.os_sandbox_unverified", "the badge reads whether the verdict could be verified")
	must("member.state?.sandbox_implementation", "the badge reads the implementation that earned the verdict")
	must("mode === 'danger-full-access' || mode === 'off'",
		"a pre-verdict Claude `off` row is a danger badge too, not a padlock on an unconfined agent")
	must("mode === 'access-control'",
		"OpenCode's soft access-control mode never earns an OS-sandbox padlock")

	// The tooltip is intentionally limited to four concise lines: resolved
	// status, implementation owner, applied profile names, and an action hint
	// only when the glyph is clickable.
	must("function sandboxImplementationLabel(member, badge)", "the compact tooltip derives an implementation label")
	must("if (badge.status === 'OFF') return 'None'", "an inactive sandbox has no active implementation")
	must("return 'TClaude'", "the tclaude-layer implementation uses the TClaude label")
	must("short}+TClaude", "stacked implementations name both active layers")
	must("return 'Unknown'", "unknown implementations are not mislabeled as harness-native")
	must("function sandboxProfileLabel(member)", "the compact tooltip derives applied profile names")
	must("names.join(' + ')", "multiple profile names retain resolution order")
	must("'Not recorded'", "legacy rows do not invent an absent profile")
	must("function sandboxTooltip(member, badge, actionable, unlocked)", "the compact tooltip has a dedicated formatter")
	must("unlocked && badge.status === 'OFF' ? 'TEMP OFF' : badge.status", "a resolved temporary disable is visually distinct from normal off")
	must("`Implementation: ${sandboxImplementationLabel(member, badge)}`", "the second tooltip line names the implementation")
	must("`Profile: ${sandboxProfileLabel(member)}`", "the third tooltip line names the applied profiles")
	must("'Click to restore normal sandbox'", "temporary overrides offer restoration")
	must("'Click to temporarily disable'", "normally confined agents offer a temporary disable")

	// The sandbox indicator rides INSIDE the harness line, trailing the effort
	// token next to the 📱 remote indicator, rather than owning a second line
	// under the control cell.
	must("showDetails=${!!snapshot?.recorded_sandbox_details_enabled}",
		"the snapshot feature flag gates the optional recorded-details action")
	must("const sandbox = html`<${SandboxBadge} member=${member}",
		"HarnessLine builds the sandbox indicator alongside the remote one")
	must("</span>${sandbox}${remote}", "both indicators trail the harness metadata text, tightly packed")

	// MemberName: the rename affordance is gated on the harness capability —
	// a non-renameable harness gets a fixed (non-editable) name.
	must("function harnessCanRename(snapshot, name)", "rename-capability lookup is defined")
	must("function MemberName({ member, snapshot, actions, grants, editorKey })", "the name cell switches on rename capability")
	must("harnessCanRename(snapshot, state.harness)", "the name cell gates rename on the agent's harness")
	must("rowname-fixed", "a non-renameable harness gets a fixed-name span")

	// CSS: the sandbox glyph + its danger variant + the fixed-name tweak are
	// styled.
	must(".sandbox-badge {", "the sandbox indicator has a style rule")
	must(".sandbox-badge.sandbox-danger", "the unconfined/unverified glyph is styled distinctly")
	// The glyph is frameless: a border or padding would rebuild the chip this
	// replaced, so assert their ABSENCE rather than freezing the exact block.
	found := false
	for _, rule := range dashboardCSSRules(t) {
		if strings.TrimSpace(rule.selectors) != ".sandbox-badge" {
			continue
		}
		if strings.Contains(rule.declarations, "border") || strings.Contains(rule.declarations, "padding") {
			t.Errorf(".sandbox-badge reintroduces the chip framing this indicator replaced: %q", rule.declarations)
		}
		found = true
	}
	if !found {
		t.Error("dashboard.css has no bare .sandbox-badge rule to check for chip framing")
	}
	must(".rowname-text.rowname-fixed", "the non-renameable name drops the click-to-edit affordance")
}

// TestDashboardHTML_SpawnHarnessMenusWired guards the JOH-162 spawn dialog:
// a harness selector that reshapes the Model + Sandbox menus per harness,
// driven off the snapshot's harness catalog, with the chosen harness +
// sandbox forwarded in the spawn POST body. Spans dashboard.html (the new
// component + plain model; asserted here for production wiring and exercised
// behaviourally by jstest/agent-spawn-preact.test.mjs.
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
	must(`class="spawn-inline-fields"`, "spawn dialog compacts Model, Effort and the auto-compaction window onto one row")
	must(`id="agent-spawn-model-claude-row"`, "the catalog model row is identifiable for toggling")
	must(`id="agent-spawn-model-codex"`, "spawn dialog has a no-suggestions fallback model input")
	must(`id="agent-spawn-effort" aria-label="Effort"`, "compact Effort select keeps an accessible label")
	must(`id="agent-spawn-sandbox"`, "spawn dialog has a sandbox selector")
	must(`#agent-spawn-modal .spawn-inline-fields`, "spawn dialog has scoped CSS for the compact launch row")
	must(`id="agent-spawn-auto-compact-window" type="text" aria-label="Auto-compact window (tokens)"`,
		"the compact auto-compaction window input keeps an accessible label")
	must(`id="agent-spawn-auto-compact-window-row"`,
		"the auto-compaction window row is identifiable, so the CSS can widen the launch row only when it is shown")

	// The state snapshots the catalog, while the pure model derives capability
	// visibility and the component renders the active model control.
	must("harnesses: Object.freeze([...(snapshot.harnesses || [])])", "open snapshots the harness catalog")
	must("export function spawnCapabilityView(", "the plain model reshapes per harness")
	must("modelSelectValue(draft, context)", "the component derives the active model control")
	must("view.models.map((model)", "the Model dropdown is rebuilt from the selected harness catalog")

	// agent-spawn-model.js: the spawn POST body always carries the dropdown's explicit
	// harness selection (including Claude) and sandbox.
	must("if (draft.harness) body.harness = draft.harness", "selected harness is always sent in the spawn body")
	must("if (view.sandbox.visible && draft.sandbox) body.sandbox = draft.sandbox", "the chosen sandbox is sent in the spawn body")

	// OpenCode's independent built-in-tool axis is catalog-gated, rendered, and
	// forwarded without appearing for Claude/Codex.
	must(`id="agent-spawn-tools"`, "spawn dialog has an OpenCode tool-governance selector")
	must("tools: ['can_tools', 'tools_modes', 'default_tools', 'tools_mode_help']", "tool governance gates on the harness catalog")
	must("if (view.tools.visible && draft.tools) body.tools = draft.tools", "the chosen tool governance is sent in the spawn body")
	must(`id=${toolsID} label="Tool governance"`, "profile and role editors share the tool-governance selector")
	must("if (surfacesTools && draft.tools) body.tools = draft.tools", "the profile editor persists tool governance")
	must("if (h?.can_tools && h.tools_modes?.length && draft.tools) body.tools = draft.tools", "the role editor persists tool governance")

	// AskUserQuestion idle-timeout (Claude-Code-only) — the row + selector exist,
	// reshape per harness off the catalog's can_ask_timeout gate, and the chosen
	// value is forwarded in the spawn body. Pins the JS/HTML so a JS-stale
	// worktree (embedded assets) trips here rather than silently at integration.
	must(`id="agent-spawn-ask-timeout"`, "spawn dialog has an AskUserQuestion-timeout selector")
	must("askTimeout: ['can_ask_timeout', 'ask_timeout_modes'", "the timeout row gates on the harness catalog's can_ask_timeout")
	must("body.ask_user_question_timeout = draft.askTimeout", "the chosen AskUserQuestion timeout is sent in the spawn body")
	// modal-profiles.js: the profile editor edits + persists the same field.
	must(`id="profile-editor-ask-timeout"`, "profile editor has an AskUserQuestion-timeout selector")
	must("body.ask_user_question_timeout = draft.ask_user_question_timeout", "the profile editor persists the AskUserQuestion timeout")

	// agent-spawn-island.js: the Effort menu is rebuilt per harness from the
	// catalog's effort_levels (single source of truth — the static HTML
	// options are only a pre-snapshot fallback), so a harness with its own
	// reasoning scale needs no dashboard edit.
	must("efforts: Array.isArray(harness?.effort_levels)", "the effort menu is rebuilt per harness")
	must("harness.effort_levels : DEFAULT_EFFORTS", "the effort menu reads the harness's effort levels from the catalog")
}

// TestDashboardHTML_ModelNamingWired guards the parts of the model-naming
// surface that a JS test cannot see: that the roster and the Costs tab both
// go through the ONE shared normaliser, and that the spend strip is wired
// into the render tree and carries styling in the shipped CSS.
//
// The RULES themselves — which suffixes are peeled, which qualifiers survive,
// how the rollup totals and dedupes — are covered behaviourally in
// pkg/claude/agentd/jstest (helpers/costs-model tests), which executes the
// real modules. Asserting the same rules here as source substrings would only
// catch a rename while reading like real coverage, so this deliberately does
// not try.
func TestDashboardHTML_ModelNamingWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// One normaliser, three call sites. A second, divergent implementation is
	// exactly how "Opus 5" and "Opus 5 (1M context)" drifted apart before.
	must("function displayModel(model, harness = '') {", "the shared model normaliser is defined once")
	must("${displayModel(model, harness)}", "the roster's visible token uses it")
	must("export function costModelLabel(agent) {", "the costs tab funnels through one label helper")
	must("costModelLabel(left).localeCompare(costModelLabel(right))", "the Model column sorts on that label")
	must("agent.harness, costModelLabel(agent)]", "the costs filter matches that label")
	must(`<td><span class="muted">${costModelLabel(agent)}</span></td>`, "the Model cell renders that label")

	// The strip renders above the table and is styled in both skins — neither
	// is observable from the module tests, which never load the stylesheet.
	must("<${ModelRollup} current=${current} />", "the per-model spend strip renders above the cost table")
	must("#costs-model-rollup {", "the strip is styled")
	must("body.wizard #costs-model-rollup {", "the strip is skinned for the wizard theme like the table below it")
}
