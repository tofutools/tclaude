package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// The Config tab's labeled "Dashboard bind" field must stay wired end to end:
// the input exists, is populated from agent.dashboard_bind on load, and is
// written back on save. Guards the discoverable control the operator asked for
// (so it can't silently regress to raw-JSON-only).
func TestDashboardConfigTab_DashboardBindFieldWired(t *testing.T) {
	for _, needle := range []string{
		`id="cfg-agent-dashboardbind"`,                          // the labeled field (HTML)
		"$('#cfg-agent-dashboardbind').value = a.dashboard_bind", // populated on load (config.js)
		"a.dashboard_bind = dbRaw",                               // written on save (config.js)
	} {
		assert.Contains(t, dashboardAssets, needle, "Config-tab dashboard_bind field wiring broken")
	}
}

// resolveDashboardBind picks the host the dashboard listener binds to:
// flag > config > default("127.0.0.1"). A whitespace-only value at a tier is
// treated as unset and falls through.
func TestResolveDashboardBind(t *testing.T) {
	cfgWith := func(bind string) *config.Config {
		return &config.Config{Agent: &config.AgentConfig{DashboardBind: bind}}
	}
	cases := []struct {
		name     string
		flag     string
		cfg      *config.Config
		wantHost string
		wantSrc  string
	}{
		{"flag wins over config", "0.0.0.0", cfgWith("::"), "0.0.0.0", "flag"},
		{"config when no flag", "", cfgWith("192.168.1.5"), "192.168.1.5", "config"},
		{"default when neither set", "", cfgWith(""), "127.0.0.1", "default (loopback)"},
		{"default when cfg nil", "", nil, "127.0.0.1", "default (loopback)"},
		{"default when agent block nil", "", &config.Config{}, "127.0.0.1", "default (loopback)"},
		{"whitespace flag is unset", "   ", cfgWith("10.0.0.9"), "10.0.0.9", "config"},
		{"flag is trimmed", "  0.0.0.0 ", cfgWith(""), "0.0.0.0", "flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, src := resolveDashboardBind(tc.flag, tc.cfg)
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, tc.wantSrc, src)
		})
	}
}

// isLoopbackHost decides which same-origin model runs: loopback → strict
// popupBaseURL pin; non-loopback → host-relative. A non-IP hostname is treated
// conservatively as non-loopback.
func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", true},
		{"127.0.0.1", true},
		{"127.0.0.53", true},
		{"::1", true},
		{"0.0.0.0", false},
		{"::", false},
		{"192.168.1.5", false},
		{"10.0.0.9", false},
		{"localhost", true}, // the literal localhost is loopback (no false network warning)
		{"box.example", false}, // any other hostname → conservative non-loopback
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			assert.Equal(t, tc.want, isLoopbackHost(tc.host))
		})
	}
}

// dashboardURLHost maps a bind host to the locally-reachable URL host: a
// wildcard bind isn't dialable, so it becomes loopback; a specific host stays.
func TestDashboardURLHost(t *testing.T) {
	assert.Equal(t, "127.0.0.1", dashboardURLHost(""))
	assert.Equal(t, "127.0.0.1", dashboardURLHost("0.0.0.0"))
	assert.Equal(t, "127.0.0.1", dashboardURLHost("::"))
	assert.Equal(t, "192.168.1.5", dashboardURLHost("192.168.1.5"))
	assert.Equal(t, "127.0.0.1", dashboardURLHost("127.0.0.1"))
}

// originHostMatchesRequest is the host-relative CSRF check used off-loopback:
// the Origin/Referer host must equal the request Host, and a cross-host or
// unparseable value is rejected.
func TestOriginHostMatchesRequest(t *testing.T) {
	const reqHost = "box.example:8080"
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"exact origin", "http://box.example:8080", true},
		{"referer with path", "http://box.example:8080/dashboard", true},
		{"https origin same host", "https://box.example:8080", true},
		{"different host", "http://evil.example:8080", false},
		{"different port", "http://box.example:9999", false},
		{"empty value", "", false},
		{"garbage value", "::::not a url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, originHostMatchesRequest(tc.value, reqHost))
		})
	}
}

// The dashboard init-token exchange 303 must carry the deep-link focus
// (?tab=...&access_request=...) and the cosmetic theme across the bare-path
// bounce, while dropping the one-shot init_token. Without this the approval
// auto-raise / tray "review" lands on the default tab instead of the request.
func TestDashboardRoot_PreservesDeepLinkParamsAcrossTokenExchange(t *testing.T) {
	initDashboardToken()
	tok := mintInitToken(initScopeDashboard)
	req := httptest.NewRequest("GET", "/?init_token="+tok+"&tab=messages&access_request=abc123&wizard=1", nil)
	rec := httptest.NewRecorder()
	handleDashboardRoot(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 See Other, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	assert.NotContains(t, loc, "init_token", "the one-shot token must drop out of the URL")
	assert.Contains(t, loc, "tab=messages", "deep-link tab must survive the redirect")
	assert.Contains(t, loc, "access_request=abc123", "deep-link request id must survive the redirect")
	assert.Contains(t, loc, "wizard=1", "the cosmetic theme param must still survive")
}

// When bound non-loopback, dashboardAuthResult must switch to the host-relative
// origin check (Origin.Host == r.Host) instead of the fixed loopback pin — so a
// browser reaching the dashboard through a LAN IP / proxy hostname authenticates,
// while a cross-host Origin is still refused.
func TestDashboardAuthResult_HostRelativeWhenNonLoopback(t *testing.T) {
	prevBind := dashboardBindHost
	dashboardBindHost = "0.0.0.0"
	t.Cleanup(func() { dashboardBindHost = prevBind })

	initDashboardToken()

	req := httptest.NewRequest("POST", "http://box.example:8080/api/access-requests/x/decision", nil)
	req.Host = "box.example:8080"
	req.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})

	// Same-host Origin → accepted.
	req.Header.Set("Origin", "http://box.example:8080")
	ok, _, _, msg := dashboardAuthResult(req)
	assert.True(t, ok, "same-host origin must pass host-relative check; msg=%s", msg)

	// Cross-host Origin → refused even with a valid cookie.
	req.Header.Set("Origin", "http://evil.example:8080")
	ok, _, code, _ := dashboardAuthResult(req)
	assert.False(t, ok, "cross-host origin must be refused")
	assert.Equal(t, 403, code)
}
