package agentd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The upgrade-reload chain: the daemon fingerprints its embedded frontend,
// stamps the served HTML with it, and repeats it on every snapshot; the page
// reloads when the two diverge. These tests pin each link — the fingerprint's
// shape, the HTML stamp, and the poll-side wiring — so a refactor cannot
// silently drop one and leave upgraded daemons serving to stale pages.

func TestDashboardAssetsVersion_ShapeAndDeterminism(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(dashboardAssetsVersion) {
		t.Fatalf("dashboardAssetsVersion = %q, want 16 lowercase hex chars", dashboardAssetsVersion)
	}
	if again := computeDashboardAssetsVersion(); again != dashboardAssetsVersion {
		t.Errorf("fingerprint not deterministic: %q then %q", dashboardAssetsVersion, again)
	}
}

func TestDashboardIndexHTML_StampedWithAssetsVersion(t *testing.T) {
	html := string(dashboardIndexHTML)
	meta := fmt.Sprintf(`<meta name="tclaude-assets-version" content="%s">`, dashboardAssetsVersion)
	if !strings.Contains(html, meta) {
		t.Errorf("served dashboard HTML missing the assets-version meta stamp %q", meta)
	}
	if strings.Contains(html, assetsVersionMarker) {
		t.Errorf("served dashboard HTML still contains the raw %s marker", assetsVersionMarker)
	}
}

// The poll must consult the watcher BEFORE any renderer touches the snapshot:
// modules from the old build must not paint data shaped by the new one.
func TestDashboardRefresh_ChecksAssetsVersion(t *testing.T) {
	refresh := dashboardAssetFile(t, "js/refresh.js")
	for needle, why := range map[string]string{
		"import { checkAssetsVersion } from './assets-version.js';": "refresh.js must import the watcher",
		"if (checkAssetsVersion(data.assets_version)) {":            "every poll must consult the fingerprint",
	} {
		if !strings.Contains(refresh, needle) {
			t.Errorf("refresh.js missing %q (%s)", needle, why)
		}
	}
	idx := strings.Index(refresh, "checkAssetsVersion(data.assets_version)")
	render := strings.Index(refresh, "setLastSnapshot(data)")
	if idx < 0 || render < 0 || idx > render {
		t.Errorf("checkAssetsVersion must run before the snapshot is published/rendered (check=%d publish=%d)", idx, render)
	}
}
