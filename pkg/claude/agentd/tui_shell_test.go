package agentd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// stubStartShell swaps the shell-session launch for a recorder, so the form can
// be driven without a tmux server, and returns what the console asked for.
func stubStartShell(t *testing.T, created session.ShellSession, err error) *tuiShellRecord {
	t.Helper()
	rec := &tuiShellRecord{}
	prev := tuiStartShell
	tuiStartShell = func(dir, label string) (session.ShellSession, error) {
		rec.called = true
		rec.dir, rec.label = dir, label
		return created, err
	}
	t.Cleanup(func() { tuiStartShell = prev })
	return rec
}

type tuiShellRecord struct {
	called bool
	dir    string
	label  string
}

// operatorShellModel is a console the daemon treats as the human, on the
// daemon's own host — the only console the shell launch is offered to.
func operatorShellModel(startupDir string) tuiModel {
	m := newTUIModel(nil)
	m.operator = true
	m.startupDir = startupDir
	return m
}

// The directory field starts where the console did, so enter-to-accept starts
// the shell in the directory the operator was standing in.
func TestTUIShellFormStartsInTheConsolesOwnDirectory(t *testing.T) {
	m := operatorShellModel("/home/op/src/tclaude")
	updated, _ := m.handleKey(tuiKey("s"))
	got := updated.(tuiModel)

	require.Equal(t, tuiModeShell, got.mode)
	assert.Equal(t, "/home/op/src/tclaude", got.shell.dir.Value())
	view := got.renderShellForm()
	assert.Contains(t, view, "/home/op/src/tclaude")
	assert.Contains(t, view, "where this console was started")
	assert.Contains(t, view, "Label:")
}

// Enter hands the typed directory and label to the launch, and the console
// goes straight to the new pane — the reason the operator started it.
func TestTUIShellFormStartsAndAttaches(t *testing.T) {
	rec := stubStartShell(t, session.ShellSession{
		SessionID:   "scratch",
		TmuxSession: "tc-scratch",
		Cwd:         "/home/op/src/other",
	}, nil)
	attached := stubAttach(t)

	m := operatorShellModel("/home/op/src/tclaude")
	updated, _ := m.handleKey(tuiKey("s"))
	got := updated.(tuiModel)
	got.shell.dir.SetValue("/home/op/src/other")
	got = got.moveShellField(1)
	got.shell.label.SetValue("scratch")

	submitted, cmd := got.handleKey(tuiEnterKey())
	got = submitted.(tuiModel)
	require.Equal(t, tuiModeList, got.mode, "the form closes and the launch runs behind the list")
	assert.True(t, got.startingShell)
	assert.Contains(t, got.summaryLine(), "starting a shell…")
	require.NotNil(t, cmd)

	msg, ok := cmd().(tuiShellStartedMsg)
	require.True(t, ok)
	require.True(t, rec.called)
	assert.Equal(t, "/home/op/src/other", rec.dir)
	assert.Equal(t, "scratch", rec.label)

	done, attachCmd := got.Update(msg)
	final := done.(tuiModel)
	assert.False(t, final.startingShell)
	assert.Contains(t, final.notice, "tc-scratch")
	assert.Contains(t, final.notice, "/home/op/src/other")
	require.NotNil(t, attachCmd)
	attachCmd()
	assert.True(t, attached.called)
	assert.Equal(t, "tc-scratch", attached.session)
}

// A launch that fails says why and leaves the console usable, rather than
// stranding it on "starting…".
func TestTUIShellStartFailureIsReported(t *testing.T) {
	m := operatorShellModel("/home/op")
	m.startingShell = true
	updated, cmd := m.Update(tuiShellStartedMsg{err: errors.New("tmux not found")})
	got := updated.(tuiModel)

	assert.Nil(t, cmd)
	assert.False(t, got.startingShell)
	assert.Contains(t, got.notice, "tmux not found")
}

