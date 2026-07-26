package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_TemporarySandboxRestartWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	must("function SandboxRestartMenuItem({ member })",
		"the agent cog owns a reversible sandbox restart item")
	must(`act="sandbox-restart"`, "the menu item dispatches the sandbox restart action")
	must(`'data-action': unlocked ? 'restore' : 'unlock'`,
		"stable agent state selects restore versus unlock")
	must(`data-act=${actionable ? 'sandbox-restart' : null}`,
		"the live sandbox glyph is a direct restart shortcut")
	must(`role=${actionable ? 'button' : 'note'}`,
		"the clickable glyph exposes button semantics")
	must("case 'sandbox-restart':", "the delegated action handler routes the click")
	must("/sandbox-restart`", "the browser posts to the dedicated agent endpoint")
	must("no background agents or shell commands",
		"the confirmation explains the authoritative idle gate")
	must("event.key !== 'Enter' || (!event.ctrlKey && !event.metaKey)",
		"the confirmation accepts Ctrl/Cmd+Enter")
	must("body.wizard #confirm-modal .modal {",
		"the shared confirmation receives the wizard-mode skin")
}
