package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The console posts a profile NAME and lets the daemon resolve what that
// profile means — harness, model, role, descr, the brief, group context, the
// owner flag, permission overrides. Two fields it cannot leave to the daemon:
// the agent's name, because the worktree branch is cut from it before the spawn
// request goes out, and sync_worktree, which decides whether a worktree is cut
// at all. These tests pin that pair.

// Picking a profile fills the Name field with its agent_name, so the operator
// sees the name their agent (and its branch) will get rather than an empty box
// that silently resolves to something else.
func TestTUISpawnProfilePrefillsTheAgentName(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "reviewer-kit", AgentName: "reviewer"}}
	m = m.openSpawnForm()

	m = m.focusSpawnField(tuiFieldProfile).cycleChoice(1)
	require.Equal(t, "reviewer-kit", m.selectedProfile())
	assert.Equal(t, "reviewer", m.form.name.Value())
}

// A name the operator typed is theirs: cycling on to another profile must not
// overwrite it, the same contract the directory prefill has with the group
// picker.
func TestTUISpawnProfileLeavesATypedNameAlone(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "reviewer-kit", AgentName: "reviewer"}}
	m = m.openSpawnForm()
	m = typeInto(m, tuiFieldName, "mine")

	m = m.focusSpawnField(tuiFieldProfile).cycleChoice(1)
	assert.Equal(t, "mine", m.form.name.Value())
}

// A profile whose sync_worktree is on gives each of its agents a worktree of
// its own, named after the agent — so picking it arms the picker and the branch
// arrives already answered.
func TestTUISpawnProfileSyncWorktreeArmsThePicker(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{
		{Name: "worktree-kit", AgentName: "reviewer", SyncWorktree: new(true)},
	}
	m = m.openSpawnForm()
	require.False(t, m.creatingWorktree(), "the form opens on (none)")

	m = m.focusSpawnField(tuiFieldProfile).cycleChoice(1)
	assert.True(t, m.creatingWorktree(), "sync_worktree asks for a worktree of the agent's own")
	assert.Equal(t, "reviewer", m.form.branch.Value(), "and the branch follows the profile's name")
}

// A profile that says nothing about worktrees leaves the picker alone: unset is
// not "off".
func TestTUISpawnProfileSilentOnWorktreesChangesNothing(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "plain-kit"}}
	m = m.openSpawnForm()
	m = m.focusSpawnField(tuiFieldWorktree).cycleChoice(1)
	require.True(t, m.creatingWorktree())

	m = m.focusSpawnField(tuiFieldProfile).cycleChoice(1)
	assert.True(t, m.creatingWorktree(), "an unset toggle must not disarm the picker")
}

// Once the operator has cycled the worktree picker themselves it is theirs, and
// a profile's toggle no longer moves it.
func TestTUISpawnProfileDoesNotOverrideATouchedWorktreePicker(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "worktree-kit", SyncWorktree: new(false)}}
	m = m.openSpawnForm()
	m = m.focusSpawnField(tuiFieldWorktree).cycleChoice(1)
	require.True(t, m.creatingWorktree())

	m = m.focusSpawnField(tuiFieldProfile).cycleChoice(1)
	assert.True(t, m.creatingWorktree(), "the picker is the operator's once they have set it")
}

// A console that may not cut worktrees is not offered the field at all, so a
// profile cannot arm one behind its back (see canCreateWorktree).
func TestTUISpawnProfileSyncWorktreeIgnoredWithoutTheOperatorGate(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = false
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{
		{Name: "worktree-kit", AgentName: "reviewer", SyncWorktree: new(true)},
	}
	m = m.openSpawnForm()

	m = m.focusSpawnField(tuiFieldProfile).cycleChoice(1)
	assert.False(t, m.creatingWorktree(), "an agent-class console cannot be given a worktree by a profile")
}

// Cycling back to "(default)" names no profile, so it takes back the name the
// last one prefilled — a form that no longer explains a value should not still
// be carrying it.
func TestTUISpawnDefaultProfileTakesBackThePrefilledName(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "reviewer-kit", AgentName: "reviewer"}}
	m = m.openSpawnForm()

	m = m.focusSpawnField(tuiFieldProfile).cycleChoice(1)
	require.Equal(t, "reviewer", m.form.name.Value())

	m = m.cycleChoice(1)
	require.Empty(t, m.selectedProfile(), "back on the (default) sentinel")
	assert.Empty(t, m.form.name.Value(), "the prefilled name goes with the profile that supplied it")
}
