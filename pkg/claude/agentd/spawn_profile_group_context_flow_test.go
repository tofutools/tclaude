package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A spawn profile's include_group_default_context toggle used to be applied
// only by the two clients that merge a profile themselves before posting: the
// CLI's mergeProfileIntoSpawn and the dashboard's spawn form. A caller that
// merely NAMES a profile and leaves the flag out — the agentd TUI's spawn form
// does exactly that — got the group context anyway, however the profile was
// configured. These flow tests pin the daemon-side resolution that closes it:
// include_group_context now rides the same explicit > named > group default >
// global default tier stack as every other profile field.

// setGroupContext stores a group's shared startup context through the same
// PATCH the operator uses, so the test rides the production write path
// (normalization included) rather than reaching past it into the DB.
func setGroupContext(t *testing.T, f *testharness.Flow, group, ctx string) {
	t.Helper()
	require.Equal(t, http.StatusOK,
		patchGroup(t, f, group, map[string]any{"default_context": ctx}),
		"PATCH default_context")
}

// Scenario: the spawn names a profile whose group-context toggle is off and
// sends no include_group_context flag of its own — the agentd TUI's request
// shape. The profile's "off" must decide: the task brief still arrives, without
// the group's shared guidance folded into it.
func TestSpawnProfileGroupContext_NamedProfileOptsOut(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "solo", "include_group_default_context": false,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "solo",
		"initial_message": "Implement the requested change.",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Implement the requested change", "the task brief still rides")
	assert.NotContains(t, msg.Body, "GROUP GUIDANCE MUST BE OMITTED")
}

// Scenario: the same toggle, one tier down — the group's DEFAULT profile turns
// group context off and the spawn names no profile at all. Ambient
// configuration speaks for the fields nobody typed, group context included.
func TestSpawnProfileGroupContext_GroupDefaultProfileOptsOut(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "team-default", "include_group_default_context": false,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "team-default").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "initial_message": "Implement the requested change.",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Implement the requested change", "the task brief still rides")
	assert.NotContains(t, msg.Body, "GROUP GUIDANCE MUST BE OMITTED")

	// A tier nobody typed at this launch decided it, so the launch echo says
	// which one — an agent must not arrive unbriefed with no trace of why.
	assert.Contains(t, string(spawn.Raw),
		`group default profile \"team-default\" include_group_context = false`,
		"resolved echo discloses which tier decided")
}

// Scenario: the profile says off, the request says on. An explicit flag is
// direct intent at this launch and outranks every profile tier — the same
// precedence the string and bool launch fields already have.
func TestSpawnProfileGroupContext_ExplicitRequestWins(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "You are part of Project Phoenix.")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "solo", "include_group_default_context": false,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "solo", "include_group_context": true,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Project Phoenix", "an explicit true beats the profile's false")
}

// Scenario: the safety direction of the same rule — the request says off, the
// profile says on. A profile must never be able to force the group's guidance
// back onto an agent whose spawner explicitly declined it (`--no-group-context`
// / the dashboard's unticked checkbox).
func TestSpawnProfileGroupContext_ExplicitOptOutBeatsProfileOptIn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "chatty", "include_group_default_context": true,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "chatty",
		"initial_message":       "Implement the requested change.",
		"include_group_context": false,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Implement the requested change", "the task brief still rides")
	assert.NotContains(t, msg.Body, "GROUP GUIDANCE MUST BE OMITTED")
}

// Scenario: two tiers disagree and neither is the request. The NAMED profile is
// the one the spawner chose at this launch, so it outranks the group's ambient
// default — the same ordering the launch fields use.
func TestSpawnProfileGroupContext_NamedProfileBeatsGroupDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "You are part of Project Phoenix.")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "team-default", "include_group_default_context": false,
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "briefed", "include_group_default_context": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "team-default").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "briefed",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Project Phoenix", "the named profile outranks the group default")
}

// Scenario: the group's default profile targets another harness. Group-context
// inclusion is policy about what the new agent is TOLD, not about how its
// harness runs, so it is inherited across the vendor boundary — unlike a model
// slug, which a foreign default tier is not allowed to supply.
func TestSpawnProfileGroupContext_ForeignHarnessProfileStillSpeaks(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-default", "harness": "codex",
		"include_group_default_context": false,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "codex-default").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": "claude",
		"initial_message": "Implement the requested change.",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Implement the requested change", "the task brief still rides")
	assert.NotContains(t, msg.Body, "GROUP GUIDANCE MUST BE OMITTED")
}

// Scenario: nobody says anything about group context — no flag, no profile
// toggle. The long-standing default holds: every spawn path inherits the
// group's shared guidance.
func TestSpawnProfileGroupContext_SilentProfileKeepsDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "You are part of Project Phoenix.")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "quiet", "model": "sonnet",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "quiet",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Project Phoenix", "a silent profile leaves the default alone")
}

// Scenario: the last tier of the stack. Only the GLOBAL default profile says
// anything — no flag on the request, no named profile, no group default — so
// the house-wide setting is what decides. This is the tier furthest from the
// spawn, which is exactly why the echo has to name it.
func TestSpawnProfileGroupContext_GlobalDefaultProfileOptsOut(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "house-solo", "include_group_default_context": false,
	}).Code)
	require.Equalf(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/spawn-profile-default", map[string]any{"name": "house-solo"}).Code,
		"set global default profile")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "initial_message": "Implement the requested change.",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, "Implement the requested change", "the task brief still rides")
	assert.NotContains(t, msg.Body, "GROUP GUIDANCE MUST BE OMITTED")
	assert.Contains(t, string(spawn.Raw),
		`global default profile \"house-solo\" include_group_context = false`,
		"resolved echo discloses which tier decided")
}
