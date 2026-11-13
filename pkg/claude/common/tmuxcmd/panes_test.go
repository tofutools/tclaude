package tmuxcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPaneAndOptionVerbArgv(t *testing.T) {
	repo := Session("repo")
	tests := []struct {
		name string
		call func(Client)
		want []string
	}{
		{"display on the active pane keeps the colon", func(c Client) { _, _ = c.Display(repo.ActivePane(), "#{pane_dead}") },
			[]string{"display-message", "-p", "-t", "=repo:", "#{pane_dead}"}},
		{"display on the first pane", func(c Client) { _, _ = c.Display(repo.FirstPane(), "#{pane_id}") },
			[]string{"display-message", "-p", "-t", "=repo:0.0", "#{pane_id}"}},
		{"a session id is a pane target as it is", func(c Client) { _, _ = c.Display(SessionID("$3").ActivePane(), "#{pane_dead}") },
			[]string{"display-message", "-p", "-t", "$3", "#{pane_dead}"}},
		{"pane status", func(c Client) { _, _ = c.PaneStatus(PaneID("%4")) },
			[]string{"display-message", "-p", "-t", "%4", "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}"}},
		{"all panes", func(c Client) { _, _ = c.AllPanes() },
			[]string{"list-panes", "-a", "-F", "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{@tclaude_exit_generation}"}},
		{"a window's panes", func(c Client) { _, _ = c.ListPanes(repo.CurrentWindow(), "#{pane_pid}") },
			[]string{"list-panes", "-t", "=repo:", "-F", "#{pane_pid}"}},
		{"sessions", func(c Client) { _, _ = c.ListSessions("#{session_name}") },
			[]string{"list-sessions", "-F", "#{session_name}"}},
		{"windows", func(c Client) { _, _ = c.ListAllWindows("#{window_id}") },
			[]string{"list-windows", "-a", "-F", "#{window_id}"}},
		{"pane option", func(c Client) { _ = c.SetPaneRemainOnExit(repo.FirstPane(), true) },
			[]string{"set-option", "-p", "-t", "=repo:0.0", "remain-on-exit", "on"}},
		{"pane option off", func(c Client) { _ = c.SetPaneRemainOnExit(repo.ActivePane(), false) },
			[]string{"set-option", "-p", "-t", "=repo:", "remain-on-exit", "off"}},
		{"exit generation", func(c Client) { _ = c.SetPaneExitGeneration(repo.FirstPane(), "abc") },
			[]string{"set-option", "-p", "-t", "=repo:0.0", "@tclaude_exit_generation", "abc"}},
		{"clear exit generation", func(c Client) { _ = c.ClearPaneExitGeneration(repo.FirstPane()) },
			[]string{"set-option", "-p", "-u", "-t", "=repo:0.0", "@tclaude_exit_generation"}},
		{"pane-died hook", func(c Client) { _ = c.SetPaneDiedHook(repo.FirstPane(), "run-shell x") },
			[]string{"set-hook", "-p", "-t", "=repo:0.0", "pane-died", "run-shell x"}},
		{"clear pane-died hook", func(c Client) { _ = c.ClearPaneDiedHook(repo.FirstPane()) },
			[]string{"set-hook", "-p", "-u", "-t", "=repo:0.0", "pane-died"}},
		{"session option", func(c Client) { _ = c.SetOption(SessionScope(repo), "mouse", "on") },
			[]string{"set-option", "-t", "=repo:", "mouse", "on"}},
		{"window option", func(c Client) { _ = c.SetOption(WindowScope(WindowID("@2")), "pane-border-status", "top") },
			[]string{"set-option", "-w", "-t", "@2", "pane-border-status", "top"}},
		{"unset window option", func(c Client) { _ = c.UnsetOption(WindowScope(WindowID("@2")), "pane-border-status") },
			[]string{"set-option", "-wu", "-t", "@2", "pane-border-status"}},
		{"global option", func(c Client) { _ = c.SetOption(GlobalScope(), "exit-empty", "off") },
			[]string{"set-option", "-g", "exit-empty", "off"}},
		{"unset global option", func(c Client) { _ = c.UnsetOption(GlobalScope(), "exit-empty") },
			[]string{"set-option", "-gu", "exit-empty"}},
		{"pane-died support probe", func(c Client) { _ = c.PaneDiedHookSupported() },
			[]string{"show-hooks", "-g", "pane-died"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := script(t, "exit 0")
			tt.call(Local())
			require.Len(t, fake.calls, 1)
			assert.Equal(t, tt.want, fake.calls[0])
		})
	}
}

