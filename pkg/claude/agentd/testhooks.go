//go:build rewire

package agentd

import (
	"context"
	"net/http"
)

// BuildHandlerForTest exposes the production /v1 mux to flow tests in
// `package agentd_test`. The mux is identical to what serve() installs
// — minus the socket plumbing. Build-tagged so the symbol only exists
// in test binaries built with `-tags=rewire`.
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
