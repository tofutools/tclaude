package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardShellPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := fs.ReadFile(dashboardAssetsFS, "js/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(body)
	}
	must := func(source, needle, why string) {
		t.Helper()
		if !strings.Contains(source, needle) {
			t.Errorf("missing %q (%s)", needle, why)
		}
	}
	mustNot := func(source, needle, why string) {
		t.Helper()
		if strings.Contains(source, needle) {
			t.Errorf("still contains %q (%s)", needle, why)
		}
	}

	htmlBody, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(htmlBody)
	loader := read("preact-loader.js")
	dashboard := read("dashboard.js")
	groupsList := read("groups-list.js")
	memberTable := read("groups-member-table.js")
	refresh := read("refresh.js")
	model := read("shell-model.js")
	island := read("shell-island.js")
	state := read("shell-state.js")

	for _, host := range []string{
		"shell-activity-root", "shell-usage-root", "shell-status-root",
		"shell-notify-root", "shell-credits-root", "shell-messages-badge-root",
		"shell-meta-root", "shell-open-prs-root", "shell-disconnect-root", "shell-confirm-root",
		"shell-toast-root", "shell-palette-button-root", "shell-palette-modal-root",
	} {
		must(html, `id="`+host+`"`, "the shell has an explicit stable host")
		must(loader, "#"+host, "the shell descriptor claims the host")
	}
	must(loader, "const shellDescriptor = createIslandDescriptor({", "the shell uses the guarded island lifecycle")
	must(loader, "mountShellIsland({", "core shell widgets mount through Preact")
	must(loader, "throw new Error('Dashboard shell failed to mount')", "critical feedback failure aborts bootstrap")
	must(loader, "mountNotifyIsland({", "notification settings mount through Preact")
	must(loader, "mountCreditsIsland({", "the credits counter mounts through Preact")
	must(loader, "mountPaletteIsland({", "the command palette mounts through Preact")
	must(dashboard, "pageCleanups.push(await mountShellFeature({ notify: toast }));", "page teardown owns the shell mount")
	must(dashboard, "for (const cleanup of pageCleanups.reverse()) cleanup?.();", "pagehide tears down every owner in reverse order")

	for _, needle := range []string{"document.", "fetch(", "innerHTML", "morphInto"} {
		mustNot(model, needle, "the pure shell model must not own browser effects")
	}
	for _, needle := range []string{"fetch(", "morphInto", "innerHTML"} {
		mustNot(island, needle, "the Preact shell renderer must not use legacy painting or API effects")
	}
	must(state, "const status = signal(", "status feedback is signal-owned")
	must(state, "const confirmation = signal(", "confirmation feedback is signal-owned")
	must(island, "state.snapshot.value", "snapshot-backed widgets read the accepted snapshot signal")
	must(island, "state.connection.value.status === 'disconnected'", "disconnect UI reads the shared connection signal")
	must(island, "mounted.slice().reverse()", "partial mounts can be rolled back safely")

	for _, legacy := range []string{
		"function renderGlobalActivity(", "function renderMessagesBadge(",
		"function renderUsage(", "function renderNotifyGlobal(", "function showStatus(",
	} {
		mustNot(groupsList, legacy, "the native Groups list does not own migrated shell DOM")
		mustNot(memberTable, legacy, "the native member table does not own migrated shell DOM")
	}
	for _, legacyCall := range []string{
		"renderGlobalActivity()", "renderMessagesBadge(", "renderUsage(",
		"renderNotifyGlobal(", "morphInto($('#meta')",
	} {
		mustNot(refresh, legacyCall, "snapshot refresh publishes state instead of repainting shell DOM")
	}
}

