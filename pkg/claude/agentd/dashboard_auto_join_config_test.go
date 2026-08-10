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
	styles, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading dashboard styles: %v", err)
	}
	for _, class := range []string{"cfg-checkbox-stack-field", "cfg-checkbox-stack"} {
		if !strings.Contains(string(markup), `class="cfg-field `+class+`"`) &&
			!strings.Contains(string(markup), `class="`+class+`"`) {
			t.Errorf("terminal startup group markup missing %q", class)
		}
		if !strings.Contains(string(styles), "."+class+" {") {
			t.Errorf("terminal startup group styles missing %q", class)
		}
	}
}
