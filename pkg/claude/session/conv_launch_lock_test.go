package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// isolateCacheAndDB points HOME + XDG_CACHE_HOME at fresh temp dirs so the
// per-conv launch lock files (under CacheDir()/locks) and the SQLite DB are
// isolated from the real environment and from each other.
func isolateCacheAndDB(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	db.ResetForTest()
}

// Two resumes of the same conversation must not both proceed: the second
// reservation fails fast while the first holds the lock, and succeeds again
// once it is released. This is the race CodeRabbit flagged — the read guard
// alone can't prevent two launches that both pass it. See JOH-332.
func TestReserveConvForLaunch_SerializesConcurrentLaunch(t *testing.T) {
	isolateCacheAndDB(t)
	conv := "aaaaaaaa-1111-4111-8111-111111111111"

	release1, reject1 := ReserveConvForLaunch(conv)
	require.Nil(t, reject1, "first reservation of an idle conv must succeed")
	require.NotNil(t, release1)

	// A concurrent reservation of the SAME conv is rejected (lock held).
	release2, reject2 := ReserveConvForLaunch(conv)
	assert.Nil(t, release2)
	require.Error(t, reject2)
	assert.Contains(t, reject2.Error(), "another launch")

	// A DIFFERENT conv is independent.
	relOther, rejOther := ReserveConvForLaunch("bbbbbbbb-2222-4222-8222-222222222222")
	require.Nil(t, rejOther, "a different conversation is not blocked")
	require.NotNil(t, relOther)
	relOther()

	// Releasing the first frees the conv for a later launch.
	release1()
	release3, reject3 := ReserveConvForLaunch(conv)
	require.Nil(t, reject3, "after release the conv can be reserved again")
	require.NotNil(t, release3)
	release3()
}

// A reservation must reject (and release its lock) when a live session already
// exists for the conv — the sequential already-live case, checked under the
// lock so it composes with the concurrency guard.
func TestReserveConvForLaunch_RejectsAlreadyLive(t *testing.T) {
	isolateCacheAndDB(t)

	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: map[string]bool{"livename": true}}
	t.Cleanup(func() { clcommon.Default = prevTmux })

	conv := "cccccccc-3333-4333-8333-333333333333"
	require.NoError(t, SaveSessionState(&SessionState{
		ID: conv, ConvID: conv, TmuxSession: "livename", Status: StatusIdle,
	}))

	release, reject := ReserveConvForLaunch(conv)
	assert.Nil(t, release)
	require.Error(t, reject)
	assert.Contains(t, reject.Error(), "already exists")

	// The lock was released on the live-session reject, so the conv is not
	// wedged: a follow-up reservation can still acquire the lock (and would
	// again reject on the still-live session — proving the lock is free).
	release2, reject2 := ReserveConvForLaunch(conv)
	assert.Nil(t, release2)
	require.Error(t, reject2, "still rejected by the live session, not by a stuck lock")
	assert.Contains(t, reject2.Error(), "already exists")
}

// An empty conv id (a fresh, non-resumed launch) reserves nothing and never
// rejects — it returns a usable no-op release.
func TestReserveConvForLaunch_EmptyConvIsNoop(t *testing.T) {
	isolateCacheAndDB(t)
	release, reject := ReserveConvForLaunch("")
	require.Nil(t, reject)
	require.NotNil(t, release)
	release() // must be safe to call
}
