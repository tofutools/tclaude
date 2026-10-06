package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func setFedTrustLevel(t *testing.T, fh *fedHarness, level string) {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{
		"instance": "bob", "level": level, "confirm_fingerprint": proto.Fingerprint(fh.peer.id.Pub),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestFederation_TrustLevelCatalogAndConfirmation(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("one")
	f.HaveGroup("two")
	f.HaveGroup("archived")
	require.NoError(t, db.ArchiveAgentGroup("archived"))
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": "bob", "level": "unrestricted"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), proto.Fingerprint(p.id.Pub))
	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	require.Equal(t, db.FederationTrustRestricted, peer.TrustLevel)
	setFedTrustLevel(t, fh, "unrestricted")
	latest := func() proto.CatalogPayload {
		cats := p.envelopes(proto.KindCatalog)
		if len(cats) == 0 {
			return proto.CatalogPayload{}
		}
		var cat proto.CatalogPayload
		require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
		return cat
	}
	fedEventually(t, "all live groups visible", func() bool { return len(latest().Groups) == 2 })
	for _, g := range latest().Groups {
		require.ElementsMatch(t, []string{proto.CapRoster, proto.CapPresence, proto.CapMail, proto.CapAttachments, proto.CapSpawn, proto.CapRoutes}, g.Caps)
	}
	rec = fedHuman(t, f, http.MethodGet, "/v1/federation/status", nil)
	require.Contains(t, rec.Body.String(), `"level":"unrestricted"`)
	setFedTrustLevel(t, fh, "restricted")
	fedEventually(t, "downgraded catalog empty", func() bool { return len(latest().Groups) == 0 })
	rec = fedHuman(t, f, http.MethodGet, "/v1/federation/status", nil)
	require.Contains(t, rec.Body.String(), `"level":"restricted"`)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": "bob", "level": "unknown"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": "bob", "level": "unrestricted", "confirm_fingerprint": proto.Fingerprint(p.id.Pub)}), "agent"))
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestFederation_UnrestrictedRequestingGrantsAndDowngrade(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const alice = "fed-trust-alice-bbbb-cccc-000000000001"
	f.HaveGroup("team")
	f.HaveConvWithTitle(alice, "alice")
	f.HaveMember("team", alice)
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{
		Name: "builders", Caps: []string{proto.CapRoster, proto.CapMail, proto.CapSpawn},
		Members: []proto.CatalogMember{{Agent: "agt_bobremote0000000000000000", Name: "bob-agent"}},
	}}}))
	fedEventually(t, "remote catalog", func() bool {
		for _, r := range fedStatus(t, f).Remote {
			if r.Label == "bob" && len(r.Groups) == 1 {
				return true
			}
		}
		return false
	})
	send := func(want int) {
		t.Helper()
		rec := postMessage(t, f, alice, map[string]any{"to": "bob-agent@bob", "body": "review"})
		require.Equal(t, want, rec.Code, rec.Body.String())
	}
	require.NoError(t, db.GrantAgentPermission(alice, agentd.PermMessageDirect, "test"))
	send(http.StatusForbidden)
	setFedTrustLevel(t, fh, "unrestricted")
	send(http.StatusOK)
	setFedTrustLevel(t, fh, "restricted")
	send(http.StatusForbidden)
	setFedTrustLevel(t, fh, "unrestricted")
	rec := postPermissionScope(t, f, "grant", map[string]any{"target": alice, "slug": agentd.PermGroupsMembersSpawn, "scope": map[string]any{"group": []string{"team"}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body)
	spawn := func(want int) {
		t.Helper()
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"peer": "bob", "group": "builders", "brief": "review"}), alice))
		require.Equal(t, want, rec.Code, rec.Body.String())
	}
	spawn(http.StatusForbidden)
	require.NoError(t, db.GrantAgentPermission(alice, agentd.PermGroupsMembersSpawn, "test"))
	spawn(http.StatusOK)
	// An unscoped grant contributed by membership also counts remotely.
	group, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	_, err = db.RevokeAgentPermission(alice, agentd.PermMessageDirect)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermMessageDirect}, "test"))
	send(http.StatusOK)
	require.NoError(t, db.SetAgentPermissionOverride(alice, agentd.PermMessageDirect, db.PermEffectDeny, "test"))
	send(http.StatusForbidden)
	_, err = db.RevokeAgentPermission(alice, agentd.PermMessageDirect)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, nil, "test"))
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	cfg.Agent.DefaultPermissions = []string{agentd.PermMessageDirect}
	require.NoError(t, config.Save(cfg))
	send(http.StatusOK)
	setFedTrustLevel(t, fh, "restricted")
	send(http.StatusForbidden)
}
