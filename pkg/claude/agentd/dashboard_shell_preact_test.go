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
			if !strings.Contains(rule.declarations, "z-index: var(--z-popover)") {
				t.Errorf("open PR footer must lift its popover to the popover layer, over the dock and windows: %q", rule.declarations)
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

	for _, id := range []string{"open-prs-popover", "open-mrs-popover"} {
		if !strings.Contains(island, `id="`+id+`" class="open-prs-popover ui-popover"`) {
			t.Errorf("the %s panel must be a .ui-popover so every skin paints it", id)
		}
	}
	for _, want := range []string{
		"body.wizard .open-prs-filters button.active {",
		"body.wizard .open-prs-final-ci { color: #b39ddb; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("open PR theming missing %q", want)
		}
	}

	// The filter row is a component both footer popovers share
	// (FooterListFilters); each popover must still place it after both result
	// variants and before its footer.
	if !strings.Contains(island, `<div class="open-prs-filters" role="group" aria-label=${label}>`) {
		t.Errorf("the shared filter row must keep its open-prs-filters group")
	}
	for _, popover := range []struct{ start, end, filters string }{
		{"function OpenPRs(", "function OpenMRs(", "<${FooterListFilters} label=\"Filter pull requests\""},
		{"function OpenMRs(", "function Disconnect(", "<${FooterListFilters} label=\"Filter merge requests\""},
	} {
		from, to := strings.Index(island, popover.start), strings.Index(island, popover.end)
		if from < 0 || to <= from {
			t.Errorf("%s not found ahead of %s", popover.start, popover.end)
			continue
		}
		body := island[from:to]
		list := strings.Index(body, "? html`<ul class=\"open-pr-list\">")
		empty := strings.Index(body, ": html`<p class=\"ui-empty-block\">")
		filters := strings.Index(body, popover.filters)
		footer := strings.Index(body, `<div class="open-prs-foot">`)
		if list < 0 || empty < 0 || filters < 0 || footer < 0 || list >= filters || empty >= filters || filters >= footer {
			t.Errorf("%s: filters must follow both result variants and precede the footer", popover.start)
		}
	}
	for _, want := range []string{
		"const [showFinalCI, setShowFinalCI] = useState(false);",
		"showFinalCI=${panel.showingRecent && showFinalCI}",
		"summary=${terminal && !showFinalCI ? null : pr.checks}",
		"panel.showingRecent ? html`<label class=\"open-prs-final-ci\"",
		"Closed and merged pull requests are never refreshed.",
	} {
		if !strings.Contains(island, want) {
			t.Errorf("closed PR final-CI opt-in missing %q", want)
		}
	}
}
