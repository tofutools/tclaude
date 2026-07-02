package agentd

import (
	"strings"
	"testing"
)

// The command palette's global "Retire ungrouped agents…" command is the
// cross-group cleanup twin of the per-group "Retire idle/offline agents
// in <group>" sweep. Ungrouped agents belong to no group, so there is no
// group retire route to reach them; instead the command opens the SAME
// preview modal (#retire-preview-modal, reused) and POSTs the human's
// explicit ticked list to the group-agnostic bulk cleanup endpoint
// (/api/cleanup/agents {mode:"retire"}) with include_online set.
//
// The repo has no JS test runner, so — like the sibling
// dashboard_retire_preview_test.go — this pins the shape of that wiring
// across the embedded HTML + JS so a refactor can't silently drop the
// preview, the opt-out, or the explicit-list POST that makes "what was
// previewed" == "what is retired". The backend retire+worktree path has
// its own flow tests (cleanup_flow_test.go: TestCleanup_Agents_Retire*).
func TestDashboardHTML_RetireUngroupedWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// 1. The palette offers the command, gated on a live count so it is
	//    never a no-op, with both the plain label and the 🧙 wizard synonym
	//    ("Banish unbound familiars…"). The plain "retire"/wizard "banish"
	//    words already bridge via the scorer's SYNONYMS map.
	must("const ungroupedCount = countUngroupedAgents();",
		"the command is gated on the live ungrouped count")
	must("if (ungroupedCount) {",
		"the command is only listed when there is at least one ungrouped agent")
	must("wiz('Retire ungrouped agents…', 'Banish unbound familiars…')",
		"the palette offers the ungrouped retire (plain + arcane Banish label)")
	must("run: () => openRetireUngroupedPreview(),",
		"the command opens the ungrouped retire preview modal")

	// 2. The driver + its cohort helpers are defined and exported, and the
	//    palette imports them.
	must("function openRetireUngroupedPreview(", "refresh.js defines the ungrouped preview driver")
	must("openRetireUngroupedPreview,", "refresh.js exports the driver")
	must("function ungroupedRetireCandidates(", "refresh.js defines the cohort builder")
	must("function countUngroupedAgents(", "refresh.js defines the gate count")
	must("countUngroupedAgents,", "refresh.js exports the gate count")
	must("openRetirePreview, openRetireUngroupedPreview, openDeleteRetiredPreview",
		"the palette imports the driver from refresh.js")

	// 3. The candidate list is built from the snapshot's ungrouped[] — every
	//    agent in no group, online and offline alike — all ticked by default.
	must("for (const a of (snap.ungrouped || [])) {",
		"the cohort is the snapshot's ungrouped agents")
	must("ungroupedRetireCandidates().map(c => ({ ...c, checked: true }))",
		"the preview seeds its candidates from the ungrouped cohort, all ticked")

	// 4. THE load-bearing property: submit posts the EXPLICIT ticked list to
	//    the bulk cleanup endpoint in retire mode, with include_online so a
	//    busy ungrouped agent the human left ticked is retired (not silently
	//    skipped by the endpoint's default skip-online guard) — never a
	//    filter the server re-resolves.
	disp := dashboardAssets
	start := strings.Index(disp, "function openRetireUngroupedPreview(")
	if start < 0 {
		t.Fatal("refresh.js: function openRetireUngroupedPreview( not found")
	}
	fnBody, _, found := strings.Cut(disp[start:], "\n}\n")
	if !found {
		t.Fatal("refresh.js: could not bound openRetireUngroupedPreview")
	}
	for _, needle := range []string{
		"const agents = candidates.filter(c => c.checked).map(c => c.agent_id || c.conv_id);",                                                     // the ticked list (agent_id, conv_id fallback)
		"JSON.stringify({ agents, mode: 'retire', include_online: true, shutdown: shutdownCb.checked, delete_worktrees: deleteWorktrees })", // posted verbatim
		"'/api/cleanup/agents'", // to the group-agnostic bulk cleanup route
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("openRetireUngroupedPreview: missing %q — the explicit-list retire POST is the whole point", needle)
		}
	}

	// 5. Busy feedback + the worktree opt-in, coupled to shutdown, exactly
	//    like the per-group retire preview — a box disabled by an unticked
	//    shutdown never sends delete_worktrees.
	for _, needle := range []string{
		`submitBtn.setAttribute('aria-busy', 'true')`,           // busy flag on click
		`class="btn-spinner"`,                                    // in-button spinner
		"wtCb.checked = true; // worktree delete defaults ON",    // default ON on open
		"const syncWtCoupling = () => {",                         // shutdown→worktree coupling
		"const deleteWorktrees = wtCb.checked && !wtCb.disabled;", // disabled box never opts in
		"shutdownCb.addEventListener('change', syncWtCoupling)",  // coupling is wired live
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("openRetireUngroupedPreview: missing %q — the preview must give in-flight feedback and couple the worktree opt-in to shutdown", needle)
		}
	}
}
