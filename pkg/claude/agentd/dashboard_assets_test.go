package agentd

import (
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// dashboardJSModules lists every embedded ES-module file under js/,
// sorted — dashboard.js (the entrypoint) plus whatever feature modules
// the Stage 2 split has extracted so far.
func dashboardJSModules() []string {
	mods, err := fs.Glob(dashboardAssetsFS, "js/*.js")
	if err != nil {
		panic("agentd: globbing embedded dashboard js/: " + err.Error())
	}
	sort.Strings(mods)
	return mods
}

// dashboardAssets is the embedded dashboard source — dashboard.html,
// dashboard.css and every js/ ES module — concatenated into one string.
//
// Before the ES-module cutover the dashboard was a single assembled
// `dashboardHTML` blob and content tests searched it directly. The
// files are now embedded and served separately, so tests that assert
// "X is present in the dashboard source" search this concatenation
// instead. Globbing js/*.js keeps it correct as the Stage 2 split
// extracts more modules. A genuinely missing file surfaces through
// TestDashboardEmbed_HasExpectedFiles, not a panic here.
var dashboardAssets = func() string {
	var b strings.Builder
	names := append([]string{"dashboard.html", "dashboard.css"}, dashboardJSModules()...)
	for _, name := range names {
		data, _ := fs.ReadFile(dashboardAssetsFS, name)
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}()

// TestDashboardEmbed_HasExpectedFiles guards that `//go:embed dashboard`
// captured the page shell, its stylesheet, and the ES-module entrypoint
// — a renamed or misplaced file would otherwise fail only at runtime,
// when the daemon serves an empty page or 404s a module.
func TestDashboardEmbed_HasExpectedFiles(t *testing.T) {
	for _, name := range []string{"dashboard.html", "dashboard.css", "js/dashboard.js"} {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("embedded dashboard asset %q not found: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("embedded dashboard asset %q is empty", name)
		}
	}
	if mods := dashboardJSModules(); len(mods) == 0 {
		t.Error("no js/*.js modules embedded")
	}
}

// TestDashboardAssets_SlopMachineWired guards the slop-mode slot
// machine: a JS helper (slopMachine) emits a .slop-machine widget with
// three .slop-reel children, and CSS swaps the regular .state-pill out
// in body.slop. The three pieces have to stay in lockstep — a rename
// in one file silently breaks the feature in the browser. Asserting
// on the embedded concatenation catches it at `go test ./...`.
func TestDashboardAssets_SlopMachineWired(t *testing.T) {
	// JS: helper is defined, exported, and wired into the row render.
	for _, needle := range []string{
		"function slopMachine(",
		"slopMachine,",                            // exported from helpers.js
		"slopMachine(state, m.online, m.conv_id)", // called from render.js
		"const SLOP_SYMBOLS",                      // reel glyph set
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — slot machine wiring broken", needle)
		}
	}
	// CSS: widget class, the working-state spin animation, and the
	// pill-hide rule that swaps slot in for pill in slop mode. The rule
	// is scoped to .state-cell — see TestDashboardCSS_SlopPillHideScopedToStateCell.
	for _, needle := range []string{
		".slop-machine",
		".slop-reel",
		".slop-strip",
		"@keyframes slop-spin",
		"body.slop .state-cell .state-pill { display: none; }",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard CSS missing %q — slot machine styling broken", needle)
		}
	}
}

// TestDashboardCSS_SlopPillHideScopedToStateCell guards the fix for the
// slop-mode bug where reused .state-pill cells went blank — the Audit
// tab's Outcome column showed nothing in slop mode. The slot machine
// only replaces the pill in the agent-row status cell (.state-cell,
// render.js), so the pill-hide rule MUST be scoped there. An unscoped
// `body.slop .state-pill { display: none; }` hides every .state-pill on
// the page — Audit Outcome, Plugins status/step pills — none of which
// have a slot machine to take their place. Pin the scoped form and
// reject the unscoped one so the bug can't silently return.
func TestDashboardCSS_SlopPillHideScopedToStateCell(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	if !strings.Contains(css, "body.slop .state-cell .state-pill { display: none; }") {
		t.Error("dashboard.css missing the .state-cell-scoped slop pill-hide rule — " +
			"reused .state-pill cells (Audit Outcome, Plugins status) go blank in slop mode")
	}
	// The unscoped global rule is the regression — it blanks every
	// .state-pill, not just the agent-row pill the slot machine replaces.
	if strings.Contains(css, "body.slop .state-pill {") {
		t.Error("dashboard.css carries the unscoped `body.slop .state-pill` rule — " +
			"it blanks the Audit/Plugins pills; scope it to .state-cell")
	}
}

