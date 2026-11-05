package agentd

import (
	"errors"
	"io"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// These pin the outcomes of tuiModel.Update that no other test reaches,
// before Update is split into one handler per message: the retire, take-
// offline and start results (failed, outcome unknown, done), a shell session
// that started, and a resolved worktree handing its form over to the spawn.
// Each asserts on the visible model: the busy flag, the notice, whether a
// refresh (and a reconciliation) began, and whether a command was returned.

// idleTUIModel is a console whose first refresh has already landed.
func idleTUIModel() tuiModel {
	m := newTUIModel(nil)
	m.refreshing = false
	return m
}

func TestTUIUpdate_MutationResults(t *testing.T) {
	ambiguous := &tuiAmbiguousMutationError{err: io.ErrUnexpectedEOF, attempts: 2}
	for _, kind := range []struct {
		name   string
		busy   func(*tuiModel) *bool
		msg    func(err error) tea.Msg
		failed string
		unsure string
	}{
		{"retire", func(m *tuiModel) *bool { return &m.retiring },
			func(err error) tea.Msg { return tuiRetiredMsg{agent: "worker", err: err} },
			"Retire failed: refused", "Retire outcome unknown: the connection was lost after the request; refreshing before another action."},
		{"take offline", func(m *tuiModel) *bool { return &m.stopping },
			func(err error) tea.Msg { return tuiStoppedMsg{agent: "worker", err: err} },
			"Take offline failed: refused", "Take-offline outcome unknown: the connection was lost after the request; refreshing before another action."},
		{"start", func(m *tuiModel) *bool { return &m.resuming },
			func(err error) tea.Msg { return tuiResumedMsg{agent: "worker", err: err} },
			"Start failed: refused", "Start outcome unknown: the connection was lost after the request; refreshing before another action."},
	} {
		run := func(t *testing.T, err error) (tuiModel, tea.Cmd) {
			m := idleTUIModel()
			*kind.busy(&m) = true
			updated, cmd := m.Update(kind.msg(err))
			got := updated.(tuiModel)
			assert.False(t, *kind.busy(&got), "the action is no longer in flight")
			return got, cmd
		}
		t.Run(kind.name+" failed", func(t *testing.T) {
			got, cmd := run(t, errors.New("refused"))
			assert.Equal(t, kind.failed, got.notice)
			assert.False(t, got.refreshing)
			assert.False(t, got.reconcilingMutation)
			assert.Nil(t, cmd)
		})
		t.Run(kind.name+" outcome unknown", func(t *testing.T) {
			got, cmd := run(t, ambiguous)
			assert.Equal(t, kind.unsure, got.notice)
			assert.True(t, got.refreshing)
			assert.True(t, got.reconcilingMutation)
			assert.Equal(t, got.refreshGeneration, got.reconciliationRefreshGen,
				"the reconciliation waits for the refresh it began")
			assert.NotNil(t, cmd)
		})
		t.Run(kind.name+" done", func(t *testing.T) {
			got, cmd := run(t, nil)
			assert.NotEmpty(t, got.notice)
			assert.NotContains(t, got.notice, "failed")
			assert.True(t, got.refreshing, "the listing is refreshed at once")
			assert.False(t, got.reconcilingMutation)
			assert.NotNil(t, cmd)
		})
	}
}

func TestTUIUpdate_AStartedShellSessionIsAttached(t *testing.T) {
	created := session.ShellSession{SessionID: "shell-1", TmuxSession: "tclaude-shell-1", Cwd: "/work"}
	for _, tc := range []struct {
		name   string
		tmux   string
		notice string
	}{
		{"outside tmux", "", "Started shell session tclaude-shell-1 in /work. Attaching; detach (ctrl-b d) to come back."},
		{"inside tmux", "/tmp/tmux-pin/default,1,0", "Started shell session tclaude-shell-1 in /work. Switching to it…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmux)
			m := idleTUIModel()
			m.startingShell = true

			updated, cmd := m.Update(tuiShellStartedMsg{created: created})

			got := updated.(tuiModel)
			assert.False(t, got.startingShell)
			assert.Equal(t, tc.notice, got.notice)
			assert.NotNil(t, cmd, "the console goes to the new session")
		})
	}
}

