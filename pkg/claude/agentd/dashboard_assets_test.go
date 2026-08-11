package agentd

import (
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
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

// dashboardAssetFile reads one embedded dashboard asset by name (e.g.
// "dashboard.css" or "js/agent-spawn-actions.js"). dashboardAssets is the right
// tool for "this string exists somewhere in the dashboard"; this is for the
// stricter assertions that only mean something within a single file — "THIS
// module forwards that dependency", "THIS rule names both buttons" — where a
// match anywhere in the concatenation would be a false pass.
func dashboardAssetFile(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(dashboardAssetsFS, name)
	if err != nil {
		t.Fatalf("reading embedded dashboard asset %s: %v", name, err)
	}
	return string(data)
}

// dashboardSourceContains is for source-shape assertions spanning formatted
// HTML/HTM. These tests should pin structure and copy, not Prettier's choice of
// line breaks around attributes or component children.
func compactDashboardSource(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
}

func dashboardSourceContains(source, needle string) bool {
	return strings.Contains(compactDashboardSource(source), compactDashboardSource(needle))
}

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

// TestDashboardFooterVersionWired guards the footer's status line: it should
// show the running tclaude version alongside the dashboard URL and refresh
// heartbeat. The JSON field itself is covered by the snapshot flow test; this
// pins the client-side render contract.
func TestDashboardFooterVersionWired(t *testing.T) {
	for _, needle := range []string{
		`id="shell-meta-root"`,
		`const view = footerMetaView(state.snapshot.value);`,
		`class="meta-version">tclaude version ${view.version}</span>`,
		`class="meta-base">${view.base}</span>`,
		`refreshed <span class="meta-time">${new Date(view.generatedAt).toLocaleTimeString()}</span>`,
		`const FOOTER_SESSION_EXPANDED_PREF = 'tclaude.dash.footer.session_expanded';`,
		`auth cookie expires ${auth.expires_at ?`,
		`minted ${new Date(auth.minted_at).toLocaleString()}`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard footer missing %q", needle)
		}
	}
}

