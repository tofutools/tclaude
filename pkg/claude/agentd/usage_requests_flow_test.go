package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-ff2gqm: a peer's operator asks for usage on this node's model
// gateway; the approver picks the gateway and may only lower the amount.
func TestPeerAccessUsageRequest(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("shared")
	fedModelPolicy(t, fh, "http://127.0.0.1:1")
	bob := fh.peer.id.ID()
	_, err := db.DeleteFederationPeerGrant(bob, agentd.PermModelsProxy, "http_proxy=model")
	require.NoError(t, err)
	require.Equal(t, 200, fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=shared"}).Code)
	h := agentd.PeerViewHandler(bob)
	ask := func(body map[string]any) (int, db.FederationPeerAccessRequest) {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/peer-access-requests", body))
		var row db.FederationPeerAccessRequest
		_ = json.Unmarshal(rec.Body.Bytes(), &row)
		return rec.Code, row
	}
	code, _ := ask(map[string]any{"permission": agentd.PermMessageDirect, "max_usd": 5})
	require.Equal(t, 400, code, "an amount goes only with models.proxy")
	code, _ = ask(map[string]any{"permission": agentd.PermModelsProxy, "max_usd": -1})
	require.Equal(t, 400, code)
	code, row := ask(map[string]any{"permission": agentd.PermModelsProxy, "max_usd": 20, "grant_ttl_seconds": 7 * 86400, "reason": "my budget ran out"})
	require.Equal(t, 202, code)
	require.Equal(t, 20.0, row.MaxUSD)

	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/access-requests", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"usage_gateways":["model"]`)
	require.Contains(t, rec.Body.String(), `"scope_display":"up to $20.00 on any gateway"`)
	decide := func(id string, body map[string]any) int {
		body["decision"] = "approve"
		return fedHuman(t, fh.f, http.MethodPost, "/v1/federation/access-requests/"+id+"/decision", body).Code
	}
	require.Equal(t, 400, decide(row.ID, map[string]any{"http_proxy": "model", "max_usd": 30}), "cannot raise the amount")
	require.Equal(t, 400, decide(row.ID, map[string]any{"http_proxy": "model", "max_usd": 0}), "cannot drop the cap")
	require.Equal(t, 400, decide(row.ID, map[string]any{"http_proxy": "nope", "max_usd": 10}), "an unknown gateway")
	require.Equal(t, 200, decide(row.ID, map[string]any{"http_proxy": "model", "max_usd": 10}))
	share := func() *db.FederationPeerGrant {
		grants, err := db.ListFederationPeerGrants(bob)
		require.NoError(t, err)
		for i := range grants {
			if grants[i].Slug == agentd.PermModelsProxy {
				return &grants[i]
			}
		}
		return nil
	}
	fedEventually(t, "usage share granted", func() bool { return share() != nil })
	g := share()
	require.Equal(t, "http_proxy=model", g.Scope)
	require.Equal(t, 10.0, g.SpawnPolicy.MaxUSD)
	require.NotNil(t, g.ExpiresAt, "a timed share")
	got, err := db.GetFederationPeerAccessRequest(row.ID)
	require.NoError(t, err)
	require.Equal(t, "model", got.GrantProxy)
	require.Equal(t, 10.0, got.GrantMaxUSD)

	// Asking again for a gateway adds to the share: $5 more.
	code, more := ask(map[string]any{"permission": agentd.PermModelsProxy, "http_proxy": "model", "max_usd": 5})
	require.Equal(t, 202, code)
	require.Equal(t, 400, decide(more.ID, map[string]any{"http_proxy": "", "max_usd": 5}), "cannot widen a gateway to any")
	require.Equal(t, 200, decide(more.ID, map[string]any{}))
	fedEventually(t, "share raised", func() bool { g := share(); return g != nil && g.SpawnPolicy.MaxUSD == 15 })
	require.Equal(t, g.CreatedAt.Unix(), share().CreatedAt.Unix(), "the share keeps counting from its start")
}
