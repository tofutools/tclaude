package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Group membership and ownership are edited from two surfaces: the daemon
// socket (/v1/groups/{g}/members|owners, used by `tclaude agent groups`) and
// the dashboard (/api/groups/{g}/members|owners). Both run the same policy;
// they differ in auth (the dashboard is human-only) and wire format: the
// socket answers errors as JSON {error, code}, the dashboard as plain text,
// which the dashboard's member actions show verbatim.

type memberSurface struct {
	name   string
	do     func(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder
	prefix string
}

func memberSurfaces() []memberSurface {
	return []memberSurface{
		{
			name: "socket",
			do: func(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
				return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, body)))
			},
			prefix: "/v1/groups/",
		},
		{
			name: "dashboard",
			do: func(t *testing.T, _ *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
				return testharness.Serve(agentd.BuildDashboardHandlerForTest(), dashReq(t, method, path, body))
			},
			prefix: "/api/groups/",
		},
	}
}

const memberConv = "a1b2c3d4-0000-4000-8000-00000000c0de"

func groupMember(t *testing.T, groupID int64, convID string) *db.AgentGroupMember {
	t.Helper()
	members, err := db.ListAgentGroupMembers(groupID)
	require.NoError(t, err)
	for _, m := range members {
		if m.ConvID == convID {
			return m
		}
	}
	return nil
}

func groupOwner(t *testing.T, groupID int64, convID string) bool {
	t.Helper()
	ok, err := db.IsAgentGroupOwner(groupID, convID)
	require.NoError(t, err)
	return ok
}

func TestGroupMembership_BothSurfacesAddUpdateRemove(t *testing.T) {
	for _, s := range memberSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			f := newFlow(t)
			g := f.HaveGroup("crew")
			f.HaveConvWithTitle(memberConv, "crew-dev")
			f.HaveEnrolledAgent(memberConv)
			members := s.prefix + "crew/members"
			owners := s.prefix + "crew/owners"

			// Missing conv is a bad request; an unknown one is not found.
			rec := s.do(t, f, http.MethodPost, members, map[string]any{})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			rec = s.do(t, f, http.MethodPost, members, map[string]any{"conv": "no-such-agent-zzz"})
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

			rec = s.do(t, f, http.MethodPost, members, map[string]any{"conv": memberConv, "role": "dev", "descr": "builds"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var added struct {
				ConvID string `json:"conv_id"`
			}
			testharness.DecodeJSON(t, rec, &added)
			require.Equal(t, memberConv, added.ConvID)
			m := groupMember(t, g.ID, memberConv)
			require.NotNil(t, m)
			require.Equal(t, "dev", m.Role)
			require.Equal(t, "builds", m.Descr)

			// Update touches only the fields present.
			rec = s.do(t, f, http.MethodPatch, members+"/"+memberConv, map[string]any{})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			rec = s.do(t, f, http.MethodPatch, members+"/"+memberConv, map[string]any{"role": "lead"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			m = groupMember(t, g.ID, memberConv)
			require.Equal(t, "lead", m.Role)
			require.Equal(t, "builds", m.Descr)

			// Ownership: grant, then revoke; revoking a non-owner is not found.
			rec = s.do(t, f, http.MethodPost, owners, map[string]any{"conv": memberConv})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			testharness.DecodeJSON(t, rec, &added)
			require.Equal(t, memberConv, added.ConvID)
			require.True(t, groupOwner(t, g.ID, memberConv))
			rec = s.do(t, f, http.MethodDelete, owners+"/"+memberConv, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			require.False(t, groupOwner(t, g.ID, memberConv))
			rec = s.do(t, f, http.MethodDelete, owners+"/"+memberConv, nil)
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

			rec = s.do(t, f, http.MethodDelete, members+"/"+memberConv, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			require.Nil(t, groupMember(t, g.ID, memberConv))

			// Updating a conv that is no longer a member is not found.
			rec = s.do(t, f, http.MethodPatch, members+"/"+memberConv, map[string]any{"role": "x"})
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		})
	}
}

// An archived group is sealed: neither surface may change its members or
// owners. The dashboard copies used to skip this check.
func TestGroupMembership_BothSurfacesRefuseArchivedGroups(t *testing.T) {
	for _, s := range memberSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			f := newFlow(t)
			g := f.HaveGroup("sealed")
			f.HaveConvWithTitle(memberConv, "crew-dev")
			f.HaveEnrolledAgent(memberConv)
			f.HaveMemberWithRole("sealed", memberConv, "dev")
			require.NoError(t, db.AddAgentGroupOwner(g.ID, memberConv, "<test>"))
			require.NoError(t, db.ArchiveAgentGroup("sealed"))
			const other = "a1b2c3d4-0000-4000-8000-0000000000ff"
			f.HaveConvWithTitle(other, "outsider")
			f.HaveEnrolledAgent(other)
			members := s.prefix + "sealed/members"
			owners := s.prefix + "sealed/owners"

			for _, c := range []struct {
				method, path string
				body         any
			}{
				{http.MethodPost, members, map[string]any{"conv": other}},
				{http.MethodPatch, members + "/" + memberConv, map[string]any{"role": "lead"}},
				{http.MethodDelete, members + "/" + memberConv, nil},
				{http.MethodPost, owners, map[string]any{"conv": other}},
				{http.MethodDelete, owners + "/" + memberConv, nil},
			} {
				rec := s.do(t, f, c.method, c.path, c.body)
				require.Equal(t, http.StatusConflict, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
				require.Contains(t, rec.Body.String(), "archived", "%s %s", c.method, c.path)
			}

			m := groupMember(t, g.ID, memberConv)
			require.NotNil(t, m, "the member stays")
			require.Equal(t, "dev", m.Role, "the role is unchanged")
			require.Nil(t, groupMember(t, g.ID, other), "no member was added")
			require.True(t, groupOwner(t, g.ID, memberConv), "the owner stays")
			require.False(t, groupOwner(t, g.ID, other), "no owner was added")
		})
	}
}
