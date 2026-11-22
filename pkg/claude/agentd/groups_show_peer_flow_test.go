package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// groups show of a group on this node, read by a linked node (bob): this
// node's federation read model decides every field, and nothing reaches
// further than bob's view of the group already does.
func TestGroupsShowFromAPeer(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	haveShowGroup(t, f)
	f.HaveGroup("hidden-group")
	grant := func(slug string) {
		t.Helper()
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=alpha"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	show := func(name string) (int, agent.GroupShow, string) {
		t.Helper()
		rec := testharness.Serve(h, httptest.NewRequest(http.MethodGet, "/v1/groups/"+name, nil))
		var out agent.GroupShow
		if rec.Code == 200 {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
		}
		body := rec.Body.String()
		assert.NotContains(t, body, "s3cret-value", "an environment value never leaves the node")
		return rec.Code, out, body
	}

	code, _, _ := show("alpha")
	assert.Equal(t, 404, code, "a group not shared with the peer, as if it did not exist")

	grant(agentd.PermGroupsRosterRead)
	code, g, body := show("alpha")
	require.Equal(t, 200, code, body)
	assert.Equal(t, "alpha", g.Name)
	require.Len(t, g.Members, 1)
	assert.Equal(t, "dev", g.Members[0].Role)
	assert.Nil(t, g.Context)
	assert.Nil(t, g.Settings)
	assert.Equal(t, map[string]string{"context": agentd.PermGroupsSettings, "settings": agentd.PermGroupsSettings}, withheld(g))
	assert.NotContains(t, body, groupsShowContext)
	assert.NotContains(t, body, "startup_dir", "the peer's view carries no member directories")
	code, _, _ = show("hidden-group")
	assert.Equal(t, 404, code)

	grant(agentd.PermGroupsSettings)
	code, g, body = show("alpha")
	require.Equal(t, 200, code, body)
	require.NotNil(t, g.Context, "groups.settings is groups.context.read's peer grant")
	assert.Equal(t, groupsShowContext, g.Context.Text)
	assert.Equal(t, db.GroupContextHash(groupsShowContext), g.Context.Hash)
	require.NotNil(t, g.Settings)
	assert.Empty(t, g.Settings.DefaultCwd, "the directory needs the spawn directory grant, as in the peer's view")
	assert.Empty(t, g.Settings.EnvironmentKeys, "the environment needs its own grant, as in the peer's view")
	assert.Empty(t, g.Withheld)

	grant(agentd.PermGroupsSettingsEnvironment)
	grant(agentd.PermGroupsMembersSpawnCwd)
	_, g, _ = show("alpha")
	require.NotNil(t, g.Settings)
	assert.Equal(t, []string{"API_TOKEN"}, g.Settings.EnvironmentKeys)
	assert.NotEmpty(t, g.Settings.DefaultCwd)

	// Unrestricted trust reads it as this node's operator does.
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	code, g, body = show("alpha")
	require.Equal(t, 200, code, body)
	require.NotNil(t, g.Context)
	assert.Len(t, g.Owners, 1, "the local summary, owners included")
}

// The operator's CLI reads a peer's group through the same route.
func TestGroupsShowNodeCLI(t *testing.T) {
	fh := newFedHarness(t)
	haveShowGroup(t, fh.f)
	for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsSettings} {
		rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=alpha"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	bridgeAgentClientToMux(t, fh.f.Mux)
	receiver := agentd.PeerViewHandler(fh.peer.id.ID())

	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	done := make(chan int, 1)
	go func() { done <- agent.RunGroupsShowOn("bob", "alpha", false, stdout, stderr) }()
	conn := acceptOutboundPeerView(t, fh.peer, 0)
	wire := readPeerViewWire(t, conn)
	assert.Equal(t, "/v1/groups/alpha", wire.URI)
	req := httptest.NewRequest(wire.Method, wire.URI, bytes.NewReader(wire.Body))
	for k, v := range wire.Header {
		req.Header[k] = v
	}
	rec := testharness.Serve(receiver, req)
	writePeerViewWire(t, conn, peerViewWire{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()})
	rc := <-done
	_ = conn.Close()
	require.Zero(t, rc, "out=%s err=%s", stdout, stderr)
	assert.Contains(t, stdout.String(), "Group alpha")
	assert.Contains(t, stdout.String(), "Use worktrees.")
}

// A peer's agent reads the summary only, though the peer holds
// groups.settings: an agent gets no group settings over a peer view.
func TestGroupsShowFromAPeerAgent(t *testing.T) {
	fh := newFedHarness(t)
	haveShowGroup(t, fh.f)
	for _, slug := range []string{agentd.PermGroupsRosterRead, agentd.PermGroupsSettings} {
		rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=alpha"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	worker := &proto.PeerViewAgent{ID: "agt_bobworker000000000000000", Name: "bob-worker", Groups: []string{"alpha"}}
	reply := agentPeerViewExchange(t, fh.peer, worker, "GET", "/v1/groups/alpha", nil)
	require.Equal(t, 200, reply.Status, string(reply.Body))
	var g agent.GroupShow
	require.NoError(t, json.Unmarshal(reply.Body, &g))
	assert.Nil(t, g.Context)
	assert.Nil(t, g.Settings)
	assert.NotContains(t, string(reply.Body), groupsShowContext)
	assert.Len(t, g.Withheld, 2)
}
