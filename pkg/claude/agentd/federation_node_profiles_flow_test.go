package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func fedNodeProfile(t *testing.T, fh *fedHarness, name string, spec db.FederationNodeProfileSpec) *db.FederationNodeProfile {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles", db.FederationNodeProfile{Name: name, Definition: spec})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var p db.FederationNodeProfile
	testharness.DecodeJSON(t, rec, &p)
	return &p
}
func fedApplyNodeProfile(t *testing.T, fh *fedHarness, p *db.FederationNodeProfile) *db.FederationNodeProfilePlan {
	t.Helper()
	path := "/v1/federation/profiles/" + p.Name + "/apply"
	rec := fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var plan db.FederationNodeProfilePlan
	testharness.DecodeJSON(t, rec, &plan)
	require.Empty(t, plan.Conflicts)
	rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"peer": "bob", "apply": true, "preview_token": plan.Token, "confirm_fingerprint": proto.Fingerprint(fh.peer.id.Pub)})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, &plan)
	require.True(t, plan.Applied)
	return &plan
}
func TestFederation_NodeProfileReapplyPreservesManualStateAndRefusesConflicts(t *testing.T) {
	fh := newFedHarness(t)
	pool := fedPool(t, fh, "rigs", false)
	p := fedNodeProfile(t, fh, "rig", db.FederationNodeProfileSpec{Pools: []string{"rigs"}, PeerGrants: []db.FederationPeerGrant{{Slug: "config.offer"}}})
	plan := fedApplyNodeProfile(t, fh, p)
	require.Equal(t, pool.ID, plan.Profile.Definition.Pools[0])
	require.True(t, plan.Pools[0].Security)
	grants, e := db.ListEffectiveFederationPeerGrants(fh.peer.id.ID())
	require.NoError(t, e)
	require.Len(t, grants, 1)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermNodeRead})
	require.Equal(t, 200, rec.Code)
	p.Definition.PeerGrants = nil
	p.Definition.Pools = nil
	rec = fedHuman(t, fh.f, http.MethodPut, "/v1/federation/profiles/rig", p)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, p)
	fedApplyNodeProfile(t, fh, p)
	grants, e = db.ListEffectiveFederationPeerGrants(fh.peer.id.ID())
	require.NoError(t, e)
	require.Len(t, grants, 1)
	require.Equal(t, agentd.PermNodeRead, grants[0].Slug)
	member, e := db.FederationNodeGroupContainsID(pool.ID, fh.peer.id.ID())
	require.NoError(t, e)
	require.False(t, member)
	fh.f.HaveGroup("team")
	p.Definition.PeerGrants = []db.FederationPeerGrant{{Slug: agentd.PermGroupsMembersSpawn, Scope: "group=team", SpawnPolicy: db.FederationSpawnPolicy{MaxLive: 2}}}
	rec = fedHuman(t, fh.f, http.MethodPut, "/v1/federation/profiles/rig", p)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, p)
	fedApplyNodeProfile(t, fh, p)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"max_live": 7}})
	require.Equal(t, 200, rec.Code)
	p.Definition.PeerGrants[0].SpawnPolicy.MaxLive = 3
	rec = fedHuman(t, fh.f, http.MethodPut, "/v1/federation/profiles/rig", p)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, p)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/apply", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	testharness.DecodeJSON(t, rec, &plan)
	require.NotEmpty(t, plan.Conflicts)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/apply", map[string]any{"peer": "bob", "apply": true, "preview_token": plan.Token})
	require.Equal(t, 409, rec.Code)
	grants, e = db.ListFederationPeerGrants(fh.peer.id.ID())
	require.NoError(t, e)
	for _, g := range grants {
		if g.Slug == agentd.PermGroupsMembersSpawn {
			require.Equal(t, 7, g.SpawnPolicy.MaxLive)
		}
	}
}
func TestFederation_NodeProfileStalePreviewAndImmutablePoolReferences(t *testing.T) {
	fh := newFedHarness(t)
	fedPool(t, fh, "rigs", false)
	p := fedNodeProfile(t, fh, "rig", db.FederationNodeProfileSpec{Pools: []string{"rigs"}})
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/apply", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	var plan db.FederationNodeProfilePlan
	testharness.DecodeJSON(t, rec, &plan)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "group:rigs", "slug": "config.offer"})
	require.Equal(t, 200, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/apply", map[string]any{"peer": "bob", "apply": true, "preview_token": plan.Token})
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "stale preview")
	rec = fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/nodes/groups/rigs", nil)
	require.Equal(t, 200, rec.Code)
	replacement := fedPool(t, fh, "rigs", false)
	require.NotEqual(t, p.Definition.Pools[0], replacement.ID)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/apply", map[string]any{"peer": "bob"})
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "no longer exists")
}
func TestFederation_NodeProfileTrustDefaultsAndFingerprint(t *testing.T) {
	fh := newFedHarness(t)
	p := fedNodeProfile(t, fh, "default", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: "config.offer"}}})
	rec := fedHuman(t, fh.f, http.MethodPut, "/v1/federation/default-peer-profile", map[string]any{"profile": p.Name})
	require.Equal(t, 200, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	preview := func(extra map[string]any) map[string]any {
		in := map[string]any{"instance": fh.peer.id.ID(), "label": "bob", "preview": true}
		for k, v := range extra {
			in[k] = v
		}
		r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", in)
		require.Equal(t, 200, r.Code, r.Body.String())
		var out map[string]any
		testharness.DecodeJSON(t, r, &out)
		return out
	}
	out := preview(nil)
	require.NotNil(t, out["profile"])
	trusted, e := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, e)
	require.Nil(t, trusted, "preview cannot trust")
	token := out["plan"].(map[string]any)["preview_token"]
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": fh.peer.id.ID(), "label": "bob", "preview_token": token})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	a, e := db.GetFederationNodeProfileAssignment(fh.peer.id.ID())
	require.NoError(t, e)
	require.Equal(t, p.ID, a.Profile.ID)
	// Editing the default does not alter a subsequent ordinary trust update.
	p.Definition.TrustLevel = db.FederationTrustUnrestricted
	rec = fedHuman(t, fh.f, http.MethodPut, "/v1/federation/profiles/default", p)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, p)
	out = preview(nil)
	require.Nil(t, out["profile"])
	out = preview(map[string]any{"profile": p.Name})
	token = out["plan"].(map[string]any)["preview_token"]
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": fh.peer.id.ID(), "profile": p.Name, "preview_token": token})
	require.Equal(t, 400, rec.Code)
	require.Contains(t, rec.Body.String(), "confirmation_required")
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": fh.peer.id.ID(), "profile": p.Name, "preview_token": token, "confirm_fingerprint": proto.Fingerprint(fh.peer.id.Pub)})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	out = preview(map[string]any{"no_default_profile": true})
	require.Nil(t, out["profile"])
	rec = fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/profiles/default", nil)
	require.Equal(t, 409, rec.Code, "configured defaults cannot be deleted")
}

