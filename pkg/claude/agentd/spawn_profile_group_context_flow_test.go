package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A spawn profile's include_group_default_context toggle used to be applied
// only by the two clients that merge a profile themselves before posting: the
// CLI's mergeProfileIntoSpawn and the dashboard's spawn form. A caller that
// merely NAMES a profile and leaves the flag out — the agentd TUI's spawn form
// does exactly that — got the group context anyway, however the profile was
// configured. These flow tests pin the daemon-side resolution that closes it:
// include_group_context now rides the same explicit > named > group default >
// global default tier stack as every other profile field.

// setGroupContext stores a group's shared startup context, failing the test if
// the write does not land.
func setGroupContext(t *testing.T, group, ctx string) {
	t.Helper()
	_, err := db.SetAgentGroupDefaultContext(group, ctx)
	require.NoError(t, err, "SetAgentGroupDefaultContext")
}

// Scenario: the spawn names a profile whose group-context toggle is off and
// sends no include_group_context flag of its own — the agentd TUI's request
// shape. The profile's "off" must decide, so the agent gets no briefing at all.
func TestSpawnProfileGroupContext_NamedProfileOptsOut(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "solo", "include_group_default_context": false,
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "profile": "solo",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	assert.Empty(t, rows, "a profile that opts out of the group context must get no briefing")
}

// Scenario: the same toggle, one tier down — the group's DEFAULT profile turns
// group context off and the spawn names no profile at all. Ambient
// configuration speaks for the fields nobody typed, group context included.
func TestSpawnProfileGroupContext_GroupDefaultProfileOptsOut(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

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
}

// Scenario: the profile says off, the request says on. An explicit flag is
// direct intent at this launch and outranks every profile tier — the same
// precedence the string and bool launch fields already have.
func TestSpawnProfileGroupContext_ExplicitRequestWins(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, "alpha", "You are part of Project Phoenix.")

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

// Scenario: the group's default profile targets another harness. Group-context
// inclusion is policy about what the new agent is TOLD, not about how its
// harness runs, so it is inherited across the vendor boundary — unlike a model
// slug, which a foreign default tier is not allowed to supply.
func TestSpawnProfileGroupContext_ForeignHarnessProfileStillSpeaks(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, "alpha", "GROUP GUIDANCE MUST BE OMITTED")

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
	setGroupContext(t, "alpha", "You are part of Project Phoenix.")

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
