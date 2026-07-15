package agentd

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// setupTestDB mirrors the helper in pkg/claude/common/db. We can't
// import it (it's a test-only function in another package) so we
// duplicate the three-line incantation here.
func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// os.UserHomeDir() reads USERPROFILE on Windows; set it too so a
	// Windows test run stays in the temp dir instead of the real home.
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
	// Drain fire-and-forget goroutines (e.g. the nudge drain a send
	// handler kicks off via goBackground) before this test's TempDir
	// RemoveAll runs. Registered last → runs first (LIFO), so an
	// orphaned goroutine can't still be scribbling into $HOME/.tclaude/
	// db.sqlite when RemoveAll fires — the macOS "directory not empty"
	// cleanup race. newFlow drains the same WG for the flow tests; the
	// internal (package agentd) tests need it too whenever the handler
	// under test enqueues a nudge.
	t.Cleanup(WaitForBackgroundForTest)
}

// resetFlushState clears the per-conv debounce map between subtests.
// Otherwise the second test runs with the previous test's "last
// flushed" timestamp and short-circuits.
func resetFlushState(_ *testing.T) {
	flushDebounceMu.Lock()
	flushDebounce = map[string]time.Time{}
	flushDebounceMu.Unlock()
}

func TestReleaseExpiredNudgeClaims_SkipsActiveDaemonOwner(t *testing.T) {
	setupTestDB(t)
	g, err := db.CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "queued",
	})
	require.NoError(t, err)

	now := time.Now()
	token, claimed, err := db.ClaimAgentMessageNudge(id, now.Add(-nudgeClaimLease-time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	registerActiveNudge(id, token)
	t.Cleanup(func() { unregisterActiveNudge(id, token) })

	releaseExpiredNudgeClaims(now)
	m, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	assert.False(t, m.NudgeClaimedAt.IsZero(), "live daemon ownership prevents duplicate injection")

	unregisterActiveNudge(id, token)
	releaseExpiredNudgeClaims(now)
	m, err = db.GetAgentMessage(id)
	require.NoError(t, err)
	assert.True(t, m.NudgeClaimedAt.IsZero(), "orphaned expired claim is recovered")
}

// drainExactConv exercises the flushQueue claim/iteration/dedup core directly
// with the alive-gate forced open, so these unit tests can assert delivery
// semantics without standing up a live tmux session (the gate's offline /
// awaiting-human behaviour is covered by the mail-hold + nudge-queue flow
// tests). It lists the exact-conv queue, the same source the production conv
// drain uses.
func drainExactConv(convID string, send flushSender) int {
	return flushQueue("test:conv:"+convID,
		func() ([]*db.AgentMessage, error) { return db.ListUndeliveredForExactConv(convID) },
		func() bool { return true },
		send)
}

func TestFlush_DeliversUndeliveredOldestFirst(t *testing.T) {
	setupTestDB(t)

	g, _ := db.CreateAgentGroup("alpha", "")
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Insert 3 undelivered messages addressed to "me".
	id1, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "first", CreatedAt: createdAt,
	})
	id2, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "second", CreatedAt: createdAt.Add(time.Minute),
	})
	id3, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "third", CreatedAt: createdAt.Add(2 * time.Minute),
	})

	// One already-delivered message — must not appear.
	idDel, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "already",
	})
	require.NoError(t, db.MarkAgentMessageDelivered(idDel), "mark delivered")

	// One addressed elsewhere — must not appear.
	idOther, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "someone-else", Body: "x",
	})

	var got []int64
	send := func(m *db.AgentMessage) bool {
		got = append(got, m.ID)
		return true
	}

	n := drainExactConv("me", send)
	assert.Equal(t, 3, n, "flush return value")
	assert.Equal(t, []int64{id1, id2, id3}, got, "delivered order")

	// All three should now be marked delivered.
	for _, id := range []int64{id1, id2, id3} {
		m, _ := db.GetAgentMessage(id)
		if assert.NotNil(t, m, "msg %d should exist", id) {
			assert.False(t, m.DeliveredAt.IsZero(), "msg %d should be delivered after flush", id)
		}
	}
	// idOther stays alone.
	m, _ := db.GetAgentMessage(idOther)
	if assert.NotNil(t, m) {
		assert.True(t, m.DeliveredAt.IsZero(), "other-recipient message should not be touched")
	}
}

