package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestPeerAccessVisibleGroupIdentityAndRequestability(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fh.f.HaveGroup("hidden")
	group, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	for _, path := range []string{"/api/snapshot", "/api/groups"} {
		rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", path, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var body struct {
			Groups []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			}
			PeerView struct {
				Omitted []struct {
					Feature          string `json:"feature"`
					Requires         string `json:"requires"`
					Requestable      *bool  `json:"requestable"`
					UnrestrictedOnly bool   `json:"unrestricted_only"`
				}
			} `json:"peer_view"`
		}
		testharness.DecodeJSON(t, rec, &body)
		require.Len(t, body.Groups, 1)
		require.Equal(t, group.ID, body.Groups[0].ID)
		require.Equal(t, "team", body.Groups[0].Name)
		features := map[string]bool{}
		requires := map[string]string{}
		unrestrictedOnly := map[string]bool{}
		for _, entry := range body.PeerView.Omitted {
			require.NotNil(t, entry.Requestable)
			features[entry.Feature] = *entry.Requestable
			requires[entry.Feature] = entry.Requires
			unrestrictedOnly[entry.Feature] = entry.UnrestrictedOnly
		}
		// The dashboard names the missing grant from requires, or unrestricted
		// trust for a feature no grant opens.
		require.Equal(t, agentd.PermAgentsStatusRead, requires["agents.status"])
		require.False(t, unrestrictedOnly["agents.status"])
		require.True(t, unrestrictedOnly["agents.manage.full"])
		require.True(t, features["spawn"])
		require.True(t, features["lifecycle.stop"])
		for _, feature := range []string{"terminals", "spawn.inline", "local_dashboard"} {
			require.Contains(t, features, feature)
			require.False(t, features[feature])
		}
	}
	// The emitted group ID is accepted by the permission-request API.
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/peer-access-requests", map[string]any{"permission": agentd.PermGroupsMembersSpawn, "group_id": group.ID, "grant_ttl_seconds": 3600}))
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var pending struct {
		ID string `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &pending)
	for _, ttl := range []int{0, 3601} {
		rec = fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+pending.ID+"/decision", map[string]any{"decision": "approve", "grant_ttl_seconds": ttl})
		require.Equal(t, 400, rec.Code, rec.Body.String())
	}
	rec = fedHuman(t, fh.f, "POST", "/v1/federation/access-requests/"+pending.ID+"/decision", map[string]string{"decision": "deny"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
