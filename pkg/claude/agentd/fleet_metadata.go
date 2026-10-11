package agentd

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/harnessops"
)

// Fleet release metadata (this node's tclaude release and the harnesses'
// latest versions) feeds the Fleet views. Reading it is internet traffic, so
// it runs only while federation runs: the federation runtime owns the loop,
// which starts with it (at daemon start or on a later connect) and stops with
// it (disconnect, disable, reload). A standalone daemon never polls; explicit
// local update and install requests still read metadata on demand.

const (
	fleetMetadataEvery = time.Hour
	// fleetMetadataRetryAfter keeps runtime restarts (reloads, reconnects,
	// identity rotation) from re-reading after a failed check: the services'
	// own hourly throttles only advance on success.
	fleetMetadataRetryAfter = 10 * time.Minute
)

var (
	fleetMetadataMu      sync.Mutex
	fleetMetadataAttempt time.Time
	// startFleetMetadata is swapped out by tests, which run many federation
	// runtimes and must not read release metadata from the internet.
	startFleetMetadata = startFleetMetadataLoop
)

// startFleetMetadataLoop runs the metadata loop until ctx (the federation
// runtime's) is cancelled; wg is the runtime's, so stopping federation waits
// for it. In-flight reads are cancelled with ctx. The harness-operations
// service itself belongs to the daemon and is closed only at daemon exit.
func startFleetMetadataLoop(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTimer(fleetMetadataFirstDelay(time.Now()))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			refreshFleetMetadata(ctx)
			timer.Reset(fleetMetadataEvery)
		}
	}()
}

// fleetMetadataFirstDelay is zero unless a check was attempted within
// fleetMetadataRetryAfter, in which case the first read waits out the rest.
func fleetMetadataFirstDelay(now time.Time) time.Duration {
	fleetMetadataMu.Lock()
	defer fleetMetadataMu.Unlock()
	if fleetMetadataAttempt.IsZero() {
		return 0
	}
	if wait := fleetMetadataRetryAfter - now.Sub(fleetMetadataAttempt); wait > 0 {
		return wait
	}
	return 0
}

func refreshFleetMetadata(ctx context.Context) {
	fleetMetadataMu.Lock()
	fleetMetadataAttempt = time.Now()
	fleetMetadataMu.Unlock()
	harnessops.RefreshLatestAsyncContext(ctx)
	service, err := localUpdateService()
	if err != nil {
		slog.Debug("self-update metadata unavailable", "error", err)
		return
	}
	if err := service.RefreshMetadata(ctx); err != nil && ctx.Err() == nil {
		slog.Debug("self-update release check failed", "error", err)
	}
}