func TestDashboardOpenPRsHoverBridge(t *testing.T) {
	foundBase := false
	foundDock := false
	foundRaisedFooter := false
	for _, rule := range dashboardCSSRules(t) {
		if strings.TrimSpace(rule.selectors) == ".open-prs.is-open::before" {
			foundBase = true
			for _, declaration := range []string{
				"position: fixed", "bottom: var(--footer-h)", "height: 10px",
			} {
				if !strings.Contains(rule.declarations, declaration) {
					t.Errorf("open PR hover bridge missing %q in %q", declaration, rule.declarations)
				}
			}
		}
		if strings.TrimSpace(rule.selectors) == "body.dock-open .open-prs.is-open::before" {
			foundDock = true
			for _, declaration := range []string{
				"right: 12px", "width: calc(var(--dock-w) + min(470px, calc(100vw - 24px)))",
			} {
				if !strings.Contains(rule.declarations, declaration) {
					t.Errorf("dock-open PR hover bridge missing %q in %q", declaration, rule.declarations)
				}
			}
		}
		if strings.TrimSpace(rule.selectors) == "footer:has(.open-prs.is-open)" {
			foundRaisedFooter = true
			if !strings.Contains(rule.declarations, "z-index: 46") {
				t.Errorf("open PR footer must paint above the z-index 45 dock: %q", rule.declarations)
			}
		}
	}
	if !foundBase {
		t.Error("dashboard.css has no open PR hover bridge")
	}
	if !foundDock {
		t.Error("dashboard.css has no dock-open PR hover bridge geometry")
	}
	if !foundRaisedFooter {
		t.Error("dashboard.css does not raise the open PR footer above the dock")
	}
}

func TestDashboardOpenPRsThemed(t *testing.T) {
	css := string(mustReadFS(dashboardAssetsFS, "dashboard.css"))
	island := string(mustReadFS(dashboardAssetsFS, "js/shell-island.js"))

	fallbackStart := strings.Index(css, "@supports not selector(::-webkit-scrollbar) {\n  .open-pr-list")
	webkitStart := strings.Index(css, ".open-pr-list::-webkit-scrollbar {")
	if fallbackStart < 0 || webkitStart <= fallbackStart {
		t.Fatal("open PR standard scrollbar fallback must precede the WebKit scrollbar rules")
	}
	fallback := css[fallbackStart:webkitStart]
	for _, want := range []string{
		"scrollbar-color: #6e7681 #0d1117",
		"body.wizard .open-pr-list { scrollbar-color: #7a5db0 #140f28; }",
	} {
		if !strings.Contains(fallback, want) {
			t.Errorf("open PR non-WebKit scrollbar fallback missing %q", want)
		}
	}

	for _, want := range []string{
		".open-pr-list::-webkit-scrollbar-thumb:hover { background: #8b949e; }",
		".open-pr-list::-webkit-scrollbar-thumb:active { background: #b1bac4; }",
		"body.wizard .open-prs-popover {",
		"background: linear-gradient(180deg, #241b3d 0%, #140f28 100%);",
		"body.wizard .open-prs-filters button.active {",
		"body.wizard .open-prs-final-ci { color: #b39ddb; }",
		"body.wizard .open-pr-list::-webkit-scrollbar-thumb:hover { background: #a97bd6; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("open PR theming missing %q", want)
		}
	}

	list := strings.Index(island, "? html`<ul class=\"open-pr-list\">")
	empty := strings.Index(island, ": html`<p class=\"open-prs-empty\">")
	filters := strings.Index(island, `<div class="open-prs-filters"`)
	footer := strings.Index(island, "? html`<div class=\"open-prs-foot\">")
	if list < 0 || empty < 0 || filters < 0 || footer < 0 || list >= filters || empty >= filters || filters >= footer {
		t.Errorf("open PR filters must follow both result variants and precede the footer")
	}
	for _, want := range []string{
		"const [showFinalCI, setShowFinalCI] = useState(false);",
		"summary=${terminal && !showFinalCI ? null : pr.checks}",
		"view.showingRecent ? html`<label class=\"open-prs-final-ci\"",
		"Closed and merged pull requests are never refreshed.",
	} {
		if !strings.Contains(island, want) {
			t.Errorf("closed PR final-CI opt-in missing %q", want)
		}
	}
}
