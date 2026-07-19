package agentd

import (
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// pidUnreadableTmux models the pane injectSoftExit's pid-unreadable branch
// sees: the session exists and accepts keystrokes (has-session and send-keys
// succeed, list-sessions keeps listing it), but every display-message query
// fails — so livePanePID reads 0 while IsTmuxSessionAlive still reports the
// session alive (its pane_dead probe treats a failed query as not-dead).
type pidUnreadableTmux struct {
	mu          sync.Mutex
	sessionName string
	sessionGone bool // list-sessions omits sessionName (confirmed disappearance)
	sendsSeen   int
}

func (r *pidUnreadableTmux) Command(args ...string) *exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "display-message":
		return exec.Command("false")
	case "list-sessions":
		if r.sessionGone {
			return exec.Command("echo", "some-other-session")
		}
		return exec.Command("echo", r.sessionName)
	case "send-keys":
		r.sendsSeen++
	}
	return exec.Command("true")
}

func (r *pidUnreadableTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{r.sessionName: {}}, nil
}

// injectSoftExit only reaches the pid branch after its /exit was DELIVERED
// (injectSlashCommand succeeded). An unreadable pane pid therefore is not a
// stop failure — it is often the delivered exit landing — so it must not
// instantly clear the delivered exit's intent: with the session still listed
// alive, the intent survives through the bounded observer window and is then
// cleaned up, mirroring the retry engines' unknown treatment.
func TestInjectSoftExit_PidUnreadableRetainsDeliveredIntentThroughWindow(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))
	t.Cleanup(SetUnknownIntentCleanupDelayForTest(time.Second))

	const (
		conv      = "pid-unreadable-conv"
		sessionID = "spwn-pid-unreadable"
		tmuxSes   = "tmux-pid-unreadable"
	)
	rt := &pidUnreadableTmux{sessionName: tmuxSes}
	prev := clcommon.Default
	clcommon.Default = rt
	t.Cleanup(func() { clcommon.Default = prev })

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, TmuxSession: tmuxSes, ConvID: conv,
		Status: session.StatusWorking, CreatedAt: time.Now(),
	}))
	ref, err := db.SetSessionExitIntent(sessionID, db.AgentExitActionReincarnate, "", time.Now())
	require.NoError(t, err)

	require.True(t, injectSoftExit(conv, "/exit", "test-pid-unreadable", &ref),
		"the first injection succeeded, so injectSoftExit reports delivery")
	rt.mu.Lock()
	assert.Positive(t, rt.sendsSeen, "the /exit was actually typed at the pane")
	rt.mu.Unlock()

	readIntent := func() string {
		t.Helper()
		d, err := db.Open()
		require.NoError(t, err)
		var intent string
		require.NoError(t, d.QueryRow(`SELECT exit_intent FROM sessions WHERE id = ?`,
			sessionID).Scan(&intent))
		return intent
	}
	// The pid-unreadable branch runs synchronously inside injectSoftExit; the
	// 1s observer window comfortably outlasts this 200ms probe, so any clear
	// observed here is the instant-clear regression, not the bounded cleanup.
	assert.Never(t, func() bool { return readIntent() == "" },
		200*time.Millisecond, 10*time.Millisecond,
		"an unreadable pane pid must not instantly clear the delivered exit's intent")

	WaitForBackgroundForTest()
	assert.Empty(t, readIntent(),
		"the bounded observer window still cleans up; retention is not a leak")
}

// When the pid is unreadable AND list-sessions confirms the session is gone,
// the delivered /exit is landing: the intent must be left untouched — no
// instant clear, and no observer-window cleanup either — because the reaper
// owns attribution of the disappearance (mirrors injectSoftExitTarget's
// confirmed-disappearance branch).
func TestInjectSoftExit_PidUnreadableSessionGoneLeavesIntentToReaper(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))
	t.Cleanup(SetUnknownIntentCleanupDelayForTest(time.Second))

	const (
		conv      = "pid-gone-conv"
		sessionID = "spwn-pid-gone"
		tmuxSes   = "tmux-pid-gone"
	)
	rt := &pidUnreadableTmux{sessionName: tmuxSes, sessionGone: true}
	prev := clcommon.Default
	clcommon.Default = rt
	t.Cleanup(func() { clcommon.Default = prev })

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, TmuxSession: tmuxSes, ConvID: conv,
		Status: session.StatusWorking, CreatedAt: time.Now(),
	}))
	ref, err := db.SetSessionExitIntent(sessionID, db.AgentExitActionReincarnate, "", time.Now())
	require.NoError(t, err)

	require.True(t, injectSoftExit(conv, "/exit", "test-pid-gone", &ref),
		"the first injection succeeded, so injectSoftExit reports delivery")

	// If the wrong branch scheduled the observer-window cleanup, this drain
	// would run it (1s window) and empty the intent; the confirmed-disappearance
	// branch schedules nothing and leaves attribution to the reaper.
	WaitForBackgroundForTest()
	d, err := db.Open()
	require.NoError(t, err)
	var intent string
	require.NoError(t, d.QueryRow(`SELECT exit_intent FROM sessions WHERE id = ?`,
		sessionID).Scan(&intent))
	assert.Equal(t, db.AgentExitActionReincarnate, intent,
		"a confirmed session disappearance must leave the delivered exit's intent for the reaper")
}
