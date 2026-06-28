package agentd

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
}

// resetFlushState clears the per-conv debounce map between subtests.
// Otherwise the second test runs with the previous test's "last
// flushed" timestamp and short-circuits.
func resetFlushState(_ *testing.T) {
	flushDebounceMu.Lock()
	flushDebounce = map[string]time.Time{}
	flushDebounceMu.Unlock()
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

	// Insert 3 undelivered messages addressed to "me".
	id1, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "first",
	})
	time.Sleep(2 * time.Millisecond)
	id2, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "second",
	})
	time.Sleep(2 * time.Millisecond)
	id3, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "third",
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

func TestFlush_NoMessagesNoCalls(t *testing.T) {
	setupTestDB(t)
	calls := 0
	send := func(*db.AgentMessage) bool { calls++; return true }
	assert.Equal(t, 0, drainExactConv("nobody", send), "flush of empty queue")
	assert.Equal(t, 0, calls, "send call count")
}

func TestFlush_FailedSendStillClaims(t *testing.T) {
	// "send returns false" simulates tmux not actually being alive
	// when we try to deliver. We still claim the message (so a
	// subsequent flush won't double-attempt) but log + move on.
	setupTestDB(t)
	g, _ := db.CreateAgentGroup("alpha", "")
	id, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "me", Body: "x",
	})

	send := func(*db.AgentMessage) bool { return false }
	assert.Equal(t, 1, drainExactConv("me", send), "flush return (claim still counted)")
	m, _ := db.GetAgentMessage(id)
	if assert.NotNil(t, m) {
		assert.False(t, m.DeliveredAt.IsZero(), "message should be marked delivered to prevent re-attempt")
	}

	// A second flush sees the row as delivered and skips it.
	assert.Equal(t, 0, drainExactConv("me", send), "second flush")
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