// Tab completes a path the operator has typed into, and moves to Label on the
// field as the form left it — the same contract the new-agent form has, and
// the only way to reach Label at all.
func TestTUIShellDirTabCompletes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workspace"), 0o755))

	m := operatorShellModel(root)
	updated, _ := m.handleKey(tuiKey("s"))
	got := updated.(tuiModel)

	// Untouched: tab is the next-field key.
	moved, _ := got.handleKey(tuiTabKey())
	assert.Equal(t, tuiShellFieldLabel, moved.(tuiModel).shell.field)

	got.shell.dir.SetValue(filepath.Join(root, "work"))
	completed, _ := got.handleKey(tuiTabKey())
	final := completed.(tuiModel)
	assert.Equal(t, tuiShellFieldDir, final.shell.field, "completing must not move the focus")
	assert.Equal(t, filepath.Join(root, "workspace")+"/", final.shell.dir.Value())
}

// Ambiguous candidates are listed under the field, on the one line the form
// always emits for them.
func TestTUIShellDirTabListsAmbiguousCandidates(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "alpha"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "alpine"), 0o755))

	m := operatorShellModel(root)
	m.width = 120
	updated, _ := m.handleKey(tuiKey("s"))
	got := updated.(tuiModel)
	got.shell.dir.SetValue(filepath.Join(root, "alp"))

	completed, _ := got.handleKey(tuiTabKey())
	final := completed.(tuiModel)
	assert.Equal(t, []string{"alpha", "alpine"}, final.shell.dirSuggestions)
	assert.Contains(t, final.renderShellForm(), "alpine")

	// The list belongs to the Tab that produced it.
	typed, _ := final.handleKey(tuiKey("h"))
	assert.Empty(t, typed.(tuiModel).shell.dirSuggestions)
}

// The launch creates a tmux session on the daemon's host, in its filesystem and
// outside any agent sandbox, so a console the daemon does not treat as the
// human is refused — the same gate attaching to a pane has.
func TestTUIShellRefusedForANonOperatorConsole(t *testing.T) {
	rec := stubStartShell(t, session.ShellSession{}, nil)

	m := newTUIModel(nil) // operator stays false
	updated, _ := m.handleKey(tuiKey("s"))
	got := updated.(tuiModel)

	assert.Equal(t, tuiModeList, got.mode)
	assert.Contains(t, got.notice, "operator console")
	assert.False(t, rec.called)
	assert.NotContains(t, got.keyHintLine(), "s new shell")
}

// A remote console shares neither the daemon's host nor its filesystem, so it
// cannot run the launch and must not advertise it.
func TestTUIShellRefusedForARemoteConsole(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.capabilities = tuiCapabilities{attachAgent: true}

	updated, _ := m.handleKey(tuiKey("s"))
	got := updated.(tuiModel)

	assert.Equal(t, tuiModeList, got.mode)
	assert.Contains(t, got.notice, "does not share the daemon's host")
	assert.NotContains(t, got.keyHintLine(), "s new shell")
}

// The key is advertised where it works, and esc backs out of the form.
func TestTUIShellKeyIsAdvertisedAndCancellable(t *testing.T) {
	m := operatorShellModel("/home/op")
	assert.Contains(t, m.keyHintLine(), "s new shell")
	assert.Contains(t, m.renderHelp(), "plain interactive shell")

	updated, _ := m.handleKey(tuiKey("s"))
	got := updated.(tuiModel)
	require.Equal(t, tuiModeShell, got.mode)

	cancelled, _ := got.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, tuiModeList, cancelled.(tuiModel).mode)
}

// The help view names the shell form as a session rather than an agent, which
// is what explains why it never shows up in the listing.
func TestTUIHelpSeparatesShellSessionsFromAgents(t *testing.T) {
	help := operatorShellModel("/home/op").renderHelp()
	assert.Contains(t, help, "New shell session")
	assert.True(t, strings.Contains(help, "session, not an agent"))
	assert.Contains(t, help, "session attach")
}
