package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The wizard terminal treatment spans the shared xterm palette, terminal shell
// toolbar, pop-out handoff, and fallback modal. These source-shape guards pin
// that wiring while the pure preference/theme selection is covered by
// jstest/terminal-theme.test.mjs.
func TestDashboardTerminalTheme_Wiring(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("read embedded dashboard asset %q: %v", name, err)
		}
		return string(data)
	}

	theme := read("js/terminal-theme.js")
	for _, needle := range []string{
		"import { dashPrefs } from './prefs.js'",
		"tclaude.dash.terminals.arcanePalette",
		"prefs.getItem(ARCANE_PALETTE_PREF) !== '0'",
		"prefs.setItem(ARCANE_PALETTE_PREF, enabled ? '1' : '0')",
		"new Channel(PALETTE_CHANNEL)",
		"syncChannel.postMessage({ type: 'arcane-palette'",
		"tclaude:terminal-palette",
	} {
		if !strings.Contains(theme, needle) {
			t.Errorf("terminal-theme.js missing %q", needle)
		}
	}

	core := read("js/terminals-core.js")
	for _, needle := range []string{
		"function syncTheme()",
		"term.options.theme = terminalThemeFor(",
		"documentRef.addEventListener('tclaude:wizard', syncTheme)",
		"documentRef.addEventListener('tclaude:terminal-palette', syncTheme)",
		"documentRef.removeEventListener('tclaude:wizard', syncTheme)",
		"documentRef.removeEventListener('tclaude:terminal-palette', syncTheme)",
	} {
		if !strings.Contains(core, needle) {
			t.Errorf("terminals-core.js missing %q", needle)
		}
	}

	shell := read("js/terminal-shell-island.js")
	for _, needle := range []string{
		"<span>Arcane palette</span>",
		"hidden=${!theme.wizard}",
		"actions.setArcanePaletteEnabled(event.currentTarget.checked)",
	} {
		if !strings.Contains(shell, needle) {
			t.Errorf("terminal-shell-island.js missing %q", needle)
		}
	}
	actions := read("js/terminal-shell-actions.js")
	if !strings.Contains(actions, "wizard: documentRef.body.classList.contains('wizard')") {
		t.Error("terminal pop-out must inherit the dashboard wizard theme")
	}

	popout := read("js/terminal-standalone.js")
	for _, needle := range []string{
		"documentRef.body.classList.toggle('wizard', seed.wizard === true)",
		"if (prefsReady) consumeHash()",
		"Promise.resolve(initPrefs()).then(",
		"initThemeSync()",
	} {
		if !strings.Contains(popout, needle) {
			t.Errorf("standalone terminal pop-out missing %q", needle)
		}
	}

	dashboard := read("js/dashboard.js")
	if await := strings.Index(dashboard, "await initDashPrefs()"); await < 0 {
		t.Error("dashboard must hydrate preferences before terminal theme sync")
	} else if sync := strings.Index(dashboard, "initTerminalThemeSync()"); sync < await {
		t.Error("dashboard starts terminal theme sync before preferences are hydrated")
	}

	css := read("mux.css")
	for _, needle := range []string{
		".mux-palette-toggle",
		"body.wizard .mux-tabs",
		"body.wizard .mux-tab-menu",
		"body.wizard .mux-tab-menu-item",
		"body.wizard .mux-tab-menu-separator",
		"body.wizard .mux-pane-header",
		"body.wizard .mux-pane-xterm",
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("mux.css missing wizard terminal chrome %q", needle)
		}
	}
}
