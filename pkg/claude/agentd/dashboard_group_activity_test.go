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
		// render.js — wired into the group summary and the global slot.
		"groupActivityChip(members)",     // dropped into <summary>
		"function activityStyles(",       // reads the per-mode styles
		"function renderGlobalActivity(", // top-bar renderer
		"renderGlobalActivity,",          // exported from render.js
		// refresh.js — called every poll.
		"renderGlobalActivity()",
		// dashboard.html — the global mount point.
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
}

// TestDashboardAssets_BotRephaseScoped guards the skip-if-unchanged copy-paste
// fix against a subtle animation regression: with the 2s poll now skipping an
// unchanged section's innerHTML swap, the group-header/global-bar activity bots
// keep animating continuously across ticks. Re-phasing a bot whose node was NOT
// just recreated shifts its still-correct phase and INTRODUCES a 2s jump — the
// exact artifact the re-phase exists to prevent. So every syncBotAnimations /
// syncWizardOrbit call MUST be scoped to the element it just rebuilt
// (renderGlobalActivity → #global-activity, renderGroupsTab → #groups-list),
// never document-wide. Pinning the scoped call strings catches a revert to the
// bare document-wide form directly: swapping syncBotAnimations(el) back to
// syncBotAnimations() drops the pinned needle and fails this test.
func TestDashboardAssets_BotRephaseScoped(t *testing.T) {
	for _, needle := range []string{
		"syncBotAnimations(el)",       // render.js renderGlobalActivity, scoped to #global-activity
		"syncBotAnimations(groupsEl)", // tabs.js renderGroupsTab, scoped to #groups-list
		"syncWizardOrbit(groupsEl)",   // tabs.js renderGroupsTab, scoped to #groups-list
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — bot re-phase must be scoped to the "+
				"rebuilt subtree, or skip-if-unchanged ticks jump the group bots", needle)
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
