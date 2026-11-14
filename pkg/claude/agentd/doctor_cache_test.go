package agentd

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/doctor"
)

// useDoctorBuildForTest swaps the check build for fn on an empty cache and
// restores both when the test ends, after any background rebuild finished.
func useDoctorBuildForTest(t *testing.T, fn func() []doctor.Check) {
	t.Helper()
	prev := buildDoctorChecks
	buildDoctorChecks = fn
	invalidateDoctorChecks()
	t.Cleanup(func() {
		waitDoctorRefreshIdle(t)
		buildDoctorChecks = prev
		invalidateDoctorChecks()
	})
}

func waitDoctorRefreshIdle(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		doctorCache.Lock()
		defer doctorCache.Unlock()
		return !doctorCache.refreshing
	}, 5*time.Second, 5*time.Millisecond, "background doctor refresh never finished")
}

func ageDoctorCacheForTest() {
	doctorCache.Lock()
	defer doctorCache.Unlock()
	doctorCache.at = time.Now().Add(-2 * doctorCacheTTL)
}

// firstDoctorID is the ID of the first cached (non-live) check.
func firstDoctorID(checks []doctor.Check) string { return checks[0].ID }

func numberedChecks(n int32) []doctor.Check {
	return []doctor.Check{{ID: fmt.Sprint(n)}}
}

// A stale read serves the cached checks and leaves the slow rebuild (the
// bwrap-through-tmux probe) to the background.
func TestDoctorStaleReadDoesNotWaitForRebuild(t *testing.T) {
	var builds atomic.Int32
	release := make(chan struct{})
	useDoctorBuildForTest(t, func() []doctor.Check {
		n := builds.Add(1)
		if n > 1 {
			<-release
		}
		return numberedChecks(n)
	})

	require.Equal(t, "1", firstDoctorID(doctorChecks(false)), "empty cache builds inline")
	ageDoctorCacheForTest()

	done := make(chan []doctor.Check, 1)
	go func() { done <- doctorChecks(false) }()
	select {
	case got := <-done:
		require.Equal(t, "1", firstDoctorID(got), "stale read serves the cached checks")
	case <-time.After(5 * time.Second):
		t.Fatal("stale doctor read blocked on the background rebuild")
	}
	require.Equal(t, "1", firstDoctorID(doctorChecks(false)), "no second rebuild while one is in flight")
	close(release)

	require.Eventually(t, func() bool {
		return firstDoctorID(doctorChecks(false)) == "2"
	}, 5*time.Second, 5*time.Millisecond, "rebuilt checks were never published")
	require.Equal(t, int32(2), builds.Load())
}

// Re-check and the first read after an invalidating write build inline: the
// operator is waiting on that answer.
func TestDoctorRefreshAndInvalidationBuildInline(t *testing.T) {
	var builds atomic.Int32
	useDoctorBuildForTest(t, func() []doctor.Check { return numberedChecks(builds.Add(1)) })

	require.Equal(t, "1", firstDoctorID(doctorChecks(false)))
	require.Equal(t, "1", firstDoctorID(doctorChecks(false)), "fresh read is cached")
	require.Equal(t, "2", firstDoctorID(doctorChecks(true)), "Re-check builds inline")
	invalidateDoctorChecks()
	require.Equal(t, "3", firstDoctorID(doctorChecks(false)), "read after invalidation builds inline")
}

// A background rebuild that started before an invalidating write must not
// publish the answer it computed from the old state.
func TestDoctorInvalidationDropsInFlightRebuild(t *testing.T) {
	var builds atomic.Int32
	release := make(chan struct{})
	useDoctorBuildForTest(t, func() []doctor.Check {
		n := builds.Add(1)
		if n == 2 {
			<-release
			return []doctor.Check{{ID: "stale-background"}}
		}
		return numberedChecks(n)
	})

	require.Equal(t, "1", firstDoctorID(doctorChecks(false)))
	ageDoctorCacheForTest()
	require.Equal(t, "1", firstDoctorID(doctorChecks(false)), "starts the background rebuild")
	invalidateDoctorChecks()
	require.Equal(t, "3", firstDoctorID(doctorChecks(false)), "post-write read builds inline")
	close(release)
	waitDoctorRefreshIdle(t)
	require.Equal(t, "3", firstDoctorID(doctorChecks(false)), "superseded rebuild was not published")
}

// A rebuild that panics keeps the previous checks.
func TestDoctorRefreshPanicKeepsPreviousChecks(t *testing.T) {
	var builds atomic.Int32
	useDoctorBuildForTest(t, func() []doctor.Check {
		if builds.Add(1) > 1 {
			panic("probe exploded")
		}
		return numberedChecks(1)
	})
	require.Equal(t, "1", firstDoctorID(doctorChecks(false)))
	ageDoctorCacheForTest()
	require.Equal(t, "1", firstDoctorID(doctorChecks(false)))
	waitDoctorRefreshIdle(t)
	require.Equal(t, "1", firstDoctorID(doctorChecks(false)), "panicked rebuild kept the previous checks")
	require.Equal(t, int32(2), builds.Load(), "failed rebuild is not retried before the TTL")
}
