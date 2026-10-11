package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestFederation_PermissionCarryReceiverDecisions(t *testing.T) {
	for _, mode := range []string{"off", "restricted", "allowlist", "unrestricted", "sensitive", "optout"} {
		t.Run(mode, func(t *testing.T) {
			fh := newFedHarness(t)
			f, p := fh.f, fh.peer
			f.HaveGroup("receiver")
			fedReceiveAgents(t, fh, "receiver")
			if mode == "unrestricted" || mode == "sensitive" {
				setFedTrustLevel(t, fh, "unrestricted")
			}
			if mode == "allowlist" {
				rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "agents.receive.permissions", "scope": "group=receiver", "spawn_policy": map[string]any{"permission_slugs": []string{"groups.members.spawn"}}})
				require.Equal(t, 200, rec.Code, rec.Body.String())
			}
			b := fedAgentBundle(t)
			b.Manifest.Agent.CarryPermissions = mode != "off"
			b.Manifest.Agent.Permissions = []agentbundle.Permission{
				{Slug: "groups.members.spawn", Effect: "grant", Scope: json.RawMessage(`{"group":["source"]}`)},
				{Slug: "config.import", Effect: "grant"},
				{Slug: "human.notify", Effect: "grant"},
				{Slug: "permissions.revoke", Effect: "deny"},
				{Slug: "groups.members.retire", Effect: "grant", Source: "ownership:source"},
				{Slug: "agent.stop", Effect: "grant", Scope: json.RawMessage(`{"target":["source-agent"]}`)},
			}
			d := fedAgentOffer(t, p, "receiver", b)
			require.Equal(t, "accepted", string(fedAckFor(t, p, d.ID).Status))
			path := "/v1/federation/bundle-offers/" + d.ID + "/import"
			opts := map[string]any{"cwd": testutil.CanonicalTempDir(t), "allow_sensitive_permissions": mode == "sensitive", "drop_permissions": mode == "optout"}
			preview := fedHuman(t, f, http.MethodPost, path, opts)
			require.Equal(t, 200, preview.Code, preview.Body.String())
			var planned struct {
				Permissions []struct {
					Slug, Effect, Decision, Reason string
					Scope                          json.RawMessage
				}
				Spawn struct {
					ConvID string `json:"conv_id"`
				}
			}
			testharness.DecodeJSON(t, preview, &planned)
			require.Len(t, planned.Permissions, 6)
			decisions := map[string]string{}
			for _, v := range planned.Permissions {
				decisions[v.Slug] = v.Decision
			}
			enabled := mode != "off" && mode != "optout"
			if enabled {
				require.Equal(t, "apply", decisions["permissions.revoke"])
			} else {
				require.Equal(t, "drop", decisions["permissions.revoke"])
			}
			grants := mode == "allowlist" || mode == "unrestricted" || mode == "sensitive"
			if grants {
				require.Equal(t, "remap", decisions["groups.members.spawn"])
				require.JSONEq(t, `{"group":["receiver"]}`, string(planned.Permissions[0].Scope))
			} else {
				require.Equal(t, "drop", decisions["groups.members.spawn"])
			}
			if mode == "sensitive" {
				require.Equal(t, "apply", decisions["human.notify"])
			} else {
				require.Equal(t, "drop", decisions["human.notify"])
			}
			require.Equal(t, "drop", decisions["groups.members.retire"])
			require.Equal(t, "drop", decisions["agent.stop"])
			opts["apply"] = true
			applied := fedHuman(t, f, http.MethodPost, path, opts)
			require.Equal(t, 200, applied.Code, applied.Body.String())
			testharness.DecodeJSON(t, applied, &planned)
			rows, err := db.ListAgentPermissionOverrideRowsForConv(planned.Spawn.ConvID)
			require.NoError(t, err)
			bySlug := map[string]string{}
			for _, row := range rows {
				bySlug[row.Slug] = row.Effect
			}
			if enabled {
				require.Equal(t, "deny", bySlug["permissions.revoke"])
			} else {
				require.Empty(t, rows)
			}
			if grants {
				require.Equal(t, "grant", bySlug["groups.members.spawn"])
			}
			if mode != "sensitive" {
				require.Empty(t, bySlug["human.notify"])
			}
		})
	}
}

func TestFederation_PermissionCarrySourceGateAndLiveExport(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("source")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "carry-source", "carry-pane", cwd)
	f.HaveMember("source", source)
	aid, err := db.AgentIDForConv(source)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentInitialSpawnConfig(aid, `{"permission_overrides":{"groups.members.spawn":"grant","human.notify":"grant"}}`))
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "groups.members.spawn", `{"group":["source"]}`, "human"))
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "agent.share", `{"peer":["`+p.id.ID()+`/destination"]}`, "human"))
	request := func() int {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/share-agent", map[string]any{"peer": "bob", "group": "destination", "agent": "self", "carry_permissions": true}), source))
		return rec.Code
	}
	require.Equal(t, 403, request(), "sharing permission alone must not enable carry")
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "self.teleport.permissions", `{"peer":["`+p.id.ID()+`/other"]}`, "human"))
	require.Equal(t, 403, request())
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "self.teleport.permissions", `{"peer":["`+p.id.ID()+`/destination"]}`, "human"))
	require.Equal(t, 200, request())
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&carry_permissions=true", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	b, err := agentbundle.Decode(rec.Body.Bytes())
	require.NoError(t, err)
	defer func() { _ = b.Close() }()
	require.True(t, b.Manifest.Agent.CarryPermissions)
	var spawnRows []agentbundle.Permission
	for _, row := range b.Manifest.Agent.Permissions {
		require.NotEqual(t, "human.notify", row.Slug, "revoked initial grant must not be exported for carry")
		if row.Slug == "groups.members.spawn" {
			spawnRows = append(spawnRows, row)
		}
	}
	require.Len(t, spawnRows, 1)
	require.JSONEq(t, `{"group":["source"]}`, string(spawnRows[0].Scope))
}
