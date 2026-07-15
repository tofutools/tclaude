package agentd

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestDashboardTerminalFitContainerWiring is a narrow source guard for the
// base terminal rules, not a CSS-cascade simulator. FitAddon measures .xterm's
// direct parent, so an inner 100%-height box must separate it from the padded
// visual host on both web-terminal surfaces.
func TestDashboardTerminalFitContainerWiring(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		asset string
		host  string
		fit   string
	}{
		{asset: "mux.css", host: ".mux-pane-xterm", fit: ".mux-pane-xterm-fit"},
		{asset: "dashboard.css", host: ".term-session-xterm", fit: ".term-session-xterm-fit"},
	} {
		t.Run(tt.asset, func(t *testing.T) {
			data, err := fs.ReadFile(dashboardAssetsFS, tt.asset)
			if err != nil {
				t.Fatal(err)
			}
			css := string(data)
			hostRule := regexp.MustCompile(regexp.QuoteMeta(tt.host) + `\s*\{([^}]*)\}`).FindStringSubmatch(css)
			if hostRule == nil || !strings.Contains(hostRule[1], "padding: 6px 6px 0") {
				t.Errorf("%s base visual host must retain only the top/side 6px inset", tt.asset)
			}
			fitRule := regexp.MustCompile(regexp.QuoteMeta(tt.fit) + `\s*,\s*` + regexp.QuoteMeta(tt.fit) + `\s+\.xterm\s*\{([^}]*)\}`).FindStringSubmatch(css)
			if fitRule == nil || !strings.Contains(fitRule[1], "height: 100%") {
				t.Errorf("%s fit container and .xterm must fill the padded host's content box", tt.asset)
			}
		})
	}

	muxCSS, err := fs.ReadFile(dashboardAssetsFS, "mux.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".mux-pane-xterm .xterm-viewport { scrollbar-width: none; }",
		".mux-pane-xterm .xterm-viewport::-webkit-scrollbar { display: none; }",
		".mux-pane-xterm .xterm-scrollable-element > .scrollbar { display: none; }",
	} {
		if !strings.Contains(string(muxCSS), want) {
			t.Errorf("terminal multiplexer missing hidden-scrollbar rule %q", want)
		}
	}
	dashboardCSS, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"html:has(#tab-terminals.active) { scrollbar-color: transparent transparent; }",
		"html:has(#tab-terminals.active) body { scrollbar-color: auto; }",
		"html:has(#tab-terminals.active)::-webkit-scrollbar-button,",
		"background: transparent; border-color: transparent; color: transparent;",
		".term-session-xterm .xterm-viewport { scrollbar-width: none; }",
		".term-session-xterm .xterm-viewport::-webkit-scrollbar { display: none; }",
		".term-session-xterm .xterm-scrollable-element > .scrollbar { display: none; }",
	} {
		if !strings.Contains(string(dashboardCSS), want) {
			t.Errorf("dashboard terminal missing hidden-scrollbar rule %q", want)
		}
	}

	core := readDashboardJS(t, "terminals-core.js")
	if !strings.Contains(core, "scrollback: 0") {
		t.Error("opaque terminal widget must disable redundant xterm scrollback")
	}

	shell := readDashboardJS(t, "terminal-shell-island.js")
	for _, want := range []string{
		"return html`<div class=${className}><div ref=${hostRef} class=${fitClassName}></div></div>`;",
		`className="mux-pane-xterm"`,
		`fitClassName="mux-pane-xterm-fit"`,
		`className="term-session-xterm"`,
		`fitClassName="term-session-xterm-fit"`,
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("Preact terminal shell missing fit-container wiring %q", want)
		}
	}
	if !strings.Contains(string(dashboardIndexHTML), `id="terminal-session-root"`) {
		t.Error("dashboard HTML missing the stable Preact terminal-modal host")
	}
}
