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
			if hostRule == nil || !strings.Contains(hostRule[1], "padding: 6px") {
				t.Errorf("%s base visual host must retain the 6px inset", tt.asset)
			}
			fitRule := regexp.MustCompile(regexp.QuoteMeta(tt.fit) + `\s*,\s*` + regexp.QuoteMeta(tt.fit) + `\s+\.xterm\s*\{([^}]*)\}`).FindStringSubmatch(css)
			if fitRule == nil || !strings.Contains(fitRule[1], "height: 100%") {
				t.Errorf("%s fit container and .xterm must fill the padded host's content box", tt.asset)
			}
		})
	}

	core := readDashboardJS(t, "terminals-core.js")
	for _, want := range []string{
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
