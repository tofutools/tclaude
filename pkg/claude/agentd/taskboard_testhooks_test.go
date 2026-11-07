package agentd

import "time"

// SweepTaskboardForTest runs one task board sweep as of now.
func SweepTaskboardForTest(now time.Time) { sweepTaskboard(now) }

// SweepTaskboardIdleForTest runs one idle-without-report sweep as of now.
func SweepTaskboardIdleForTest(now time.Time) { sweepTaskboardIdle(now) }
