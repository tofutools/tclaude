package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardAssets_ForceBlockWired guards the deployed-task-force block
// (JOH-247), whose pieces span render.js, row-actions.js and dashboard.css and
// must stay in lockstep — there's no JS render test, so we assert on the
// embedded concatenation at `go test ./...`. A rename in any one file silently
// breaks the force view (or its re-brief control) only in the browser:
//   - render.js builds the block (renderForceBlock) and wires it into the group
//     subtable, classifies member liveness, and emits the re-brief + stand-down
//     buttons;
//   - row-actions.js handles the re-brief and stand-down actions (confirm →
//     POST /api rebrief / stand-down);
//   - dashboard.css skins the block, the per-liveness member pills, the stalling
//     hint and the control buttons (new inline buttons render white unskinned).
func TestDashboardAssets_ForceBlockWired(t *testing.T) {
	for _, needle := range []string{
		// render.js — the block, its force-detection gate, the roles rollup +
		// stalling helpers, and the subtable wiring.
		"function renderForceBlock(",
		"function isDeployedForce(",
		"function forceRolesRollup(",
		"function forceStalling(",
		"function forceMemberLiveness(",
		"${renderForceBlock(g, members)}",
		// render.js + row-actions.js — the re-brief control + its handler.
		`data-act="rebrief-force"`,
		"case 'rebrief-force':",
		"`/api/groups/${encodeURIComponent(group)}/rebrief`",
		// render.js + row-actions.js — the stand-down control + its handler (JOH-345).
		`data-act="stand-down-force"`,
		"case 'stand-down-force':",
		"`/api/groups/${encodeURIComponent(group)}/stand-down`",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — force-block wiring broken", needle)
		}
	}

	// dashboard.css — the block, the per-liveness member pills, the stalling
	// hint and the re-brief button skin.
	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	for _, needle := range []string{
		".group-force-block {",
		".force-member-working",
		".force-member-idle",
		".force-member-dead",
		".force-stalling {",
		".force-rebrief-btn, .force-standdown-btn {",
		".force-standdown-btn:hover",
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("dashboard.css missing %q — force-block styling broken", needle)
		}
	}
}
