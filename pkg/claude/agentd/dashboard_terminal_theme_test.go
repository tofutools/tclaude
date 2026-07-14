package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The wizard terminal treatment spans the shared xterm palette, multiplexer
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
		"paletteLabel.textContent = 'Arcane palette'",
		"paletteToggle.hidden = !wizardActive()",
		"paletteCheckbox.addEventListener('change'",
		"p.term.options.theme = theme",
		"document.addEventListener('tclaude:wizard', syncTerminalTheme)",
		"document.addEventListener('tclaude:terminal-palette', syncTerminalTheme)",
		"hideConv: p.seed.hideConv, wizard: wizardActive()",
	} {
		if !strings.Contains(core, needle) {
			t.Errorf("terminals-core.js missing %q", needle)
		}
	}

	modal := read("js/modal-term.js")
	if !strings.Contains(modal, "term.options.theme = terminalThemeFor(") {
		t.Error("fallback terminal modal must repaint through the shared terminal theme")
	}

	popout := read("js/terminals.js")
	for _, needle := range []string{
		"import { initDashPrefs } from './prefs.js'",
		"import { initTerminalThemeSync } from './terminal-theme.js'",
		"document.body.classList.toggle('wizard', seed.wizard === true)",
		"if (prefsReady) consumeHash()",
		"initDashPrefs().then(",
		"initTerminalThemeSync()",
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
		"body.wizard .mux-pane-header",
		"body.wizard .mux-pane-xterm",
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("mux.css missing wizard terminal chrome %q", needle)
		}
	}
}
