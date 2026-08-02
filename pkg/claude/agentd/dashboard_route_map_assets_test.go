package agentd

import (
	"strings"
	"testing"
)

func TestDashboardRouteMapAssetsAreReadOnlyAndResponsive(t *testing.T) {
	js := dashboardAssetFile(t, "js/groups-route-map.js")
	for _, needle := range []string{
		"Graph", "Exact list", "Route detail", "route_map", "darwin_boundary", "darwin_capacity", "in use across",
		"tclaude:navigated", "tclaude:restore-location", "Stable reference",
		"Endpoint addresses, targets, capabilities, credentials, and payload data are never shown",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("route map JS missing %q", needle)
		}
	}
	for _, forbidden := range []string{"/api/routes", "fetch(", "route.target", "launch_generation", "endpoint_error"} {
		if strings.Contains(js, forbidden) {
			t.Errorf("route map JS must not contain %q", forbidden)
		}
	}
	css := dashboardAssetFile(t, "dashboard.css")
	for _, needle := range []string{".route-map-content", "@media (max-width: 800px)", "@media (max-width: 560px)", ".route-map-edge.is-focused"} {
		if !strings.Contains(css, needle) {
			t.Errorf("route map CSS missing %q", needle)
		}
	}
	html := dashboardAssetFile(t, "dashboard.html")
	if !strings.Contains(html, `id="groups-route-map-root"`) {
		t.Error("dashboard HTML missing route map island host")
	}
}
