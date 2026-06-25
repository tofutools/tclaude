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
// <summary> + the top-bar global slot (render.js), the bot/animation CSS
// (dashboard.css) and the #global-activity mount (dashboard.html). A
// rename in any one silently breaks the feature in the browser — there's
// no JS render test, so we assert on the embedded concatenation at
// `go test ./...`. (The aggregation LOGIC is covered separately by the
// Node suite jstest/group-activity.test.mjs.)
func TestDashboardAssets_GroupActivityWired(t *testing.T) {
	needles := []string{
		// group-activity.js — pure module surface (emoji + sprite + dispatch).
		"export function activitySummary(",
		"export function groupActivityHTML(",
		"export function memberVariant(",
		"export function spriteBotsHTML(",
		"export function styledBotsHTML(",
		"export function aggregateActivity(",
		// render.js — wired into the group summary and the global slot.
		"groupActivityChip(members)",     // dropped into <summary>
		"function activityStyles(",       // reads the per-mode styles
		"function renderGlobalActivity(", // top-bar renderer
		"renderGlobalActivity,",          // exported from render.js
		// refresh.js — called every poll.
		"renderGlobalActivity()",
		// dashboard.html — the global mount point.
		`id="global-activity"`,
		// dashboard.css — emoji bots, sprite bots, and the per-mode swap.
		".ga-regular",
		".ga-slop",
		".actbot-working",
		"@keyframes actbot-dance",
		".actbot-sprite",
		"@keyframes spr-dance",
		"url(sprites/dance_0.png)", // sprite frames referenced
		// Config per-mode dropdowns + snapshot flag.
		`id="cfg-dashboard-activity-bots-regular"`,
		`id="cfg-dashboard-activity-bots-slop"`,
		"lastSnapshot.activity_bots", // render reads the snapshot styles
		"activity_bots",              // assemble/populate in config.js
	}
	for _, needle := range needles {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — group activity indicator wiring broken", needle)
		}
	}
}

// TestDashboardAssets_SpriteFramesEmbedded guards that every pixellab
// frame the sprite-bot CSS references is actually embedded under
// dashboard/sprites — a missing PNG would otherwise 404 only in the
// browser (an invisible bot), never at `go test`. Frame counts match the
// @keyframes blocks in dashboard.css (dance/asking/error = 9, idle = 4)
// plus the shared static frame.
func TestDashboardAssets_SpriteFramesEmbedded(t *testing.T) {
	want := []string{"sprites/bot.png"}
	for _, a := range []struct {
		name string
		n    int
	}{{"dance", 9}, {"asking", 9}, {"error", 9}, {"idle", 4}} {
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
