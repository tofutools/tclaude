package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
)

// remoteSessionCookieName is the remote listener's session cookie. Pinned here
// (rather than importing the unexported const) so the test also guards the
// wire name the phone's browser holds.
const remoteSessionCookieName = "tclaude_remote_session"

// TestRemoteDashboard_RealRoutesServed proves the REGULAR dashboard is fully
// viewable over the remote (mTLS + passphrase) listener: with a valid session
// the real route set serves the dashboard HTML, a real /static JS module, and
// a live /api/snapshot carrying the agent we set up — and without a session
// the same /api/snapshot is refused at the middleware boundary. This is the
// "view the regular dashboard from another computer" capability, end to end on
// the real routes (not the stubbed mux in remote_server_test.go).
func TestRemoteDashboard_RealRoutesServed(t *testing.T) {
	const conv = "remotedash-1111-2222-3333-444444444444"
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSession(conv, "spwn-rd", "tmux-rd", "/tmp/rd")
	f.HaveMember("squad", conv)

	// Material lands in the flow's temp HOME (testharness.New set $HOME).
	if _, err := remoteaccess.Setup(remoteaccess.SetupOptions{
		Bind:        "0.0.0.0:8443",
		Passphrase:  "pp-pp-pp-pp",
		ClientName:  "phone",
		P12Password: "p12pw",
	}); err != nil {
		t.Fatalf("remoteaccess.Setup: %v", err)
	}
	m, err := remoteaccess.Load()
	require.NoError(t, err, "remoteaccess.Load")

	handler := agentd.BuildRemoteDashboardHandlerForTest(m)
	session := &http.Cookie{
		Name:  remoteSessionCookieName,
		Value: remoteaccess.SignCookie(m.CookieKey(), "human", time.Hour),
	}

	get := func(path string, withSession bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if withSession {
			r.AddCookie(session)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	// The regular dashboard HTML.
	rootRec := get("/", true)
	assert.Equal(t, http.StatusOK, rootRec.Code, "GET / body=%s", rootRec.Body.String())
	assert.Contains(t, rootRec.Body.String(), "<!DOCTYPE html>", "GET / serves the dashboard HTML")

	// A real ES-module static asset.
	jsRec := get("/static/js/dashboard.js", true)
	assert.Equal(t, http.StatusOK, jsRec.Code, "GET /static/js/dashboard.js")

	// A live /api/snapshot — the real handler, carrying the agent we created.
	snapRec := get("/api/snapshot", true)
	require.Equal(t, http.StatusOK, snapRec.Code, "GET /api/snapshot body=%s", snapRec.Body.String())
	assert.Contains(t, snapRec.Body.String(), conv, "/api/snapshot includes the live agent over the remote listener")

	// Without a session cookie the same route is refused at the boundary —
	// the routes aren't open just because they're mounted on the remote mux.
	noSessRec := get("/api/snapshot", false)
	assert.NotEqual(t, http.StatusOK, noSessRec.Code, "/api/snapshot must require a remote session")
}
