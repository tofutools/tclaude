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