func TestFlush_InlineOperatorMessageIsConsumedAtomically(t *testing.T) {
	setupTestDB(t)
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: "me", Body: "Please inspect the failing test.", OperatorAuthored: true,
	})
	require.NoError(t, err)
	var nudge string
	assert.Equal(t, 1, drainExactConv("me", func(m *db.AgentMessage) bool {
		nudge = messageNudgeText(m.ID)
		return true
	}))
	assert.Contains(t, nudge, "from the human operator")
	assert.Contains(t, nudge, "Please inspect the failing test.")
	assert.NotContains(t, nudge, "regular chat/output")
	assert.NotContains(t, nudge, "human.notify")
	assert.NotContains(t, nudge, "subject:")
	assert.True(t, strings.HasSuffix(nudge, "] Please inspect the failing test."),
		"only the operator body should follow the metadata bracket: %q", nudge)
	m, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	assert.False(t, m.DeliveredAt.IsZero())
	assert.False(t, m.ReadAt.IsZero(), "inline archival copy is consumed in queue completion")
}

func TestInlineOperatorMessageKeepsExplicitSubjectInMetadata(t *testing.T) {
	setupTestDB(t)
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: "me", Subject: "priority", Body: "Please inspect the failing test.", OperatorAuthored: true,
	})
	require.NoError(t, err)
	nudge := messageNudgeText(id)
	assert.Contains(t, nudge, "; subject: priority]")
	assert.True(t, strings.HasSuffix(nudge, "] Please inspect the failing test."),
		"explicit subject belongs inside metadata, before the operator body: %q", nudge)
}

func TestFlush_MultilineOperatorMessageKeepsUnreadPointer(t *testing.T) {
	setupTestDB(t)
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: "me", Body: "first line\nsecond line", OperatorAuthored: true,
	})
	require.NoError(t, err)
	var nudge string
	assert.Equal(t, 1, drainExactConv("me", func(m *db.AgentMessage) bool {
		nudge = messageNudgeText(m.ID)
		return true
	}))
	assert.Contains(t, nudge, "tclaude agent inbox read")
	assert.NotContains(t, nudge, "first line")
	m, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	assert.False(t, m.DeliveredAt.IsZero())
	assert.True(t, m.ReadAt.IsZero(), "pointer delivery remains unread until inbox read")
}

func TestFlush_NoMessagesNoCalls(t *testing.T) {
	setupTestDB(t)
	calls := 0
	send := func(*db.AgentMessage) bool { calls++; return true }
	assert.Equal(t, 0, drainExactConv("nobody", send), "flush of empty queue")
	assert.Equal(t, 0, calls, "send call count")
}

func TestFlush_FailedSendReleasesClaimAndBacksOff(t *testing.T) {
	// "send returns false" simulates tmux failing after the durable claim.
	// The row must stay undelivered, release its lease, and retain attempt
	// metadata so an immediate second flush cannot spam the pane.
	setupTestDB(t)
	g, _ := db.CreateAgentGroup("alpha", "")
	id, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "x",
	})

	send := func(*db.AgentMessage) bool { return false }
	assert.Equal(t, 0, drainExactConv("me", send), "failed delivery is not counted")
	m, _ := db.GetAgentMessage(id)
	if assert.NotNil(t, m) {
		assert.True(t, m.DeliveredAt.IsZero(), "failed send stays undelivered")
		assert.True(t, m.NudgeClaimedAt.IsZero(), "failed send releases its claim")
		assert.Equal(t, 1, m.NudgeAttempts)
	}

	assert.Equal(t, 0, drainExactConv("me", send), "immediate second flush is backed off")
}

