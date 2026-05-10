package agentd

import (
	"context"
	"net/http"
)

// BuildHandlerForTest exposes the production /v1 mux to flow tests in
// `package agentd_test`. The mux is identical to what serve() installs
// — minus the socket plumbing. The _test.go suffix keeps it out of
// production builds; only test binaries see it.
func BuildHandlerForTest() http.Handler {
	return buildMux()
}

// AsHumanPeer attaches a synthetic peer context that requirePermission
// treats as the human (HasClaudeAncestor=false). All permission gates
// pass.
func AsHumanPeer(r *http.Request) *http.Request {
	p := &peer{PID: 99999, HasClaudeAncestor: false}
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
}

// AsAgentPeer attaches a synthetic peer context that requirePermission
// resolves to convID. Default-permission lookups (config + DB) still
// run, so grants must be in place for the endpoint to succeed.
func AsAgentPeer(r *http.Request, convID string) *http.Request {
	p := &peer{PID: 99999, HasClaudeAncestor: true, ConvID: convID}
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
}

// SetPopupBaseURLForTest overrides the popup base URL so flow tests
// can reach the X-Tclaude-Ask-Human escalation branch without binding
// a real loopback HTTP server. Returns a restore function tests can
// schedule via t.Cleanup.
func SetPopupBaseURLForTest(url string) func() {
	prev := popupBaseURL
	popupBaseURL = url
	return func() { popupBaseURL = prev }
}

// StubApprovalForTest swaps the human-approval popup with a stub that
// returns `decision` immediately. Returns a restore function. The
// approvalRequest type stays unexported; the stub closes over `decision`
// and discards the request body since flow tests only care about the
// outcome, not the popup payload.
func StubApprovalForTest(decision bool) func() {
	prev := RequestHumanApprovalImpl
	RequestHumanApprovalImpl = func(*approvalRequest, string) bool {
		return decision
	}
	return func() { RequestHumanApprovalImpl = prev }
}

// BuildDashboardHandlerForTest exposes the dashboard mux (the
// loopback-port mux that hosts `/`, `/api/snapshot`,
// `/api/groups/...` mutation endpoints, and the popup `/approve/...`
// route in production). Flow tests use this when asserting the
// dashboard's snapshot or edit endpoints — `BuildHandlerForTest` only
// covers the /v1 Unix-socket mux.
//
// Initialises the dashboard session token if it isn't already, then
// returns a `dashTestHandler` that injects a valid cookie + Origin on
// every request — the dashboard's auth checks would otherwise refuse
// the synthetic httptest peer.
func BuildDashboardHandlerForTest() http.Handler {
	initDashboardToken()
	mux := http.NewServeMux()
	registerDashboardRoutes(mux)
	return &dashTestHandler{inner: mux}
}

type dashTestHandler struct{ inner http.Handler }

func (h *dashTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Cookie") == "" {
		r.AddCookie(&http.Cookie{
			Name:  dashboardCookieName,
			Value: dashboardSessionToken,
		})
	}
	if popupBaseURL != "" && r.Header.Get("Origin") == "" {
		r.Header.Set("Origin", popupBaseURL)
	}
	h.inner.ServeHTTP(w, r)
}
