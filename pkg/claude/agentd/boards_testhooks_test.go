package agentd

import "time"

// ResetBoardEventsForTest clears the board event rate limiter and swaps its
// clock; the returned func restores both.
func ResetBoardEventsForTest(now func() time.Time) func() {
	boardEvents.Lock()
	boardEvents.last = map[string]time.Time{}
	boardEvents.Unlock()
	prev := boardNow
	boardNow = now
	return func() {
		boardNow = prev
		boardEvents.Lock()
		boardEvents.last = map[string]time.Time{}
		boardEvents.Unlock()
	}
}