func TestTUIUpdate_AResolvedWorktreeHandsItsFormToTheSpawn(t *testing.T) {
	wt := tuiWorktreeResponse{Path: "/work/feature", Branch: "feature", Created: true}
	for _, tc := range []struct {
		name     string
		formGen  uint64
		wantMode tuiMode
	}{
		{"the form that asked closes", 7, tuiModeList},
		{"a form opened since stays open", 6, tuiModeSpawn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := idleTUIModel()
			m.mode = tuiModeSpawn
			m.form.gen = 7
			m.spawning = true

			updated, cmd := m.Update(tuiWorktreeResolvedMsg{group: "dev", formGen: tc.formGen, wt: wt})

			got := updated.(tuiModel)
			require.NotNil(t, cmd, "the spawn is sent")
			assert.Equal(t, tc.wantMode, got.mode)
			assert.Equal(t, wt, got.spawnWorktree)
			assert.True(t, got.spawning, "the spawn is still in flight")
			assert.Equal(t, "Created worktree /work/feature on feature. Spawning in group dev…", got.notice)
		})
	}
}

// These pin handleKey's four confirm dialogs before they move into their own
// handler: which keys go through (only y; ctrl-c too for quit) and that every
// other key cancels back to the list and forgets the target.
func TestTUIHandleKey_ConfirmDialogs(t *testing.T) {
	keys := map[string]tea.KeyPressMsg{
		"y":      {Code: 'y', Text: "y"},
		"Y":      {Code: 'Y', Text: "Y"},
		"ctrl+c": {Code: 'c', Mod: tea.ModCtrl},
		"enter":  {Code: tea.KeyEnter},
		"n":      {Code: 'n', Text: "n"},
	}
	agentTarget := agentListRow(tuiAgentRow{ConvID: "c1", Title: "worker", Online: true})
	sessionTarget := tuiListRow{kind: tuiRowSession, session: tuiSessionRow{TmuxSession: "tclaude-shell-1"}}
	for _, tc := range []struct {
		name     string
		mode     tuiMode
		target   tuiListRow
		confirms []string
		busy     func(tuiModel) bool
		notice   string
	}{
		{"quit", tuiModeConfirmQuit, tuiListRow{}, []string{"y", "Y", "ctrl+c"}, nil, ""},
		{"take offline", tuiModeConfirmStop, agentTarget, []string{"y", "Y"},
			func(m tuiModel) bool { return m.stopping }, "Taking worker offline…"},
		{"end session", tuiModeConfirmKillSession, sessionTarget, []string{"y", "Y"},
			func(m tuiModel) bool { return m.killingSession }, "Ending session tclaude-shell-1…"},
		{"retire", tuiModeConfirmRetire, agentTarget, []string{"y", "Y"},
			func(m tuiModel) bool { return m.retiring }, "Retiring worker…"},
	} {
		for name, key := range keys {
			t.Run(tc.name+" "+name, func(t *testing.T) {
				m := idleTUIModel()
				m.mode = tc.mode
				m.lifecycleTarget = tc.target

				updated, cmd := m.handleKey(key)

				got := updated.(tuiModel)
				confirmed := false
				for _, c := range tc.confirms {
					confirmed = confirmed || c == name
				}
				switch {
				case confirmed && tc.mode == tuiModeConfirmQuit:
					require.NotNil(t, cmd)
					assert.Equal(t, tea.QuitMsg{}, cmd(), "quit is confirmed")
					assert.Equal(t, tuiModeConfirmQuit, got.mode)
				case confirmed:
					assert.Equal(t, tuiModeList, got.mode)
					assert.Equal(t, tuiListRow{}, got.lifecycleTarget)
					assert.True(t, tc.busy(got), "the confirmed action is in flight")
					assert.Equal(t, tc.notice, got.notice)
					assert.NotNil(t, cmd)
				default:
					assert.Equal(t, tuiModeList, got.mode, "any other key cancels")
					assert.Equal(t, tuiListRow{}, got.lifecycleTarget)
					if tc.busy != nil {
						assert.False(t, tc.busy(got))
					}
					assert.Empty(t, got.notice)
					assert.Nil(t, cmd)
				}
			})
		}
	}
}

// While an ambiguous mutation is being reconciled, a key that would start
// another one cancels the open dialog and says why.
func TestTUIHandleKey_ReconciliationCancelsAStaleDialog(t *testing.T) {
	m := idleTUIModel()
	m.reconcilingMutation = true
	m.mode = tuiModeConfirmRetire
	m.lifecycleTarget = agentListRow(tuiAgentRow{ConvID: "c1", Title: "worker"})

	updated, cmd := m.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})

	got := updated.(tuiModel)
	assert.Nil(t, cmd)
	assert.False(t, got.retiring, "nothing is retired")
	assert.Equal(t, tuiModeList, got.mode)
	assert.Equal(t, tuiListRow{}, got.lifecycleTarget)
	assert.Equal(t, "Waiting for a successful refresh to reconcile the previous action before another mutation.", got.notice)
}
