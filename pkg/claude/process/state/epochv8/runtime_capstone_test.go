package epochv8

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

func TestAttachedRuntimeApplyCASLoserIsStale(t *testing.T) {
	runID := "runtime-apply-cas"
	source0 := testTemplateSource(runID)
	attached := runtimeAttachedFixture(t, runID)
	frontier := findAuthority(t, attached.Checkpoint.View().Authorities, func(authority AuthorityRecord) bool {
		return authority.Kind == AuthorityFrontier && authority.State == AuthorityVerifiedUnclaimed
	})
	sourceA := testTemplateSource("runtime-apply-cas-a")
	planA := testPlan(t, attached.Checkpoint, "runtime-apply-cas-a", frontier.Identity, "next-a", "next-ra")
	planB := testPlan(t, attached.Checkpoint, "runtime-apply-cas-b", frontier.Identity, "next-b", "next-rb")
	if planA.ProposalDigest() == planB.ProposalDigest() {
		t.Fatal("competing proposals have the same digest")
	}
	winner, err := ApplyTransferHead(t.Context(), attached.Checkpoint, attached.ArtifactJSON, source0, sourceA, planA)
	if err != nil || winner.Disposition != DispositionApplied {
		t.Fatalf("winner: disposition=%q err=%v", winner.Disposition, err)
	}
	winnerBytes, err := EncodeCheckpointV8(winner.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	assertStaleAndUnchanged := func(t *testing.T, applyErr error) {
		t.Helper()
		if !errors.Is(applyErr, ErrInvalid) || !strings.Contains(applyErr.Error(), "runtime apply is stale") {
			t.Fatalf("stale apply error = %v", applyErr)
		}
		got, encodeErr := EncodeCheckpointV8(winner.Checkpoint)
		if encodeErr != nil || !bytes.Equal(got, winnerBytes) {
			t.Fatalf("rejected apply mutated the checkpoint: %v", encodeErr)
		}
	}

	t.Run("stale_loser_retain_head", func(t *testing.T) {
		_, err := ApplyRetainHead(t.Context(), winner.Checkpoint, winner.ArtifactJSON, sourceA, planB)
		assertStaleAndUnchanged(t, err)
	})
	t.Run("stale_loser_transfer_head", func(t *testing.T) {
		_, err := ApplyTransferHead(t.Context(), winner.Checkpoint, winner.ArtifactJSON, sourceA, testTemplateSource("runtime-apply-cas-b"), planB)
		assertStaleAndUnchanged(t, err)
	})

	preview, err := PreviewApply(winner.Checkpoint, ApplyDraft{
		BaseBinding: winner.Checkpoint.Binding(), Candidate: supportedCandidate(t, "runtime-apply-cas-retain"),
		Handoffs: retainAll(winner.Checkpoint.View().ProtectedAuthorities),
	})
	if err != nil || preview.Plan == nil {
		t.Fatalf("retain preview: %+v, %v", preview, err)
	}
	if _, err := ApplyRetainHead(t.Context(), winner.Checkpoint, winner.ArtifactJSON, sourceA, preview.Plan); err != nil {
		t.Fatalf("control retain apply: %v", err)
	}

	t.Run("mismatched_predecessor_epoch", func(t *testing.T) {
		tampered := resealedApplyPlan(t, runID, preview.Plan, func(core *applyCore) {
			core.PredecessorEpoch = EpochID(testDigest("bogus-predecessor"))
			core.CandidateEpoch.PredecessorEpochID = core.PredecessorEpoch
		})
		_, err := ApplyRetainHead(t.Context(), winner.Checkpoint, winner.ArtifactJSON, sourceA, tampered)
		assertStaleAndUnchanged(t, err)
	})
	t.Run("mismatched_candidate_ordinal", func(t *testing.T) {
		tampered := resealedApplyPlan(t, runID, preview.Plan, func(core *applyCore) {
			core.CandidateEpoch.Ordinal++
		})
		_, err := ApplyRetainHead(t.Context(), winner.Checkpoint, winner.ArtifactJSON, sourceA, tampered)
		assertStaleAndUnchanged(t, err)
	})
}

// resealedApplyPlan mutates a sealed plan core and recomputes every dependent
// digest so the result is self-consistent. The attached apply lattice must
// still refuse it on the base/epoch freshness clauses alone.
func resealedApplyPlan(t *testing.T, runID string, plan *ApplyPlan, mutate func(*applyCore)) *ApplyPlan {
	t.Helper()
	core := cloneApplyCore(plan.core)
	mutate(&core)
	var err error
	core.CandidateEpoch.ID, err = epochIdentity(runID, core.CandidateEpoch)
	if err != nil {
		t.Fatal(err)
	}
	basis, err := applyHandoffBasis(core)
	if err != nil {
		t.Fatal(err)
	}
	for i := range core.HandoffSet {
		handoff := &core.HandoffSet[i]
		handoff.ID, err = handoffIdentity(handoff.Source, handoff.Action, handoff.Target, basis)
		if err != nil {
			t.Fatal(err)
		}
	}
	core.HandoffSetDigest, err = handoffSetDigest(core.HandoffSet)
	if err != nil {
		t.Fatal(err)
	}
	core.ProposalDigest, err = proposalDigest(core)
	if err != nil {
		t.Fatal(err)
	}
	return &ApplyPlan{core: core}
}

func TestPreflightAuditedSettlementIsPureAndMatchesRealSettlement(t *testing.T) {
	current, source := runtimeFailedAttemptFixture(t, "runtime-settlement-preflight")
	input, err := pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := pathv1.SettleExclusiveAttempt(t.Context(), input, pathv1.AuditedSettlementInput{
		NodeID: "work", BlockedAttempt: 1, Decision: "skip", Actor: "human:operator",
		Reason: "approved skip", EvidenceRef: "ticket:TCL-608",
		Timestamp: time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeCheckpoint, err := EncodeCheckpointV8(current.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	beforeArtifact := bytes.Clone(current.ArtifactJSON)

	preflight, err := PreflightAuditedSettlement(t.Context(), current.Checkpoint, current.ArtifactJSON, source, settlement)
	if err != nil || preflight.Disposition != DispositionApplied {
		t.Fatalf("preflight: disposition=%q err=%v", preflight.Disposition, err)
	}
	afterCheckpoint, err := EncodeCheckpointV8(current.Checkpoint)
	if err != nil || !bytes.Equal(afterCheckpoint, beforeCheckpoint) || !bytes.Equal(current.ArtifactJSON, beforeArtifact) {
		t.Fatalf("preflight mutated its input: %v", err)
	}

	settled, err := AuditedSettlement(t.Context(), current.Checkpoint, current.ArtifactJSON, source, settlement)
	if err != nil {
		t.Fatal(err)
	}
	preflightBytes, err := EncodeCheckpointV8(preflight.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	settledBytes, err := EncodeCheckpointV8(settled.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(preflightBytes, settledBytes) || !bytes.Equal(preflight.ArtifactJSON, settled.ArtifactJSON) ||
		!reflect.DeepEqual(preflight.Artifact, settled.Artifact) || preflight.Disposition != settled.Disposition ||
		preflight.Binding != settled.Binding || preflight.Provenance != settled.Provenance {
		t.Fatal("preflight result differs from the real settlement")
	}

	replayed, err := AuditedSettlement(t.Context(), settled.Checkpoint, settled.ArtifactJSON, source, settlement)
	if err != nil || replayed.Disposition != DispositionReplayed {
		t.Fatalf("settlement replay: disposition=%q err=%v", replayed.Disposition, err)
	}
	replayedBytes, err := EncodeCheckpointV8(replayed.Checkpoint)
	if err != nil || !bytes.Equal(replayedBytes, settledBytes) || !bytes.Equal(replayed.ArtifactJSON, settled.ArtifactJSON) {
		t.Fatalf("replay of the real settlement was not idempotent: %v", err)
	}
}

func runtimeFailedAttemptFixture(t *testing.T, runID string) (RuntimeTransitionResult, []byte) {
	t.Helper()
	source := testTemplateSource(runID)
	checkpoint, err := Initialize(runID, supportedCandidate(t, runID), []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := AttachGenesis(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	input, err := pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePath(t, current.Artifact.Checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	start, err := pathv1.AdvanceExclusiveStart(t.Context(), input, aggregate.Authority.Genesis.OutputPathID)
	if err != nil {
		t.Fatal(err)
	}
	current, err = AdvanceHead(t.Context(), current.Checkpoint, current.ArtifactJSON, source, start)
	if err != nil {
		t.Fatal(err)
	}
	input, err = pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err = pathv1.CurrentAggregateCheckpoint(mustDecodePath(t, current.Artifact.Checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	var workPath pathv1.PathID
	for _, path := range aggregate.Routing.Paths {
		activation := aggregate.Routing.Activations[path.SourceActivation.ID]
		if path.Kind == pathv1.PathActivationOutput && path.State == pathv1.PathLive && aggregate.Routing.Reservations[activation.ReservationID].NodeID == "work" {
			workPath = path.ID
		}
	}
	plan, err := pathv1.PlanExclusiveAttempt(t.Context(), input, workPath, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := pathv1.ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	current, err = ClaimExternal(t.Context(), current.Checkpoint, current.ArtifactJSON, source, claim)
	if err != nil {
		t.Fatal(err)
	}
	input, err = pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	recovered, found, err := pathv1.RecoverExclusiveAttempt(t.Context(), input)
	if err != nil || !found {
		t.Fatalf("recover claimed attempt: found=%v err=%v", found, err)
	}
	observed, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{Outcome: "fail", Actor: "human:operator"}, false)
	if err != nil {
		t.Fatal(err)
	}
	current, err = FinishClaimedHead(t.Context(), current.Checkpoint, current.ArtifactJSON, source, observed, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return current, source
}
