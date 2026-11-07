package agentd

import "time"

// ResetDrawingBoardEventsForTest clears the drawing board event rate limiter and presence
// and swaps their clock; the returned func restores both.
func ResetDrawingBoardEventsForTest(now func() time.Time) func() {
	reset := func() {
		drawingBoardEvents.Lock()
		drawingBoardEvents.last = map[string]time.Time{}
		drawingBoardEvents.Unlock()
		drawingBoardPresence.Lock()
		drawingBoardPresence.drawingBoards = map[string]*drawingBoardPresenceEntry{}
		drawingBoardPresence.Unlock()
	}
	reset()
	prev := drawingBoardNow
	drawingBoardNow = now
	return func() {
		drawingBoardNow = prev
		reset()
	}
}
