package agentd

import (
	"strings"
	"testing"
)

func TestDashboardAutomaticGroupConfigAssets(t *testing.T) {
	for _, needle := range []string{
		"cfg-session-auto-join-group",
		"cfg-session-auto-join-or-create-group",
		"cfg-checkbox-stack-field",
		"cfg-checkbox-stack",
		"session.auto_join_group",
		"session.auto_join_or_create_group",
		"--auto-join-group[=false]",
		"--auto-join-or-create-group",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard config assets missing %q", needle)
		}
	}
}
