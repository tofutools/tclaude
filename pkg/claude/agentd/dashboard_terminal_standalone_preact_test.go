package agentd

import (
	"strings"
	"testing"
)

func TestStandaloneTerminalShell_PreactOwnsStableRoot(t *testing.T) {
	html := string(terminalsPageHTML)
	for _, want := range []string{
		`<body class="solo">`,
		`<div id="terminals-root"></div>`,
		`<script type="importmap">`,
		`"preact/hooks": "/static/vendor/preact/hooks.module.js"`,
		`"@preact/signals-core": "/static/vendor/preact/signals-core.module.js"`,
		`"@preact/signals": "/static/vendor/preact/signals.module.js"`,
		`"htm": "/static/vendor/preact/htm.module.js"`,
		`<script type="module" src="/static/js/terminals.js"></script>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("standalone terminal page missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`id="mux"`, `id="mux-tabs"`, `id="mux-panes"`, `id="mux-empty"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("standalone terminal page retains static shell writer %q", forbidden)
		}
	}

	entry := readDashboardJS(t, "terminals.js")
	for _, want := range []string{
		"createStandaloneTerminalsPage({",
		"host: document.getElementById('terminals-root')",
		"initPrefs: initDashPrefs",
		"initThemeSync: initTerminalThemeSync",
		"void page.start()",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("standalone terminal entry missing %q", want)
		}
	}
	for _, forbidden := range []string{"createElement", "innerHTML", "append(", "mountMux"} {
		if strings.Contains(entry, forbidden) {
			t.Errorf("standalone terminal entry retains DOM writer %q", forbidden)
		}
	}

	core := readDashboardJS(t, "terminals-core.js")
	for _, forbidden := range []string{"mountMux", "document.createElement", "querySelector", "append("} {
		if strings.Contains(core, forbidden) {
			t.Errorf("opaque terminal core retains shell writer %q", forbidden)
		}
	}
	for _, want := range []string{
		"export function departedAgentSelectors(",
		"export function createAgentRosterReconciler()",
		"export function normalizeSeed(",
		"export function mountTerminalWidget({",
	} {
		if !strings.Contains(core, want) {
			t.Errorf("terminal core missing retained plain/opaque boundary %q", want)
		}
	}

	island := readDashboardJS(t, "terminal-shell-island.js")
	for _, want := range []string{
		"export function mountStandaloneTerminalShell({",
		"<${TerminalTabs}",
		"solo=${true}",
		"manageTitle=${true}",
		"empty=${true}",
	} {
		if !strings.Contains(island, want) {
			t.Errorf("standalone mount does not reuse the shared Preact pane shell %q", want)
		}
	}
}

func TestStandaloneTerminalShell_LifecycleContractsStayOutsideWidgetCore(t *testing.T) {
	lifecycle := readDashboardJS(t, "terminal-standalone.js")
	for _, want := range []string{
		"historyRef.replaceState",
		"documentRef.body.classList.toggle('wizard', seed.wizard === true)",
		"windowRef.addEventListener('hashchange', onHashChange)",
		"windowRef.addEventListener('tclaude:auth-expired', onAuthExpired)",
		"event.detail.returnTo",
		"navigatorRef.sendBeacon('/api/hide/'",
		"windowRef.addEventListener('pagehide', onPageHide)",
		"windowRef.addEventListener('unload', dispose)",
		"if (disposed) return",
		"Promise.resolve(initPrefs()).then(",
		"initThemeSync()",
		"mountShell({ host, state, actions, widgetFactory })",
	} {
		if !strings.Contains(lifecycle, want) {
			t.Errorf("standalone terminal lifecycle missing %q", want)
		}
	}
	if prefs := strings.Index(lifecycle, "Promise.resolve(initPrefs()).then("); prefs < 0 {
		t.Fatal("standalone terminal lifecycle does not hydrate preferences")
	} else if theme := strings.Index(lifecycle[prefs:], "initThemeSync()"); theme < 0 {
		t.Fatal("standalone terminal lifecycle does not start theme sync after preferences")
	} else if mount := strings.Index(lifecycle[prefs:], "mountShell({ host, state, actions, widgetFactory })"); mount < theme {
		t.Fatal("standalone terminal shell mounts before preference hydration and theme sync")
	}
}
