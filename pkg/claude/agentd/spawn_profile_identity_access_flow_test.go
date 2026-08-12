package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A spawn profile's identity fields (agent_name / role / descr /
// initial_message), its auto_focus toggle and its birth-time access controls
// (is_owner / permission_overrides) used to be applied only by the two clients
// that merge a profile themselves before posting: the CLI's
// mergeProfileIntoSpawn and the dashboard's spawn form. A caller that merely
// NAMES a profile got none of them — the agentd TUI's spawn form is exactly such
// a caller, which is how a profile with "Group owner = on" still produced an
// ordinary member.
//
// These flow tests pin the daemon-side resolution that closes it: each field now
// rides the same explicit > named > group default > global default tier stack
// #2045 gave include_group_context, with PRESENCE on the wire (not a non-empty
// value) marking the caller as having spoken.

// Scenario: the reported bug. The spawn names a profile whose "Group owner"
// toggle is on and sends no is_owner of its own — the agentd TUI's request
// shape. The new agent must come up owning the group.
func TestSpawnProfileAccess_NamedProfileMakesOwner(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "lead-profile", "is_owner": true,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"profile": "lead-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	assert.True(t, ownsGroup(t, g.ID, spawn.ConvID),
		"a profile with Group owner = on must produce an owner even when the caller sends no is_owner")
}

// Scenario: the same toggle one tier down — the group's DEFAULT profile turns
// owner on and the spawn names no profile at all. Ambient configuration speaks
// for the fields nobody typed, and the launch echo names the tier that decided,
// because an agent silently owning its group is exactly the action-at-a-distance
// that echo exists to surface.
func TestSpawnProfileAccess_GroupDefaultProfileMakesOwner(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "team-default", "is_owner": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "team-default").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	assert.True(t, ownsGroup(t, g.ID, spawn.ConvID), "the group default profile decides")
	assert.Contains(t, string(spawn.Raw),
		`group default profile \"team-default\" is_owner = true`,
		"resolved echo discloses which tier made the new agent an owner")
}

// Scenario: the safety direction. The dashboard posts is_owner explicitly —
// false for an unticked box — and an explicit choice at this launch outranks
// every profile tier. A visible checkbox must never be silently overridden by a
// profile the operator is not looking at.
func TestSpawnProfileAccess_ExplicitFalseBeatsProfile(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "lead-profile", "is_owner": true,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "lead-profile", "is_owner": false,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	assert.False(t, ownsGroup(t, g.ID, spawn.ConvID),
		"an explicit is_owner:false must beat the profile's true")
}

// Scenario: two tiers disagree and neither is the request. The NAMED profile is
// the one the spawner chose at this launch, so it outranks the group's ambient
// default.
func TestSpawnProfileAccess_NamedProfileBeatsGroupDefault(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "team-default", "is_owner": true,
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "plain", "is_owner": false,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "team-default").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "plain",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	assert.False(t, ownsGroup(t, g.ID, spawn.ConvID),
		"the named profile's false outranks the group default's true")
}

// Scenario: a profile's permission overrides reach a caller that only names it.
func TestSpawnProfileAccess_NamedProfileAppliesOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "granting",
		"permission_overrides": map[string]any{
			agentd.PermGroupsMembersSpawn: "grant",
			"self.rename":                 "deny",
		},
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"profile": "granting"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermGroupsMembersSpawn], "profile grant applied at birth")
	assert.Equal(t, "deny", overrides["self.rename"], "profile deny applied at birth")
}

// A saved role is a behavior/access preset independent of launch policy. A
// direct spawn that selects one gets its brief and grants, and defaults the
// membership label to the role name when no distinct label was supplied.
func TestSpawnRoleRef_AppliesBehaviorAndAccess(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name": "cold-auditor", "brief": "Review this change cold.",
		"permissions": []string{"human.notify"},
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "role_ref": "cold-auditor",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides["human.notify"])
	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "## Role")
	assert.Contains(t, msg.Body, "Review this change cold.")
	members := f.ListGroupMembers("alpha")
	for _, member := range members {
		if member.ConvID == spawn.ConvID {
			assert.Equal(t, "cold-auditor", member.Role)
			return
		}
	}
	t.Fatal("spawned member not found")
}

func TestSpawnProfileRoleRef_AppliesAndRoleDeleteIsGuarded(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name": "ux-tester", "brief": "Test the user-visible behavior.",
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "test-kit", "role_ref": "ux-tester", "model": "haiku",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "test-kit",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Contains(t, soleInboxMessage(t, spawn.ConvID).Body, "Test the user-visible behavior.")

	rec := humanReq(t, f, http.MethodDelete, "/v1/roles/ux-tester", nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "test-kit")
}

// Scenario: the dashboard posts permission_overrides as an empty object when the
// operator has cleared the editor. Presence is what marks the caller as having
// spoken, so the profile's overrides must not come back.
func TestSpawnProfileAccess_ExplicitEmptyOverridesBeatProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name":                 "granting",
		"permission_overrides": map[string]any{agentd.PermGroupsMembersSpawn: "grant"},
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "granting",
		"permission_overrides": map[string]any{},
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Empty(t, overrides, "a cleared editor keeps the agent's overrides empty")
}

