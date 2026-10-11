package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederationGrantIdentityOrphanAndNumericNameCollision(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("gone")
	deleted, err := db.GetAgentGroupByName("gone")
	require.NoError(t, err)
	require.NotNil(t, deleted)
	numericName := fmt.Sprint(deleted.ID)
	fh.f.HaveGroup(numericName)
	live, err := db.GetAgentGroupByName(numericName)
	require.NoError(t, err)
	require.NotNil(t, live)
	pool, err := db.CreateFederationNodeGroup("fleet")
	require.NoError(t, err)
	require.NoError(t, db.DeleteAgentGroup("gone"))
	for _, selector := range []string{"bob", "group:fleet"} {
		t.Run(selector, func(t *testing.T) {
			// Model rows retained by a pre-fix version, after deletion.
			for _, id := range []int64{deleted.ID, live.ID} {
				grant := db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermGroupsRosterRead, Scope: db.FederationGroupScope(id)}
				if selector == "bob" {
					require.NoError(t, db.UpsertFederationPeerGrant(grant))
				} else {
					require.NoError(t, db.UpsertFederationNodeGroupGrant(pool.ID, grant))
				}
			}
			rec := fedHuman(t, fh.f, "GET", "/v1/federation/grants?peer="+selector, nil)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var response struct {
				Grants []struct {
					Scope        string `json:"scope"`
					GroupID      int64  `json:"group_id"`
					GroupName    string `json:"group_name"`
					GroupDeleted bool   `json:"group_deleted"`
				} `json:"grants"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Len(t, response.Grants, 2)
			orphanScope := ""
			for _, g := range response.Grants {
				require.Equal(t, fmt.Sprintf("group_id=%d", g.GroupID), g.Scope)
				if g.GroupID == deleted.ID {
					require.True(t, g.GroupDeleted)
					require.Empty(t, g.GroupName)
					orphanScope = g.Scope
				} else {
					require.False(t, g.GroupDeleted)
					require.Equal(t, numericName, g.GroupName)
				}
			}
			require.NotEmpty(t, orphanScope)
			body := map[string]any{"peer": selector, "slug": agentd.PermGroupsRosterRead, "scope": orphanScope}
			rec = fedHuman(t, fh.f, "POST", "/v1/federation/grants", body)
			require.Equal(t, 400, rec.Code, "cannot grant a missing group by ID")
			rec = fedHuman(t, fh.f, "DELETE", "/v1/federation/grants", body)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			rec = fedHuman(t, fh.f, "GET", "/v1/federation/grants?peer="+selector, nil)
			require.Equal(t, 200, rec.Code)
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Len(t, response.Grants, 1)
			require.Equal(t, live.ID, response.Grants[0].GroupID, "orphan revoke must preserve the numeric-named group's grant")
			body["scope"] = "group=" + numericName
			rec = fedHuman(t, fh.f, "DELETE", "/v1/federation/grants", body)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			body["scope"] = fmt.Sprintf("group_id=%d", live.ID)
			rec = fedHuman(t, fh.f, "POST", "/v1/federation/grants", body)
			require.Equal(t, 200, rec.Code)
			for _, invalid := range []string{"group_id=0", "group_id=-1", "group_id=01", "group_id=+1", "group_id=1,other=2"} {
				body["scope"] = invalid
				rec = fedHuman(t, fh.f, "DELETE", "/v1/federation/grants", body)
				require.Equal(t, 400, rec.Code, invalid)
			}
		})
	}
	// The local UI wrapper exposes and round-trips the same typed identity.
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, "/api/federation/grants?peer=bob", nil))
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), fmt.Sprintf(`"scope":"group_id=%d"`, live.ID))
	rec = testharness.Serve(h, testharness.JSONRequest(t, http.MethodDelete, "/api/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsRosterRead, "scope": fmt.Sprintf("group_id=%d", live.ID)}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
