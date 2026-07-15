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
// visual host on both web-terminal surfaces. The dashboard app shell must also
// inherit the document's scrollbar-adjusted height so horizontal page overflow
// reduces the space FitAddon receives without any JS scrollbar arithmetic.
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
	rootHeightRule := regexp.MustCompile(`html:has\(#tab-terminals\.active\)\s*\{([^}]*)\}`).FindStringSubmatch(string(dashboardCSS))
	if rootHeightRule == nil || !strings.Contains(rootHeightRule[1], "height: 100%") || strings.Contains(rootHeightRule[1], "scrollbar-color") {
		t.Error("terminal document root must expose its scrollbar-adjusted content height without overriding native scrollbar color")
	}
	bodyHeightRule := regexp.MustCompile(`body:has\(#tab-terminals\.active\)\s*\{([^}]*)\}`).FindStringSubmatch(string(dashboardCSS))
	if bodyHeightRule == nil || !strings.Contains(bodyHeightRule[1], "height: 100%") || strings.Contains(bodyHeightRule[1], "100vh") {
		t.Error("terminal app shell must inherit the root content height instead of using viewport units")
	}
	for _, unwanted := range []string{
		"html:has(#tab-terminals.active) body { scrollbar-color:",
		"html:has(#tab-terminals.active)::-webkit-scrollbar",
	} {
		if strings.Contains(string(dashboardCSS), unwanted) {
			t.Errorf("dashboard-level scrollbar must retain native chrome; found %q", unwanted)
		}
	}
	for _, want := range []string{
		".term-session-xterm .xterm-viewport { scrollbar-width: none; }",
		".term-session-xterm .xterm-viewport::-webkit-scrollbar { display: none; }",
		".term-session-xterm .xterm-scrollable-element > .scrollbar { display: none; }",
	} {
		if !strings.Contains(string(dashboardCSS), want) {
			t.Errorf("dashboard terminal missing hidden-scrollbar rule %q", want)
		}
	}

	core := readDashboardJS(t, "terminals-core.js")
	for _, want := range []string{
		"scrollback: 0",
		"fitHost.className = 'mux-pane-xterm-fit'",
		"host.append(fitHost)",
		"term.open(fitHost)",
		"p.ro.observe(fitHost)",
	} {
		if !strings.Contains(core, want) {
			t.Errorf("terminal multiplexer missing fit-container wiring %q", want)
		}
	}

	modal := readDashboardJS(t, "modal-term.js")
	for _, want := range []string{
		"scrollback: 0",
		"term.open($('#term-session-xterm-fit'))",
		".observe($('#term-session-xterm-fit'))",
	} {
		if !strings.Contains(modal, want) {
			t.Errorf("modal terminal missing fit-container wiring %q", want)
		}
	}
	if !strings.Contains(string(dashboardIndexHTML), `class="term-session-xterm-fit" id="term-session-xterm-fit"`) {
		t.Error("modal terminal HTML missing the inner fit container")
	}
}
