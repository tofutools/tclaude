package db

import (
	"sync"
	"testing"
)

// TestResetForTest_ConcurrentOpenIsRaceFree models the production-test
// reality that a background goroutine left over from a prior flow test (a
// daemon loop, a conv monitor mid-startup-scan) may still be calling Open()
// at the exact moment the next test's testharness.New runs ResetForTest.
// Before the fix, ResetForTest reassigned the package-global sync.Once,
// corrupting its internal mutex under the concurrent Open and parking the
// next Open() caller forever (the macOS CI 10m timeout). Run with -race to
// catch the unsynchronized access deterministically.
func TestResetForTest_ConcurrentOpenIsRaceFree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ResetForTest()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Background openers: leaked prior-test goroutines hammering Open().
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = Open()
				}
			}
		})
	}

	// The next test's setup repeatedly resets the singleton underneath.
	for range 200 {
		ResetForTest()
	}
	close(stop)
	wg.Wait()
}
