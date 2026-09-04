package agentd

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paste delivers a bracketed paste the way the terminal does: one message of
// its own, through Update, rather than the key presses that would have typed
// the same text.
func paste(m tuiModel, text string) tuiModel {
	updated, _ := m.Update(tea.PasteMsg{Content: text})
	return updated.(tuiModel)
}

// Every text field of the new-agent form takes a paste, and it lands at the
// cursor like typed text rather than replacing what is there.
func TestTUISpawnFormFieldsAcceptAPaste(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m = m.openSpawnForm()

	// Pasted text lands at the cursor, so a paste onto the front of a typed
	// name inserts rather than appending or replacing.
	m = typeInto(m, tuiFieldName, "reviewer")
	atStart, _ := m.handleSpawnKey(tea.KeyPressMsg{Code: tea.KeyHome})
	m = paste(atStart.(tuiModel), "senior-")
	assert.Equal(t, "senior-reviewer", m.form.name.Value())

	m = m.focusSpawnField(tuiFieldDir)
	m.form.dir.SetValue("")
	m = paste(m, "/tmp/repo")
	assert.Equal(t, "/tmp/repo", m.form.dir.Value())

	m = paste(m.focusSpawnField(tuiFieldBrief), "review the open PRs")
	assert.Equal(t, "review the open PRs", m.form.brief.Value())

	// The paste is text for the field, not keys for the form: it neither
	// submits nor moves off the field.
	assert.Equal(t, tuiModeSpawn, m.mode)
	assert.Equal(t, tuiFieldBrief, m.form.field)
}

// A brief copied out of a ticket arrives with its line breaks in it. These
// fields are single-line, so the text is folded onto one line rather than
// truncated at the first break or refused.
func TestTUISpawnPasteFoldsMultilineText(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m = m.openSpawnForm().focusSpawnField(tuiFieldBrief)

	m = paste(m, "review the open PRs\nthen report back")
	assert.Equal(t, "review the open PRs then report back", m.form.brief.Value())
}

// A pasted path edits the Directory field exactly as typing does, so the Tab
// candidate list retires with it — it answers a path the field no longer has.
func TestTUISpawnPasteRetiresTheDirCandidateList(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project-alpha"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project-beta"), 0o755))

	m := spawnFormOnDir(t, filepath.Join(root, "proj"))
	listed, _ := m.handleSpawnKey(tuiTabKey())
	m = listed.(tuiModel)
	require.NotEmpty(t, m.form.dirSuggestions)

	m = paste(m, "alpha")
	assert.Equal(t, filepath.Join(root, "project-alpha"), m.form.dir.Value())
	assert.Empty(t, m.form.dirSuggestions)
}

// The shell form's Directory field keeps the same bargain.
func TestTUIShellPasteRetiresTheDirCandidateList(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "alpha"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "alpine"), 0o755))

	m := operatorShellModel(root)
	m.width = 120
	opened, _ := m.handleKey(tuiKey("s"))
	m = opened.(tuiModel)
	m.shell.dir.SetValue(filepath.Join(root, "alp"))

	listed, _ := m.handleKey(tuiTabKey())
	m = listed.(tuiModel)
	require.NotEmpty(t, m.shell.dirSuggestions)

	m = paste(m, "ha")
	assert.Equal(t, filepath.Join(root, "alpha"), m.shell.dir.Value())
	assert.Empty(t, m.shell.dirSuggestions)
}

// The branch follows a pasted name as it follows a typed one, and a paste
// into the branch itself is the operator naming it — which ends the sync.
func TestTUISpawnPasteKeepsTheWorktreeBranchContract(t *testing.T) {
	m := spawnFormOnWorktree(t, nil)
	m = paste(m.focusSpawnField(tuiFieldName), "reviewer")
	assert.Equal(t, "reviewer", m.form.branch.Value())
	assert.True(t, m.form.branchSynced)

	m = paste(m.focusSpawnField(tuiFieldWorktreeBranch), "-fix")
	assert.Equal(t, "reviewer-fix", m.form.branch.Value())
	assert.False(t, m.form.branchSynced, "an edited branch stops following the name")

	m = paste(m.focusSpawnField(tuiFieldName), "-2")
	assert.Equal(t, "reviewer-fix", m.form.branch.Value())
}

// Both fields of the shell form take a paste too.
func TestTUIShellFormFieldsAcceptAPaste(t *testing.T) {
	m := operatorShellModel("/home/op")
	updated, _ := m.handleKey(tuiKey("s"))
	m = updated.(tuiModel)

	m.shell.dir.SetValue("")
	m = paste(m, "/tmp/scratch")
	assert.Equal(t, "/tmp/scratch", m.shell.dir.Value())

	m = paste(m.moveShellField(1), "build logs")
	assert.Equal(t, tuiShellFieldLabel, m.shell.field)
	assert.Equal(t, "build logs", m.shell.label.Value())
}

// Outside the forms there is no field to paste into. A paste is not a way to
// press the listing's keys, so the content is dropped rather than acted on.
func TestTUIPasteOutsideAFormIsDropped(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev"}, {Name: "ops"}}

	listing := paste(m, "q")
	assert.Equal(t, tuiModeList, listing.mode)
	assert.Empty(t, listing.notice)

	// A choice field has no text to paste into either, and the picker must
	// not move under it.
	form := paste(m.openSpawnForm().focusSpawnField(tuiFieldGroup), "ops")
	assert.Equal(t, 0, form.form.groupIdx)
}

// The fields take the terminal's paste but not textinput's ctrl+v, which
// reads the host's system clipboard — a console a harness can drive through
// tmux send-keys must not have a keystroke that pulls whatever the human last
// copied into a field it can read back.
func TestTUIFormFieldsDoNotReadTheSystemClipboard(t *testing.T) {
	spawn := newTUIModel(nil).openSpawnForm()
	opened, _ := operatorShellModel("/home/op").handleKey(tuiKey("s"))
	shell := opened.(tuiModel)

	for name, enabled := range map[string]bool{
		"name":        spawn.form.name.KeyMap.Paste.Enabled(),
		"dir":         spawn.form.dir.KeyMap.Paste.Enabled(),
		"branch":      spawn.form.branch.KeyMap.Paste.Enabled(),
		"brief":       spawn.form.brief.KeyMap.Paste.Enabled(),
		"shell dir":   shell.shell.dir.KeyMap.Paste.Enabled(),
		"shell label": shell.shell.label.KeyMap.Paste.Enabled(),
	} {
		assert.False(t, enabled, "the %s field should not bind a clipboard read", name)
	}
}