// Scenario: the escalation gate still binds a profile-supplied owner flag. A
// profile the caller NAMED is direct intent, so an agent without groups.owners.manage is
// refused loudly rather than quietly getting a non-owner child — the same 403 an
// explicit is_owner already produced.
func TestSpawnProfileAccess_NamedProfileOwnerStillGated(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "lead-profile", "is_owner": true,
	}).Code)

	const spawner = "spwn-1111-2222-3333-4444"
	f.HaveMember("alpha", spawner)
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsMembersSpawn, "test"))

	spawn := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{
		"name": "henchman", "profile": "lead-profile",
	})
	assert.Equalf(t, http.StatusForbidden, spawn.Code,
		"a NAMED profile's is_owner is direct intent and must 403 without groups.owners.manage; body=%s", spawn.Raw)
}

// Scenario: the same flag one tier down is ambient configuration nobody typed at
// this launch. An operator's house default must not start refusing every spawn
// its own agents make, so the tier is skipped and disclosed instead of refused.
func TestSpawnProfileAccess_DefaultTierOwnerFallsThroughForUnprivilegedAgent(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "team-default", "is_owner": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "team-default").Code)

	const spawner = "spwn-2222-3333-4444-5555"
	f.HaveMember("alpha", spawner)
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsMembersSpawn, "test"))

	spawn := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code,
		"an ambient default profile must not fail the spawn; body=%s", spawn.Raw)

	assert.False(t, ownsGroup(t, g.ID, spawn.ConvID), "the unauthorized owner grant was skipped")
	assert.Contains(t, string(spawn.Raw), `is_owner ignored (caller lacks `+agentd.PermGroupsOwnersManage,
		"the skip is disclosed rather than silent")
}

// Scenario: identity. A caller whose form has no box for role or descr omits
// them, and the profile's fill them in.
func TestSpawnProfileIdentity_NamedProfileFillsRoleAndDescr(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "reviewer-profile", "role": "reviewer", "descr": "reviews the diff",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "reviewer-profile",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	members := f.ListGroupMembers("alpha")
	var found bool
	for _, m := range members {
		if m.ConvID == spawn.ConvID {
			found = true
			assert.Equal(t, "reviewer", m.Role, "the profile's role rides")
			assert.Equal(t, "reviews the diff", m.Descr, "the profile's descr rides")
		}
	}
	assert.True(t, found, "the spawned agent is listed in the group")
}

// Scenario: the dashboard posts role and descr on every spawn, empty string
// included. An operator who clears a profile-prefilled Role keeps it cleared.
func TestSpawnProfileIdentity_ExplicitBlankKeepsFieldBlank(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "reviewer-profile", "role": "reviewer", "descr": "reviews the diff",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "reviewer-profile", "role": "", "descr": "",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	var found bool
	for _, m := range f.ListGroupMembers("alpha") {
		if m.ConvID == spawn.ConvID {
			found = true
			assert.Empty(t, m.Role, "a posted empty role stays empty")
			assert.Empty(t, m.Descr, "a posted empty descr stays empty")
		}
	}
	// Without this the loop asserts nothing the day the member stops being
	// listed, and it is the only test pinning this direction of the rule.
	require.True(t, found, "the spawned agent is listed in the group")
}

// Scenario: a profile's agent_name names the agent when the caller sends none —
// the TUI leaves its Name box empty and gets the profile's.
func TestSpawnProfileIdentity_NamedProfileNamesTheAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "reviewer-profile", "agent_name": "reviewer",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"profile": "reviewer-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	f.AssertSpawnName(spawn.ConvID, "reviewer", 10*time.Second)
}

// Scenario: a profile's agent_name is held to looser rules than a spawn name, so
// one that is not a safe branch token is normalized on the way in — the same
// coercion a typed name gets.
func TestSpawnProfileIdentity_ProfileNameNormalized(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "reviewer-profile", "agent_name": "code reviewer",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"profile": "reviewer-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	f.AssertSpawnName(spawn.ConvID, "code-reviewer", 10*time.Second)
}

// Scenario: a profile's initial_message is a replaceable task default — it fills
// the brief for a caller that sent none, and yields to one that did.
func TestSpawnProfileIdentity_ProfileInitialMessageIsATaskDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "briefed", "initial_message": "Review the open PR queue.",
	}).Code)

	inherited := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "briefed",
	})
	require.Equalf(t, http.StatusOK, inherited.Code, "spawn body=%s", inherited.Raw)
	assert.Contains(t, soleInboxMessage(t, inherited.ConvID).Body, "Review the open PR queue",
		"the profile's brief fills an unspoken initial_message")

	own := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker2", "profile": "briefed", "initial_message": "Do this instead.",
	})
	require.Equalf(t, http.StatusOK, own.Code, "spawn body=%s", own.Raw)
	body := soleInboxMessage(t, own.ConvID).Body
	assert.Contains(t, body, "Do this instead")
	assert.NotContains(t, body, "Review the open PR queue", "the caller's own brief wins")
}

