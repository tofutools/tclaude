package agentd

import (
	"sync"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// setupTestDB mirrors the helper in pkg/claude/common/db. We can't
// import it (it's a test-only function in another package) so we
// duplicate the three-line incantation here.
func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
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
	if err := db.MarkAgentMessageDelivered(idDel); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	// One addressed elsewhere — must not appear.
	idOther, _ := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g, FromConv: "peer", ToConv: "someone-else", Body: "x",
	})

	var got []int64
	send := func(m *db.AgentMessage) bool {
		got = append(got, m.ID)
		return true
	}

	n := flush("me", send)
	if n != 3 {
		t.Errorf("flush returned %d, want 3", n)
	}
	if len(got) != 3 || got[0] != id1 || got[1] != id2 || got[2] != id3 {
		t.Errorf("delivered order = %v, want [%d %d %d]", got, id1, id2, id3)
	}

	// All three should now be marked delivered.
	for _, id := range []int64{id1, id2, id3} {
		m, _ := db.GetAgentMessage(id)
		if m == nil || m.DeliveredAt.IsZero() {
			t.Errorf("msg %d should be delivered after flush", id)
		}
	}
	// idOther stays alone.
	m, _ := db.GetAgentMessage(idOther)
	if m == nil || !m.DeliveredAt.IsZero() {
		t.Errorf("other-recipient message should not be touched")
	}
}

func TestFlush_NoMessagesNoCalls(t *testing.T) {
	setupTestDB(t)
	calls := 0
	send := func(*db.AgentMessage) bool { calls++; return true }
	if got := flush("nobody", send); got != 0 {
		t.Errorf("flush of empty queue returned %d, want 0", got)
	}
	if calls != 0 {
		t.Errorf("send called %d times, want 0", calls)
	}
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
	if got := flush("me", send); got != 1 {
		t.Errorf("flush returned %d, want 1 (claim still counted)", got)
	}
	m, _ := db.GetAgentMessage(id)
	if m == nil || m.DeliveredAt.IsZero() {
		t.Errorf("message should be marked delivered to prevent re-attempt")
	}

	// A second flush sees the row as delivered and skips it.
	again := flush("me", send)
	if again != 0 {
		t.Errorf("second flush returned %d, want 0", again)
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
			flush("me", send)
		}()
	}
	wg.Wait()

	if len(delivered) != 10 {
		t.Errorf("expected 10 distinct deliveries, got %d", len(delivered))
	}
	for id, count := range delivered {
		if count != 1 {
			t.Errorf("msg %d delivered %d times, want 1", id, count)
		}
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
	if after != before {
		t.Errorf("empty conv-id should not touch debounce map")
	}

	// First call records a flush time. Second within window does not
	// reschedule. We can't easily observe the goroutine itself, but
	// the timestamp in the debounce map is observable.
	maybeFlushUndelivered("me")
	flushDebounceMu.Lock()
	first, ok := flushDebounce["me"]
	flushDebounceMu.Unlock()
	if !ok {
		t.Fatal("expected debounce entry after first call")
	}

	maybeFlushUndelivered("me")
	flushDebounceMu.Lock()
	second := flushDebounce["me"]
	flushDebounceMu.Unlock()
	if !second.Equal(first) {
		t.Errorf("second call within debounce window should not update timestamp; first=%v second=%v", first, second)
	}
}
