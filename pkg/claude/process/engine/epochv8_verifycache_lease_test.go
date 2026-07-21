package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func epochLeaseTestTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "epoch-lease", Start: "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
}

// TestEpochV8ShortTTLDriveKeepsEngineLeaseWithFreshHolderReload runs the
// production 1-task schema-8 flow with a short lease TTL and a fast fake
// heartbeat timer. Before the checkpoint-verification memo, repeated full
// receipt-chain replays held the run flock long enough to starve
// RenewEngineLease past the TTL, surfacing "engine lease is absent, expired,
// or has a different token/generation". The run's checkpoints are unique
// fresh bytes, so the drive replays each checkpoint uncached exactly once.
// The second host is a fresh lease holder reloading and re-verifying state
// from disk in the same process; the process-global verification memo stays
// warm across it, so this is deliberately NOT claimed as a cache-clearing
// restart — cold-cache memo behavior is pinned by the epochv8 package's
// cache-instance/reset regressions.
//
// The short-TTL drive is a plain-build regression only. Race instrumentation
// slows the CPU-bound uncached replays several-fold, re-creating the
// documented residual cold-history lease limit (see the non-release
// decision) below the 6s test TTL, so under -race this case would assert
// that known limit instead of the memoization regression it pins. The
// replacement-generation fencing test below runs under -race unchanged.
func TestEpochV8ShortTTLDriveKeepsEngineLeaseWithFreshHolderReload(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("short-TTL drive is a plain-build regression: -race re-creates the documented cold-history lease limit below the 6s test TTL")
	}
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := fs.PutTemplate(t.Context(), epochLeaseTestTemplate())
	if err != nil {
		t.Fatal(err)
	}
	run, err := Instantiate(t.Context(), fs, InstantiateRequest{TemplateRef: record.Ref, RunID: "epoch-lease-run"})
	if err != nil {
		t.Fatal(err)
	}
	kind, err := fs.RunStateSchemaKind(t.Context(), run.ID)
	if err != nil || kind != store.RunSchemaEpochV8 {
		t.Fatalf("schema kind = %q, %v", kind, err)
	}

	const shortTTL = 6 * time.Second
	adapter := &countingReleaseAdapter{}
	host := New(fs, "epoch-lease-engine", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	host.LeaseTTL = shortTTL
	host.Now = time.Now

	// The fake heartbeat timer delivers real renewal ticks every 100ms, far
	// more often than the production TTL/3 cadence, so any lease loss is a
	// genuine token/generation failure rather than a slow test clock.
	ticks := make(chan time.Time, 1)
	restore := host.SetHeartbeatTimerForTest(func(interval time.Duration) (<-chan time.Time, func()) {
		if interval != shortTTL/3 {
			t.Errorf("heartbeat interval = %s, want TTL/3 = %s", interval, shortTTL/3)
		}
		return ticks, func() {}
	})
	defer restore()
	stopPump := make(chan struct{})
	var pump sync.WaitGroup
	pump.Add(1)
	go func() {
		defer pump.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopPump:
				return
			case now := <-ticker.C:
				select {
				case ticks <- now:
				default:
				}
			}
		}
	}()

	began := time.Now()
	results, err := host.Tick(t.Context())
	elapsed := time.Since(began)
	close(stopPump)
	pump.Wait()
	if err != nil || len(results) != 1 {
		t.Fatalf("cold schema-8 tick = %#v, %v", results, err)
	}
	if results[0].Error != "" {
		t.Fatalf("cold drive lost its lease or failed: %q", results[0].Error)
	}
	if results[0].Status != state.RunStatusCompleted {
		t.Fatalf("cold drive status = %q, want completed", results[0].Status)
	}
	if adapter.calls != 2 {
		t.Fatalf("adapter calls = %d, want validate+perform", adapter.calls)
	}
	// Generous supplementary ceiling: the deterministic lease and call-count
	// assertions are the primary evidence; this only guards against the old
	// ~30s-per-tick full-replay profile returning wholesale. The test skips
	// under -race above, so the ceiling always applies when reached.
	if elapsed > 20*time.Second {
		t.Fatalf("cold 1-task drive took %s, want well below the 30s lease boundary", elapsed)
	}

	// Fresh-holder reload: a new holder re-acquires the lease, reloads state
	// from disk, and re-verifies the terminal checkpoint without lease
	// errors. The process-global verification memo remains warm here; this is
	// a disk-reload check, not a cache-clearing restart.
	restarted := New(fs, "epoch-lease-engine-restarted", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: &countingReleaseAdapter{}})
	restarted.LeaseTTL = shortTTL
	restoreRestarted := restarted.SetHeartbeatTimerForTest(func(time.Duration) (<-chan time.Time, func()) {
		return make(chan time.Time), func() {}
	})
	defer restoreRestarted()
	results, err = restarted.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCompleted {
		t.Fatalf("restart-shaped tick = %#v, %v", results, err)
	}
}

// TestEpochV8ReplacementGenerationStillFencesExpiredWorker pins the fencing
// half of the lease contract: when a worker genuinely loses its lease to a
// replacement generation, its renewals and releases keep failing closed even
// though checkpoint verification is now memoized.
func TestEpochV8ReplacementGenerationStillFencesExpiredWorker(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := fs.PutTemplate(t.Context(), epochLeaseTestTemplate())
	if err != nil {
		t.Fatal(err)
	}
	run, err := Instantiate(t.Context(), fs, InstantiateRequest{TemplateRef: record.Ref, RunID: "epoch-lease-fence-run"})
	if err != nil {
		t.Fatal(err)
	}

	const ttl = 5 * time.Second
	current := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	restore := fs.SetNowForTest(func() time.Time { return current })
	defer restore()

	leaseA, err := fs.AcquireEngineLease(t.Context(), run.ID, "worker-a", ttl)
	if err != nil {
		t.Fatal(err)
	}
	leaseA, err = fs.RenewEngineLease(t.Context(), leaseA, ttl)
	if err != nil {
		t.Fatalf("live worker heartbeat renewal failed: %v", err)
	}

	// The old worker stalls past its TTL; a replacement acquires a fresh
	// token and a strictly newer generation.
	current = current.Add(ttl + time.Second)
	leaseB, err := fs.AcquireEngineLease(t.Context(), run.ID, "worker-b", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if leaseB.Generation <= leaseA.Generation || leaseB.Token == leaseA.Token {
		t.Fatalf("replacement lease generation/token did not advance: A=%d B=%d", leaseA.Generation, leaseB.Generation)
	}

	if _, err := fs.RenewEngineLease(t.Context(), leaseA, ttl); err == nil ||
		!errors.Is(err, store.ErrLeaseHeld) || !strings.Contains(err.Error(), "different token/generation") {
		t.Fatalf("stale worker renewal = %v, want token/generation fence", err)
	}
	if err := fs.ReleaseEngineLease(t.Context(), leaseA); err == nil || !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("stale worker release = %v, want token/generation fence", err)
	}
	if _, err := fs.RenewEngineLease(t.Context(), leaseB, ttl); err != nil {
		t.Fatalf("replacement worker renewal failed: %v", err)
	}
}