func TestPaneStatus(t *testing.T) {
	script(t, "echo 'repo|%4|4242|0|||abc'")
	st, err := Local().PaneStatus(PaneID("%4"))
	require.NoError(t, err)
	assert.Equal(t, PaneStatus{Session: "repo", PaneID: "%4", PID: 4242, ExitGeneration: "abc"}, st)

	script(t, "echo 'repo|%4|4242|1|137||abc'")
	st, err = Local().PaneStatus(PaneID("%4"))
	require.NoError(t, err)
	assert.Equal(t, PaneStatus{Session: "repo", PaneID: "%4", PID: 4242, Dead: true, ExitStatus: "137", ExitGeneration: "abc"}, st)

	// A dead pane whose child tmux has not reaped yet has neither a status
	// nor a signal, and that is not an error.
	script(t, "echo 'repo|%4|4242|1|||'")
	st, err = Local().PaneStatus(PaneID("%4"))
	require.NoError(t, err)
	assert.True(t, st.Dead)
	assert.Empty(t, st.ExitStatus)

	script(t, "echo 'repo|%4||1||KILL|'")
	st, err = Local().PaneStatus(PaneID("%4"))
	require.NoError(t, err)
	assert.Zero(t, st.PID, "a pid tmux did not report is zero, for the caller to judge")
	assert.Equal(t, "KILL", st.ExitSignal)
}

// A target tmux does not know is ErrNotFound on every tmux version: an error
// before 3.7, a reply with every field empty since.
func TestPaneStatusNotFoundOnEveryTmuxVersion(t *testing.T) {
	script(t, "echo \"can't find session: repo\" >&2; exit 1")
	_, err := Local().PaneStatus(Session("repo").ActivePane())
	require.ErrorIs(t, err, ErrNotFound)

	script(t, "echo '||||||'")
	_, err = Local().PaneStatus(Session("repo").ActivePane())
	require.ErrorIs(t, err, ErrNotFound)
	assert.NotErrorIs(t, err, ErrMalformedReply)

	script(t, "echo ''")
	_, err = Local().PanePID(Session("repo").FirstPane())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPaneStatusMalformedReplyIsNotNotFound(t *testing.T) {
	script(t, "echo 'repo|%4|4242'")
	_, err := Local().PaneStatus(PaneID("%4"))
	require.ErrorIs(t, err, ErrMalformedReply)
	assert.NotErrorIs(t, err, ErrNotFound)

	script(t, "echo 'not-a-pid'")
	_, err = Local().PanePID(PaneID("%4"))
	require.ErrorIs(t, err, ErrMalformedReply)
}

func TestAllPanesSkipsRowsInAnotherShape(t *testing.T) {
	script(t, `printf '%s\n' 'a|%1|75501|0|gen-a' 'dead|%2|100|1|gen-d' 'garbage' '|%5|1|0|x' 'nopid|%6||0|'`)
	panes, err := Local().AllPanes()
	require.NoError(t, err)
	assert.Equal(t, []PaneInfo{
		{Session: "a", PaneID: "%1", PID: 75501, ExitGeneration: "gen-a"},
		{Session: "dead", PaneID: "%2", PID: 100, Dead: true, ExitGeneration: "gen-d"},
		{Session: "nopid", PaneID: "%6"},
	}, panes)
}

func TestShowWindowOptionTellsUnsetFromEmpty(t *testing.T) {
	fake := script(t, "exit 0")
	value, set, err := Local().ShowWindowOption(WindowID("@2"), "@saved")
	require.NoError(t, err)
	assert.False(t, set)
	assert.Empty(t, value)
	assert.Equal(t, [][]string{{"show-options", "-w", "-q", "-t", "@2", "@saved"}}, fake.calls)

	fake = script(t, `echo '@saved "a b"'`)
	_, set, err = Local().ShowWindowOption(WindowID("@2"), "@saved")
	require.NoError(t, err)
	assert.True(t, set)
	require.Len(t, fake.calls, 2)
	assert.Equal(t, []string{"show-options", "-w", "-qv", "-t", "@2", "@saved"}, fake.calls[1])
}

func TestPaneDiedHookSupported(t *testing.T) {
	script(t, "echo 'pane-died'")
	assert.True(t, Local().PaneDiedHookSupported())
	script(t, "echo 'pane-died[0] run-shell x'")
	assert.False(t, Local().PaneDiedHookSupported(), "only the bare hook name counts, as before")
	script(t, "exit 1")
	assert.False(t, Local().PaneDiedHookSupported())
}

func TestEmptyPaneAndScopeTargetsAreRefused(t *testing.T) {
	fake := script(t, "exit 0")
	c := Local()
	_, err := c.Display(Session("").ActivePane(), "#{pane_id}")
	require.ErrorIs(t, err, ErrInvalidTarget)
	_, err = c.PaneStatus(Session("").FirstPane())
	require.ErrorIs(t, err, ErrInvalidTarget)
	require.ErrorIs(t, c.SetPaneRemainOnExit(PaneID(""), true), ErrInvalidTarget)
	require.ErrorIs(t, c.ClearPaneDiedHook(PaneID("")), ErrInvalidTarget)
	require.ErrorIs(t, c.SetOption(SessionScope(Session("")), "mouse", "on"), ErrInvalidTarget)
	require.ErrorIs(t, c.UnsetOption(WindowScope(WindowID("")), "x"), ErrInvalidTarget)
	_, err = c.ListPanes(Session("").CurrentWindow(), "#{pane_pid}")
	require.ErrorIs(t, err, ErrInvalidTarget)
	assert.Empty(t, fake.calls)
}