type fedProfileBirthSpawner struct {
	inner testharness.SpawnerLike
	check func(clcommon.SpawnArgs)
}

func (s *fedProfileBirthSpawner) SpawnNew(a clcommon.SpawnArgs) error {
	s.check(a)
	return s.inner.SpawnNew(a)
}
func (s *fedProfileBirthSpawner) SpawnResume(a clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(a)
}
func TestFederation_NodeProfileWorkerDefaultsBeforeLaunchAndFrozen(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			fh := newFedHarness(t)
			group := fh.f.HaveGroup("team")
			fh.f.HaveGroup("allowed")
			require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermGroupsMembersStop, "self.rename"}, "test"))
			slug := agentd.PermGroupsRosterRead
			if automatic {
				slug = agentd.PermGroupsMembersSpawn
			}
			p := fedNodeProfile(t, fh, "workers", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: slug, Scope: "group=team"}}, WorkerPermissions: map[string]db.PermissionOverride{"self.rename": {Effect: "deny"}, agentd.PermGroupsMembersStop: {Effect: "grant", Scope: `{"group":["allowed"]}`}}})
			fedApplyNodeProfile(t, fh, p)
			previous := agentd.Spawn
			births := 0
			agentd.Spawn = &fedProfileBirthSpawner{inner: previous, check: func(args clcommon.SpawnArgs) {
				births++
				require.NotEmpty(t, args.SessionID)
				rows, e := db.ListAgentPermissionOverridesForConv(args.SessionID)
				require.NoError(t, e)
				require.Equal(t, "deny", rows["self.rename"], "deny must exist before the subprocess starts")
			}}
			t.Cleanup(func() { agentd.Spawn = previous })
			env := fh.peer.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "worker", Brief: "review"})
			fh.peer.send(env)
			require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, env.ID).Status)
			var row *db.FederationSpawnRequest
			fedEventually(t, "stored peer request", func() bool {
				rows, e := db.ListFederationSpawnRequests(100)
				if e != nil {
					return false
				}
				for _, candidate := range rows {
					if candidate.EnvelopeID == env.ID {
						row = candidate
						return true
					}
				}
				return false
			})
			if !automatic {
				rec := fedHuman(t, fh.f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", row.ID), map[string]any{})
				require.Equal(t, 200, rec.Code, rec.Body.String())
			}
			fedEventually(t, "approved worker", func() bool {
				var e error
				row, e = db.GetFederationSpawnRequest(row.ID)
				return e == nil && row.Status == db.FedSpawnApproved
			})
			actor, e := db.GetAgent(row.ResultAgent)
			require.NoError(t, e)
			require.Equal(t, 1, births)
			rec := agentReq(t, fh.f, actor.CurrentConvID, http.MethodPost, "/v1/groups/team/stop", nil)
			require.Equal(t, 403, rec.Code, rec.Body.String(), "scoped per-agent tier must narrow broad receiving-group grant")
			rec = agentReq(t, fh.f, actor.CurrentConvID, http.MethodPost, "/v1/whoami/rename", map[string]any{"title": "changed"})
			require.Equal(t, 403, rec.Code, rec.Body.String())
			rows, e := db.ListAgentPermissionOverrideRowsForConv(actor.CurrentConvID)
			require.NoError(t, e)
			raw, _ := json.Marshal(rows)
			require.Contains(t, string(raw), "node-profile:workers")
			p.Definition.WorkerPermissions["self.rename"] = db.PermissionOverride{Effect: "grant"}
			rec = fedHuman(t, fh.f, http.MethodPut, "/v1/federation/profiles/workers", p)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			testharness.DecodeJSON(t, rec, p)
			fedApplyNodeProfile(t, fh, p)
			snapshot, e := db.GetFederationWorkerDefaults(actor.AgentID)
			require.NoError(t, e)
			require.Equal(t, "deny", snapshot.Permissions["self.rename"].Effect)
			next, e := db.ResolveFederationWorkerDefaults(fh.peer.id.ID())
			require.NoError(t, e)
			require.Equal(t, "grant", next.Permissions["self.rename"].Effect)
			fh.f.AssertGroupMember("team", actor.CurrentConvID, "worker", 5*time.Second)
		})
	}
}

