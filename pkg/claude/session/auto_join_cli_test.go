package session

import (
	"bytes"
	"errors"
	"testing"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/common"
)

var (
	errTestJoined = errors.New("test: joined a group")
	errTestSolo   = errors.New("test: started solo")
)

type autoJoinOutcome struct {
	err    error // nil only if neither a join nor a solo start was reached
	params *NewParams
	notice string
}

// runAutoJoinCLI parses args exactly as `tclaude` / `session new` does and
// runs RunNewFromCommand from a directory whose group is matchedGroup ("" for
// none). The group join and the solo tmux start are the two stubbed
// boundaries; everything between them is production code.
func runAutoJoinCLI(t *testing.T, matchedGroup string, args ...string) autoJoinOutcome {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // session.auto_join_group unset

	prevJoin, prevMatch, prevSolo, prevOut := JoinGroupHandler, AutomaticGroupMatcher, startSoloSession, autoJoinNoticeOut
	t.Cleanup(func() {
		JoinGroupHandler, AutomaticGroupMatcher, startSoloSession, autoJoinNoticeOut = prevJoin, prevMatch, prevSolo, prevOut
	})
	var out autoJoinOutcome
	var notice bytes.Buffer
	autoJoinNoticeOut = &notice
	AutomaticGroupMatcher = func(*NewParams) (string, error) { return matchedGroup, nil }
	JoinGroupHandler = func(p *NewParams) error {
		if matchedGroup == "" && p.JoinGroup == "" {
			return ErrNoAutomaticGroupMatch
		}
		c := *p
		out.params = &c
		return errTestJoined
	}
	startSoloSession = func(p *NewParams, _ explicitLaunchFields) error {
		c := *p
		out.params = &c
		return errTestSolo
	}

	cmd := boa.CmdT[NewParams]{
		Use:         "tclaude",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *NewParams, cmd *cobra.Command, _ []string) {
			out.err = RunNewFromCommand(params, cmd)
		},
	}.ToCobra()
	cmd.SetArgs(append([]string{"--dir", t.TempDir()}, args...))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())
	out.notice = notice.String()
	return out
}

// TestAutoJoinCLIUserTable pins every row of the original report, run from a
// group's default directory with session.auto_join_group unset.
func TestAutoJoinCLIUserTable(t *testing.T) {
	const labelNotice = "--label keeps this session solo, so it won't join group \"kivra\". " +
		"To join with a custom name, use -n NAME instead of --label; to stay solo, pass --auto-join-group=false.\n"

	t.Run("name joins", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-n", "autojoin-test")
		require.ErrorIs(t, got.err, errTestJoined)
		assert.Equal(t, "autojoin-test", got.params.Name)
		assert.Empty(t, got.notice)
	})
	t.Run("initial-prompt now joins and carries the prompt", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-n", "t1", "--initial-prompt", "hi")
		require.ErrorIs(t, got.err, errTestJoined)
		// agent.RunJoinGroup turns it into the startup brief (pinned there).
		assert.Equal(t, "hi", got.params.InitialPrompt)
	})
	t.Run("label stays solo with a notice", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-n", "t2", "-l", "t2")
		require.ErrorIs(t, got.err, errTestSolo)
		assert.Equal(t, "t2", got.params.Label)
		assert.False(t, got.params.AutoJoinGroup)
		assert.Equal(t, labelNotice, got.notice)
	})
	t.Run("sandbox-profile joins", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "--sandbox-profile", "elevated")
		require.ErrorIs(t, got.err, errTestJoined)
		assert.Equal(t, "elevated", got.params.SandboxProfile)
	})
	t.Run("initial-message joins", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-i", "hi")
		require.ErrorIs(t, got.err, errTestJoined)
		assert.Equal(t, "hi", got.params.InitialMessage)
	})
	t.Run("label with initial-prompt stays solo and keeps the harness prompt", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-l", "binder", "--initial-prompt", "hi")
		require.ErrorIs(t, got.err, errTestSolo)
		assert.Equal(t, "hi", got.params.InitialPrompt)
		assert.Equal(t, labelNotice, got.notice)
	})
	t.Run("label with initial-message is refused, not dropped", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-l", "binder", "-i", "brief")
		require.Error(t, got.err)
		assert.Nil(t, got.params, "neither a join nor a solo start may happen")
		assert.Equal(t, "--initial-message needs a group launch, but --label keeps this session solo, so it won't join group \"kivra\". "+
			"To join with a custom name, use -n NAME instead of --label; to stay solo, drop --initial-message", got.err.Error())
	})
}

