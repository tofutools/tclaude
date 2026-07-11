package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardScribeVisibilityWiring(t *testing.T) {
	tabs, err := fs.ReadFile(dashboardAssetsFS, "js/tabs.js")
	if err != nil {
		t.Fatal(err)
	}
	palette, err := fs.ReadFile(dashboardAssetsFS, "js/palette.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatal(err)
	}

	for name, src := range map[string]string{
		"Groups tab":      string(tabs),
		"command palette": string(palette),
	} {
		if !strings.Contains(src, "scribeGroupVisible(g, showOfflineScribes)") {
			t.Errorf("%s does not apply the shared live-scribe visibility policy", name)
		}
	}
	if !strings.Contains(string(html), "<span>show offline scribes</span>") {
		t.Error("scribe visibility checkbox does not describe its offline-only effect")
	}
}
