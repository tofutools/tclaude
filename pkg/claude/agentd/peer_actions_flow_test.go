package agentd_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestPeerActionsWholeFootprintAndBodyLimits(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	for _, name := range []string{"one", "two", "owned"} {
		f.HaveGroup(name)
	}
	f.HaveAliveSession(moveSourceConv, "remote-action-source", "remote-action-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("one", moveSourceConv)
	f.HaveMember("two", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	post := func(action string, body any) int {
		return testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/agents/"+aid+"/"+action, body)).Code
	}
	grant := func(slug, group string) {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=" + group})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	require.Equal(t, 404, post("stop", nil), "no visibility")
	grant(agentd.PermGroupsRosterRead, "one")
	// The source actor's own broad local grant cannot authorize a remote peer.
	require.NoError(t, db.SetAgentPermissionOverride(moveSourceConv, agentd.PermAgentStop, db.PermEffectGrant, "fixture"))
	require.Equal(t, 403, post("stop", nil))
	grant(agentd.PermGroupsMembersStop, "one")
	require.Equal(t, 403, post("stop", nil), "partial footprint cannot stop a multi-group agent")
	grant(agentd.PermGroupsMembersStop, "two")
	require.Equal(t, 200, post("stop", nil))
	for _, slug := range []string{agentd.PermGroupsMembersRetire, agentd.PermGroupsMembersClone, agentd.PermAgentMove} {
		grant(slug, "one")
		grant(slug, "two")
	}
	require.Equal(t, 400, post("retire?delete_worktree=1", nil))
	require.Equal(t, 400, post("clone", map[string]any{"cwd": "/operator/private"}))
	require.Equal(t, 400, post("move", map[string]any{"group": "receiver", "peer": "third-node"}))
	require.Equal(t, 400, post("teleport", map[string]any{"group": "receiver", "credentials": "proxy:secret@third-node"}))
	owned, err := db.GetAgentGroupByName("owned")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(owned.ID, moveSourceConv, "fixture"))
	require.Equal(t, 403, post("clone", map[string]any{"cwd": "/operator/private"}), "clone footprint includes owned groups")
	grant(agentd.PermGroupsMembersClone, "owned")
	require.Equal(t, 400, post("clone", map[string]any{"cwd": "/operator/private"}))
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/one/spawn", map[string]any{"brief": "test task"}))
	require.Equal(t, 403, rec.Code)
	grant(agentd.PermGroupsMembersSpawn, "one")
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/one/spawn", map[string]any{"brief": "test task", "cwd": "/operator/private"}))
	require.Equal(t, 400, rec.Code)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/one/spawn", map[string]any{"brief": "test task", "profile": "unallowed"}))
	require.Equal(t, 403, rec.Code)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/one/spawn", map[string]any{"brief": "test task", "name": "peer-worker"}))
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var job struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job))
	require.Positive(t, job.ID)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", fmt.Sprintf("/api/spawn-requests/%d", job.ID), nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	// Wait for the external spawn attempt before fixture teardown.
	require.Eventually(t, func() bool {
		r, err := db.GetFederationSpawnRequest(job.ID)
		return err == nil && r != nil && (r.Status == db.FedSpawnApproved || r.Reason != "")
	}, 5*time.Second, 10*time.Millisecond)
	_, err = db.UntrustFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	require.Equal(t, 403, post("stop", nil))
}

func TestPeerMoveRevokedBeforeRunningConfirmation(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	for _, slug := range []string{agentd.PermAgentMove, agentd.PermGroupsMembersRetire} {
		rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=source"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "POST", "/api/agents/"+aid+"/move", map[string]any{"group": "receiver"}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var reply struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), reply.Offer.D.ID)
	require.NoError(t, err)
	require.Equal(t, "peer:"+fh.peer.id.ID(), m.Initiator)
	require.False(t, m.Human)
	group, err := db.GetAgentGroupByName("source")
	require.NoError(t, err)
	_, err = db.DeleteFederationPeerGrant(fh.peer.id.ID(), agentd.PermAgentMove, db.FederationGroupScope(group.ID))
	require.NoError(t, err)
	fedMoveConfirm(t, fh, reply.Offer.D, reply.Offer.D.SHA256)
	require.Eventually(t, func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), reply.Offer.D.ID)
		return m != nil && m.State == "blocked"
	}, 5*time.Second, 10*time.Millisecond)
	actor, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, actor.Active(), "revoked peer cannot retire the source on confirmation")
}