func TestAutoJoinCLISoloFlagEdges(t *testing.T) {
	t.Run("sandbox-profile with label is refused", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-l", "x", "--sandbox-profile", "elevated")
		require.Error(t, got.err)
		assert.Nil(t, got.params)
		assert.Contains(t, got.err.Error(), "--sandbox-profile needs a group launch, but --label keeps this session solo")
	})
	t.Run("explicit auto-join-group with label is a conflict", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "--auto-join-group=true", "-l", "x")
		require.Error(t, got.err)
		assert.Nil(t, got.params)
		assert.Equal(t, "--auto-join-group asks to join a group, but --label keeps this session solo. "+
			"To join with a custom name, use -n NAME instead of --label; to stay solo, drop --auto-join-group", got.err.Error())
	})
	t.Run("explicit auto-join-group=false with label is quietly solo", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "--auto-join-group=false", "-l", "x")
		require.ErrorIs(t, got.err, errTestSolo)
		assert.Empty(t, got.notice)
	})
	t.Run("auto-join off refuses a group-only flag", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "--auto-join-group=false", "-i", "brief")
		require.Error(t, got.err)
		assert.Nil(t, got.params)
		assert.Contains(t, got.err.Error(), "--initial-message only applies when this launch joins a group, but auto-join is off")
	})
	t.Run("several solo flags are all named", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "-l", "x", "-w")
		require.ErrorIs(t, got.err, errTestSolo)
		assert.Equal(t, "--label and --wait-for-rate-limit keep this session solo, so it won't join group \"kivra\". "+
			"To join, drop --wait-for-rate-limit and use -n NAME instead of --label; to stay solo, pass --auto-join-group=false.\n", got.notice)
	})
	t.Run("no matching group: label is solo without a notice", func(t *testing.T) {
		got := runAutoJoinCLI(t, "", "-l", "x")
		require.ErrorIs(t, got.err, errTestSolo)
		assert.Empty(t, got.notice)
	})
	t.Run("no matching group: label with initial-message gives no false join hint", func(t *testing.T) {
		got := runAutoJoinCLI(t, "", "-l", "x", "-i", "brief")
		require.Error(t, got.err)
		assert.Nil(t, got.params)
		assert.Equal(t, "--initial-message needs a group launch, but --label keeps this session solo and no group matches this directory; "+
			"pass --join-group GROUP without --label, or drop --initial-message", got.err.Error())
	})
	t.Run("explicit join-group refuses a solo-only flag", func(t *testing.T) {
		got := runAutoJoinCLI(t, "", "--join-group", "kivra", "--permission-profile", "locked")
		require.Error(t, got.err)
		assert.Nil(t, got.params)
		assert.Equal(t, "--permission-profile applies to a solo session and can't be used with --join-group. To join, drop --permission-profile", got.err.Error())
	})
	t.Run("shell launch refuses a typed group-only flag", func(t *testing.T) {
		got := runAutoJoinCLI(t, "kivra", "--shell", "-i", "brief")
		require.Error(t, got.err)
		assert.Nil(t, got.params)
		assert.Contains(t, got.err.Error(), "--initial-message only applies when a launch joins a group, and a resume, shell or pass-through launch never does")
	})
	t.Run("no matching group: initial-message keeps the unmatched error", func(t *testing.T) {
		got := runAutoJoinCLI(t, "", "-i", "brief")
		require.Error(t, got.err)
		assert.Nil(t, got.params)
		assert.Contains(t, got.err.Error(), "no group matches this directory")
	})
}
