package epochv8

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// mutateWireKeepClaimedDigest clones the wire, applies the mutation, and
// deliberately keeps the stale claimed digest so the forged bytes differ from
// the previously verified canonical bytes.
func mutateWireKeepClaimedDigest(checkpoint *CheckpointV8, mutate func(*checkpointWire)) *CheckpointV8 {
	wire := cloneWire(checkpoint.wire)
	mutate(&wire)
	return &CheckpointV8{wire: wire}
}

func TestVerifyCacheProductionFlowReplaysOncePerUniqueCheckpoint(t *testing.T) {
	checkpoint := runtimeFinishedFixture(t, "verify-cache-flow")

	checkpointVerifyCache.resetForTest()
	base := checkpointVerifyCache.replayCount()

	encoded, err := EncodeCheckpointV8(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCheckpointV8(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeCheckpointV8(decoded); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckpointV8(decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpointV8(encoded); err != nil {
		t.Fatal(err)
	}
	if delta := checkpointVerifyCache.replayCount() - base; delta != 1 {
		t.Fatalf("cold+warm encode/decode/verify replays = %d, want exactly 1", delta)
	}

	// A warm repeat of the whole sequence must add no replays.
	if _, err := EncodeCheckpointV8(checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpointV8(encoded); err != nil {
		t.Fatal(err)
	}
	if delta := checkpointVerifyCache.replayCount() - base; delta != 1 {
		t.Fatalf("warm repeat replays = %d, want still 1", delta)
	}

	// A reset models a process restart: the next verification is cold again.
	checkpointVerifyCache.resetForTest()
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		t.Fatal(err)
	}
	if delta := checkpointVerifyCache.replayCount() - base; delta != 2 {
		t.Fatalf("post-reset replays = %d, want exactly 2", delta)
	}
}

func TestVerifyCacheConstructorChainReplaysOncePerUniqueCheckpoint(t *testing.T) {
	checkpointVerifyCache.resetForTest()
	base := checkpointVerifyCache.replayCount()

	// Initialize, AttachGenesis, AdvanceHead, ClaimExternal, and
	// FinishClaimedHead each mint exactly one new unique checkpoint; every
	// current-checkpoint re-verification along the way must be a cache hit.
	runtimeFinishedFixture(t, "verify-cache-chain")

	const uniqueCheckpoints = 5
	if delta := checkpointVerifyCache.replayCount() - base; delta != uniqueCheckpoints {
		t.Fatalf("constructor chain replays = %d, want one per unique checkpoint (%d)", delta, uniqueCheckpoints)
	}
}

func TestVerifyCacheTamperAfterWarmMissesAndFailsClosed(t *testing.T) {
	started := runtimeStartedFixture(t, "verify-cache-tamper")
	checkpoint := started.Checkpoint
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		t.Fatal(err)
	}

	variants := map[string]*CheckpointV8{
		"changed_anchor": mutateWireKeepClaimedDigest(checkpoint, func(wire *checkpointWire) {
			wire.Anchor.RunID = "forged-run"
		}),
		"changed_history": mutateWireKeepClaimedDigest(checkpoint, func(wire *checkpointWire) {
			wire.History = wire.History[:len(wire.History)-1]
		}),
		"changed_witness": mutateWireKeepClaimedDigest(checkpoint, func(wire *checkpointWire) {
			receipt := wire.History[len(wire.History)-1].Runtime
			receipt.ExecutionWitness.Pre = receipt.ExecutionWitness.Post
		}),
		"changed_authority": mutateWireKeepClaimedDigest(checkpoint, func(wire *checkpointWire) {
			wire.Authorities[0].State = AuthorityClaimed
		}),
		"changed_runtime_binding": mutateWireKeepClaimedDigest(checkpoint, func(wire *checkpointWire) {
			wire.RuntimeBinding.Revision++
		}),
	}
	for name, forged := range variants {
		t.Run(name, func(t *testing.T) {
			if err := VerifyCheckpointV8(forged); err == nil {
				t.Fatal("stale-claimed-digest tamper was accepted after warm cache")
			}
			if err := VerifyCheckpointV8(forged); err == nil {
				t.Fatal("repeated tampered verification was accepted (cache poisoned)")
			}
		})
	}

	t.Run("coherently_rehashed_forgery", func(t *testing.T) {
		forged := rehashLastRuntimeReceipt(t, checkpoint, RuntimeAdvanceHead, func(receipt *RuntimeReceipt) {
			receipt.ExecutionWitness.RouteObservation.Mode = "pending_route"
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "execution witness replay") {
			t.Fatalf("rehashed witness forgery after warm cache: %v", err)
		}
	})

	t.Run("noncanonical_json_on_warm_cache", func(t *testing.T) {
		encoded, err := EncodeCheckpointV8(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeCheckpointV8(encoded); err != nil {
			t.Fatal(err)
		}
		leading := append([]byte(" "), encoded...)
		if _, err := DecodeCheckpointV8(leading); err == nil {
			t.Fatal("leading-whitespace noncanonical checkpoint was accepted on warm cache")
		}
		trailing := bytes.Replace(encoded, []byte("}\n"), []byte("} \n"), 1)
		if bytes.Equal(trailing, encoded) {
			t.Fatal("trailing-whitespace mutation did not apply")
		}
		if _, err := DecodeCheckpointV8(trailing); err == nil {
			t.Fatal("trailing-whitespace noncanonical checkpoint was accepted on warm cache")
		}
	})

	t.Run("runtime_artifact_and_source_tamper", func(t *testing.T) {
		source := testTemplateSource("verify-cache-tamper")
		if _, err := VerifyRuntimeArtifact(t.Context(), checkpoint, started.ArtifactJSON, source); err != nil {
			t.Fatal(err)
		}
		tamperedSource := append(bytes.Clone(source), '#')
		if _, err := VerifyRuntimeArtifact(t.Context(), checkpoint, started.ArtifactJSON, tamperedSource); err == nil {
			t.Fatal("tampered template source was accepted on warm cache")
		}
		tamperedArtifact := bytes.Replace(bytes.Clone(started.ArtifactJSON), []byte(`"version"`), []byte(`"Version"`), 1)
		if bytes.Equal(tamperedArtifact, started.ArtifactJSON) {
			t.Fatal("artifact mutation did not apply")
		}
		if _, err := VerifyRuntimeArtifact(t.Context(), checkpoint, tamperedArtifact, source); err == nil {
			t.Fatal("tampered runtime artifact was accepted on warm cache")
		}
	})

	// The valid checkpoint stays verifiable after every failed variant.
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCacheEvictionRevalidatesAndNeverInheritsSuccess(t *testing.T) {
	seeds := []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}}
	checkpointA := testCheckpoint(t, "verify-cache-evict-a", seeds)
	checkpointB := testCheckpoint(t, "verify-cache-evict-b", seeds)
	checkpointC := testCheckpoint(t, "verify-cache-evict-c", seeds)

	cache := newVerifyCache(2, 1<<30)
	mustVerify := func(c *verifyCache, checkpoint *CheckpointV8) {
		t.Helper()
		if err := c.verify(checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	mustVerify(cache, checkpointA)
	mustVerify(cache, checkpointB)
	mustVerify(cache, checkpointA) // A becomes most recent
	if count := cache.replayCount(); count != 2 {
		t.Fatalf("replays before eviction = %d, want 2", count)
	}
	mustVerify(cache, checkpointC) // evicts B (LRU)
	if count := cache.replayCount(); count != 3 {
		t.Fatalf("replays after inserting C = %d, want 3", count)
	}
	if entries, _ := cache.retainedForTest(); entries != 2 {
		t.Fatalf("entries after eviction = %d, want 2", entries)
	}
	mustVerify(cache, checkpointB) // evicted valid B revalidates via full replay
	if count := cache.replayCount(); count != 4 {
		t.Fatalf("replays after B revalidation = %d, want 4", count)
	}

	tamperedA := mutateWireKeepClaimedDigest(checkpointA, func(wire *checkpointWire) {
		wire.CurrentEpochID = wire.Epochs[0].ID + "0"
	})
	for range 2 {
		if err := cache.verify(tamperedA); err == nil {
			t.Fatal("tampered A inherited A's cached success")
		}
	}

	t.Run("byte_bound_eviction", func(t *testing.T) {
		encodedA, err := EncodeCheckpointV8(checkpointA)
		if err != nil {
			t.Fatal(err)
		}
		encodedB, err := EncodeCheckpointV8(checkpointB)
		if err != nil {
			t.Fatal(err)
		}
		byteBound := newVerifyCache(10, len(encodedA)+len(encodedB)-1)
		mustVerify(byteBound, checkpointA)
		mustVerify(byteBound, checkpointB) // retained bytes overflow: A evicted
		entries, retained := byteBound.retainedForTest()
		if entries != 1 || retained != len(encodedB) {
			t.Fatalf("byte-bound cache state = %d entries / %d bytes, want 1 / %d", entries, retained, len(encodedB))
		}
		mustVerify(byteBound, checkpointB)
		if count := byteBound.replayCount(); count != 2 {
			t.Fatalf("byte-bound replays = %d, want 2 (B stays warm)", count)
		}
		mustVerify(byteBound, checkpointA)
		if count := byteBound.replayCount(); count != 3 {
			t.Fatalf("byte-bound replays after A revalidation = %d, want 3", count)
		}
	})

	t.Run("oversize_verifies_without_retention", func(t *testing.T) {
		oversize := newVerifyCache(4, 8)
		mustVerify(oversize, checkpointA)
		if entries, retained := oversize.retainedForTest(); entries != 0 || retained != 0 {
			t.Fatalf("oversize input was retained: %d entries / %d bytes", entries, retained)
		}
		mustVerify(oversize, checkpointA)
		if count := oversize.replayCount(); count != 2 {
			t.Fatalf("oversize replays = %d, want 2 (never cached)", count)
		}
	})
}

func TestVerifyCacheConcurrentIdenticalDecodesCoalesceToOneReplay(t *testing.T) {
	seeds := []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}}
	checkpoint := testCheckpoint(t, "verify-cache-concurrent-same", seeds)

	t.Run("same_checkpoint_single_replay", func(t *testing.T) {
		cache := newVerifyCache(verifyCacheMaxEntries, verifyCacheMaxRetainedBytes)
		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- cache.verify(checkpoint)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if count := cache.replayCount(); count != 1 {
			t.Fatalf("concurrent identical verifications replayed %d times, want exactly 1", count)
		}
	})

	t.Run("production_decode_coalesces", func(t *testing.T) {
		encoded, err := EncodeCheckpointV8(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		checkpointVerifyCache.resetForTest()
		base := checkpointVerifyCache.replayCount()
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, decodeErr := DecodeCheckpointV8(encoded)
				errs <- decodeErr
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if delta := checkpointVerifyCache.replayCount() - base; delta != 1 {
			t.Fatalf("concurrent identical decodes replayed %d times, want exactly 1", delta)
		}
	})

	t.Run("mixed_distinct_checkpoints", func(t *testing.T) {
		cache := newVerifyCache(verifyCacheMaxEntries, verifyCacheMaxRetainedBytes)
		distinct := []*CheckpointV8{
			checkpoint,
			testCheckpoint(t, "verify-cache-concurrent-b", seeds),
			testCheckpoint(t, "verify-cache-concurrent-c", seeds),
			testCheckpoint(t, "verify-cache-concurrent-d", seeds),
		}
		var wg sync.WaitGroup
		errs := make(chan error, len(distinct)*8)
		for _, target := range distinct {
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- cache.verify(target)
				}()
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if count := cache.replayCount(); count != uint64(len(distinct)) {
			t.Fatalf("mixed distinct replays = %d, want %d", count, len(distinct))
		}
	})

	t.Run("eviction_concurrent_with_hit_and_miss", func(t *testing.T) {
		cache := newVerifyCache(1, 1<<30)
		targets := []*CheckpointV8{
			checkpoint,
			testCheckpoint(t, "verify-cache-concurrent-evict", seeds),
		}
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for i := range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- cache.verify(targets[i%len(targets)])
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("valid_and_tampered_same_claimed_digest", func(t *testing.T) {
		cache := newVerifyCache(verifyCacheMaxEntries, verifyCacheMaxRetainedBytes)
		tampered := mutateWireKeepClaimedDigest(checkpoint, func(wire *checkpointWire) {
			wire.Anchor.RunID = "forged-run"
		})
		if tampered.wire.Digest != checkpoint.wire.Digest {
			t.Fatal("tampered variant lost the claimed digest")
		}
		var wg sync.WaitGroup
		type outcome struct {
			tampered bool
			err      error
		}
		outcomes := make(chan outcome, 32)
		for i := range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					outcomes <- outcome{tampered: false, err: cache.verify(checkpoint)}
					return
				}
				outcomes <- outcome{tampered: true, err: cache.verify(tampered)}
			}()
		}
		wg.Wait()
		close(outcomes)
		for result := range outcomes {
			if result.tampered && result.err == nil {
				t.Fatal("tampered checkpoint verified under concurrency")
			}
			if !result.tampered && result.err != nil {
				t.Fatalf("valid checkpoint failed under concurrency: %v", result.err)
			}
		}
	})
}
