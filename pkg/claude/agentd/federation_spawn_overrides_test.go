package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// What an operator approval launches with, given the fields the peer may
// still choose and the operator's own values.
func TestFedSpawnOverridesApplied(t *testing.T) {
	all := map[string]bool{"cwd": true, "harness": true, "model": true, "effort": true}
	asked := db.FederationSpawnOverrides{Cwd: "/srv/peer", Harness: "codex", Model: "gpt-5", Effort: "high"}
	for _, tc := range []struct {
		name     string
		allowed  map[string]bool
		operator fedSpawnApproveReq
		want     db.FederationSpawnOverrides
		dropped  []string
	}{
		{name: "everything still allowed", allowed: all, want: asked},
		{name: "operator values replace the request's", allowed: all, operator: fedSpawnApproveReq{Cwd: "/srv/mine", Model: "gpt-5-mini"},
			want: db.FederationSpawnOverrides{Harness: "codex", Effort: "high"}},
		{name: "the requested harness by hand keeps its model", allowed: all, operator: fedSpawnApproveReq{Harness: "codex"},
			want: db.FederationSpawnOverrides{Cwd: "/srv/peer", Model: "gpt-5", Effort: "high"}},
		{name: "another harness drops the model and effort chosen for codex", allowed: all, operator: fedSpawnApproveReq{Harness: "claude"},
			want: db.FederationSpawnOverrides{Cwd: "/srv/peer"}, dropped: []string{"model", "effort"}},
		{name: "another harness keeps a model the operator set", allowed: all, operator: fedSpawnApproveReq{Harness: "claude", Model: "opus"},
			want: db.FederationSpawnOverrides{Cwd: "/srv/peer"}, dropped: []string{"effort"}},
		{name: "a dropped harness takes model and effort along", allowed: map[string]bool{"model": true, "effort": true},
			want: db.FederationSpawnOverrides{}, dropped: []string{"cwd", "harness", "model", "effort"}},
		{name: "no grant left drops everything", allowed: map[string]bool{},
			want: db.FederationSpawnOverrides{}, dropped: []string{"cwd", "harness", "model", "effort"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := fedSpawnOverridesApplied(asked, tc.allowed, tc.operator)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.dropped, dropped)
		})
	}

	// A request that named no harness: the operator choosing one means the
	// peer's model was not chosen for it.
	got, dropped := fedSpawnOverridesApplied(db.FederationSpawnOverrides{Model: "opus"}, all, fedSpawnApproveReq{Harness: "codex"})
	require.Equal(t, db.FederationSpawnOverrides{}, got)
	require.Equal(t, []string{"model"}, dropped)
}

// Roles and a sandbox profile follow the same rule on an approval: applied
// while the peer may still choose them, dropped otherwise.
func TestFedSpawnOverridesAppliedRolesAndSandbox(t *testing.T) {
	asked := db.FederationSpawnOverrides{Roles: []string{"builder"}, SandboxProfile: "tight"}
	got, dropped := fedSpawnOverridesApplied(asked, map[string]bool{"roles": true, "sandbox": true}, fedSpawnApproveReq{})
	require.Equal(t, asked, got)
	require.Empty(t, dropped)
	got, dropped = fedSpawnOverridesApplied(asked, map[string]bool{}, fedSpawnApproveReq{})
	require.True(t, got.Empty())
	require.ElementsMatch(t, []string{"roles", "sandbox"}, dropped)
}

func TestFedSpawnLaunchSettingsApplyPerField(t *testing.T) {
	asked := db.FederationSpawnOverrides{Launch: map[string]json.RawMessage{
		"is_owner": json.RawMessage(`true`), "approval": json.RawMessage(`"plan"`), "prompt_refs": json.RawMessage(`[]`),
	}}
	got, dropped := fedSpawnOverridesApplied(asked, map[string]bool{"owner": true, "prompts": true}, fedSpawnApproveReq{})
	require.Equal(t, []string{"permissions"}, dropped)
	require.Equal(t, map[string]json.RawMessage{"is_owner": json.RawMessage(`true`), "prompt_refs": json.RawMessage(`[]`)}, got.Launch)
	require.ErrorContains(t, validateFedSpawnLaunch(map[string]json.RawMessage{"attachments": json.RawMessage(`[]`)}), "cannot set")
	require.Error(t, validateFedSpawnLaunch(map[string]json.RawMessage{"is_owner": json.RawMessage(`"yes"`)}))

	// An explicit empty survives into the launch body: the profile tiers
	// must not refill a prompt set or permission map sent empty.
	raw, err := fedSpawnLaunchBody(agent.SpawnRequest{Name: "w", Descr: "spawned for x"}, got.Launch)
	require.NoError(t, err)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &body))
	require.JSONEq(t, `[]`, string(body["prompt_refs"]))
	require.JSONEq(t, `true`, string(body["is_owner"]))
	require.JSONEq(t, `"spawned for x"`, string(body["descr"]))
}

// An approval that launches another harness drops the harness-specific
// launch settings, as it drops the requested model and effort.
func TestFedSpawnLaunchSettingsDropOnOtherHarness(t *testing.T) {
	asked := db.FederationSpawnOverrides{Harness: "codex", Launch: map[string]json.RawMessage{
		"approval": json.RawMessage(`"never"`), "is_owner": json.RawMessage(`true`),
	}}
	all := map[string]bool{"harness": true, "permissions": true, "owner": true}
	got, dropped := fedSpawnOverridesApplied(asked, all, fedSpawnApproveReq{Harness: "claude"})
	require.Equal(t, map[string]json.RawMessage{"is_owner": json.RawMessage(`true`)}, got.Launch)
	require.Contains(t, dropped, "approval")
	got, _ = fedSpawnOverridesApplied(asked, all, fedSpawnApproveReq{})
	require.Len(t, got.Launch, 2, "the requested harness keeps them")
	require.Contains(t, fedSpawnOverridesText(asked), `approval: "never"`, "the notice shows values")
}
