package agentd

import (
	"strings"
	"testing"
)

// The keyed transaction adapter must preserve the daemon's 409 dangling
// signal through visual handoff, explicit confirmation, and raw-conv DELETE.
// Backend behavior remains covered end-to-end in retire_dangling_flow_test.go.
func TestDashboardHTML_DanglingRetireWired(t *testing.T) {
	for _, needle := range []string{
		"response.status === 409 && payload?.dangling",
		"convID: String(payload.conv_id || conv)",
		"state.handoff()",
		"title: 'Remove dangling agent entry?'",
		"method: 'DELETE', credentials: 'same-origin'",
		"state.finish(result)",
		"removed: true",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Preact dangling-retire ownership missing %q", needle)
		}
	}

	// Every single-retire entry point supplies its raw conv-keyed descriptor to
	// the shared controller; no launcher retains a private 409 path.
	for _, needle := range []string{
		"await openRetireAgentDialog(conv, label)",
		"openRetireAgentDialog(a.conv_id, label)",
		"result = await openRetireAgentDialog(conv, label)",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dangling-capable launcher missing %q", needle)
		}
	}
	for _, retired := range []string{
		"maybeHandleDanglingRetire",
		"retireAgentInteractive",
	} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("legacy dangling owner still embedded: %q", retired)
		}
	}

	// DnD waits for the final dangling result. Decline/failure reconciles the
	// drag presentation; successful deletion was already refreshed by actions.
	retire := dndFuncBody(t, "runDndRetire")
	for _, needle := range []string{
		"result = await openRetireAgentDialog(conv, label)",
		"if (retireResultNeedsReconcile(result)) await refresh()",
	} {
		if !strings.Contains(retire, needle) {
			t.Errorf("runDndRetire: missing %q", needle)
		}
	}
}
