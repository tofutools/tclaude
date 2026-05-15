package agentd

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TestShouldAutoLaunchDashboard pins the OR between the
// --auto-launch-dashboard serve flag and the persistent
// agent.auto_launch_dashboard config field — either one opts in.
func TestShouldAutoLaunchDashboard(t *testing.T) {
	cases := []struct {
		name    string
		flagSet bool
		cfg     *config.Config
		want    bool
	}{
		{"flag on, no config", true, nil, true},
		{"flag on, config off", true, &config.Config{Agent: &config.AgentConfig{}}, true},
		{"flag off, config on", false,
			&config.Config{Agent: &config.AgentConfig{AutoLaunchDashboard: true}}, true},
		{"flag off, config off", false,
			&config.Config{Agent: &config.AgentConfig{AutoLaunchDashboard: false}}, false},
		{"flag off, nil agent", false, &config.Config{}, false},
		{"flag off, nil config", false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAutoLaunchDashboard(tc.flagSet, tc.cfg); got != tc.want {
				t.Fatalf("shouldAutoLaunchDashboard(%v, %+v) = %v, want %v",
					tc.flagSet, tc.cfg, got, tc.want)
			}
		})
	}
}

// TestAutoLaunchDashboard_OpensSingleUseTokenURL verifies the startup
// launch mints a real init token and points the browser at it — the
// same token-exchange URL `tclaude agent dashboard` produces, so the
// browser's GET / can swap it for the session cookie. The token must
// be valid exactly once (single-use).
func TestAutoLaunchDashboard_OpensSingleUseTokenURL(t *testing.T) {
	const base = "http://127.0.0.1:54321"
	prevBase := popupBaseURL
	popupBaseURL = base
	t.Cleanup(func() { popupBaseURL = prevBase })

	var opened string
	prevOpener := dashboardBrowserOpener
	dashboardBrowserOpener = func(u string) error { opened = u; return nil }
	t.Cleanup(func() { dashboardBrowserOpener = prevOpener })

	autoLaunchDashboard()

	const prefix = base + "/?init_token="
	if !strings.HasPrefix(opened, prefix) {
		t.Fatalf("opened URL %q does not start with %q", opened, prefix)
	}
	tok := strings.TrimPrefix(opened, prefix)
	if tok == "" {
		t.Fatalf("opened URL carries an empty init token: %q", opened)
	}
	if !consumeDashboardInitToken(tok) {
		t.Fatalf("minted init token %q was not valid", tok)
	}
	if consumeDashboardInitToken(tok) {
		t.Fatalf("init token %q accepted twice — must be single-use", tok)
	}
}

// TestAutoLaunchDashboard_NoLoopbackURL covers the headless / failed-
// bind path: with no popup listener there is no dashboard to open, so
// the browser must not be launched.
func TestAutoLaunchDashboard_NoLoopbackURL(t *testing.T) {
	prevBase := popupBaseURL
	popupBaseURL = ""
	t.Cleanup(func() { popupBaseURL = prevBase })

	called := false
	prevOpener := dashboardBrowserOpener
	dashboardBrowserOpener = func(string) error { called = true; return nil }
	t.Cleanup(func() { dashboardBrowserOpener = prevOpener })

	autoLaunchDashboard()

	if called {
		t.Fatal("autoLaunchDashboard opened a browser with no loopback URL bound")
	}
}
