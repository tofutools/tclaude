package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These flow tests pin the tri-state the operator asked for (JOH): every launch
// settings level must tell an ACTIVELY-SET value apart from an OMITTED one. The
// bug they guard against: the daemon used to collapse an explicit `inherit` to
// "" (same as unset), so a group default profile silently re-filled its own
// value over the human's explicit "use my settings.json as-is" choice. The fix
// carries `inherit` as a first-class sentinel through resolve + the profile
// overlay, collapsing it to "no override" only at the harness's flag emission —
// so an explicit inherit survives the overlay while an omitted field still
// inherits the profile. The assertions sit at the Spawner boundary
// (World.SpawnSandbox / SpawnApproval / SpawnAskTimeout record the resolved
// value the production path threaded into `tclaude session new`).

// --- explicit inherit is NOT overwritten by a group default profile ---------

// TestSpawnInherit_AskTimeoutNotOverriddenByGroupDefault: a group default
// profile pins ask-timeout=5m; a spawn that explicitly chooses `inherit` keeps
// inherit — the profile's 5m must NOT win over the human's explicit choice.
func TestSpawnInherit_AskTimeoutNotOverriddenByGroupDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "p5m", "ask_user_question_timeout": "5m"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "p5m").Code, "set default_profile")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":                      "worker",
		"ask_user_question_timeout": "inherit",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	got, ok := f.World.SpawnAskTimeout(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "inherit", got,
		"an explicit inherit must survive the group default profile (not be re-filled with 5m)")
}

// TestSpawnInherit_SandboxNotOverriddenByGroupDefault: same for the OS sandbox —
// a group default profile forcing sandbox=on must not override an explicit
// inherit (use my settings.json sandbox config as-is).
func TestSpawnInherit_SandboxNotOverriddenByGroupDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "pon", "sandbox": "on"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "pon").Code, "set default_profile")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":    "worker",
		"sandbox": "inherit",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	got, ok := f.World.SpawnSandbox(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "inherit", got,
		"an explicit inherit must survive the group default profile (not be re-filled with on)")
}

// TestSpawnInherit_ApprovalNotOverriddenByGroupDefault: same for the permission
// mode — a group default profile pinning approval=plan must not override an
// explicit inherit (keep my settings.json permission rules + the agentd popup).
func TestSpawnInherit_ApprovalNotOverriddenByGroupDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "pplan", "approval": "plan"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "pplan").Code, "set default_profile")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":     "worker",
		"approval": "inherit",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	got, ok := f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "inherit", got,
		"an explicit inherit must survive the group default profile (not be re-filled with plan)")
}

// --- an OMITTED field still inherits the group default profile --------------
//
// The counterpart guard: the tri-state fix must NOT break the normal profile
// fill. A spawn that leaves the field OUT (distinct from choosing inherit) still
// inherits the profile's value, so the profile default keeps working.

// TestSpawnOmitted_SandboxInheritsGroupDefault: no sandbox in the request →
// inherits the profile's sandbox=on (an omitted field is still filled).
func TestSpawnOmitted_SandboxInheritsGroupDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "pon", "sandbox": "on"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "pon").Code, "set default_profile")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	got, ok := f.World.SpawnSandbox(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "on", got, "an omitted sandbox must inherit the group default profile's on")
}

// TestSpawnOmitted_ApprovalInheritsGroupDefault: no approval in the request →
// inherits the profile's approval=plan.
func TestSpawnOmitted_ApprovalInheritsGroupDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "pplan", "approval": "plan"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "pplan").Code, "set default_profile")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	got, ok := f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "plan", got, "an omitted approval must inherit the group default profile's plan")
}
