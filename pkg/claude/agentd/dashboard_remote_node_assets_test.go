package agentd

import (
	"strings"
	"testing"
)

// The peer-view fetch router must wrap fetch before any dashboard module
// captures it, and after auth-session.js so an expired session on a peer view
// still reaches the sign-in redirect.
func TestDashboardRemoteNodeRouterLoadsBeforeApp(t *testing.T) {
	html := string(dashboardIndexHTML)
	auth := strings.Index(html, `<script src="/static/js/auth-session.js"></script>`)
	router := strings.Index(html, `<script src="/static/js/remote-node.js"></script>`)
	app := strings.Index(html, `<script type="module" src="/static/js/dashboard.js"></script>`)
	if router < 0 {
		t.Fatal("dashboard must load the remote-node fetch router")
	}
	if auth < 0 || auth > router || app < 0 || router > app {
		t.Fatalf("want auth-session < remote-node < module graph, got %d, %d, %d", auth, router, app)
	}
	if len(mustReadFS(dashboardAssetsFS, "js/remote-node.js")) == 0 {
		t.Fatal("remote-node.js missing from the embedded assets")
	}
}