func TestFederation_NodeProfileConfigOfferIsSeparateIdempotentAndLabelsOnly(t *testing.T) {
	fh := newFedHarness(t)
	p := fedNodeProfile(t, fh, "rig", db.FederationNodeProfileSpec{Labels: []string{"gpu"}})
	fedApplyNodeProfile(t, fh, p)
	offers, e := db.ListFederationBundleOffers("out")
	require.NoError(t, e)
	require.Empty(t, offers, "apply must not offer or configure the peer")
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/offer", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		Offer db.FederationBundleOffer `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &out)
	require.NotEmpty(t, out.Offer.Descriptor.ID)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/offer", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "unchanged")
	fedApplyNodeProfile(t, fh, p)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/offer", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "unchanged")
	for _, state := range []string{"declined", "expired", "removed"} {
		priorID := out.Offer.Descriptor.ID
		if state == "removed" {
			require.NoError(t, db.DeleteFederationBundleOffer("out", fh.peer.id.ID(), priorID))
		} else {
			require.NoError(t, db.SetFederationBundleOfferState("out", fh.peer.id.ID(), priorID, state, ""))
		}
		rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/rig/offer", map[string]any{"peer": "bob"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		testharness.DecodeJSON(t, rec, &out)
		require.NotEqual(t, priorID, out.Offer.Descriptor.ID, "terminal/missing offers must be retryable")
	}
	// Import only this narrow portable setting; federation credentials stay excluded.
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": map[string]any{"format": "tclaude-config-bundle", "format_version": 1, "sections": map[string]any{"config": []any{map[string]any{"name": "federation.node_labels", "value": []string{"gpu"}}}}}, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": map[string]any{"format": "tclaude-config-bundle", "format_version": 1, "sections": map[string]any{"config": []any{map[string]any{"name": "federation", "value": map[string]any{"invite": "secret"}}}}}, "apply": true, "replace": true})
	require.Equal(t, 400, rec.Code, rec.Body.String())
}

func TestFederation_NodeProfilePreviewDoesNotRevokeAwayApproval(t *testing.T) {
	fh := newFedHarness(t)
	p := fedNodeProfile(t, fh, "rig", db.FederationNodeProfileSpec{})
	fh.f.HaveConvWithTitle("requester", "requester")
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermApprovalsAnswer})
	require.Equal(t, 200, rec.Code)
	setFedAway(t, fh, time.Time{})
	id := "dddddddddddddddddddddddddddddddd"
	_, cleanup := agentd.StartFederationAwayApprovalForTest(id, "requester", 8*time.Second)
	t.Cleanup(cleanup)
	_, epoch := fedAwayTicket(t, fh.peer, id)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles/"+p.Name+"/apply", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": "bob", "profile": p.Name, "preview": true})
	require.Equal(t, 200, rec.Code)
	answer := sendFedAwayDecision(t, fh.peer, id, epoch, "approve")
	awaitFedAwayAck(t, fh.peer, answer.ID, proto.AckAccepted)
}

func TestFederation_NodeProfileManagementIsOperatorOnly(t *testing.T) {
	fh := newFedHarness(t)
	const caller = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/federation/profiles", nil},
		{http.MethodPost, "/v1/federation/profiles", map[string]any{"name": "bad"}},
		{http.MethodPut, "/v1/federation/default-peer-profile", map[string]any{"profile": "bad"}},
		{http.MethodPost, "/v1/federation/profiles/bad/apply", map[string]any{"peer": "bob", "apply": true}},
		{http.MethodPost, "/v1/federation/profiles/bad/offer", map[string]any{"peer": "bob"}},
	} {
		rec := agentReq(t, fh.f, caller, call.method, call.path, call.body)
		require.Equal(t, 403, rec.Code, call.path+": "+rec.Body.String())
	}
}
