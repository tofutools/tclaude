package agentd_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type peerSpawnOptionsView struct {
	Group          string   `json:"group"`
	Catalog        bool     `json:"catalog"`
	DefaultPrompts []string `json:"default_prompts"`
	Profiles       []struct {
		Name string `json:"name"`
	} `json:"profiles"`
	Approval  string            `json:"approval"`
	Overrides map[string]bool   `json:"overrides"`
	Harnesses []string          `json:"harnesses"`
	Defaults  map[string]string `json:"defaults"`
	PeerView  struct {
		Included []string `json:"included"`
	} `json:"peer_view"`
}

func peerSpawnOptions(t *testing.T, peer, group string) (int, peerSpawnOptionsView) {
	t.Helper()
	rec := testharness.Serve(agentd.PeerViewHandler(peer), testharness.JSONRequest(t, "GET", "/api/spawn-options/"+group, nil))
	var out peerSpawnOptionsView
	if rec.Code == 200 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec.Code, out
}

func peerSpawnPost(t *testing.T, peer, group string, body map[string]any) (int, int64, string) {
	t.Helper()
	rec := testharness.Serve(agentd.PeerViewHandler(peer), testharness.JSONRequest(t, "POST", "/api/groups/"+group+"/spawn", body))
	var out struct {
		ID   int64  `json:"id"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.ID, out.Code
}

// waitSpawnSettled waits for an automatic spawn attempt to finish before the
// fixture tears down.
func waitSpawnSettled(t *testing.T, id int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		r, err := db.GetFederationSpawnRequest(id)
		return err == nil && r != nil && (r.Status == db.FedSpawnApproved || r.Reason != "")
	}, 5*time.Second, 10*time.Millisecond)
}

// A peer that can only see a group still gets the spawn dialog: its request
// waits for this node's operator and may set no launch settings.
func TestPeerSpawnRequestWithoutGrantWaitsForOperator(t *testing.T) {
	f := newFlow(t)
	builders := f.HaveGroup("builders")
	f.HaveGroup("hidden")
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))

	code, _ := peerSpawnOptions(t, bob.ID(), "builders")
	require.Equal(t, 404, code, "an invisible group offers nothing")
	code, _, _ = peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task"})
	require.Equal(t, 404, code)

	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: bob.ID(), Slug: agentd.PermGroupsVisibilityRead, Scope: db.FederationGroupScope(builders.ID)}))
	code, opts := peerSpawnOptions(t, bob.ID(), "builders")
	require.Equal(t, 200, code)
	require.Equal(t, "operator", opts.Approval)
	require.Equal(t, map[string]bool{"cwd": false, "harness": false, "model": false, "effort": false, "profile": false, "roles": false, "sandbox": false, "permissions": false, "environment": false, "owner": false, "features": false, "prompts": false, "worktree": false}, opts.Overrides)
	require.Empty(t, opts.Defaults, "a restricted peer learns no local paths")
	require.Contains(t, opts.PeerView.Included, "spawn.request")
	require.NotContains(t, opts.PeerView.Included, "spawn")
	code, _ = peerSpawnOptions(t, bob.ID(), "hidden")
	require.Equal(t, 404, code)

	for _, field := range []string{"cwd", "harness", "model", "effort"} {
		value := map[string]string{"cwd": "/srv/work", "harness": "claude", "model": "opus", "effort": "high"}[field]
		code, _, errCode := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", field: value})
		require.Equal(t, 403, code, field)
		require.Equal(t, "override_not_allowed", errCode, field)
	}

	code, id, _ := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "port the parser", "name": "parser"})
	require.Equal(t, 202, code)
	req, err := db.GetFederationSpawnRequest(id)
	require.NoError(t, err)
	require.Equal(t, db.FedSpawnPending, req.Status)
	require.True(t, req.Overrides.Empty())
	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, db.MaxHumanMessagesPage)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Subject, fmt.Sprintf("remote spawn request #%d", id)) {
			found = true
			require.Contains(t, m.Body, "port the parser")
		}
	}
	require.True(t, found, "the operator is asked to decide")
	rec := testharness.Serve(agentd.PeerViewHandler(bob.ID()), testharness.JSONRequest(t, "GET", fmt.Sprintf("/api/spawn-requests/%d", id), nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())

	_, err = db.DeleteFederationPeerGrant(bob.ID(), agentd.PermGroupsVisibilityRead, db.FederationGroupScope(builders.ID))
	require.NoError(t, err)
	rec = testharness.Serve(agentd.PeerViewHandler(bob.ID()), testharness.JSONRequest(t, "GET", fmt.Sprintf("/api/spawn-requests/%d", id), nil))
	require.Equal(t, 404, rec.Code, "a group that is no longer visible hides its requests like spawn-options does")
}

// grantSpawnFields grants peer an automatic spawn into group and the
// groups.members.spawn.<field> grants named.
func grantSpawnFields(t *testing.T, f *testharness.Flow, peer, group string, policy map[string]any, fields ...string) {
	t.Helper()
	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": peer, "slug": agentd.PermGroupsMembersSpawn, "scope": "group=" + group, "spawn_policy": policy})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	for _, field := range fields {
		rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": peer, "slug": agentd.PermGroupsMembersSpawn + "." + field, "scope": "group=" + group})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
}

// Each launch setting a peer may choose is its own grant; the request
// stores them for the launch.
func TestPeerSpawnGrantOpensListedOverrides(t *testing.T) {
	f := newFlow(t)
	builders := f.HaveGroup("builders")
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))

	rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=builders", "spawn_policy": map[string]any{"allowed_overrides": []string{"model"}}})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "groups.members.spawn.model", "the replaced list is refused with the grant to use instead")
	// A field grant alone opens nothing: an operator-approved request
	// carries no launch settings.
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawnModel, "scope": "group=builders"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsVisibilityRead, "scope": "group=builders"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	code, opts := peerSpawnOptions(t, bob.ID(), "builders")
	require.Equal(t, 200, code)
	require.False(t, opts.Overrides["model"])
	grantSpawnFields(t, f, "bob", "builders", nil, "effort")
	_ = builders

	code, opts = peerSpawnOptions(t, bob.ID(), "builders")
	require.Equal(t, 200, code)
	require.Equal(t, "automatic", opts.Approval)
	require.Equal(t, map[string]bool{"cwd": false, "harness": false, "model": true, "effort": true, "profile": false, "roles": false, "sandbox": false, "permissions": false, "environment": false, "owner": false, "features": false, "prompts": false, "worktree": false}, opts.Overrides)
	require.Contains(t, opts.PeerView.Included, "spawn")

	code, _, errCode := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "cwd": "/srv/work"})
	require.Equal(t, 403, code)
	require.Equal(t, "override_not_allowed", errCode)
	code, _, _ = peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "model": "bad model;rm"})
	require.Equal(t, 400, code, "malformed values are refused before any grant check")

	code, id, _ := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "model": "opus", "effort": "high"})
	require.Equal(t, 202, code)
	req, err := db.GetFederationSpawnRequest(id)
	require.NoError(t, err)
	require.Equal(t, db.FederationSpawnOverrides{Model: "opus", Effort: "high"}, req.Overrides)
	waitSpawnSettled(t, id)
}

// Unrestricted trust opens every override and names the receiver's defaults;
// values are still validated.
func TestPeerSpawnUnrestrictedOpensAllOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("builders")
	carol, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: carol.ID(), PubKey: carol.Pub, Label: "carol", TrustLevel: db.FederationTrustUnrestricted}))

	code, opts := peerSpawnOptions(t, carol.ID(), "builders")
	require.Equal(t, 200, code)
	require.Equal(t, "automatic", opts.Approval)
	require.Equal(t, map[string]bool{"cwd": true, "harness": true, "model": true, "effort": true, "profile": true, "roles": true, "sandbox": true, "permissions": true, "environment": true, "owner": true, "features": true, "prompts": true, "worktree": true}, opts.Overrides)
	require.Contains(t, opts.Harnesses, "claude")
	require.NotContains(t, opts.Harnesses, "shell", "a spawn request always carries a brief, which shell refuses")
	require.Contains(t, opts.Defaults, "cwd")

	for _, bad := range []map[string]any{
		{"brief": "task", "cwd": "relative/dir"},
		{"brief": "task", "cwd": "/srv/../etc"},
		{"brief": "task", "harness": "no-such-harness"},
		{"brief": "task", "harness": "shell"},
		{"brief": "task", "model": "-x"},
		{"brief": "task", "model": "--dangerously-skip-permissions"},
		{"brief": "task", "effort": "HIGH!"},
	} {
		code, _, _ := peerSpawnPost(t, carol.ID(), "builders", bad)
		require.Equal(t, 400, code, bad)
	}
	dir := t.TempDir()
	code, id, _ := peerSpawnPost(t, carol.ID(), "builders", map[string]any{"brief": "task", "cwd": dir, "harness": "claude"})
	require.Equal(t, 202, code)
	req, err := db.GetFederationSpawnRequest(id)
	require.NoError(t, err)
	require.Equal(t, dir, req.Overrides.Cwd)
	require.Equal(t, "claude", req.Overrides.Harness)
	waitSpawnSettled(t, id)
}

// An operator approval shows what the request asked for, applies only the
// settings the peer may still choose (or the operator's own), and names the
// dropped ones.
func TestPeerSpawnApprovalDropsOverridesThePeerMayNoLongerChoose(t *testing.T) {
	f := newFlow(t)
	builders := f.HaveGroup("builders")
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: bob.ID(), Slug: agentd.PermGroupsVisibilityRead, Scope: db.FederationGroupScope(builders.ID)}))
	dir := t.TempDir()
	// As left behind by a grant that allowed these and has since been revoked.
	id, err := db.InsertFederationSpawnRequest(&db.FederationSpawnRequest{
		FromInstance: bob.ID(), EnvelopeID: "env-dropped", FromName: "bob", GroupID: builders.ID, GroupName: "builders",
		Brief: "port the parser", Status: db.FedSpawnPending, ExpiresAt: time.Now().Add(time.Hour),
		Overrides: db.FederationSpawnOverrides{Cwd: "/srv/peer-chosen", Model: "opus", Effort: "high"},
	}, 10)
	require.NoError(t, err)

	rec := fedHuman(t, f, "GET", "/v1/federation/spawn-requests", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var rows []struct {
		ID        int64                        `json:"id"`
		Overrides *db.FederationSpawnOverrides `json:"overrides"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, &db.FederationSpawnOverrides{Cwd: "/srv/peer-chosen", Model: "opus", Effort: "high"}, rows[0].Overrides, "the operator sees what was asked for")

	rec = fedHuman(t, f, "POST", fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", id), map[string]any{"cwd": dir})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		Dropped []string `json:"dropped_overrides"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, []string{"model", "effort"}, out.Dropped, "the operator's own cwd wins; the rest is no longer the peer's to choose")
	waitSpawnSettled(t, id)

	// A dropped harness takes the model chosen for it along, and a stored
	// value the operator replaces (an old malformed directory) does not block
	// the approval.
	id, err = db.InsertFederationSpawnRequest(&db.FederationSpawnRequest{
		FromInstance: bob.ID(), EnvelopeID: "env-dropped-harness", FromName: "bob", GroupID: builders.ID, GroupName: "builders",
		Brief: "port the lexer", Status: db.FedSpawnPending, ExpiresAt: time.Now().Add(time.Hour),
		Overrides: db.FederationSpawnOverrides{Cwd: "not/absolute", Harness: "codex", Model: "gpt-5"},
	}, 10)
	require.NoError(t, err)
	rec = fedHuman(t, f, "GET", "/v1/federation/spawn-requests", nil)
	var listed []struct {
		ID        int64    `json:"id"`
		WouldDrop []string `json:"would_drop"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	for _, r := range listed {
		if r.ID == id {
			require.Equal(t, []string{"cwd", "harness", "model"}, r.WouldDrop, "the listing says what an approval would drop")
		}
	}
	rec = fedHuman(t, f, "POST", fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", id), map[string]any{"cwd": dir})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	out.Dropped = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, []string{"harness", "model"}, out.Dropped)
	waitSpawnSettled(t, id)
}

// A harness override that contradicts the profile the launch runs (the
// grant's pinned profile here) is refused instead of silently dropping that
// profile's harness-specific settings.
func TestPeerSpawnHarnessOverrideConflictsWithPinnedProfile(t *testing.T) {
	f := newFlow(t)
	builders := f.HaveGroup("builders")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "codexy", Harness: "codex", Model: "gpt-5.4"})
	require.NoError(t, err)
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	grantSpawnFields(t, f, "bob", "builders", map[string]any{"profile": "codexy"}, "harness")

	code, _, errCode := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "harness": "claude"})
	require.Equal(t, 409, code)
	require.Equal(t, "harness_profile_conflict", errCode)

	code, id, _ := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "harness": "codex"})
	require.Equal(t, 202, code, "the profile's own harness is no conflict")
	waitSpawnSettled(t, id)

	// On an operator approval the advice is the operator's options.
	carol, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: carol.ID(), PubKey: carol.Pub, Label: "carol", TrustLevel: db.FederationTrustUnrestricted}))
	id, err = db.InsertFederationSpawnRequest(&db.FederationSpawnRequest{
		FromInstance: carol.ID(), EnvelopeID: "env-conflict", FromName: "carol", GroupID: builders.ID, GroupName: "builders",
		Brief: "task", Status: db.FedSpawnPending, ExpiresAt: time.Now().Add(time.Hour),
		Overrides: db.FederationSpawnOverrides{Harness: "claude"},
	}, 10)
	require.NoError(t, err)
	rec := fedHuman(t, f, "POST", fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", id), map[string]any{"profile": "codexy"})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "harness_profile_conflict")
	require.Contains(t, rec.Body.String(), "approve with --harness")
}
