package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestRunNewPreservesExplicitFastModeInheritForJoinGroup(t *testing.T) {
	stop := errors.New("join handler reached")
	previous := JoinGroupHandler
	t.Cleanup(func() { JoinGroupHandler = previous })

	var captured *NewParams
	JoinGroupHandler = func(params *NewParams) error {
		copy := *params
		captured = &copy
		return stop
	}

	err := runNew(&NewParams{
		Harness: harness.CodexName, FastMode: "inherit", JoinGroup: "test-group",
		ManagedLaunch: true,
	})
	require.ErrorIs(t, err, stop)
	require.NotNil(t, captured)
	assert.Equal(t, "inherit", captured.FastMode,
		"join-group must distinguish explicit inherit from an omitted flag")
}
