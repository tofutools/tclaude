package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardAssets_ForceBlockWired guards the deployed-task-force block
// (JOH-247), whose pieces span groups-list.js, row-actions.js and dashboard.css and
// must stay in lockstep — there's no JS render test, so we assert on the
// embedded concatenation at `go test ./...`. A rename in any one file silently
// breaks the force view (or its re-brief control) only in the browser:
//   - groups-list.js builds ForceBlock and wires it into the group
//     subtable, classifies member liveness, and emits the re-brief + stand-down
//     buttons;
//   - row-actions.js handles the re-brief and stand-down actions (confirm →
//     POST /api rebrief / stand-down);
//   - dashboard.css skins the block, the per-liveness member pills, the stalling
//     hint and the control buttons (new inline buttons render white unskinned).
func TestDashboardAssets_ForceBlockWired(t *testing.T) {
	for _, needle := range []string{
		// groups-list.js — the native block, force-detection gate, roles
		// rollup, stalling predicate, liveness mapping, and subtable wiring.
		"function ForceBlock(",
		"function isForce(",
		"const roles = new Map();",
		"live.length > 0 && live.every((member) => member.state?.status === 'idle')",
		"const liveness = !member.online ? 'dead' : member.state?.status === 'idle' ? 'idle' : 'working';",
		"<${ForceBlock} group=${group} />",
		// groups-list.js + row-actions.js — the re-brief control + its handler.
		`data-act="rebrief-force"`,
		"case 'rebrief-force':",
		"`/api/groups/${encodeURIComponent(group)}/rebrief`",
		// groups-list.js + row-actions.js — the stand-down control + its handler (JOH-345).
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
// force info card. Like the block itself its pieces span groups-list.js,
// row-actions.js and dashboard.css and must stay in lockstep — a rename in one
// file silently breaks the fold only in the browser:
//   - groups-list.js reads the fold dashPref, gates ForceBlock
//     on it, and emits the toggle button in the group action row
//     (forceFoldToggleHTML, data-act="toggle-force-fold");
//   - row-actions.js handles the toggle (flip the dashPref, re-render);
//   - dashboard.css skins the toggle's folded accent + its wizard label swap.
//
// The default-open contract is load-bearing: a freshly deployed force must show
// its card, so the pref is ABSENT by default and only a stored '1' folds it.
func TestDashboardAssets_ForceFoldWired(t *testing.T) {
	for _, needle := range []string{
		// groups-list.js — the fold-state read, gate, and native toggle button.
		"function GroupActions(",
		"function ForceBlock(",
		"`tclaude.dash.forcefold.${group.name}`",
		"if (!isForce(group) || dashPrefs.getItem(`tclaude.dash.forcefold.${group.name}`) === '1') return null;",
		`data-act="toggle-force-fold"`,
		"isForce(group) ? html`<button class=${`force-fold-btn${folded ? ' folded' : ''}`}",
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
