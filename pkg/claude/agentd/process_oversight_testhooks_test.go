package agentd

import "time"

// SetProcessOversightClockForTest replaces the oversight clock and returns a
// restore func.
func SetProcessOversightClockForTest(now func() time.Time) func() {
	prev := processOversightNow
	processOversightNow = now
	return func() { processOversightNow = prev }
}

// ProcessConcernPatienceForTest is how long a holder has to answer.
func ProcessConcernPatienceForTest() time.Duration { return processConcernPatience }

// SweepProcessConcernsForTest runs the oversight sweep once, as the runtime
// ticker does.
func SweepProcessConcernsForTest() { sweepProcessConcerns() }

// SweepProcessStuckSignalsForTest runs the stuck-signal sweep once, as the
// runtime ticker does.
func SweepProcessStuckSignalsForTest() { sweepProcessStuckSignals() }