// TestDashboardJS_SlopPullPausesRefresh guards the fix for the
// slop-mode bug where the 2s auto-refresh cancelled a slot machine the
// user had just pulled: refreshSuspended() (refresh.js) now defers the
// poll while any .slop-machine is mid-pull, detected via the sentinel
// data-status values manualPull() (slop-fx.js) tags the cell with for
// the pull's ~2.7s lifetime. The two files are coupled only through
// those literal strings, so a rename in one silently reopens the bug —
// asserting both halves are present at `go test ./...` catches it. Keep
// these needles in sync with both files if the sentinels ever change.
func TestDashboardJS_SlopPullPausesRefresh(t *testing.T) {
	for _, needle := range []string{
		// refresh.js: the suspension check keys on the in-pull cell.
		`.slop-machine[data-status="pull-spinning"], .slop-machine[data-status="pull-stopped"]`,
		// slop-fx.js: manualPull tags the cell with each sentinel.
		`machine.setAttribute('data-status', 'pull-spinning')`,
		`machine.setAttribute('data-status', 'pull-stopped')`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — slop-pull refresh pause regressed", needle)
		}
	}
}

// TestDashboardCSS_SpawnFieldsCannotOverflow guards the fix for the
// spawn/clone modals' horizontal scrollbar: the worktree <select>'s
// options carry long "branch — ~/path" labels, and a flex child's
// default min-width:auto pinned the box to that widest option, forcing
// the row past the modal's max-width. The shared .cron-create-row form
// control rule must keep min-width:0 (so the control shrinks to the
// available width) and the select must ellipsise its clipped label. A
// refactor dropping either silently brings the scrollbar back.
func TestDashboardCSS_SpawnFieldsCannotOverflow(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	for _, needle := range []string{
		"min-width: 0;", // form controls may shrink below content width
		".cron-create-row select { text-overflow: ellipsis; }", // selected label clips with an ellipsis
		"resize: both;", // modal is a resizable escape hatch (both axes)
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("dashboard.css missing %q — spawn modal field-width clamp regressed", needle)
		}
	}
}

// TestDashboardCSS_ModalScrollbarsThemed guards the themed scrollbars on the
// spawn/clone/cron/group/profile modal family. The tall dialogs (and their
// multiline textareas) scroll, and an unstyled scroll container falls back to
// the browser's default light, classic scrollbar — loud against the dark
// dialog, and especially against the near-black wizard re-skin. Two layers:
//  1. a base dark scrollbar on the shared .cron-create-modal class (all modes);
//  2. a wizard arcane override on the five wizard-reskinned dialogs, scoped by
//     #id via :is() so the dark-chrome clone/reincarnate/message dialogs (same
//     class, NOT reskinned) keep the base dark bar — the same positive-scoping
//     the surface re-skins follow. A refactor dropping either, or widening the
//     wizard rule to an unscoped .cron-create-modal, regresses the look.
func TestDashboardCSS_ModalScrollbarsThemed(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	for _, needle := range []string{
		".cron-create-modal::-webkit-scrollbar-thumb { background: #3a4553;",        // base dark thumb
		".cron-create-row textarea::-webkit-scrollbar-thumb { background: #3a4553;", // base dark textarea thumb
		"scrollbar-color: #3a4553 #161b22;",                                         // base Firefox (dialog body)
		"scrollbar-color: #7a5db0 #140f28;",                                         // wizard Firefox (arcane)
		"linear-gradient(180deg, #7a5db0, #3a2a63)",                                 // wizard arcane thumb
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("dashboard.css missing %q — modal scrollbar theming regressed", needle)
		}
	}
	// The wizard override MUST name the five reskinned dialogs by #id (via :is)
	// and MUST NOT widen to an unscoped `.cron-create-modal` scrollbar, which
	// would arcane-paint the dark clone/reincarnate/message dialogs' bars too.
	for _, id := range []string{
		"#agent-spawn-modal", "#cron-create-modal", "#group-create-modal",
		"#profile-editor-modal", "#export-agent-modal",
	} {
		if !strings.Contains(css, id) {
			t.Errorf("dashboard.css wizard scrollbar :is() list missing %q", id)
		}
	}
	if strings.Contains(css, "body.wizard .cron-create-modal::-webkit-scrollbar") {
		t.Error("wizard scrollbar override is unscoped — would repaint clone/reincarnate/message dialogs")
	}
}