// Scenario: the auto-focus toggle. A profile that asks for a terminal window
// gets one for a caller that merely names it.
func TestSpawnProfileToggles_NamedProfileAutoFocuses(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	var gotCmd string
	t.Cleanup(agentd.SetOpenTerminalForTest(func(cmd string) error {
		gotCmd = cmd
		return nil
	}))

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "focused", "auto_focus": true,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "focused",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	assert.Contains(t, gotCmd, "session attach",
		"a profile's auto_focus must open the attach terminal")
}

// Scenario: remote control. The daemon resolves the profile's default under the
// group's remote-control policy, so unlike the CLI it can let a NAMED profile
// speak — which is what makes the toggle real for a caller that only names one.
func TestSpawnProfileToggles_NamedProfileArmsRemoteControl(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "reachable", "remote_control": true,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "reachable",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// Both halves, as remote_control_spawn_flow_test.go asserts them: the row's
	// best-known state is the production read the dashboard indicator and the
	// toggle's direction logic go through, and the sim spawner's record is the
	// only place that shows --remote-control actually reached the launch. A row
	// tagged on without the flag threaded would be a lie the DB alone cannot
	// catch.
	rc, err := db.RemoteControlForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.True(t, rc, "a named profile's remote_control must tag the new row armed")

	got, ok := f.World.SpawnRemoteControl(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.True(t, got, "and must thread --remote-control through the spawn path")
}

// Scenario: the permission-override twin of the owner gate. A profile the
// caller NAMED is direct intent, so an agent without permissions.grant is
// refused loudly rather than quietly getting an unprivileged child.
func TestSpawnProfileAccess_NamedProfileOverridesStillGated(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name":                 "granting",
		"permission_overrides": map[string]any{agentd.PermGroupsMembersSpawn: "grant"},
	}).Code)

	const spawner = "spwn-3333-4444-5555-6666"
	f.HaveMember("alpha", spawner)
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsMembersSpawn, "test"))

	spawn := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{
		"name": "henchman", "profile": "granting",
	})
	assert.Equalf(t, http.StatusForbidden, spawn.Code,
		"a NAMED profile's overrides are direct intent and must 403 without %s; body=%s",
		agentd.PermPermissionsGrant, spawn.Raw)
}

// Scenario: the same overrides one tier down are ambient configuration, so an
// unprivileged caller has them skipped and disclosed rather than the spawn
// failing — the twin of the is_owner fall-through.
func TestSpawnProfileAccess_DefaultTierOverridesFallThroughForUnprivilegedAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name":                 "team-default",
		"permission_overrides": map[string]any{agentd.PermGroupsMembersSpawn: "grant"},
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "team-default").Code)

	const spawner = "spwn-4444-5555-6666-7777"
	f.HaveMember("alpha", spawner)
	require.NoError(t, db.GrantAgentPermission(spawner, agentd.PermGroupsMembersSpawn, "test"))

	spawn := f.AsAgent(spawner).SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code,
		"an ambient default profile must not fail the spawn; body=%s", spawn.Raw)

	overrides, err := db.ListAgentPermissionOverridesForConv(spawn.ConvID)
	require.NoError(t, err)
	assert.Empty(t, overrides, "the unauthorized grants were skipped")
	assert.Contains(t, string(spawn.Raw),
		`permission_overrides ignored (caller lacks `+agentd.PermPermissionsGrant,
		"the skip is disclosed rather than silent")
}

// Scenario: an agent_name that cannot become a spawn name even after
// normalization (nothing in the safe charset survives) does not fail the launch
// — the tier is skipped and disclosed, and the agent gets its auto-generated
// label. Nobody typed this name at this launch.
func TestSpawnProfileIdentity_UnusableProfileNameSkippedNotFatal(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "emoji-profile", "agent_name": "🎉🎉",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"profile": "emoji-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code,
		"an unusable profile agent_name must not fail the spawn; body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), `name ignored (not a usable name)`,
		"the skip is disclosed rather than silent")
}

// Scenario: the same profile with auto-normalization turned OFF. The strict
// path rejects "code reviewer" for a typed name, so the profile tier that
// carries it is skipped here too rather than 400ing a launch nobody typed it
// into — an operator's house default must not start refusing spawns.
func TestSpawnProfileIdentity_ProfileNameSkippedWhenNormalizeOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "spacey", "agent_name": "code reviewer",
	}).Code)

	off := false
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{SpawnNameNormalize: &off}}))

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"profile": "spacey"})
	require.Equalf(t, http.StatusOK, spawn.Code,
		"a profile name the strict gate refuses must not fail the spawn; body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), `name ignored (not a usable name)`,
		"the skip is disclosed rather than silent")
}
