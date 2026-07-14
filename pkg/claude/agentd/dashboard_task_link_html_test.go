package agentd

import (
	"strings"
	"testing"
)

func TestDashboardTaskLinkEditorAndWizardSkinWired(t *testing.T) {
	for _, needle := range []string{
		`id="task-link-modal"`,
		`id="task-link-url"`,
		`id="task-link-label"`,
		`Bind a quest link`,
		`✒ Bind quest!`,
		`body.wizard #task-link-modal .modal`,
		`body.wizard .task-edit-icon::before`,
		`content: "✒"`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard task-link editor missing %q", needle)
		}
	}
}
