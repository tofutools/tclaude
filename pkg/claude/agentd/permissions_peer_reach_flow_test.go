package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type peerReachRows struct {
	Rows []struct {
		Kind  string   `json:"kind"`
		Group string   `json:"group"`
		Conv  string   `json:"conv"`
		Title string   `json:"title"`
		Slug  string   `json:"slug"`
		Peers []string `json:"peers"`
	} `json:"rows"`
}

func peerReach(t *testing.T, fh *fedHarness, body map[string]any) (int, peerReachRows, string) {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/permissions/peer-reach", body)
	var out peerReachRows
	if rec.Code == http.StatusOK {
		testharness.DecodeJSON(t, rec, &out)
	}
	return rec.Code, out, rec.Body.String()
}

func reachOf(rows peerReachRows, kind, holder, slug string) []string {
	for _, r := range rows.Rows {
		if r.Kind == kind && r.Slug == slug && (r.Group == holder || r.Conv == holder) {
			return r.Peers
		}
	}
	return nil
}

func TestPeerReach_MergesPeerGroupsIntoAnAgentsGrantAndRevokesOne(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	bob := fh.peer.id.ID()
	const agent = "019fe740-43a4-7023-b8ae-1ee64459f3a1"
	f.HaveGroup("ops")
	f.HaveConvWithTitle(agent, "reach-agent")
	f.HaveAliveSession(agent, "reach-agent", "reach-agent-pane", f.TestCwd("reach"))
	f.HaveMember("ops", agent)
	grantScoped(t, f, agent, agentd.PermSessionsWatch, map[string]any{"peer": []string{"bob/lab"}})

	code, rows, body := peerReach(t, fh, map[string]any{"target": agent, "slugs": []string{"message.direct", "sessions.watch"}, "peer": "bob/builders"})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{bob + "/builders"}, reachOf(rows, "agent", agent, "message.direct"), "the label resolves to the peer's instance ID")
	require.Equal(t, []string{bob + "/builders", bob + "/lab"}, reachOf(rows, "agent", agent, "sessions.watch"), "an existing peer group is kept")

	code, rows, body = peerReach(t, fh, map[string]any{"target": agent, "slugs": []string{"sessions.watch"}, "peer": "bob/builders", "remove": true})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{bob + "/lab"}, reachOf(rows, "agent", agent, "sessions.watch"))
	code, rows, body = peerReach(t, fh, map[string]any{"target": agent, "slugs": []string{"message.direct"}, "peer": bob + "/builders", "remove": true})
	require.Equal(t, http.StatusOK, code, body)
	require.Nil(t, reachOf(rows, "agent", agent, "message.direct"), "the last peer group takes the grant with it")

	rec := fedHuman(t, f, http.MethodGet, "/v1/permissions/peer-reach?peer=bob", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listed peerReachRows
	testharness.DecodeJSON(t, rec, &listed)
	require.Len(t, listed.Rows, 1)
	require.Equal(t, "reach-agent", listed.Rows[0].Title)

	rec = agentReq(t, f, agent, http.MethodPost, "/v1/permissions/peer-reach", map[string]any{"target": agent, "slugs": []string{"jobs.run"}, "peer": "bob"})
	require.Equal(t, http.StatusForbidden, rec.Code, "an agent cannot widen its own reach this way")
}

func TestPeerReach_RefusesARowHeldForLocalUseAndWritesNothing(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	const agent = "019fe740-43a4-7023-b8ae-1ee64459f3a2"
	f.HaveGroup("ops")
	f.HaveAliveSession(agent, "local-agent", "local-agent-pane", f.TestCwd("local"))
	f.HaveMember("ops", agent)
	grantScoped(t, f, agent, agentd.PermMessageDirect, nil)

	code, _, body := peerReach(t, fh, map[string]any{"target": agent, "slugs": []string{"sessions.watch", "message.direct"}, "peer": "bob/builders"})
	require.Equal(t, http.StatusConflict, code, body)
	require.Contains(t, body, "peer_reach_conflict")
	require.Contains(t, body, "everywhere on this node")
	require.Contains(t, body, "a group it is in")
	_, rows, _ := peerReach(t, fh, map[string]any{"target": agent, "slugs": []string{"sessions.watch"}, "peer": "bob/nothing", "remove": true})
	require.Nil(t, reachOf(rows, "agent", agent, "sessions.watch"), "the refusal on one slug wrote none of them")

	code, _, body = peerReach(t, fh, map[string]any{"target": agent, "slugs": []string{"message.direct"}, "peer": "nobody/builders"})
	require.Equal(t, http.StatusBadRequest, code, body, "an untrusted peer is not a scope")
	code, _, body = peerReach(t, fh, map[string]any{"target": agent, "slugs": []string{"groups.roster.read"}, "peer": "bob/builders"})
	require.Equal(t, http.StatusBadRequest, code, body, "a slug without a peer scope cannot take reach")
}

func TestPeerReach_OnAGroupKeepsItsOtherGrants(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	bob := fh.peer.id.ID()
	f.HaveGroup("ops")
	rec := fedHuman(t, f, http.MethodPatch, "/v1/groups/ops", map[string]any{"permissions": []string{"groups.roster.read"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	code, rows, body := peerReach(t, fh, map[string]any{"group": "ops", "slugs": []string{"message.direct", "routes.consume"}, "peer": "bob/builders"})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{bob + "/builders"}, reachOf(rows, "group", "ops", "message.direct"))
	require.Equal(t, []string{bob + "/builders"}, reachOf(rows, "group", "ops", "routes.consume"))

	rec = fedHuman(t, f, http.MethodGet, "/v1/permissions", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var state struct {
		GroupGrants map[string][]string `json:"group_grants"`
	}
	testharness.DecodeJSON(t, rec, &state)
	require.Equal(t, []string{"groups.roster.read", "message.direct", "routes.consume"}, state.GroupGrants["ops"], "the group's other grant is kept")

	code, _, body = peerReach(t, fh, map[string]any{"group": "missing", "slugs": []string{"message.direct"}, "peer": "bob"})
	require.Equal(t, http.StatusNotFound, code, body)
}