// TestDashboardJS_SelectTooltipWired guards the readability half of the
// spawn-field fix: because the width-limited <select> clips long labels,
// the worktree options carry a full-path title and a helper mirrors the
// selected option's label/title into the <select> so it's legible on
// hover. The three pieces — helper, worktree option title, and the
// modal-level binding — must stay wired together.
func TestDashboardJS_SelectTooltipWired(t *testing.T) {
	for _, needle := range []string{
		"function syncSelectTitle(",                 // helper exists (helpers.js)
		"function bindSelectTitles(",                // modal-level binder exists (helpers.js)
		"syncSelectTitle(select)",                   // worktree picker syncs after repopulate (modal-link-wt.js)
		"bindSelectTitles($('#agent-spawn-modal'))", // spawn modal wires it (modal-spawn.js)
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — select tooltip wiring broken", needle)
		}
	}
}

// TestDashboardJS_ModalResizePersisted guards that the resizable spawn /
// clone dialogs persist their dragged size: a helper stores width+height
// in dashPrefs and both modals wire it to their resizable card. A drop
// here means the modal would silently forget its size across reopens.
func TestDashboardJS_ModalResizePersisted(t *testing.T) {
	for _, needle := range []string{
		"function makeModalResizable(",                                  // helper exists (helpers.js)
		"makeModalResizable($('#agent-spawn-modal .cron-create-modal')",     // spawn modal wires it
		"makeModalResizable($('#clone-agent-modal .cron-create-modal')",     // clone modal wires it
		"makeModalResizable($('#template-editor-modal .cron-create-modal')", // template editor wires it (JOH-357)
		"tclaude.dash.modalSize.agent-spawn",                                // per-modal pref key
		"tclaude.dash.modalSize.template-editor",                            // template editor pref key (JOH-357)
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — modal resize persistence broken", needle)
		}
	}
}

// TestDashboardCSS_TemplateEditorResizable guards the paired CSS half of the
// resizable summoning-circle editor (JOH-357): the id-scoped card must carry
// `resize: both` with non-visible overflow on both axes (else the grip is
// inert), and a raised max-width drag ceiling. Scoped to the #id, NOT the
// shared .template-editor-modal class, so the profile/role editor cards that
// also carry that class stay unaffected. A refactor dropping the override (or
// widening it to the shared class) regresses this.
func TestDashboardCSS_TemplateEditorResizable(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	// Pin the whole rule body verbatim: id-scoped card, resize on both axes with
	// non-visible overflow, and the raised max-width drag ceiling (min(1100px,…)
	// also appears on the base .cron-create-modal, so only the full id-scoped
	// block proves it's this editor's rule).
	needle := "#template-editor-modal .cron-create-modal {\n" +
		"  resize: both; overflow: auto;\n" +
		"  max-width: min(1100px, calc(100vw - 32px));\n" +
		"}"
	if !strings.Contains(css, needle) {
		t.Errorf("dashboard.css missing %q — template editor resize regressed", needle)
	}
}

// TestDashboardJS_ModalMinSizePinned guards the resize floor: the modal
// can't be dragged below its natural default size. A helper measures that
// size live (no hardcoded number) and makeModalResizable re-pins it each
// time the modal opens — watched off the overlay's `show` class so no open
// path needs a manual hook. Dropping either lets the grip crush the dialog
// below where its fields fit.
func TestDashboardJS_ModalMinSizePinned(t *testing.T) {
	for _, needle := range []string{
		"function refreshModalMinSize(", // measures + pins the natural min size (helpers.js)
		"refreshModalMinSize(modalEl)",  // makeModalResizable invokes it
		"new MutationObserver(",         // re-measures when the overlay gains `show`
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — modal min-size floor regressed", needle)
		}
	}
}

// TestDashboardJS_MailColsResizable guards the draggable Messages-tab
// column layout: two .mail-gutter drag bars sit in the mail-client grid,
// mail-resize.js owns the drag + persists the layout to dashPrefs under
// tclaude.dash.mail.cols, and mail.js wires it in at init. The CSS grid
// must keep its five-track shape (sidebar | gutter | list | gutter |
// reader) for the gutter placement to line up. A drop in any of these
// pieces silently breaks resize or its persistence.
func TestDashboardJS_MailColsResizable(t *testing.T) {
	for _, needle := range []string{
		"function initMailResize(",     // resizer module exists (mail-resize.js)
		"initMailResize()",             // mail.js calls it from initMail
		"tclaude.dash.mail.cols",       // per-layout pref key
		`data-boundary="sidebar-list"`, // left gutter (HTML)
		`data-boundary="list-reader"`,  // right gutter (HTML)
		".mail-gutter {",               // gutter styling (CSS)
		"grid-template-columns: 240px 10px minmax(260px, 1fr) 10px minmax(320px, 1.4fr);", // five-track default
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Messages-tab column resize wiring broken", needle)
		}
	}
}

