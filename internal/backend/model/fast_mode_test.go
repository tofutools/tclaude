package model

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestFastModeValidationAndTeamInheritance(t *testing.T) {
	for _, mode := range []FastMode{"", FastModeOn, FastModeOff} {
		require.NoError(t, mode.Validate("codex"))
	}
	require.Error(t, FastMode("invalid").Validate("codex"))
	for _, h := range []string{"claude", "opencode", "copilot"} {
		require.Error(t, FastModeOff.Validate(h))
		require.NoError(t, FastMode("").Validate(h))
	}
	d := DesiredConfiguration{Harness: "codex", FastMode: FastModeOn}
	require.Equal(t, FastModeOn, (&TeamProfileOverrides{}).Apply(d).FastMode)
	off := FastModeOff
	require.Equal(t, FastModeOff, (&TeamProfileOverrides{FastMode: &off}).Apply(d).FastMode)
	inherit := FastMode("")
	require.Empty(t, (&TeamProfileOverrides{FastMode: &inherit}).Apply(d).FastMode)
	h := "claude"
	require.Empty(t, (&TeamProfileOverrides{Harness: &h}).Apply(d).FastMode)
	require.False(t, d.Equal(DesiredConfiguration{Harness: "codex", FastMode: FastModeOff}))
}
