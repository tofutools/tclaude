package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

func backgroundObservationRow(now time.Time) *db.SessionRow {
	return &db.SessionRow{
		ID: "background-observation", ExecutionID: platformexec.ID("11111111111111111111111111111111"),
		PID: 42, Harness: "claude", CreatedAt: now.Add(-time.Minute),
	}
}

func resetBackgroundObservationTestState(t *testing.T) {
	t.Helper()
	previousDescendants := bgShellDescendantCommandLines
	previousProcess := backgroundMainProcessInstance
	t.Cleanup(func() {
		bgShellDescendantCommandLines = previousDescendants
		backgroundMainProcessInstance = previousProcess
		bgShellReconcileMu.Lock()
		bgShellReconcileMu.last = nil
		bgShellReconcileMu.Unlock()
	})
	bgShellReconcileMu.Lock()
	bgShellReconcileMu.last = nil
	bgShellReconcileMu.Unlock()
}

func TestObserveBackgroundWorkKnownEmptyNeedsNoProcessProbe(t *testing.T) {
	resetBackgroundObservationTestState(t)
	now := time.Now()
	row := backgroundObservationRow(now)
	processCalls, scanCalls := 0, 0
	backgroundMainProcessInstance = func(int) (string, bool) {
		processCalls++
		return "", false
	}
	bgShellDescendantCommandLines = func(int) ([]string, bool) {
		scanCalls++
		return nil, false
	}

	observation := observeBackgroundWork(row, true, now)
	resolution := resolveBackgroundObservation(observation)

	assert.Equal(t, backgroundObservationKnown, observation.Validity)
	assert.True(t, resolution.ConfirmedIdle)
	assert.Zero(t, resolution.Counts.Shells)
	assert.Zero(t, processCalls)
	assert.Zero(t, scanCalls)
}

func TestObserveBackgroundWorkCacheKeepsOriginalSampleTime(t *testing.T) {
	resetBackgroundObservationTestState(t)
	now := time.Now()
	row := backgroundObservationRow(now)
	row.BgShellsJSON = db.BgShellSet(nil).Add("shell", "npm run dev", now).Encode()
	processCalls, scanCalls := 0, 0
	backgroundMainProcessInstance = func(int) (string, bool) {
		processCalls++
		return "process-42", true
	}
	bgShellDescendantCommandLines = func(int) ([]string, bool) {
		scanCalls++
		return []string{"/bin/sh -c npm run dev"}, true
	}

	first := observeBackgroundWork(row, true, now)
	second := observeBackgroundWork(row, true, first.CompletedAt.Add(500*time.Millisecond))

	require.Equal(t, backgroundObservationKnown, first.Validity)
	assert.Equal(t, first.EvidenceID, second.EvidenceID)
	assert.Equal(t, first.StartedAt, second.StartedAt)
	assert.Equal(t, first.CompletedAt, second.CompletedAt,
		"a cache read must not refresh the sample boundary")
	assert.Equal(t, 1, scanCalls)
	assert.Equal(t, 3, processCalls,
		"collection revalidates once; a cache lookup still proves the current process instance")
}

func TestResolveBackgroundObservationUnknownScanNeverConfirmsIdle(t *testing.T) {
	resetBackgroundObservationTestState(t)
	now := time.Now()
	row := backgroundObservationRow(now)
	row.BgShellsJSON = db.BgShellSet(nil).
		Add("shell", "npm run dev", now.Add(-db.BgShellTTL-time.Minute)).Encode()
	backgroundMainProcessInstance = func(int) (string, bool) { return "process-42", true }
	bgShellDescendantCommandLines = func(int) ([]string, bool) { return nil, false }

	observation := observeBackgroundWork(row, true, now)
	resolution := resolveBackgroundObservation(observation)

	assert.Equal(t, backgroundObservationUnknown, observation.Validity)
	assert.Equal(t, "process_enumeration_failed", observation.Failure)
	assert.Zero(t, resolution.Counts.Shells,
		"the legacy TTL fallback may hide expired positive evidence")
	assert.False(t, resolution.ConfirmedIdle,
		"expiry plus a failed scan cannot newly establish idle")
}

func TestObserveBackgroundWorkRejectsProcessReplacementDuringScan(t *testing.T) {
	resetBackgroundObservationTestState(t)
	now := time.Now()
	row := backgroundObservationRow(now)
	row.BgShellsJSON = db.BgShellSet(nil).Add("shell", "npm run dev", now).Encode()
	processCalls := 0
	backgroundMainProcessInstance = func(int) (string, bool) {
		processCalls++
		if processCalls == 1 {
			return "predecessor", true
		}
		return "successor", true
	}
	bgShellDescendantCommandLines = func(int) ([]string, bool) {
		return []string{"/bin/sh -c npm run dev"}, true
	}

	observation := observeBackgroundWork(row, true, now)
	resolution := resolveBackgroundObservation(observation)

	assert.Equal(t, backgroundObservationUnknown, observation.Validity)
	assert.Equal(t, "main_process_changed", observation.Failure)
	assert.Equal(t, 1, resolution.Counts.Shells,
		"unknown falls back to the still-fresh hook ledger")
	assert.False(t, resolution.ConfirmedIdle)
}
