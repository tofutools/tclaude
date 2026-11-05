package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The session row's key: an explicit label beats a resumed conversation's
// full id, which beats a random synthetic one.
func TestResolveLaunchIdentityKeyPriority(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const conv = "0b9d9a6e-3c8c-4f43-9d0c-2b8b6f1e7a10"
	cwd := t.TempDir()
	gemini := harness.MustGet(harness.GeminiName)

	id, err := resolveLaunchIdentity(gemini, &NewParams{Resume: conv, Label: "spwn-label"}, cwd)
	require.NoError(t, err)
	assert.Equal(t, launchIdentity{fullConvID: conv, sessionID: "spwn-label", cwd: cwd}, id)

	id, err = resolveLaunchIdentity(gemini, &NewParams{Resume: conv}, cwd)
	require.NoError(t, err)
	assert.Equal(t, launchIdentity{fullConvID: conv, sessionID: conv, cwd: cwd}, id)

	id, err = resolveLaunchIdentity(gemini, &NewParams{}, cwd)
	require.NoError(t, err)
	assert.Empty(t, id.fullConvID)
	assert.NotEmpty(t, id.sessionID, "a fresh launch gets a synthetic key")
	assert.Equal(t, cwd, id.cwd)
}

// A claimed resume holds its conversation reservation until the caller
// releases it; a claim that fails afterwards (here: the label belongs to a
// live session) must not leave the conversation wedged.
func TestClaimLaunchIdentityHoldsTheReservationOnlyOnSuccess(t *testing.T) {
	isolateCacheAndDB(t)
	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: map[string]bool{"live-label-pane": true}}
	t.Cleanup(func() { clcommon.Default = prevTmux })
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-taken", TmuxSession: "live-label-pane", Status: StatusIdle,
	}))
	const conv = "dddddddd-4444-4444-8444-444444444444"

	t.Run("failure releases", func(t *testing.T) {
		id := launchIdentity{fullConvID: conv, sessionID: "spwn-taken", cwd: t.TempDir()}
		release, err := claimLaunchIdentity(context.Background(),
			&NewParams{Label: "spwn-taken", ManagedLaunch: true}, &id)
		require.ErrorContains(t, err, "choose a different --label")
		assert.Nil(t, release)

		again, reject := ReserveConvForLaunch(conv)
		require.NoError(t, reject, "the failed claim must have released its reservation")
		again()
	})

	t.Run("success holds", func(t *testing.T) {
		id := launchIdentity{fullConvID: conv, sessionID: conv, cwd: t.TempDir()}
		release, err := claimLaunchIdentity(context.Background(),
			&NewParams{ManagedLaunch: true}, &id)
		require.NoError(t, err)
		require.NotNil(t, release)
		assert.NotEmpty(t, id.tmuxSession, "a claim names the tmux session")

		_, reject := ReserveConvForLaunch(conv)
		require.ErrorContains(t, reject, "another launch", "the claim holds the reservation")
		release()
		again, reject := ReserveConvForLaunch(conv)
		require.NoError(t, reject, "release frees the conversation")
		again()
	})
}
