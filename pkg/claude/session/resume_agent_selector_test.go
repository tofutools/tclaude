package session

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	resumeSelectorConv  = "abcdef01-2345-6789-abcd-ef0123456789"
	resumeSelectorOther = "12345678-2345-6789-abcd-ef0123456789"
)

// seedResumeSelectorConv indexes a Claude conversation in project cwd.
func seedResumeSelectorConv(t *testing.T, convID, cwd string) {
	t.Helper()
	projectDir := convops.GetClaudeProjectPath(cwd)
	require.NoError(t, os.MkdirAll(projectDir, 0o755)) // the global resolver walks project dirs
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  projectDir,
		ProjectPath: cwd,
		Created:     "2026-01-01T00:00:00Z",
	}))
}

func setupResumeSelector(t *testing.T, resolver func(string) (string, error)) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	previous := AgentResumeResolver
	AgentResumeResolver = resolver
	t.Cleanup(func() { AgentResumeResolver = previous })
	return t.TempDir()
}

func TestExpandAgentResumeSelector(t *testing.T) {
	toConv := func(string) (string, error) { return resumeSelectorConv, nil }

	t.Run("agent id reaches the resolver whole and lifts the project scope", func(t *testing.T) {
		var got string
		cwd := setupResumeSelector(t, func(s string) (string, error) { got = s; return resumeSelectorConv, nil })
		seedResumeSelectorConv(t, resumeSelectorConv, "/some/other/project")

		params := &NewParams{Resume: "agt_85bdf803", Dir: cwd}
		require.NoError(t, expandAgentResumeSelector(params))
		assert.Equal(t, "agt_85bdf803", got, "the completion decoder must not cut an agent id at its underscore")
		assert.Equal(t, resumeSelectorConv, params.Resume)
		assert.True(t, params.Global)
	})

	t.Run("agent name resolves when no conversation matches", func(t *testing.T) {
		cwd := setupResumeSelector(t, toConv)
		seedResumeSelectorConv(t, resumeSelectorConv, "/some/other/project")

		params := &NewParams{Resume: "kivra-20261030-1355-1adb", Dir: cwd}
		require.NoError(t, expandAgentResumeSelector(params))
		assert.Equal(t, resumeSelectorConv, params.Resume)
	})

	t.Run("a conversation in scope wins over an agent", func(t *testing.T) {
		cwd := setupResumeSelector(t, toConv)
		seedResumeSelectorConv(t, resumeSelectorOther, cwd)

		params := &NewParams{Resume: "12345678", Dir: cwd}
		require.NoError(t, expandAgentResumeSelector(params))
		assert.Equal(t, "12345678", params.Resume)
		assert.False(t, params.Global)
	})

	t.Run("a miss is left for runNew to report", func(t *testing.T) {
		cwd := setupResumeSelector(t, func(string) (string, error) { return "", nil })

		params := &NewParams{Resume: "nobody", Dir: cwd}
		require.NoError(t, expandAgentResumeSelector(params))
		assert.Equal(t, "nobody", params.Resume)
	})

	t.Run("an agent without a transcript gets a specific error", func(t *testing.T) {
		cwd := setupResumeSelector(t, toConv)

		err := expandAgentResumeSelector(&NewParams{Resume: "agt_85bdf803", Dir: cwd})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "before its first message")
	})

	t.Run("resolver errors surface", func(t *testing.T) {
		cwd := setupResumeSelector(t, func(string) (string, error) { return "", errors.New("no agent matches") })

		err := expandAgentResumeSelector(&NewParams{Resume: "agt_85bdf803", Dir: cwd})
		require.ErrorContains(t, err, "no agent matches")
	})

	t.Run("managed launches pass their id through", func(t *testing.T) {
		cwd := setupResumeSelector(t, toConv)

		params := &NewParams{Resume: "agt_85bdf803", Dir: cwd, ManagedLaunch: true}
		require.NoError(t, expandAgentResumeSelector(params))
		assert.Equal(t, "agt_85bdf803", params.Resume)
	})
}
