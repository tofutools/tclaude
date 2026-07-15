package agentd

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestDashboardTerminalFitPaddingPlacement is a narrow source guard for the
// base terminal rules, not a CSS-cascade simulator. FitAddon measures the
// terminal host's border-box but subtracts padding on the generated .xterm
// element, so these declarations must keep the inset on .xterm itself.
func TestDashboardTerminalFitPaddingPlacement(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		asset string
		host  string
	}{
		{asset: "mux.css", host: ".mux-pane-xterm"},
		{asset: "dashboard.css", host: ".term-session-xterm"},
	} {
		t.Run(tt.asset, func(t *testing.T) {
			data, err := fs.ReadFile(dashboardAssetsFS, tt.asset)
			if err != nil {
				t.Fatal(err)
			}
			css := string(data)
			hostRule := regexp.MustCompile(regexp.QuoteMeta(tt.host) + `\s*\{([^}]*)\}`).FindStringSubmatch(css)
			if hostRule == nil || strings.Contains(hostRule[1], "padding:") {
				t.Errorf("%s base host rule must not carry padding", tt.asset)
			}
			xtermRule := regexp.MustCompile(regexp.QuoteMeta(tt.host) + `\s+\.xterm\s*\{([^}]*)\}`).FindStringSubmatch(css)
			if xtermRule == nil || !strings.Contains(xtermRule[1], "padding: 6px") {
				t.Errorf("%s base .xterm rule must carry the 6px inset", tt.asset)
			}
		})
	}
}
