package agentd

import (
	"strings"
	"testing"
)

// Retirement may be initiated outside the current dashboard, so the terminal
// cleanup must hang off the authoritative active-roster transition rather than
// only the per-row retire button. The selector diff itself is covered by the
// Node suite in jstest/terminals-core.test.mjs; these guards pin the browser
// wiring and the mux boundary.
func TestRetiredAgentClosesAllOwnedTerminalPanes(t *testing.T) {
	core := readDashboardJS(t, "terminals-core.js")
	for _, needle := range []string{
		"export function departedAgentSelectors(",
		"function closeForAgents(",
		"const agent = p.seed && p.seed.agent",
		"closeForAgents,",
	} {
		if !strings.Contains(core, needle) {
			t.Errorf("terminals-core.js missing %q — retired-agent pane cleanup is broken", needle)
		}
	}

	tab := readDashboardJS(t, "terminals-tab.js")
	if !strings.Contains(tab, "export function reconcileTerminalsForAgentRoster(") ||
		!strings.Contains(tab, "mux.closeForAgents(departed)") {
		t.Error("terminals-tab.js must reconcile active-roster departures through closeForAgents")
	}

	refresh := readDashboardJS(t, "refresh.js")
	if !strings.Contains(refresh, "reconcileTerminalsForAgentRoster(prevSnap.agents, data.agents)") {
		t.Error("refresh.js must reconcile terminal panes against every accepted active-agent snapshot")
	}
}
