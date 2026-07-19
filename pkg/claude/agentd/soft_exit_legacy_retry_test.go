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

// legacyRetryTmux models exactly what the pid-keyed soft-exit retry
// needs from tmux: a live pane with a stable pid, a session that stays
// listed, and — when failFirstSend is set — a send-keys stream whose
// FIRST send fails (the transient re-send error under test). Everything
// else succeeds as a no-op.
type legacyRetryTmux struct {
	mu            sync.Mutex
	sessionName   string
	panePID       string
	failFirstSend bool
	sendsFailed   int
	sendsSeen     int
}

func (r *legacyRetryTmux) Command(args ...string) *exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "display-message":
		return exec.Command("echo", "0|"+r.panePID)
	case "list-sessions":
		return exec.Command("echo", r.sessionName)
	case "send-keys":
		r.sendsSeen++
		if r.failFirstSend && r.sendsSeen == 1 {
			r.sendsFailed++
			return exec.Command("false")
		}
	}
	return exec.Command("true")
}

func (r *legacyRetryTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{r.sessionName: {}}, nil
}

// The pid-keyed retry engine (scheduleSoftExitRetry — the reincarnate
// soft-exit path) only ever runs after the first /exit was delivered. A
// transient failure of a RE-send must therefore not instantly erase the
// delivered exit's attribution: with the session still alive, the intent
// survives through the bounded observer window and is then cleaned up —
// mirroring the selected-pane watchdog's re-send-failure treatment.
func TestLegacySoftExitRetry_SendFailurePreservesDeliveredIntentThroughWindow(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))
	t.Cleanup(SetSoftExitRetryDelayForTest(time.Millisecond))
	t.Cleanup(SetUnknownIntentCleanupDelayForTest(time.Second))

	const (
		conv      = "legacy-retry-conv"
		sessionID = "spwn-legacy-retry"
		tmuxSes   = "tmux-legacy-retry"
	)
	rt := &legacyRetryTmux{sessionName: tmuxSes, panePID: "4242", failFirstSend: true}
	prev := clcommon.Default
	clcommon.Default = rt
	t.Cleanup(func() { clcommon.Default = prev })

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, TmuxSession: tmuxSes, ConvID: conv,
		Status: session.StatusWorking, CreatedAt: time.Now(),
	}))
	ref, err := db.SetSessionExitIntent(sessionID, db.AgentExitActionReincarnate, "", time.Now())
	require.NoError(t, err)

	scheduleSoftExitRetry(conv, tmuxSes, 4242, "/exit", "test-legacy-retry", &ref)

	readIntent := func() string {
		t.Helper()
		d, err := db.Open()
		require.NoError(t, err)
		var intent string
		require.NoError(t, d.QueryRow(`SELECT exit_intent FROM sessions WHERE id = ?`,
			sessionID).Scan(&intent))
		return intent
	}
	// The failed re-send lands within a few milliseconds (1ms retry delay);
	// the 1s observer window comfortably outlasts this 200ms probe, so any
	// clear observed here is the instant-clear regression, not the bounded
	// cleanup.
	assert.Never(t, func() bool { return readIntent() == "" },
		200*time.Millisecond, 10*time.Millisecond,
		"a failed re-send must not instantly clear the delivered exit's intent")
	rt.mu.Lock()
	assert.Equal(t, 1, rt.sendsFailed, "exactly the first re-send failed")
	rt.mu.Unlock()

	WaitForBackgroundForTest()
	assert.Empty(t, readIntent(),
		"the bounded observer window still cleans up; retention is not a leak")
}

// After the FINAL re-send is delivered, the legacy engine re-checks the
// pane with no settle delay — a pane honoring that just-delivered /exit
// is often still alive at that instant. The post-loop path must mirror
// the target engine's final-attempt treatment: retain attribution
// through the bounded observer window instead of erasing it moments
// before the exit lands (a genuinely wedged pane reaches the same
// cleared end state, just bounded).
func TestLegacySoftExitRetry_FinalAttemptAlivePaneRetainsIntentThroughWindow(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetInjectSettleDelayForTest(0))
	t.Cleanup(SetSoftExitRetryDelayForTest(time.Millisecond))
	t.Cleanup(SetUnknownIntentCleanupDelayForTest(time.Second))

	const (
		conv      = "legacy-final-conv"
		sessionID = "spwn-legacy-final"
		tmuxSes   = "tmux-legacy-final"
	)
	rt := &legacyRetryTmux{sessionName: tmuxSes, panePID: "4242"}
	prev := clcommon.Default
	clcommon.Default = rt
	t.Cleanup(func() { clcommon.Default = prev })

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, TmuxSession: tmuxSes, ConvID: conv,
		Status: session.StatusWorking, CreatedAt: time.Now(),
	}))
	ref, err := db.SetSessionExitIntent(sessionID, db.AgentExitActionReincarnate, "", time.Now())
	require.NoError(t, err)

	scheduleSoftExitRetry(conv, tmuxSes, 4242, "/exit", "test-legacy-final", &ref)

	readIntent := func() string {
		t.Helper()
		d, err := db.Open()
		require.NoError(t, err)
		var intent string
		require.NoError(t, d.QueryRow(`SELECT exit_intent FROM sessions WHERE id = ?`,
			sessionID).Scan(&intent))
		return intent
	}
	// Every re-send succeeds and the pane stays alive, so the loop runs to
	// completion within a few milliseconds; the 1s observer window
	// comfortably outlasts this 200ms probe.
	assert.Never(t, func() bool { return readIntent() == "" },
		200*time.Millisecond, 10*time.Millisecond,
		"a still-alive pane after the final delivered re-send must not instantly clear the intent")

	WaitForBackgroundForTest()
	assert.Empty(t, readIntent(),
		"the bounded observer window still cleans up; retention is not a leak")
}