func TestFlush_RechecksSelectedPaneStatusAfterClaimGate(t *testing.T) {
	setupTestDB(t)
	defer SetInjectSettleDelayForTest(0)()

	g, err := db.CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "queued",
	})
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "status-race", TmuxSession: "tmux-status-race", ConvID: "me",
		Status: session.StatusWorking, CreatedAt: time.Now(),
	}))

	rt := &recordingTmux{}
	previousTmux := clcommon.Default
	clcommon.Default = rt
	t.Cleanup(func() { clcommon.Default = previousTmux })

	gateCalls := 0
	canDeliver := func() bool {
		gateCalls++
		allowed := deliverablePane("me")
		if gateCalls == 2 {
			// Model the pane entering a permission dialog immediately after the
			// final pre-claim gate approved it. sendNudgeBracket must observe
			// this newer status on the row it selects for injection.
			rows, findErr := db.FindSessionsByConvID("me")
			require.NoError(t, findErr)
			require.NotEmpty(t, rows)
			rows[0].Status = session.StatusAwaitingPermission
			require.NoError(t, db.SaveSession(rows[0]))
		}
		return allowed
	}

	delivered := flushQueue("test:conv:me",
		func() ([]*db.AgentMessage, error) { return db.ListUndeliveredForExactConv("me") },
		canDeliver,
		realFlushSender)
	assert.Zero(t, delivered)

	m, err := db.GetAgentMessage(id)
	require.NoError(t, err)
	assert.True(t, m.DeliveredAt.IsZero(), "blocked pane is not marked delivered")
	assert.True(t, m.NudgeClaimedAt.IsZero(), "blocked send releases its claim")
	assert.Equal(t, 1, m.NudgeAttempts, "the post-claim hold remains durable retry history")
	for _, key := range rt.snapshot() {
		assert.False(t, strings.Contains(key, "new agent message"), "no nudge may reach the blocked pane: %q", key)
	}
}

func TestFlush_ConcurrentClaimsAreRaceFree(t *testing.T) {
	// Spin up N goroutines that all flush the same conv. The claim
	// primitive must ensure each message is delivered exactly once.
	setupTestDB(t)
	g, _ := db.CreateAgentGroup("alpha", "")
	for i := 0; i < 10; i++ {
		_, _ = db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g, FromConv: "peer", ToConv: "me", Body: "x",
		})
	}

	var mu sync.Mutex
	delivered := map[int64]int{}
	send := func(m *db.AgentMessage) bool {
		mu.Lock()
		delivered[m.ID]++
		mu.Unlock()
		return true
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drainExactConv("me", send)
		}()
	}
	wg.Wait()

	assert.Len(t, delivered, 10, "expected 10 distinct deliveries")
	for id, count := range delivered {
		assert.Equal(t, 1, count, "msg %d delivered count", id)
	}
}

func TestMaybeFlushUndelivered_DebouncesPerConv(t *testing.T) {
	setupTestDB(t)
	resetFlushState(t)

	g, _ := db.CreateAgentGroup("alpha", "")
	for i := 0; i < 3; i++ {
		_, _ = db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g, FromConv: "peer", ToConv: "me", Body: "x",
		})
	}

	// Empty conv-id is a no-op fast path.
	flushDebounceMu.Lock()
	before := len(flushDebounce)
	flushDebounceMu.Unlock()
	maybeFlushUndelivered("")
	flushDebounceMu.Lock()
	after := len(flushDebounce)
	flushDebounceMu.Unlock()
	assert.Equal(t, before, after, "empty conv-id should not touch debounce map")

	// First call records a flush time. Second within window does not
	// reschedule. We can't easily observe the goroutine itself, but
	// the timestamp in the debounce map is observable.
	maybeFlushUndelivered("me")
	flushDebounceMu.Lock()
	first, ok := flushDebounce["me"]
	flushDebounceMu.Unlock()
	require.True(t, ok, "expected debounce entry after first call")

	maybeFlushUndelivered("me")
	flushDebounceMu.Lock()
	second := flushDebounce["me"]
	flushDebounceMu.Unlock()
	assert.True(t, second.Equal(first),
		"second call within debounce window should not update timestamp; first=%v second=%v", first, second)
}
