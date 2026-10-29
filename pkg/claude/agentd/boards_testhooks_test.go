package agentd

import "time"

// ResetBoardEventsForTest clears the board event rate limiter and presence
// and swaps their clock; the returned func restores both.
func ResetBoardEventsForTest(now func() time.Time) func() {
	reset := func() {
		boardEvents.Lock()
		boardEvents.last = map[string]time.Time{}
		boardEvents.Unlock()
		boardPresence.Lock()
		boardPresence.boards = map[string]*boardPresenceEntry{}
		boardPresence.Unlock()
	}
	reset()
	prev := boardNow
	boardNow = now
	return func() {
		boardNow = prev
		reset()
	}
}