// TestDashboardAssets_GroupQuickFoldWired guards the group quick-options
// auto-fold accordion (config dashboard.group_quick_options), whose pieces
// span six files that must stay in lockstep — there's no JS render test, so we
// assert on the embedded concatenation at `go test ./...`. The Go resolver +
// round-trip is covered separately by config.TestGroupQuickOptions. A rename in
// any one file silently breaks the fold (or its pin) only in the browser:
//   - render.js wraps the variable chip text in .qo-text and stamps
//     .quick-pinned on a pinned group's <details> + emits the ⚙ pin toggle;
//   - refresh.js toggles body.group-quick-fold off the snapshot flag;
//   - row-actions.js handles the pin toggle (a per-browser dashPref);
//   - dashboard.css collapses .qo-text at rest and expands it on header hover,
//     scoped to hover-capable pointers and skipping pinned groups;
//   - config.js + dashboard.html expose the Config-tab checkbox.
func TestDashboardAssets_GroupQuickFoldWired(t *testing.T) {
	for _, needle := range []string{
		// render.js — the collapsible text wrapper, the pin class, the pin
		// pref key and the ⚙ menu toggle.
		`<span class="qo-text">`,
		"quick-pinned",
		"tclaude.dash.quickpin.",
		`data-act="toggle-quick-pin"`,
		// refresh.js — drives the body class off the snapshot flag, and tracks
		// the hovered group so the reveal survives the 2s innerHTML re-render.
		"'group-quick-fold', data.group_quick_options !== 'expanded'",
		"export let hoveredGroupKey",
		"function bindGroupQuickHover(",
		// dashboard.js — wires the hover tracker in at init.
		"bindGroupQuickHover()",
		// render.js — re-stamps .quick-hover from the tracked key each render.
		"g.name === hoveredGroupKey",
		// row-actions.js — the pin toggle handler.
		"case 'toggle-quick-pin':",
		// config.js — load + gather the Config-tab checkbox.
		"#cfg-dashboard-group-quick-fold",
		"dashboard.group_quick_options = 'expanded'",
		// dashboard.html — the Config-tab control.
		`id="cfg-dashboard-group-quick-fold"`,
		// dashboard.css — collapse at rest (gated to hover pointers, skipping
		// pinned groups) and reveal on header hover.
		// The accordion is scoped to collapsed groups (:not([open])) so an
		// expanded group keeps its quick options fully shown.
		"body.group-quick-fold details[data-group-key]:not(.quick-pinned):not([open]) > summary .qo-text",
		"body.group-quick-fold details[data-group-key]:not(.quick-pinned):not([open]) > summary:hover .qo-text",
		"body.group-quick-fold details[data-group-key]:not(.quick-pinned):not([open]).quick-hover > summary .qo-text",
		"@media (hover: hover) {",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — group quick-fold wiring broken", needle)
		}
	}
}

// TestDashboardAssets_DefaultTerminalWired guards the "web terminal as the
// default" routing (config dashboard.default_terminal="web"), whose pieces span
// several files that must stay in lockstep — there's no JS render test, so we
// assert on the embedded concatenation at `go test ./...`. The Go resolver +
// round-trip is covered separately by config.TestDefaultTerminal. A rename in
// any one file silently breaks the routing only in the browser:
//   - dashboard.js exposes webTerminalDefault() off the snapshot flag;
//   - terminals-tab.js owns the shared openWebWindowPane / openWebTermPane
//     pane-openers the web buttons AND the routed actions both call;
//   - row-actions.js routes jump / open-window / term / term-dir / msg-focus;
//   - palette.js routes the command-palette "focus window";
//   - config.js + dashboard.html expose the Config-tab checkbox.
func TestDashboardAssets_DefaultTerminalWired(t *testing.T) {
	for _, needle := range []string{
		// dashboard.js — the resolver + the snapshot flag it reads.
		"export function webTerminalDefault()",
		"lastSnapshot.default_terminal === 'web'",
		// terminals-tab.js — the shared pane-openers.
		"export function openWebWindowPane(",
		"export function openWebTermPane(",
		// row-actions.js — the routed per-row actions (native → web branches).
		"if (webTerminalDefault()) { openWebWindowPane(agent, label); toast(",
		"if (webTerminalDefault()) { openWebTermPane(agent, label, termDirModal({ label })); return; }",
		"if (webTerminalDefault()) { openWebWindowPane(agent, label); return; }",
		"if (webTerminalDefault()) { openWebTermPane(agent, label, which); return; }",
		// palette.js — the command-palette "focus window" branch.
		"if (webTerminalDefault()) { openWebWindowPane(conv, label); toast(",
		// config.js — load + gather the Config-tab checkbox.
		"#cfg-dashboard-default-web-terminal",
		"dashboard.default_terminal = 'web'",
		// dashboard.html — the Config-tab control.
		`id="cfg-dashboard-default-web-terminal"`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — default-terminal routing broken", needle)
		}
	}
}

