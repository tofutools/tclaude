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

// TestDashboardAssets_ForceFoldWired guards the 🎯 hide/show toggle for the
// force info card. Like the block itself its pieces span render.js,
// row-actions.js and dashboard.css and must stay in lockstep — a rename in one
// file silently breaks the fold only in the browser:
//   - render.js reads the fold dashPref (isForceFolded), gates renderForceBlock
//     on it, and emits the toggle button in the group action row
//     (forceFoldToggleHTML, data-act="toggle-force-fold");
//   - row-actions.js handles the toggle (flip the dashPref, re-render);
//   - dashboard.css skins the toggle's folded accent + its wizard label swap.
//
// The default-open contract is load-bearing: a freshly deployed force must show
// its card, so the pref is ABSENT by default and only a stored '1' folds it.
func TestDashboardAssets_ForceFoldWired(t *testing.T) {
	for _, needle := range []string{
		// render.js — the fold-state read, the gate, and the toggle button.
		"function isForceFolded(",
		"function forceFoldToggleHTML(",
		"'tclaude.dash.forcefold.' + name",
		"if (isForceFolded(g.name)) return '';",
		`data-act="toggle-force-fold"`,
		"isDeployedForce(g) ? forceFoldToggleHTML(g) : ''",
		// row-actions.js — the toggle handler flipping the same pref.
		"case 'toggle-force-fold':",
		"'tclaude.dash.forcefold.' + group",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — force-fold wiring broken", needle)
		}
	}

	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	css := string(cssBytes)
	for _, needle := range []string{
		".force-fold-btn.folded {",
		"body.wizard .force-fold-label-regular",
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("dashboard.css missing %q — force-fold styling broken", needle)
		}
	}
}
