//go:build !windows

package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// `session attach` right after a spawn or resume can find the launch's row
// before its pane exists. It waits for the pane inside the start window rather
// than failing and marking a healthy launch exited.

func withAttachClock(t *testing.T, start time.Time, step time.Duration, onSleep func()) {
	t.Helper()
	now := start
	prevNow, prevSleep := attachNow, attachSleep
	attachNow = func() time.Time { return now }
	attachSleep = func(time.Duration) {
		now = now.Add(step)
		if onSleep != nil {
			onSleep()
		}
	}
	t.Cleanup(func() { attachNow, attachSleep = prevNow, prevSleep })
}

func TestAttachWaitsForALaunchingSessionsPane(t *testing.T) {
	isolateCacheAndDB(t)
	alive := map[string]bool{}
	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: alive}
	t.Cleanup(func() { clcommon.Default = prevTmux })

	launched := time.Now()
	state := &SessionState{ID: "spwn-wait", TmuxSession: "spwn-wait", Status: StatusIdle, Created: launched}
	require.NoError(t, SaveSessionState(state))
	polls := 0
	withAttachClock(t, launched.Add(time.Second), time.Second, func() {
		polls++
		if polls == 3 {
			alive["spwn-wait"] = true // the pane comes up
		}
	})

	got := waitForLaunchingSession(state)
	assert.True(t, IsTmuxSessionAlive(got.TmuxSession), "the wait returns once the pane is up")
	assert.Equal(t, 3, polls)
	reloaded, err := LoadSessionState("spwn-wait")
	require.NoError(t, err)
	assert.NotEqual(t, StatusExited, reloaded.Status, "a launch that came up is never marked exited")
}

func TestAttachStopsWaitingWhenTheStartWindowEnds(t *testing.T) {
	isolateCacheAndDB(t)
	prevTmux := clcommon.Default
	clcommon.Default = fakeTmux{alive: map[string]bool{}}
	t.Cleanup(func() { clcommon.Default = prevTmux })

	launched := time.Now()
	state := &SessionState{ID: "spwn-gone", TmuxSession: "spwn-gone", Status: StatusIdle, Created: launched}
	require.NoError(t, SaveSessionState(state))
	polls := 0
	withAttachClock(t, launched, 10*time.Second, func() { polls++ })

	got := waitForLaunchingSession(state)
	assert.False(t, IsTmuxSessionAlive(got.TmuxSession))
	assert.Equal(t, 3, polls, "the wait is bounded by the start window")
}

func TestAttachDoesNotWaitForAnOldOrExitedSession(t *testing.T) {
	prevSleep := attachSleep
	attachSleep = func(time.Duration) { t.Fatal("must not wait") }
	t.Cleanup(func() { attachSleep = prevSleep })
	old := &SessionState{ID: "a", TmuxSession: "a", Status: StatusIdle, Created: time.Now().Add(-time.Hour)}
	exited := &SessionState{ID: "b", TmuxSession: "b", Status: StatusExited, Created: time.Now()}
	assert.Same(t, old, waitForLaunchingSession(old))
	assert.Same(t, exited, waitForLaunchingSession(exited))
}
