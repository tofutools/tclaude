package agentd

import (
	"strings"
	"testing"
)

// The dashboard's retire surfaces must turn a dangling agent entry — one
// whose conversation data is gone, which the daemon answers with HTTP 409
// + {dangling:true} — into a "remove the dangling entry?" confirm instead
// of a dead-end error toast. The wiring spans three embedded JS modules:
// the shared handler (refresh.js) and the two retire call sites
// (row-actions.js's retire-agent case, dnd.js's runDndRetire). The repo
// has no JS test runner, so — like the sibling dashboard_*_test.go
// structural guards — this pins the shape so a refactor can't silently
// drop the cleanup path. The end-to-end behaviour (409 signal → DELETE
// purge) has its own backend flow tests (retire_dangling_flow_test.go).
func TestDashboardHTML_DanglingRetireWired(t *testing.T) {
	src := dashboardAssets

	// 1) The shared handler exists and does the three things that unstick
	//    a dangling entry: recognise the 409 dangling signal, confirm with
	//    the human, and DELETE the orphan via the agent delete endpoint.
	start := strings.Index(src, "async function maybeHandleDanglingRetire(")
	if start < 0 {
		t.Fatal("refresh.js: `async function maybeHandleDanglingRetire(` not found")
	}
	// Bound at the function's own column-0 closing brace — every nested
	// brace in a native ES module sits indented, so the first "\n}\n"
	// after the signature is this function's end.
	body, _, found := strings.Cut(src[start:], "\n}\n")
	if !found {
		t.Fatal("refresh.js: could not bound maybeHandleDanglingRetire")
	}
	for _, needle := range []string{
		"status !== 409",   // recognises the dangling HTTP signal
		".dangling",        // gated on the dangling flag, not any 409
		"confirmModal(",    // asks the human before deleting
		"method: 'DELETE'", // purges the orphan
		"/api/agents/",     // …via the agent delete endpoint
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("maybeHandleDanglingRetire: missing %q", needle)
		}
	}

	// 2) The per-row retire-agent dispatcher delegates to the shared
	//    retireAgentInteractive flow, which routes a FAILED retire through
	//    the handler before surfacing its own error toast.
	caseIdx := strings.Index(src, "case 'retire-agent': {")
	if caseIdx < 0 {
		t.Fatal("row-actions.js: `case 'retire-agent': {` not found")
	}
	caseBody := src[caseIdx:]
	if next := strings.Index(caseBody[len("case 'retire-agent': {"):], "\n        case "); next >= 0 {
		caseBody = caseBody[:len("case 'retire-agent': {")+next]
	}
	if !strings.Contains(caseBody, "retireAgentInteractive(agent, label)") {
		t.Error("retire-agent case: must delegate to the shared retireAgentInteractive flow")
	}
	// retireAgentInteractive itself must route a failed retire through the
	// dangling handler (it owns the confirm → POST → recovery path now).
	raiStart := strings.Index(src, "async function retireAgentInteractive(")
	if raiStart < 0 {
		t.Fatal("refresh.js: `async function retireAgentInteractive(` not found")
	}
	raiBody, _, raiFound := strings.Cut(src[raiStart:], "\n}\n")
	if !raiFound {
		t.Fatal("refresh.js: could not bound retireAgentInteractive")
	}
	if !strings.Contains(raiBody, "maybeHandleDanglingRetire(") {
		t.Error("retireAgentInteractive: a failed retire must route through maybeHandleDanglingRetire")
	}

	// 3) The drag-onto-Retired gesture (runDndRetire) does the same.
	if !strings.Contains(dndFuncBody(t, "runDndRetire"), "maybeHandleDanglingRetire(") {
		t.Error("runDndRetire: a failed retire must route through maybeHandleDanglingRetire")
	}
}
