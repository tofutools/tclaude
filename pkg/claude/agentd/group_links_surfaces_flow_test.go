package agentd_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Group links are edited from two surfaces: the daemon socket
// (/v1/groups/{g}/links, used by `tclaude agent groups link`) and the
// dashboard (/api/groups/{g}/links). Both run the same link policy; they
// differ only in auth (the dashboard is human-only) and in wire format: the
// socket answers errors as a JSON {error, code} body, the dashboard as plain
// text, which links-actions.js shows verbatim.

type linkSurface struct {
	name string
	// do issues a request on this surface as the human operator.
	do func(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder
	// prefix is the surface's group route root.
	prefix string
	// errText extracts the operator-facing error text from a failed reply.
	errText func(t *testing.T, rec *httptest.ResponseRecorder) string
}

func linkSurfaces() []linkSurface {
	return []linkSurface{
		{
			name: "socket",
			do: func(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
				return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, body)))
			},
			prefix: "/v1/groups/",
			errText: func(t *testing.T, rec *httptest.ResponseRecorder) string {
				var e struct {
					Error string `json:"error"`
				}
				testharness.DecodeJSON(t, rec, &e)
				return e.Error
			},
		},
		{
			name: "dashboard",
			do: func(t *testing.T, _ *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
				return testharness.Serve(agentd.BuildDashboardHandlerForTest(), dashReq(t, method, path, body))
			},
			prefix: "/api/groups/",
			errText: func(_ *testing.T, rec *httptest.ResponseRecorder) string {
				return rec.Body.String()
			},
		},
	}
}

type linkAddReply struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Mode      string `json:"mode"`
	ReverseID any    `json:"reverse_id"`
}

type linkUpdateReply struct {
	ID      int64  `json:"id"`
	Mode    string `json:"mode"`
	Changed bool   `json:"changed"`
}

func TestGroupLinks_BothSurfacesAddUpdateRemove(t *testing.T) {
	for _, s := range linkSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			f := newFlow(t)
			a := f.HaveGroup("alpha")
			b := f.HaveGroup("beta")
			f.HaveGroup("gamma")
			links := s.prefix + "alpha/links"

			// Add with the default mode and a reverse edge.
			rec := s.do(t, f, http.MethodPost, links, map[string]any{"to": "beta", "bidir": true})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var added linkAddReply
			testharness.DecodeJSON(t, rec, &added)
			require.Equal(t, "alpha", added.From)
			require.Equal(t, "beta", added.To)
			require.Equal(t, db.LinkModeMembersToMembers, added.Mode)
			require.NotNil(t, added.ReverseID, "bidir creates the reverse edge")
			out, err := db.ListAgentGroupLinks(a.ID, db.LinkOut)
			require.NoError(t, err)
			require.Len(t, out, 1)
			in, err := db.ListAgentGroupLinks(b.ID, db.LinkOut)
			require.NoError(t, err)
			require.Len(t, in, 1, "the reverse edge beta→alpha exists")

			// A repeated add is a conflict.
			rec = s.do(t, f, http.MethodPost, links, map[string]any{"to": "beta"})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

			// An unknown mode is refused before anything is written.
			rec = s.do(t, f, http.MethodPost, links, map[string]any{"to": "gamma", "mode": "sideways"})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, s.errText(t, rec), "sideways")

			// Update: a no-op reports changed=false, a real change true.
			linkPath := fmt.Sprintf("%s/%d", links, added.ID)
			rec = s.do(t, f, http.MethodPatch, linkPath, map[string]any{"mode": db.LinkModeMembersToMembers})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var upd linkUpdateReply
			testharness.DecodeJSON(t, rec, &upd)
			require.False(t, upd.Changed)
			rec = s.do(t, f, http.MethodPatch, linkPath, map[string]any{"mode": db.LinkModeOwnersToMembers})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			testharness.DecodeJSON(t, rec, &upd)
			require.True(t, upd.Changed)
			require.Equal(t, db.LinkModeOwnersToMembers, upd.Mode)

			// Changing into an existing from/to/mode row is a conflict.
			second, err := db.InsertAgentGroupLink(a.ID, b.ID, db.LinkModeMembersToMembers, "")
			require.NoError(t, err)
			rec = s.do(t, f, http.MethodPatch, fmt.Sprintf("%s/%d", links, second), map[string]any{"mode": db.LinkModeOwnersToMembers})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

			// Bad ids and links that do not touch the group are not found.
			rec = s.do(t, f, http.MethodPatch, links+"/nope", map[string]any{"mode": db.LinkModeOwnersToMembers})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			rec = s.do(t, f, http.MethodDelete, links+"/999999", nil)
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			rec = s.do(t, f, http.MethodDelete, fmt.Sprintf("%sgamma/links/%d", s.prefix, added.ID), nil)
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

			// Remove from the TO side works for the human operator.
			rec = s.do(t, f, http.MethodDelete, fmt.Sprintf("%sbeta/links/%d", s.prefix, added.ID), nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			got, err := db.GetAgentGroupLinkByID(added.ID)
			require.NoError(t, err)
			require.Nil(t, got)
		})
	}
}

// An archived group is sealed: neither surface may open a link from it or
// into it. The dashboard used to skip this check (its handler was a hand copy
// of the socket's that predated the archived guard).
func TestGroupLinks_BothSurfacesRefuseArchivedGroups(t *testing.T) {
	for _, s := range linkSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			f := newFlow(t)
			a := f.HaveGroup("alpha")
			f.HaveGroup("beta")
			f.HaveGroup("sealed")
			require.NoError(t, db.ArchiveAgentGroup("sealed"))

			rec := s.do(t, f, http.MethodPost, s.prefix+"sealed/links", map[string]any{"to": "alpha"})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			require.Contains(t, s.errText(t, rec), "archived")

			rec = s.do(t, f, http.MethodPost, s.prefix+"alpha/links", map[string]any{"to": "sealed"})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			require.Contains(t, s.errText(t, rec), "archived")

			out, err := db.ListAgentGroupLinks(a.ID, db.LinkBoth)
			require.NoError(t, err)
			require.Empty(t, out, "no link touches the archived group")

			// Links between active groups are unaffected.
			rec = s.do(t, f, http.MethodPost, s.prefix+"alpha/links", map[string]any{"to": "beta"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}