func TestPeerCloneAndRetireProductionCores(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveConvWithTitle(moveSourceConv, "peer-worker")
	f.HaveEnrolledAgent(moveSourceConv)
	f.HaveAliveSession(moveSourceConv, "peer-clone-source", "peer-clone-pane", f.TestCwd("work"))
	f.HaveGroup("source")
	f.HaveMember("source", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	for _, slug := range []string{agentd.PermGroupsMembersClone, agentd.PermGroupsMembersRetire} {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=source"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/agents/"+aid+"/clone", map[string]any{"no_copy_conv": true, "follow_up": "remote task"}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var reply struct {
		Conv string `json:"new_conv"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	require.NotEmpty(t, reply.Conv)
	f.AssertSpawnInitialPrompt(reply.Conv, "remote task", 10*time.Second)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/agents/"+aid+"/retire", map[string]any{}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	actor, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.False(t, actor.Active())
}

func TestPeerTeleportProductionOffer(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedTeleportSource(t, fh)
	for _, slug := range []string{agentd.PermAgentMove, agentd.PermGroupsMembersRetire} {
		rec := fedHuman(t, fh.f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=source"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "POST", "/api/agents/"+aid+"/teleport", map[string]any{"group": "receiver", "note": "task handoff"}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var reply struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	require.NotNil(t, reply.Offer.D.Teleport)
	require.Equal(t, aid, reply.Offer.D.Teleport.SourceAgent)
	m, err := db.GetFederationAgentMove("out", fh.peer.id.ID(), reply.Offer.D.ID)
	require.NoError(t, err)
	require.Equal(t, "peer:"+fh.peer.id.ID(), m.Initiator)
	require.False(t, m.Human)
}

// A peer brings an agent back with groups.members.resume; restarting also
// needs groups.members.stop, and only an unrestricted peer may restart one
// with its sandbox off. Resume options that recreate directories or switch a
// Codex drive stay local.
func TestPeerResumeAndRestartGrants(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("one")
	f.HaveAliveSession(moveSourceConv, "remote-resume-source", "remote-resume-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("one", moveSourceConv)
	aid, err := db.AgentIDForConv(moveSourceConv)
	require.NoError(t, err)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	post := func(action string, body any) (int, string) {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/agents/"+aid+"/"+action, body))
		return rec.Code, rec.Body.String()
	}
	grant := func(slug string) {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=one"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	grant(agentd.PermGroupsRosterRead)
	for _, action := range []string{"resume", "restart", "sandbox-restart"} {
		code, body := post(action, map[string]any{"action": "restore"})
		require.Equal(t, 403, code, action+": "+body)
	}
	grant(agentd.PermGroupsMembersResume)
	code, body := post("resume?recreate=1", nil)
	require.Equal(t, 400, code, body)
	code, body = post("resume?send_keys=1", nil)
	require.Equal(t, 400, code, body)
	code, body = post("resume", nil)
	require.Equal(t, 200, code, body)
	code, body = post("restart", nil)
	require.Equal(t, 403, code, "restart stops the agent first: "+body)
	grant(agentd.PermGroupsMembersStop)
	code, body = post("restart", nil)
	require.NotEqual(t, 403, code, body)
	code, body = post("sandbox-restart", map[string]any{"action": "unlock"})
	require.Equal(t, 403, code, "sandbox off needs unrestricted trust: "+body)
	for _, padded := range []string{" unlock", "unlock\n", "\tunlock", "UNLOCK"} {
		code, body = post("sandbox-restart", map[string]any{"action": padded})
		require.Equal(t, 400, code, "only the exact closed values pass: "+body)
	}
	code, body = post("sandbox-restart", map[string]any{"action": "restore", "cwd": "/x"})
	require.Equal(t, 400, code, body)
	code, body = post("sandbox-restart", map[string]any{"action": "restore"})
	require.NotEqual(t, 403, code, body)

	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	code, body = post("sandbox-restart", map[string]any{"action": "unlock"})
	require.NotEqual(t, 403, code, "unrestricted trust implies the grants and may unlock: "+body)
}