// TestDashboardAssets_ShowAgentHideButtonWired guards the per-agent "hide
// window" button's default-hidden toggle (config
// dashboard.show_agent_hide_button), whose pieces span several files that must
// stay in lockstep — there's no JS render test, so we assert on the embedded
// concatenation at `go test ./...`. The Go resolver + round-trip is covered
// separately by config.TestShowAgentHideButton. A rename in any one file
// silently breaks the toggle only in the browser:
//   - refresh.js toggles body.show-agent-hide-btn off the snapshot flag;
//   - dashboard.css hides the row's data-act="hide" button by default and
//     restores it under body.show-agent-hide-btn;
//   - config.js + dashboard.html expose the Config-tab checkbox.
//
// The button itself (helpers.js focusHideButtons) still always renders
// data-act="hide"; visibility is purely the CSS class, so nothing here
// touches the render path.
func TestDashboardAssets_ShowAgentHideButtonWired(t *testing.T) {
	for _, needle := range []string{
		// refresh.js — drives the body class off the snapshot flag.
		"'show-agent-hide-btn', !!data.show_agent_hide_button",
		// dashboard.css — hide by default, restore under the body class.
		`.row-actions .icon-btn[data-act="hide"] { display: none; }`,
		`body.show-agent-hide-btn .row-actions .icon-btn[data-act="hide"] { display: inline-flex; }`,
		// helpers.js — the button still renders (hidden only via CSS).
		`data-act="hide"`,
		// config.js — load + gather the Config-tab checkbox.
		"#cfg-dashboard-show-agent-hide-btn",
		"dashboard.show_agent_hide_button = true",
		// dashboard.html — the Config-tab control.
		`id="cfg-dashboard-show-agent-hide-btn"`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — agent hide-button toggle broken", needle)
		}
	}
}

// TestDashboardAssets_TUIColorSchemeWired guards the interactive-TUI color
// scheme selector (config tui.color_scheme), whose Config-tab pieces span
// dashboard.html + config.js and must stay in lockstep — there's no JS render
// test, so we assert on the embedded concatenation at `go test ./...`. The Go
// resolver + round-trip is covered separately by config.TestTUIColorScheme. A
// rename in either file silently breaks the selector only in the browser:
//   - dashboard.html — the Config-tab <select> and both option values;
//   - config.js — load (fill) + gather (save) the scheme.
func TestDashboardAssets_TUIColorSchemeWired(t *testing.T) {
	for _, needle := range []string{
		// dashboard.html — the Config-tab control + both option values.
		`id="cfg-tui-color-scheme"`,
		`value="dark-high-contrast"`,
		// config.js — load + gather the scheme (the non-default value that
		// actually writes the key).
		"#cfg-tui-color-scheme",
		"tui.color_scheme = scheme",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — TUI color-scheme selector broken", needle)
		}
	}
}

// TestDashboardHTML_ReferencesStaticAssets pins that the served
// dashboard.html loads the stylesheet and the ES-module entrypoint from
// the /static/ route by absolute path (so it resolves the same whatever
// path the document was served from), and that the retired Stage-1
// inline splice points (<style></style> / <script></script>) are gone.
func TestDashboardHTML_ReferencesStaticAssets(t *testing.T) {
	html := string(dashboardIndexHTML)
	for _, needle := range []string{
		`<link rel="stylesheet" href="/static/dashboard.css">`,
		`<script type="module" src="/static/js/dashboard.js"></script>`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("dashboard.html missing %q", needle)
		}
	}
	for _, stale := range []string{"<style></style>", "<script></script>"} {
		if strings.Contains(html, stale) {
			t.Errorf("dashboard.html still carries the retired splice point %q", stale)
		}
	}
}
