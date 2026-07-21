package epochv8

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// BenchmarkVerifyCheckpointV8UncachedFirstVerification exposes the remaining
// first-verification cost curve over growing runtime receipt histories. The
// memo removes structurally repeated verifications, but the first
// verification of freshly loaded bytes still replays the complete receipt
// chain; this benchmark reports how that cost scales. Numbers are reported,
// not asserted.
func BenchmarkVerifyCheckpointV8UncachedFirstVerification(b *testing.B) {
	for _, cycles := range []int{0, 2, 4, 8} {
		checkpoint := benchmarkRetryHistoryCheckpoint(b, cycles)
		receipts := 0
		for _, event := range checkpoint.wire.History {
			if event.Runtime != nil {
				receipts++
			}
		}
		b.Run(fmt.Sprintf("runtime_receipts_%d", receipts), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := verifyCheckpointV8Uncached(checkpoint); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchmarkRetryHistoryCheckpoint grows a schema-8 runtime receipt history by
// repeating fail -> finish -> audited retry settlement cycles on the "work"
// node, mirroring the production settlement-retry flow.
func benchmarkRetryHistoryCheckpoint(b *testing.B, cycles int) *CheckpointV8 {
	b.Helper()
	ctx := context.Background()
	runID := fmt.Sprintf("verify-bench-%d", cycles)
	source := testTemplateSource(runID)
	classification, err := ClassifyTemplateSource(source)
	if err != nil || classification.Status != EligibilitySupported {
		b.Fatalf("benchmark classification = %+v, err = %v", classification, err)
	}
	checkpoint, err := Initialize(runID, classification.Candidate(), []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		b.Fatal(err)
	}
	current, err := AttachGenesis(ctx, checkpoint, source)
	if err != nil {
		b.Fatal(err)
	}
	input, err := pathv1.VerifyExecutionInput(ctx, current.Artifact.Checkpoint, source)
	if err != nil {
		b.Fatal(err)
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(benchDecodePath(b, current.Artifact.Checkpoint))
	if err != nil {
		b.Fatal(err)
	}
	start, err := pathv1.AdvanceExclusiveStart(ctx, input, aggregate.Authority.Genesis.OutputPathID)
	if err != nil {
		b.Fatal(err)
	}
	current, err = AdvanceHead(ctx, current.Checkpoint, current.ArtifactJSON, source, start)
	if err != nil {
		b.Fatal(err)
	}
	for attempt := 1; attempt <= cycles; attempt++ {
		input, err = pathv1.VerifyExecutionInput(ctx, current.Artifact.Checkpoint, source)
		if err != nil {
			b.Fatal(err)
		}
		aggregate, err = pathv1.CurrentAggregateCheckpoint(benchDecodePath(b, current.Artifact.Checkpoint))
		if err != nil {
			b.Fatal(err)
		}
		var workPath pathv1.PathID
		for _, path := range aggregate.Routing.Paths {
			activation := aggregate.Routing.Activations[path.SourceActivation.ID]
			if path.Kind == pathv1.PathActivationOutput && path.State == pathv1.PathLive && aggregate.Routing.Reservations[activation.ReservationID].NodeID == "work" {
				workPath = path.ID
			}
		}
		plan, err := pathv1.PlanExclusiveAttempt(ctx, input, workPath, uint64(attempt), nil)
		if err != nil {
			b.Fatal(err)
		}
		claim, err := pathv1.ClaimExclusiveAttempt(ctx, input, plan)
		if err != nil {
			b.Fatal(err)
		}
		current, err = ClaimExternal(ctx, current.Checkpoint, current.ArtifactJSON, source, claim)
		if err != nil {
			b.Fatal(err)
		}
		input, err = pathv1.VerifyExecutionInput(ctx, current.Artifact.Checkpoint, source)
		if err != nil {
			b.Fatal(err)
		}
		recovered, found, err := pathv1.RecoverExclusiveAttempt(ctx, input)
		if err != nil || !found {
			b.Fatalf("recover claimed attempt: found=%v err=%v", found, err)
		}
		observed, err := pathv1.ObserveExclusiveAttempt(ctx, input, recovered, pathv1.ExclusiveObservation{Outcome: "fail", Actor: "human:operator"}, false)
		if err != nil {
			b.Fatal(err)
		}
		current, err = FinishClaimedHead(ctx, current.Checkpoint, current.ArtifactJSON, source, observed, strings.Repeat("a", 64))
		if err != nil {
			b.Fatal(err)
		}
		input, err = pathv1.VerifyExecutionInput(ctx, current.Artifact.Checkpoint, source)
		if err != nil {
			b.Fatal(err)
		}
		settlement, err := pathv1.SettleExclusiveAttempt(ctx, input, pathv1.AuditedSettlementInput{
			NodeID: "work", BlockedAttempt: uint64(attempt), Decision: "retry", Actor: "human:operator",
			Reason: "benchmark retry", EvidenceRef: fmt.Sprintf("ticket:bench-%d", attempt),
			Timestamp: time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC).Add(time.Duration(attempt) * time.Minute),
		})
		if err != nil {
			b.Fatal(err)
		}
		current, err = AuditedSettlement(ctx, current.Checkpoint, current.ArtifactJSON, source, settlement)
		if err != nil {
			b.Fatal(err)
		}
	}
	return current.Checkpoint
}

func benchDecodePath(b *testing.B, data []byte) *pathv1.CheckpointV7 {
	b.Helper()
	checkpoint, err := pathv1.DecodeCheckpointV7(data)
	if err != nil {
		b.Fatal(err)
	}
	return checkpoint
}
