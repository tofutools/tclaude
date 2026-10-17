package agentd_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the operator views their own (unrestricted) node from another
// machine and hovers a PR link, CI pill or review badge. The proxied
// /api/pr-details and /api/pr-checks reads reach this node's caches; a
// restricted peer, which never sees PR links, is refused.
func TestPeerViewPRHoverCardsUnrestrictedOnly(t *testing.T) {
	const prURL = "https://github.com/acme/app/pull/88"
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	savePresentedPRPolicy(t, "github.com/acme")
	t.Cleanup(agentd.SetPRDetailsFetcherForTest(func(string) (string, bool) {
		return `{"title":"Remote hover","state":"OPEN","author":{"login":"alice"},"headRefName":"x","baseRefName":"main"}`, true
	}))
	t.Cleanup(agentd.SetPRChecksResolverForTest(func(string) (string, bool) {
		return `[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]`, true
	}))
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	paths := []string{"/api/pr-details?url=" + url.QueryEscape(prURL), "/api/pr-checks?url=" + url.QueryEscape(prURL)}

	for _, path := range paths {
		rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, path, nil))
		require.Equalf(t, http.StatusForbidden, rec.Code, "restricted peer %s: %s", path, rec.Body.String())
	}

	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))

	for _, path := range paths {
		rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, path, nil))
		require.Equalf(t, http.StatusOK, rec.Code, "unrestricted peer %s: %s", path, rec.Body.String())
	}
	agentd.WaitForBackgroundForTest()

	var details prDetailsResp
	testharness.DecodeJSON(t, testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, paths[0], nil)), &details)
	require.True(t, details.Resolved)
	require.Equal(t, "Remote hover", details.Details.Title)
	var checks struct {
		Resolved bool `json:"resolved"`
		Checks   []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	testharness.DecodeJSON(t, testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, paths[1], nil)), &checks)
	require.True(t, checks.Resolved)
	require.Len(t, checks.Checks, 1)
	require.Equal(t, "test", checks.Checks[0].Name)
}
