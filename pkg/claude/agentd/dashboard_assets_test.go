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
		"makeModalResizable($('#agent-spawn-modal .cron-create-modal')", // spawn modal wires it
		"makeModalResizable($('#clone-agent-modal .cron-create-modal')", // clone modal wires it
		"tclaude.dash.modalSize.agent-spawn",                            // per-modal pref key
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — modal resize persistence broken", needle)
		}
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
