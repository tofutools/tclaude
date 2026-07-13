package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardScribeVisibilityWiring(t *testing.T) {
	groupsState, err := fs.ReadFile(dashboardAssetsFS, "js/groups-state.js")
	if err != nil {
		t.Fatal(err)
	}
	palette, err := fs.ReadFile(dashboardAssetsFS, "js/palette.js")
	if err != nil {
		t.Fatal(err)
	}
	groupsIsland, err := fs.ReadFile(dashboardAssetsFS, "js/groups-island.js")
	if err != nil {
		t.Fatal(err)
	}

	for name, src := range map[string]string{
		"Groups tab":      string(groupsState),
		"command palette": string(palette),
	} {
		if !strings.Contains(src, "scribeGroupVisible(") {
			t.Errorf("%s does not apply the shared live-scribe visibility policy", name)
		}
	}
	if !strings.Contains(string(groupsIsland), "${option.label}") ||
		!strings.Contains(string(groupsState), "label: 'show offline scribes'") {
		t.Error("scribe visibility checkbox does not describe its offline-only effect")
	}
}
