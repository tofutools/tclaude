package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardAutomaticGroupConfigAssets(t *testing.T) {
	for _, needle := range []string{
		"cfg-session-auto-join-group",
		"cfg-session-auto-join-or-create-group",
		"session.auto_join_group",
		"session.auto_join_or_create_group",
		"--auto-join-group[=false]",
		"--auto-join-or-create-group",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard config assets missing %q", needle)
		}
	}

	markup, err := fs.ReadFile(dashboardAssetsFS, "js/config-form-markup.js")
	if err != nil {
		t.Fatalf("reading config form markup: %v", err)
	}
	// The two startup-group settings sit together as their own switch rows
	// in one "Terminal startup groups" card.
	group := strings.Index(string(markup), `<${Group} title="Terminal startup groups">`)
	join := strings.Index(string(markup), `<${Toggle} id="cfg-session-auto-join-group" />`)
	create := strings.Index(string(markup), `<${Toggle} id="cfg-session-auto-join-or-create-group" />`)
	if group < 0 || join < group || create < join {
		t.Errorf("terminal startup group switches are not grouped in their card (group=%d join=%d create=%d)", group, join, create)
	}
}
