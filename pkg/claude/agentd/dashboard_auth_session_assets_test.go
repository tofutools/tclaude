package agentd

import (
	"strings"
	"testing"
)

func TestDashboardAuthSessionWrapperLoadsBeforeApp(t *testing.T) {
	html := string(dashboardIndexHTML)
	wrapper := `<script src="/static/js/auth-session.js"></script>`
	app := `<script type="module" src="/static/js/dashboard.js"></script>`
	wrapperAt, appAt := strings.Index(html, wrapper), strings.Index(html, app)
	if wrapperAt < 0 {
		t.Fatal("dashboard must load the auth-session fetch wrapper")
	}
	if appAt < 0 || wrapperAt >= appAt {
		t.Fatal("auth-session wrapper must load before the dashboard module graph")
	}

	source := string(mustReadFS(dashboardAssetsFS, "js/auth-session.js"))
	for _, want := range []string{
		"X-Tclaude-Login-Required",
		"tclaude:auth-expired",
		"window.location.replace",
		"return_to=",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("auth-session wrapper missing %q", want)
		}
	}

	terminalsHTML := string(terminalsPageHTML)
	termApp := `<script type="module" src="/static/js/terminals.js"></script>`
	if wrapperAt, appAt := strings.Index(terminalsHTML, wrapper), strings.Index(terminalsHTML, termApp); wrapperAt < 0 || appAt < 0 || wrapperAt >= appAt {
		t.Fatal("standalone terminals must load auth-session before its module graph")
	}
	termCore := string(mustReadFS(dashboardAssetsFS, "js/terminals-core.js"))
	for _, want := range []string{"/api/auth/session", "tclaude:auth-expired"} {
		if !strings.Contains(termCore, want) {
			t.Fatalf("terminal transport missing auth recovery wiring %q", want)
		}
	}
	termEntry := string(mustReadFS(dashboardAssetsFS, "js/terminals.js"))
	for _, want := range []string{
		"if (location.hash) history.replaceState",
		"event.detail.returnTo",
		"#open=",
	} {
		if !strings.Contains(termEntry, want) {
			t.Fatalf("solo terminal popout auth recovery missing %q", want)
		}
	}
}
