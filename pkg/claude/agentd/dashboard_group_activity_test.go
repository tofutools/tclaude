package agentd

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardAssets_GroupActivityWired guards the group activity-bot
// indicator, whose pieces live in four files that must stay in lockstep:
// the pure aggregation module (group-activity.js), its use in the group
// <summary> (render.js) + the Preact-owned top-bar global slot
// (shell-model.js/shell-island.js), the bot/animation CSS (dashboard.css),
// and the explicit shell host (dashboard.html). A
// rename in any one silently breaks the feature in the browser — there's
// no JS render test, so we assert on the embedded concatenation at
// `go test ./...`. (The aggregation LOGIC is covered separately by the
// Node suite jstest/group-activity.test.mjs.)
func TestDashboardAssets_GroupActivityWired(t *testing.T) {
	needles := []string{
		// group-activity.js — pure module surface (emoji + sprite + wizard + dispatch).
		"export function activitySummary(",
		"export function groupActivityHTML(",
		"export function memberVariant(",
		"export function spriteBotsHTML(",
		"export function wizardBotsHTML(",
		"export function wizardSpriteBotsHTML(", // wizard pixel-sprite opt-in row
		"export function styledWizardBotsHTML(", // wizard glyph/sprite/off switchboard
		"export function styledBotsHTML(",
		"export function aggregateActivity(",
		// render.js — still wired into each legacy group summary.
		"groupActivityChip(members)", // dropped into <summary>
		"function activityStyles(",   // reads the per-mode styles
		// shell-model.js derives the global view from the accepted snapshot.
		"export function globalActivityView(snapshot, wizard = false)",
		"const groups = snapshot.groups || []",
		"const styles = snapshot.activity_bots || {}",
		// shell-island.js subscribes to the Signal and gives the activity VNodes
		// stable Preact ownership under an explicit HTML host.
		"const snapshot = state.snapshot.value",
		"const view = globalActivityView(snapshot, wizard)",
		"trustedHTMLToVNodes(view.markup)",
		`id="shell-activity-root"`,
		`id="global-activity"`,
		// dashboard.css — emoji bots, sprite bots, wizard bots, and the swaps.
		".ga-regular",
		".ga-slop",
		".ga-wizard",
		"body.wizard .ga-wizard", // wizard theme shows its own bot row
		".actbot-working",
		"@keyframes actbot-dance",
		".actbot-sprite",
		"@keyframes spr-dance",
		"url(sprites/dance_0.png)", // sprite frames referenced
		// Wizard pixel-sprite opt-in: the aspect marker, keyframes + a frame.
		".actbot-sprite.actbot-wiz.actbot-working",
		"@keyframes spr-wiz-cast",
		"url(sprites/wiz_bot.png)",
		// Config per-mode dropdowns + snapshot flag.
		`id="cfg-dashboard-activity-bots-regular"`,
		`id="cfg-dashboard-activity-bots-slop"`,
		`id="cfg-dashboard-activity-bots-wizard"`,
		"lastSnapshot.activity_bots", // render reads the snapshot styles
		"activity_bots",              // assemble/populate in config.js
	}
	for _, needle := range needles {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — group activity indicator wiring broken", needle)
		}
	}
	for _, retired := range []string{"function renderGlobalActivity(", "renderGlobalActivity()"} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("dashboard assets still carry retired imperative global activity painter %q", retired)
		}
	}
}

// TestDashboardAssets_SpriteFramesEmbedded guards that every pixellab
// frame the sprite-bot CSS references is actually embedded under
// dashboard/sprites — a missing PNG would otherwise 404 only in the
// browser (an invisible bot), never at `go test`. Frame counts match the
// @keyframes blocks in dashboard.css: the slop robots (dance/asking/error =
// 9, idle = 4) and the wizard sprites (wiz_cast/ask/error = 9, wiz_idle = 7),
// plus each set's shared static frame.
func TestDashboardAssets_SpriteFramesEmbedded(t *testing.T) {
	want := []string{"sprites/bot.png", "sprites/wiz_bot.png"}
	for _, a := range []struct {
		name string
		n    int
	}{{"dance", 9}, {"asking", 9}, {"error", 9}, {"idle", 4},
		{"wiz_cast", 9}, {"wiz_ask", 9}, {"wiz_error", 9}, {"wiz_idle", 7}} {
		for i := 0; i < a.n; i++ {
			want = append(want, fmt.Sprintf("sprites/%s_%d.png", a.name, i))
		}
	}
	for _, name := range want {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("sprite frame %q not embedded: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("sprite frame %q is empty", name)
		}
		// Cheap PNG magic check so a truncated/renamed asset is caught.
		if len(data) >= 8 && string(data[1:4]) != "PNG" {
			t.Errorf("sprite frame %q is not a PNG", name)
		}
	}
}
