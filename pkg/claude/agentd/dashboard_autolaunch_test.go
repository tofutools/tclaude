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

	autoLaunchDashboard("")

	const prefix = base + "/?init_token="
	if !strings.HasPrefix(opened, prefix) {
		t.Fatalf("opened URL %q does not start with %q", opened, prefix)
	}
	tok := strings.TrimPrefix(opened, prefix)
	if tok == "" {
		t.Fatalf("opened URL carries an empty init token: %q", opened)
	}
	if strings.Contains(opened, "slop") {
		t.Fatalf("opened URL %q tagged as slop without --slop", opened)
	}
	if !consumeInitToken(tok, initScopeDashboard) {
		t.Fatalf("minted init token %q was not valid", tok)
	}
	if consumeInitToken(tok, initScopeDashboard) {
		t.Fatalf("init token %q accepted twice — must be single-use", tok)
	}
}

// TestAutoLaunchDashboard_SlopMode verifies --slop tags the auto-launched
// URL with &slop=1 — the cosmetic theme switch the dashboard JS reads. The
// init token must still be valid: slop is layered on top of the regular
// auth flow, not a bypass.
func TestAutoLaunchDashboard_SlopMode(t *testing.T) {
	const base = "http://127.0.0.1:54321"
	prevBase := popupBaseURL
	popupBaseURL = base
	t.Cleanup(func() { popupBaseURL = prevBase })

	var opened string
	prevOpener := dashboardBrowserOpener
	dashboardBrowserOpener = func(u string) error { opened = u; return nil }
	t.Cleanup(func() { dashboardBrowserOpener = prevOpener })

	autoLaunchDashboard("slop")

	if !strings.HasSuffix(opened, "&slop=1") {
		t.Fatalf("opened URL %q missing &slop=1 suffix", opened)
	}
	const prefix = base + "/?init_token="
	if !strings.HasPrefix(opened, prefix) {
		t.Fatalf("opened URL %q does not start with %q", opened, prefix)
	}
	tok := strings.TrimSuffix(strings.TrimPrefix(opened, prefix), "&slop=1")
	if !consumeInitToken(tok, initScopeDashboard) {
		t.Fatalf("slop-tagged URL carried an invalid init token: %q", tok)
	}
}

// TestAutoLaunchDashboard_WizardMode is the wizard twin of _SlopMode:
// theme "wizard" tags the auto-launched URL with &wizard=1. The init token
// must still be valid — the theme rides on top of the regular auth flow.
func TestAutoLaunchDashboard_WizardMode(t *testing.T) {
	const base = "http://127.0.0.1:54321"
	prevBase := popupBaseURL
	popupBaseURL = base
	t.Cleanup(func() { popupBaseURL = prevBase })

	var opened string
	prevOpener := dashboardBrowserOpener
	dashboardBrowserOpener = func(u string) error { opened = u; return nil }
	t.Cleanup(func() { dashboardBrowserOpener = prevOpener })

	autoLaunchDashboard("wizard")

	if !strings.HasSuffix(opened, "&wizard=1") {
		t.Fatalf("opened URL %q missing &wizard=1 suffix", opened)
	}
	if strings.Contains(opened, "slop") {
		t.Fatalf("wizard launch %q must not carry the slop param", opened)
	}
	const prefix = base + "/?init_token="
	if !strings.HasPrefix(opened, prefix) {
		t.Fatalf("opened URL %q does not start with %q", opened, prefix)
	}
	tok := strings.TrimSuffix(strings.TrimPrefix(opened, prefix), "&wizard=1")
	if !consumeInitToken(tok, initScopeDashboard) {
		t.Fatalf("wizard-tagged URL carried an invalid init token: %q", tok)
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

	autoLaunchDashboard("")

	if called {
		t.Fatal("autoLaunchDashboard opened a browser with no loopback URL bound")
	}
}
