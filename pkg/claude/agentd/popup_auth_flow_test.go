package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// popupReq builds a request to the loopback popup mux with a loopback
// RemoteAddr — handlePopupApprove refuses anything else.
func popupReq(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "127.0.0.1:54321"
	return r
}

// Scenario: a bare GET of an approval popup — no init token, no cookie
// — is refused. The session cookie is never handed out for free, so a
// process that only scraped the approval id cannot mint itself one.
func TestPopupAuth_RefusesBareGet(t *testing.T) {
	const id = "appr-bare-1111"
	t.Cleanup(agentd.SeedPendingApprovalForTest(id))

	mux := http.NewServeMux()
	agentd.RegisterPopupRoutesForTest(mux)

	rec := testharness.Serve(mux, popupReq(http.MethodGet, "/approve/"+id))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"bare GET /approve/{id} must be refused; body=%s", rec.Body.String())
}

// Scenario: the init-token exchange. A valid token → 303 + the popup
// session cookie; the token is single-use; a bogus token is refused.
func TestPopupAuth_TokenExchange(t *testing.T) {
	const id = "appr-exch-2222"
	t.Cleanup(agentd.SeedPendingApprovalForTest(id))

	mux := http.NewServeMux()
	agentd.RegisterPopupRoutesForTest(mux)

	// Bogus token — refused.
	rec := testharness.Serve(mux, popupReq(http.MethodGet, "/approve/"+id+"?init_token=deadbeefdeadbeef"))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"bogus init token must be refused; body=%s", rec.Body.String())

	// Valid token — 303 redirect that sets the popup session cookie.
	tok := agentd.MintApproveInitTokenForTest(id)
	rec = testharness.Serve(mux, popupReq(http.MethodGet, "/approve/"+id+"?init_token="+tok))
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"valid init token must 303-redirect; body=%s", rec.Body.String())
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "tclaude_popup_"+id {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "exchange must set the popup session cookie")

	// The same token a second time — refused (single-use).
	rec = testharness.Serve(mux, popupReq(http.MethodGet, "/approve/"+id+"?init_token="+tok))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"init token must be single-use; body=%s", rec.Body.String())
}

// Scenario: an init token minted for one approval cannot be redeemed
// against another. Scope binding stops a token scraped for approval A
// from being replayed against approval B.
func TestPopupAuth_TokenScopedToApproval(t *testing.T) {
	const idA = "appr-aaaa-3333"
	const idB = "appr-bbbb-4444"
	t.Cleanup(agentd.SeedPendingApprovalForTest(idA))
	t.Cleanup(agentd.SeedPendingApprovalForTest(idB))

	mux := http.NewServeMux()
	agentd.RegisterPopupRoutesForTest(mux)

	tokA := agentd.MintApproveInitTokenForTest(idA)
	rec := testharness.Serve(mux, popupReq(http.MethodGet, "/approve/"+idB+"?init_token="+tokA))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"approval A's token must not unlock approval B; body=%s", rec.Body.String())
}

// Scenario: with the exchanged cookie a POST records the decision; a
// POST without the cookie is refused. Confirms the exchange yields a
// working credential and that checkPopupAuth still gates writes.
func TestPopupAuth_DecideRequiresCookie(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const id = "appr-deci-5555"
	t.Cleanup(agentd.SeedPendingApprovalForTest(id))

	mux := http.NewServeMux()
	agentd.RegisterPopupRoutesForTest(mux)

	// POST approve without the cookie — refused.
	rec := testharness.Serve(mux, popupReq(http.MethodPost, "/approve/"+id+"/approve"))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"POST without the session cookie must be refused; body=%s", rec.Body.String())

	// Exchange a token for the cookie.
	tok := agentd.MintApproveInitTokenForTest(id)
	rec = testharness.Serve(mux, popupReq(http.MethodGet, "/approve/"+id+"?init_token="+tok))
	require.Equal(t, http.StatusSeeOther, rec.Code, "exchange body=%s", rec.Body.String())
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "tclaude_popup_"+id {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "exchange must set the popup session cookie")

	// POST approve with the cookie + matching Origin — recorded.
	req := popupReq(http.MethodPost, "/approve/"+id+"/approve")
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	rec = testharness.Serve(mux, req)
	require.Equal(t, http.StatusOK, rec.Code, "POST approve body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Approved",
		"the approve callback page should confirm the decision")
}