// TestDashboardAssets_SlopMachineWired guards the slop-mode slot
// machine: the native SlopMachine component emits a .slop-machine widget with
// three .slop-reel children, and CSS swaps the regular .state-pill out
// in body.slop. The three pieces have to stay in lockstep — a rename
// in one file silently breaks the feature in the browser. Asserting
// on the embedded concatenation catches it at `go test ./...`.
func TestDashboardAssets_SlopMachineWired(t *testing.T) {
	// JS: the component is defined and wired into the native row render. Its
	// opaque host explicitly hands ownership back on a status edge.
	for _, needle := range []string{
		"function SlopMachine(",
		"<${SlopMachine} state=${state} online=${online} conv=${conv} />",
		`data-opaque-host="slop-reels"`,
		"host.replaceChildren(root);",
		"render(html`<${SlopReels} status=${status} conv=${conv || ''} />`, root);",
		"SLOP_SYMBOLS,", // shared reel glyph set
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
		"body.slop .state-cell .state-pill-wrap > .state-pill { display: none; }",
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
// groups-member-table.js), so the pill-hide rule MUST be scoped there. An unscoped
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
	if !strings.Contains(css, "body.slop .state-cell .state-pill-wrap > .state-pill { display: none; }") {
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

// TestDashboardCSS_AgentStatePillTruncates guards the Groups table against
// hook details containing long tool names. The visual cap belongs only to the
// agent-row state cell because Audit, Jobs, and Plugins also reuse .state-pill
// for unrelated values. The component's full-value tooltip remains the escape
// hatch for clipped text.
func TestDashboardCSS_AgentStatePillTruncates(t *testing.T) {
	css := dashboardAssetFile(t, "dashboard.css")
	var scopedRule string
	var focusTooltipRule string
	withoutComments := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	for _, match := range regexp.MustCompile(`(?s)([^{}]*)\{([^{}]*)\}`).FindAllStringSubmatch(withoutComments, -1) {
		for branch := range strings.SplitSeq(match[1], ",") {
			selector := strings.Join(strings.Fields(branch), " ")
			if selector == ".state-cell .state-pill" {
				scopedRule = match[2]
			}
			if selector == ".state-pill-wrap:focus::after" {
				focusTooltipRule = match[2]
			}
			if selector == ".state-pill" &&
				regexp.MustCompile(`\bmax-width\s*:`).MatchString(match[2]) {
				t.Error("state-pill truncation is unscoped — would cap unrelated Audit, Jobs, and Plugins pills")
			}
		}
	}
	if scopedRule == "" {
		t.Fatal("dashboard.css is missing the Groups-scoped agent state-pill rule")
	}
	for _, declaration := range []string{
		`max-width:\s*24ch`, `overflow:\s*hidden`, `text-overflow:\s*ellipsis`,
	} {
		if !regexp.MustCompile(declaration).MatchString(scopedRule) {
			t.Errorf("Groups agent state-pill rule is missing %q", declaration)
		}
	}
	if focusTooltipRule == "" {
		t.Fatal("dashboard.css is missing the focused full-status tooltip rule")
	}
	if !regexp.MustCompile(`content:\s*attr\(data-full-status\)`).MatchString(focusTooltipRule) {
		t.Error("focused agent state wrapper no longer reveals its full data-full-status value")
	}
}

// TestDashboardJS_SlopPullUsesPreactOwnershipBoundary guards the slop-mode bug
// where a snapshot publish cancelled a slot machine the user had just pulled.
// manualPull still tags its ~2.7s phases, but the Groups Preact bridge now treats
// the reel body as an opaque nested root and retains the keyed wrapper instead
// of suspending the dashboard's global poll.
func TestDashboardJS_SlopPullUsesPreactOwnershipBoundary(t *testing.T) {
	for _, needle := range []string{
		// slop-fx.js: manualPull tags the cell with each sentinel.
		`machine.setAttribute('data-status', 'pull-spinning')`,
		`machine.setAttribute('data-status', 'pull-stopped')`,
		// groups-member-table.js: same-status publishes preserve the nested root;
		// the status-edge layout effect explicitly remounts its reel tree.
		`function SlopMachine({ state, online, conv })`,
		`}, [status, conv]);`,
		`host.replaceChildren();`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — slop-pull ownership boundary regressed", needle)
		}
	}
	if strings.Contains(dashboardAssets, `.slop-machine[data-status="pull-spinning"], .slop-machine[data-status="pull-stopped"]`) {
		t.Error("refresh suspension still keys on slop pull sentinels")
	}
}

// TestDashboardJS_PendingSpawnsRenderInTargetGroups guards the dashboard
// presentation for Codex pending spawns: the backend keeps them in
// pending_spawns until a conv-id exists, but the Groups tab buckets each
// pending row under its intended real group and uses the virtual Pending group
// only as a fallback for rows whose target group is gone/hidden.
func TestDashboardJS_PendingSpawnsRenderInTargetGroups(t *testing.T) {
	for _, needle := range []string{
		"function distributePendingToGroups(",
		"return { ...group, pending: rows };",
		"if (distributed.fallback.length) list.unshift(virtualPendingGroup(distributed.fallback));",
		"list.unshift(virtualPendingGroup(distributed.fallback));",
		"function PendingTable({ rows })",
		"group.pending?.length ? html`<span class=\"group-pending-chip\"",
		"group.pending?.length ? html`<div class=\"group-pending-block\"",
		"open=${realGroupOpen(group, dashPrefs)}",
		".group-pending-block",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — pending spawns may no longer render in their target groups", needle)
		}
	}
	if strings.Contains(dashboardAssets, "list.unshift(virtualPendingGroup(pending));") {
		t.Error("dashboard still prepends every pending spawn into the global virtual Pending group")
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

// TestDashboardJS_SelectTooltipWired guards full-path tooltip ownership on
// the maintained Preact worktree controls. Spawn gives each shortened option
// its full path, while clone also derives the closed select's controlled title.
func TestDashboardJS_SelectTooltipWired(t *testing.T) {
	for _, needle := range []string{
		"title=${`${branch}${main} — ${worktree.path}`}", // Preact worktree options retain the full path
		"title=${selectTitle}",                           // clone's closed select follows controlled selection
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — select tooltip wiring broken", needle)
		}
	}
}

// TestDashboardJS_ModalResizePersisted guards that the resizable spawn /
// clone dialogs persist their dragged size: a helper stores width+height
// in dashPrefs and both the legacy spawn binder and Preact clone overlay wire
// it to their resizable card. A drop
// here means the modal would silently forget its size across reopens.
func TestDashboardJS_ModalResizePersisted(t *testing.T) {
	for _, needle := range []string{
		"function makeModalResizable(",                     // helper exists (helpers.js)
		`resizeKey="tclaude.dash.modalSize.agent-spawn"`,   // Preact spawn overlay wires it
		`resizeKey="tclaude.dash.modalSize.clone-agent"`,   // Preact clone modal wires it
		"makeModalResizable(dialogRef.current, resizeKey)", // Preact management overlays wire it
		`resizeKey: 'tclaude.dash.modalSize.templates-manage'`,
		`fitContent: false`,
		"tclaude.dash.modalSize.agent-spawn",            // per-modal pref key
		"tclaude.dash.modalSize.operator-message",       // terminal operator composer
		"tclaude.dash.modalSize.template-editor",        // template editor pref key (JOH-357)
		"tclaude.dash.modalSize.sandbox-profile-editor", // sandbox-profile editor pref key
	} {
		if !dashboardSourceContains(dashboardAssets, needle) {
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

// TestDashboardCSS_SandboxProfileEditorResizable guards the paired CSS half of
// the resizable sandbox-profile editor: without the id in the shared
// `resize: both` rule the JS wires makeModalResizable to an inert (non-resizable)
// card and the grip never appears, yet TestDashboardJS_ModalResizePersisted
// still passes. Pin the whole comma-joined selector group verbatim so a refactor
// that drops the sandbox editor from it fails here.
func TestDashboardCSS_SandboxProfileEditorResizable(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	needle := "#agent-spawn-modal .cron-create-modal,\n" +
		"#clone-agent-modal .cron-create-modal,\n" +
		"#sandbox-profile-editor-modal .cron-create-modal {\n" +
		"  resize: both; overflow: auto;\n" +
		"}"
	if !strings.Contains(css, needle) {
		t.Errorf("dashboard.css missing %q — sandbox-profile editor resize regressed", needle)
	}
}

// TestDashboardCSS_SandboxProfileSelectLayouts guards the distinct authored and
// inherited row layouts. Authored filesystem access is a segmented control in
// the shared grid; only immutable inherited-access badges retain the old narrow
// column. Included-profile selects are intentionally intrinsic rather than
// stretching across their section.
func TestDashboardCSS_SandboxProfileSelectLayouts(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	if !strings.Contains(css, ".sbx-global-row .sbx-access { flex: 0 0 7em; }") {
		t.Error("dashboard.css inherited access badge must retain its narrow read-only column")
	}
	// Three trailing auto columns since TCL-866: Browse, the "mount at" control,
	// and remove. The path column keeps its minmax so the added action does not
	// come out of the path's width.
	if !strings.Contains(css, ".sbx-filesystem-row {\n  display: grid;\n"+
		"  /* access · path · Browse · mount-at · remove */\n"+
		"  grid-template-columns: 10rem minmax(10rem, 1fr) auto auto auto;") {
		t.Error("dashboard.css authored filesystem rows must use the shared segmented/path/action grid")
	}
	if !strings.Contains(css, ".sbx-row .sbx-inc-name {\n  flex: 0 1 auto; min-width: 0; width: auto; max-width: 100%;") {
		t.Error("dashboard.css included-profile selector must retain intrinsic width")
	}
}

// TestDashboardCSS_TerminalStacksAboveEditors guards the browser-terminal
// fallback used by modal actions such as sandbox profiles' "configure with
// agent". The terminal must cover the editor that launched it, while the
// shared confirmation dialog must still cover the terminal.
func TestDashboardCSS_TerminalStacksAboveEditors(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	zIndexOf := func(selector string) int {
		t.Helper()
		re := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{[^}]*z-index:\s*(\d+)`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("no z-index rule found for %s", selector)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad z-index for %s: %v", selector, err)
		}
		return n
	}

	terminal := zIndexOf("#term-session-modal")
	for _, editor := range []string{"#sandbox-profile-editor-modal", "#perm-edit-modal"} {
		if editorZ := zIndexOf(editor); terminal <= editorZ {
			t.Errorf("#term-session-modal z-index (%d) must be strictly above %s (%d)", terminal, editor, editorZ)
		}
	}
	if toast := zIndexOf(".toast"); terminal >= toast {
		t.Errorf("#term-session-modal z-index (%d) must stay below .toast (%d)", terminal, toast)
	}
	if confirm := zIndexOf("#confirm-modal"); terminal >= confirm {
		t.Errorf("#term-session-modal z-index (%d) must stay below #confirm-modal (%d)", terminal, confirm)
	}
}

func TestDashboardCSS_ToastIsBottomCentered(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	for _, want := range []string{
		"bottom: calc(var(--footer-h) + 16px); left: 50%;",
		"transform: translate(-50%, 8px);",
		".toast.show { opacity: 1; transform: translate(-50%, 0); pointer-events: auto; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("dashboard toast must remain bottom-centered; missing %q", want)
		}
	}
}

// TestDashboardCSS_RegistryManagePanelsResizable guards the paired CSS half of
// the resizable registry management PANELS — group templates, spawn profiles,
// roles and sandbox profiles. They are LIST panels, not forms, so unlike the
// editors they carry a fixed min-height floor (the JS opts out of
// content-tracking min via fitContent:false — a content-tracking floor would
// pin at the 86vh cap and make a long list un-shrinkable). Each card must carry
// `resize: both` with non-visible overflow on both axes (overflow:auto
// overriding the base overflow-y:auto, else the grip is inert), an explicit
// width that keeps the default 880 while max-width raises only the drag
// ceiling, and the min-height floor.
//
// The rule is still scoped by #id rather than the shared .manage-modal class:
// the links panel also carries that class and is NOT wired for resize on the JS
// side, so a class-wide rule would give it an inert grip.
func TestDashboardCSS_RegistryManagePanelsResizable(t *testing.T) {
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	needle := "#templates-manage-modal .manage-modal,\n" +
		"#profiles-manage-modal .manage-modal,\n" +
		"#roles-manage-modal .manage-modal,\n" +
		"#sandbox-profiles-manage-modal .manage-modal {\n" +
		"  resize: both; overflow: auto;\n" +
		"  width: min(880px, calc(100vw - 32px));\n" +
		"  max-width: min(1100px, calc(100vw - 32px));\n" +
		"  min-height: 260px;\n" +
		"}"
	if !strings.Contains(css, needle) {
		t.Errorf("dashboard.css missing %q — registry manage-panel resize regressed", needle)
	}
	// Every panel that can be enlarged also needs its list host to flex, or the
	// footer Close button floats in the middle of the grown box.
	listFlex := "#templates-manage-modal #templates-list,\n" +
		"#profiles-manage-modal #profiles-list,\n" +
		"#roles-manage-modal #roles-list,\n" +
		"#sandbox-profiles-manage-modal #sandbox-profiles-list { flex: 1 1 auto; }"
	if !strings.Contains(css, listFlex) {
		t.Errorf("dashboard.css missing %q — enlarged panel footers would float", listFlex)
	}
	// The links panel shares .manage-modal but has no resize wiring, so it must
	// not have been swept into the rule. Check this structurally rather than by
	// spelling: scan every rule that declares `resize` and reject one whose
	// selector list mentions the links panel, so a differently-formatted or
	// separately-authored rule cannot slip past.
	for _, rule := range cssRulesDeclaring(css, "resize:") {
		if strings.Contains(rule.selector, "#links-manage-modal") {
			t.Errorf("dashboard.css gives #links-manage-modal a resize grip its JS never persists (selector %q)", rule.selector)
		}
	}
}

// cssRulesDeclaring returns the flat (non-nested) CSS rules whose declaration
// block contains `needle`, paired with their selector list. Comments are
// stripped first so a rule discussed in prose is not mistaken for a real one.
// Good enough for asset guards: dashboard.css has no at-rule-nested blocks that
// declare the properties these tests police.
func cssRulesDeclaring(css, needle string) []struct{ selector, body string } {
	var out []struct{ selector, body string }
	for {
		start := strings.Index(css, "/*")
		if start < 0 {
			break
		}
		end := strings.Index(css[start:], "*/")
		if end < 0 {
			css = css[:start]
			break
		}
		css = css[:start] + " " + css[start+end+2:]
	}
	for rest := css; ; {
		open := strings.Index(rest, "{")
		if open < 0 {
			return out
		}
		close := strings.Index(rest[open:], "}")
		if close < 0 {
			return out
		}
		selector := strings.TrimSpace(rest[:open])
		body := rest[open+1 : open+close]
		if strings.Contains(body, needle) {
			out = append(out, struct{ selector, body string }{selector, body})
		}
		rest = rest[open+close+1:]
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
//   - Groups component state owns semantic hover identity and cleanup;
//   - groups-list.js wraps the variable chip text in .qo-text and stamps
//     .quick-pinned on a pinned group's <details> + emits the native ⚙ pin toggle;
//   - refresh.js toggles body.group-quick-fold off the snapshot flag;
//   - groups-actions.js handles the pin toggle (a per-browser dashPref);
//   - dashboard.css collapses .qo-text at rest and expands it on header hover,
//     scoped to hover-capable pointers and skipping pinned groups;
//   - config.js + dashboard.html expose the Config-tab checkbox.
func TestDashboardAssets_GroupQuickFoldWired(t *testing.T) {
	for _, needle := range []string{
		// groups-list.js — the collapsible text wrapper, the pin class, the pin
		// pref key and the ⚙ menu toggle.
		`<span class="qo-text">`,
		"quick-pinned",
		"tclaude.dash.quickpin.",
		"actions.toggleQuickPin(group)",
		"toggleQuickPin(group)",
		// refresh.js drives the body class; GroupsList owns hover on the stable
		// host so the reveal survives keyed polling reconciliation.
		"'group-quick-fold', data.group_quick_options !== 'expanded'",
		"host.addEventListener('mouseover', onMouseOver)",
		"host.removeEventListener('mouseover', onMouseOver)",
		// groups-list.js — re-stamps .quick-hover from the tracked key each render.
		"hoveredGroupKey === group.name",
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

// TestDashboardAssets_GroupProfileIconDimming guards the per-group spawn and
// sandbox profile indicators' deliberately narrow brightness states. The
// glyphs need their own wrapper so the chip's text-color hover cannot brighten
// them indirectly; they should follow the whole clickable chip's hover area,
// or stay bright while their group is open and the profile is set.
func TestDashboardAssets_GroupProfileIconDimming(t *testing.T) {
	groups := dashboardAssetFile(t, "js/groups-list.js")
	css := dashboardAssetFile(t, "dashboard.css")
	for _, needle := range []string{
		`<span class="group-profile-icon">${sandbox ? '🛡' : '🧠'}</span>`,
		".group-default-model:hover > .group-profile-icon,",
		".group-sandbox-profile:hover > .group-profile-icon,",
		"details[data-group-key][open] > summary .group-default-model:not(.unset) > .group-profile-icon",
		"details[data-group-key][open] > summary .group-sandbox-profile:not(.unset) > .group-profile-icon",
	} {
		if !strings.Contains(groups+css, needle) {
			t.Errorf("dashboard assets missing %q — group profile icon dimming regressed", needle)
		}
	}
	iconRule := regexp.MustCompile(`(?s)\.group-profile-icon\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if len(iconRule) != 2 {
		t.Fatal("dashboard CSS is missing the group profile icon's base rule")
	}
	for _, declaration := range []string{"color: #d4d4d4;", "opacity: 0.35;"} {
		if !strings.Contains(iconRule[1], declaration) {
			t.Errorf("group profile icon base rule missing %q", declaration)
		}
	}
	for _, forbidden := range []string{
		"summary:hover .group-default-model,",
		".quick-hover > summary .group-default-model,",
	} {
		if strings.Contains(css, forbidden) {
			t.Errorf("dashboard CSS still contains broad profile-chip opacity selector %q", forbidden)
		}
	}
}

// TestDashboardAssets_QuickChipKeyboardOperability guards the keyboard
// operability of the quick-option chips (TCL-330), whose pieces span three
// files that must stay in lockstep — there's no JS render test, so we assert
// on the embedded concatenation at `go test ./...`. A rename in any one file
// silently regresses the chips back to click-only in the browser:
//   - groups-list.js stamps tabindex="0" role="button" on every actionable
//     group-header chip (the picker 📁 sub-affordance included);
//   - row-actions.js delegates Enter/Space on those spans into the shared
//     click dispatcher, and the inline chip editors return focus to the
//     restored chip on Escape;
//   - dashboard.html serves the toolbar 🧠 chip as a native <button>
//     (its 🛡 sibling already is one — see the sandbox-profiles test);
//   - dashboard.css reveals the folded .qo-text labels for a focused
//     collapsed group and draws the shared focus ring.
func TestDashboardAssets_QuickChipKeyboardOperability(t *testing.T) {
	for _, needle := range []string{
		// groups-list.js — every native actionable group-header chip is
		// focusable and announces as a button.
		"className=${`group-descr${group.descr ? '' : ' unset'}`} action=\"set-group-descr\"",
		"className=${`group-default-cwd${group.default_cwd ? '' : ' unset'}`} action=\"set-group-dir\"",
		`class="gdc-pick" tabindex="0" role="button" data-act="pick-group-dir"`,
		`action="set-group-max-members"`,
		`data-act=${sandbox ? 'set-group-sandbox-profile' : 'set-group-profile'}`,
		`class="group-link-chips" tabindex="0" role="button"`,
		// row-actions.js — delegated Enter/Space activation for the chip
		// spans, funneled through the click dispatcher.
		`liveActionSource(event, 'span[data-act][role="button"]')`,
		"chip.click();",
		// groups-interactions.js / groups-list.js — generic native editors use
		// the exact keyed fallback, while profile chips focus the replacement
		// trigger from their own post-commit layout effect.
		"if (target?.isConnected)",
		".find((node) => node.dataset.editorKey === key)?.focus();",
		"restoreFocusRef.current = true;",
		"if (active || !restoreFocusRef.current) return;",
		"triggerRef.current?.focus();",
		// toolbar-profile-picker-island.js — each toolbar chip keeps native
		// button semantics until Preact swaps it for the inline select.
		`<button ref=${triggerRef} type="button" id=${copy.id}`,
		// toolbar-profile-renderers.js — its accessible name tracks the picked profile.
		"`Set ${GLOBAL_DEFAULT_PROFILE_NAME.toLowerCase()}`",
		// dashboard.css — tabbing onto a collapsed group's chips reveals the
		// folded labels (keyboard mirror of the hover reveal)…
		"body.group-quick-fold details[data-group-key]:not(.quick-pinned):not([open]) > summary:focus-within .qo-text",
		// …and the chips share the focus ring of the other focusable icons.
		".group-descr:focus-visible, .group-default-cwd:focus-visible, .gdc-pick:focus-visible,",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — quick-chip keyboard operability regressed", needle)
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
//   - transaction-dialog-actions.js routes the bulk windows-modal focus;
//   - the shell palette injects the snapshot preference into palette.js's
//     command/action boundary;
//   - config.js + dashboard.html expose the Config-tab checkbox.
func TestDashboardAssets_DefaultTerminalWired(t *testing.T) {
	for _, needle := range []string{
		// dashboard.js — the resolver + the snapshot flag it reads.
		"export function webTerminalDefault()",
		"lastSnapshot.default_terminal === 'web'",
		// agent spawn — the frozen snapshot choice suppresses native auto-focus,
		// then opens the new label through the web-terminal websocket.
		"defaultTerminal: snapshot.default_terminal === 'web' ? 'web' : 'native'",
		"if (draft.autoFocus && context.defaultTerminal === 'web') body.auto_focus_web = true;",
		"await openTerminalPane({\n            ws: payload.focus_ws",
		"openTerminalPane,\n      celebrateSlop",
		"key: `window:${agent}`",
		// terminals-tab.js — the shared pane-openers.
		"export function openWebWindowPane(",
		"export function openWebTermPane(",
		// row-actions.js — the routed per-row actions (native → web branches).
		"if (webTerminalDefault()) { openWebWindowPane(agent, label, { ...terminalPaneOptions, harness }); toast(",
		"if (webTerminalDefault()) { openWebTermPane(agent, label, chooseTerminalDirectory(label)); return; }",
		"if (webTerminalDefault()) { openWebWindowPane(agent, label, { harness }); return; }",
		"if (webTerminalDefault()) { openWebTermPane(agent, label, which); return; }",
		// palette.js — the shell-owned command palette passes the snapshot-derived
		// preference into the command's "focus window" action.
		"if (preferWebTerminal) { openWebWindowPane(conv, label, { harness }); toast(",
		// transaction-dialog-actions.js — bulk focus opens every selected agent as a web pane and
		// skips the native-only /api/agent-windows focus endpoint.
		"if (request.direction === 'focus' && request.webTerminal) {",
		"openWebWindowPane(target.selector, target.label, { harness: target.harness });",
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
	if strings.Contains(dashboardAssets, "openTerminal: openTermModal") {
		t.Error("spawn web auto-focus must open a Terminals-tab pane, not the terminal overlay")
	}
}

func TestDashboardAssets_DirectoryPickerWired(t *testing.T) {
	for _, needle := range []string{
		`id="directory-picker-root"`,
		"mountDirectoryPickerFeature",
		"configureDirectoryPickerBridge",
		"/api/browse-directories",
		"lastSnapshot?.default_directory_picker === 'web'",
		"#cfg-dashboard-default-web-directory-picker",
		"dashboard.default_directory_picker = 'web'",
		`id="directory-picker-modal"`,
		`#directory-picker-modal {`,
		`z-index: 10002`,
		`.toast.error { border-left-color: #f85149; z-index: 10003; }`,
		"body.wizard .directory-picker-modal",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — web directory picker wiring broken", needle)
		}
	}
	html, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	rootAt := strings.LastIndex(string(html), `id="directory-picker-root"`)
	lastStaticOverlayAt := strings.LastIndex(string(html), `class="modal-overlay`)
	if rootAt < lastStaticOverlayAt {
		t.Error("directory picker root must follow every static modal overlay so stacked Browse dialogs paint above their caller and own keyboard focus")
	}
}

// TestDashboardAssets_GroupWebTerminalWired guards the group ⚙ menu's "open web
// terminal" item — the group counterpart of the per-agent "web term" button. Its
// pieces span three JS files plus a server route (dashboard_edit.go), and there's
// no JS render test, so a rename in any one of them would silently break the
// feature in the browser. Assert on the embedded concatenation at `go test ./...`;
// the server route + resolve is covered by TestGroupTermWS_* below.
//   - groups-list.js builds the gated native menu item;
//   - row-actions.js imports the pane-opener and dispatches the data-act;
//   - terminals-tab.js exports the pane-opener that hits /api/group-term-ws.
func TestDashboardAssets_GroupWebTerminalWired(t *testing.T) {
	for _, needle := range []string{
		// groups-list.js — the gated native menu item + its data-act.
		"function GroupMenuItems(",
		`data-act="group-web-term"`,
		"group.default_cwd ? html`<${MenuButton}",
		// row-actions.js — the import and the dispatch case.
		"openGroupWebTermPane,",
		"case 'group-web-term':",
		"openGroupWebTermPane(group, label);",
		// terminals-tab.js — the exported pane-opener + its WS path.
		"export function openGroupWebTermPane(",
		"/api/group-term-ws/",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — group web-terminal wiring broken", needle)
		}
	}
}

func TestDashboardAssets_DefaultDirectoryAutoJoinGroupWired(t *testing.T) {
	for _, needle := range []string{
		`data-act="set-default-spawn-group"`,
		`group.default_spawn_group`,
		`make directory auto-join default`,
		`case 'set-default-spawn-group':`,
		`JSON.stringify({ default_spawn_group: enabled })`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — default directory auto-join group wiring broken", needle)
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

// TestDashboardAssets_ShowGroupDescriptionWired guards the group-description
// chip's default-hidden toggle (config dashboard.show_group_description), the
// deprecation of the display-only group-description feature. Its pieces span
// several files that must stay in lockstep — there's no JS render test, so we
// assert on the embedded concatenation at `go test ./...`. The Go resolver +
// round-trip is covered separately by config.TestShowGroupDescription. A rename
// in any one file silently breaks the toggle only in the browser:
//   - refresh.js toggles body.show-group-description off the snapshot flag;
//   - dashboard.css hides the .group-descr chip AND the group-create dialog's
//     Descr row by default and restores both under body.show-group-description;
//   - config.js + dashboard.html expose the Config-tab checkbox.
//
// The chip itself (groups-list.js) still always renders .group-descr; visibility is
// purely the CSS class, so nothing here touches the render path.
func TestDashboardAssets_ShowGroupDescriptionWired(t *testing.T) {
	for _, needle := range []string{
		// refresh.js — drives the body class off the snapshot flag.
		"'show-group-description', !!data.show_group_description",
		// dashboard.css — hide by default, restore under the body class.
		`.group-descr { display: none; }`,
		`body.show-group-description .group-descr { display: inline; }`,
		// dashboard.css + dashboard.html — the group-create dialog's Descr row
		// follows the same deprecation (hidden unless opted in).
		`.group-create-descr-row { display: none; }`,
		`body.show-group-description .group-create-descr-row { display: flex; }`,
		`class="cron-create-row group-create-descr-row"`,
		// config.js — load + gather the Config-tab checkbox.
		"#cfg-dashboard-show-group-description",
		"dashboard.show_group_description = true",
		// dashboard.html — the Config-tab control.
		`id="cfg-dashboard-show-group-description"`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — group-description toggle broken", needle)
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

// TestDashboardAssets_UsageReadoutWired guards the usage-readout knobs
// (config usage.idle_timeout + usage.poll_anthropic_api), whose Config-tab
// pieces span dashboard.html + config.js and must stay in lockstep — there's
// no JS render test, so we assert on the embedded concatenation at
// `go test ./...`. The Go resolver + round-trip is covered separately by
// config.TestResolvedUsageIdleTimeout / config.TestPollAnthropicUsageAPI. A
// rename in either file silently breaks the fields only in the browser:
//   - dashboard.html — the Config-tab controls;
//   - config.js — load (fill) + gather (save) the values.
func TestDashboardAssets_UsageReadoutWired(t *testing.T) {
	for _, needle := range []string{
		// dashboard.html — the Config-tab controls.
		`id="cfg-usage-poll-anthropic-api"`,
		`id="cfg-usage-idle-timeout"`,
		// config.js — load + gather the values (the keys that actually write).
		"#cfg-usage-poll-anthropic-api",
		"usage.poll_anthropic_api = true",
		"#cfg-usage-idle-timeout",
		"usage.idle_timeout = uitRaw",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — usage readout config broken", needle)
		}
	}
}

// TestDashboardAssets_FeatureFlagsWired guards the experimental feature flags
// across their exact Preact markup and adapter owners. The Go accessors +
// round-trip are covered separately in common/config.
func TestDashboardAssets_FeatureFlagsWired(t *testing.T) {
	owners := map[string][]string{
		"js/config-form-markup.js": {
			`id="cfg-feature-processes"`, `features.processes`,
			`id="cfg-feature-groups-route-map"`, `features.groups_route_map`,
			`id="cfg-feature-group-attachments"`, `features.group_attachments`,
			`id="cfg-feature-terminal-command-palette-shortcut"`,
			`features.terminal_command_palette_shortcut`,
			`id="cfg-feature-recorded-sandbox-details"`,
			`features.recorded_sandbox_details`,
		},
		"js/config-form-adapter.js": {
			"#cfg-feature-processes", "feats.processes = true",
			"#cfg-feature-groups-route-map", "feats.groups_route_map = true",
			"#cfg-feature-group-attachments", "feats.group_attachments = groupAttachments",
			"#cfg-feature-terminal-command-palette-shortcut",
			"feats.terminal_command_palette_shortcut = true",
			"#cfg-feature-recorded-sandbox-details",
			"feats.recorded_sandbox_details = true",
		},
	}
	for name, needles := range owners {
		contents := string(mustReadFS(dashboardAssetsFS, name))
		for _, needle := range needles {
			if !strings.Contains(contents, needle) {
				t.Errorf("%s missing %q — experimental feature flag config broken", name, needle)
			}
		}
	}
}

// TestDashboardAssets_ProcessesTabWired pins the feature-gated template shell
// and the stable editor mount contract.
// The module has no build step, so literal asset pins catch a renamed
// DOM id or route before it becomes a browser-only failure.
func TestDashboardAssets_ProcessesTabWired(t *testing.T) {
	for _, needle := range []string{
		`<a data-tab="processes"`,
		`id="processes-root"`,
		`data-process-subtab="templates"`,
		`id="process-editor-canvas"`,
		`data-process-mount="editor"`,
		"name === 'templates' ? '/v1/process/templates'",
		"'/v1/process/template-heads'",
		"applyProcessesTabVisibility(data)",
		`body.hide-processes nav [data-tab="processes"]`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Processes tab contract broken", needle)
		}
	}
}

// TestDashboardAssets_ClaudeCleanupPeriodWired guards the Claude Code
// transcript-retention override (config claude_cleanup_period_days → Claude
// Code's cleanupPeriodDays), whose Config-tab pieces span dashboard.html +
// config.js and must stay in lockstep — there's no JS render test, so we assert
// on the embedded concatenation at `go test ./...`. The Go accessor + round-trip
// is covered separately by config.TestClaudeCleanupPeriodDaysOverride /
// config.TestClaudeCleanupPeriodDays_JSONRoundTrip. A rename in either file
// silently breaks the field only in the browser:
//   - dashboard.html — the Config-tab number input;
//   - config.js — load (fill) + gather (save) the value.
func TestDashboardAssets_ClaudeCleanupPeriodWired(t *testing.T) {
	for _, needle := range []string{
		// dashboard.html — the Config-tab control.
		`id="cfg-claude-cleanup-days"`,
		// config.js — load + gather the value (the key that actually writes).
		"#cfg-claude-cleanup-days",
		"cfg.claude_cleanup_period_days = cfgInt",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Claude cleanup-period config broken", needle)
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
