package epochv8

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

func TestAuditedSettlementCreatesFreshRetryAuthority(t *testing.T) {
	source := testTemplateSource("audited retry")
	checkpoint, err := Initialize("runtime-rescue", supportedCandidate(t, "audited retry"), []AuthoritySeed{{
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
	input, _ := pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	aggregate, _ := pathv1.CurrentAggregateCheckpoint(mustDecodePath(t, current.Artifact.Checkpoint))
	start, err := pathv1.AdvanceExclusiveStart(t.Context(), input, aggregate.Authority.Genesis.OutputPathID)
	if err != nil {
		t.Fatal(err)
	}
	current, err = AdvanceHead(t.Context(), current.Checkpoint, current.ArtifactJSON, source, start)
	if err != nil {
		t.Fatal(err)
	}
	input, _ = pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	aggregate, _ = pathv1.CurrentAggregateCheckpoint(mustDecodePath(t, current.Artifact.Checkpoint))
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
	claim, _ := pathv1.ClaimExclusiveAttempt(t.Context(), input, plan)
	current, err = ClaimExternal(t.Context(), current.Checkpoint, current.ArtifactJSON, source, claim)
	if err != nil {
		t.Fatal(err)
	}
	input, _ = pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	recovered, found, err := pathv1.RecoverExclusiveAttempt(t.Context(), input)
	if err != nil || !found {
		t.Fatalf("recover: found=%v err=%v", found, err)
	}
	observed, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{Outcome: "fail", Actor: "human:operator"}, false)
	if err != nil {
		t.Fatal(err)
	}
	current, err = FinishClaimedHead(t.Context(), current.Checkpoint, current.ArtifactJSON, source, observed, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	failedAuthorities := current.Checkpoint.View().Authorities
	input, _ = pathv1.VerifyExecutionInput(t.Context(), current.Artifact.Checkpoint, source)
	for _, decision := range []string{"skip", "cancel"} {
		terminalSettlement, settleErr := pathv1.SettleExclusiveAttempt(t.Context(), input, pathv1.AuditedSettlementInput{
			NodeID: "work", BlockedAttempt: 1, Decision: decision, Actor: "human:operator",
			Reason: "approved " + decision, EvidenceRef: "ticket:TCL-604-" + decision,
			Timestamp: time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC),
		})
		if settleErr != nil {
			t.Fatal(settleErr)
		}
		terminal, settleErr := AuditedSettlement(t.Context(), current.Checkpoint, current.ArtifactJSON, source, terminalSettlement)
		if settleErr != nil {
			t.Fatal(settleErr)
		}
		if added := addedVerifiedFrontiers(failedAuthorities, terminal.Checkpoint.View().Authorities); len(added) != 0 {
			t.Fatalf("%s settlement minted frontier %v", decision, added)
		}
	}
	settlement, err := pathv1.SettleExclusiveAttempt(t.Context(), input, pathv1.AuditedSettlementInput{
		NodeID: "work", BlockedAttempt: 1, Decision: "retry", Actor: "human:operator",
		Reason: "approved rescue", EvidenceRef: "ticket:TCL-604", Timestamp: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	rescued, err := AuditedSettlement(t.Context(), current.Checkpoint, current.ArtifactJSON, source, settlement)
	if err != nil {
		t.Fatal(err)
	}
	added := addedVerifiedFrontiers(failedAuthorities, rescued.Checkpoint.View().Authorities)
	if len(added) != 1 {
		t.Fatalf("retry frontier delta = %v", added)
	}
	for _, old := range failedAuthorities {
		if old.Kind == AuthorityFrontier && old.NodeID == "work" && old.State == AuthorityFailed {
			preserved, ok := authorityByID(rescued.Checkpoint.View().Authorities, old.Identity)
			if !ok || preserved.State != AuthorityFailed {
				t.Fatalf("failed owner authority was not retained")
			}
		}
	}
	input, _ = pathv1.VerifyExecutionInput(t.Context(), rescued.Artifact.Checkpoint, source)
	second, err := pathv1.PlanExclusiveAttempt(t.Context(), input, workPath, 2, nil)
	if err != nil {
		t.Fatalf("retry attempt plan: %v", err)
	}
	secondClaim, _ := pathv1.ClaimExclusiveAttempt(t.Context(), input, second)
	claimed, err := ClaimExternal(t.Context(), rescued.Checkpoint, rescued.ArtifactJSON, source, secondClaim)
	if err != nil {
		t.Fatal(err)
	}
	retryOwner, ok := authorityByID(claimed.Checkpoint.View().Authorities, added[0])
	if !ok || retryOwner.State != AuthorityClaimed {
		t.Fatalf("fresh retry authority was not claimed: %+v", retryOwner)
	}
}

func TestAttachGenesisBindsExactOwnerRuntime(t *testing.T) {
	source := testTemplateSource("runtime")
	checkpoint, err := Initialize("runtime-run", supportedCandidate(t, "runtime"), []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := AttachGenesis(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Checkpoint.View().RuntimeBinding.Revision != 1 || result.Artifact == nil || len(result.ArtifactJSON) == 0 {
		t.Fatalf("incomplete runtime attachment: %+v", result)
	}
	verified, err := VerifyRuntimeArtifact(t.Context(), result.Checkpoint, result.ArtifactJSON, source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(verified.Checkpoint, result.Artifact.Checkpoint) || verified.HeadOwner == "" {
		t.Fatalf("runtime verification changed artifact")
	}
}

func TestAdvanceHeadUsesClosedSealedPathTransition(t *testing.T) {
	source := testTemplateSource("advance")
	checkpoint, err := Initialize("runtime-advance", supportedCandidate(t, "advance"), []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := AttachGenesis(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePath(t, attached.Artifact.Checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	transition, err := pathv1.AdvanceExclusiveStart(t.Context(), input, aggregate.Authority.Genesis.OutputPathID)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := AdvanceHead(t.Context(), attached.Checkpoint, attached.ArtifactJSON, source, transition)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Checkpoint.View().RuntimeBinding.Revision != 2 || advanced.Artifact.Digest == attached.Artifact.Digest {
		t.Fatalf("runtime did not advance exactly once")
	}
}

func mustDecodePath(t *testing.T, data []byte) *pathv1.CheckpointV7 {
	t.Helper()
	checkpoint, err := pathv1.DecodeCheckpointV7(data)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func TestRuntimeAdvanceKindClosedExactSet(t *testing.T) {
	allowed := []string{
		pathv1.TransitionClaimWait, pathv1.TransitionObserveWait, pathv1.TransitionRouteObservation,
		pathv1.TransitionClaimCompletion, pathv1.TransitionObserveCompletion, pathv1.TransitionParallelSplit,
		pathv1.TransitionParallelAll, pathv1.TransitionParallelAny, pathv1.TransitionParallelRoute,
		pathv1.TransitionParallelExclusiveArrival, pathv1.TransitionParallelEnd, pathv1.TransitionParallelPropagation,
		pathv1.TransitionParallelPropagationSeed, pathv1.TransitionParallelTerminalClosure,
		pathv1.TransitionParallelDetachedSink, pathv1.TransitionParallelDetachmentIntern,
		pathv1.TransitionScheduleContact, pathv1.TransitionMarkContactDue, pathv1.TransitionNudgeContact,
		pathv1.TransitionEscalateContact, pathv1.TransitionPauseContact, pathv1.TransitionLatchContactHuman,
		pathv1.TransitionClearContactHumanLatch, pathv1.TransitionRecoverContact,
	}
	for _, kind := range allowed {
		if !runtimeAdvanceKind(kind) {
			t.Fatalf("released transition %q is not admitted", kind)
		}
	}
	for _, kind := range []string{"", "invented", pathv1.TransitionClaimAttempt, pathv1.TransitionObserveAttempt} {
		if runtimeAdvanceKind(kind) {
			t.Fatalf("special/unknown transition %q admitted generically", kind)
		}
	}
}

func TestRuntimeArtifactStrictDecodeAndOwnerSource(t *testing.T) {
	source := testTemplateSource("runtime-strict")
	checkpoint, err := Initialize("runtime-strict", supportedCandidate(t, "runtime-strict"), []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := AttachGenesis(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(result.ArtifactJSON, []byte(`"version":1`), []byte(`"unknown":1,"version":1`), 1)
	if _, err := VerifyRuntimeArtifact(t.Context(), result.Checkpoint, tampered, source); err == nil {
		t.Fatal("unknown runtime field accepted")
	}
	if _, err := VerifyRuntimeArtifact(t.Context(), result.Checkpoint, result.ArtifactJSON, testTemplateSource("other")); err == nil {
		t.Fatal("wrong owner source accepted")
	}
}

func TestRuntimeApplyRetainThenBareTransfer(t *testing.T) {
	source0 := testTemplateSource("epoch-zero")
	checkpoint, err := Initialize("runtime-apply", supportedCandidate(t, "epoch-zero"), []AuthoritySeed{{
		LocalID: "initial-frontier", ReservationID: "initial-reservation", NodeID: "start",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := AttachGenesis(t.Context(), checkpoint, source0)
	if err != nil {
		t.Fatal(err)
	}
	source1 := testTemplateSource("epoch-one")
	preview, err := PreviewApply(attached.Checkpoint, ApplyDraft{
		BaseBinding: attached.Checkpoint.Binding(), Candidate: supportedCandidate(t, "epoch-one"),
		Handoffs: retainAll(attached.Checkpoint.View().ProtectedAuthorities),
	})
	if err != nil || preview.Plan == nil {
		t.Fatalf("retain preview: %+v, %v", preview, err)
	}
	retained, err := ApplyRetainHead(t.Context(), attached.Checkpoint, attached.ArtifactJSON, source0, preview.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Checkpoint.View().RuntimeBinding != attached.Checkpoint.View().RuntimeBinding || retained.Artifact.EpochID != attached.Artifact.EpochID {
		t.Fatal("retain changed owner runtime")
	}

	frontier := findAuthority(t, retained.Checkpoint.View().Authorities, func(authority AuthorityRecord) bool {
		return authority.Kind == AuthorityFrontier && authority.State == AuthorityVerifiedUnclaimed
	})
	source2 := testTemplateSource("epoch-two")
	transfer := testPlan(t, retained.Checkpoint, "epoch-two", frontier.Identity, "epoch-two-frontier", "epoch-two-reservation")
	transferred, err := ApplyTransferHead(t.Context(), retained.Checkpoint, retained.ArtifactJSON, source0, source2, transfer)
	if err != nil {
		t.Fatal(err)
	}
	if len(transferred.Checkpoint.View().Epochs) != 3 || transferred.Artifact.EpochID != transfer.CandidateEpoch().ID || transferred.Artifact.HeadOwner == frontier.Identity {
		t.Fatalf("transfer did not install exactly one candidate head")
	}

	// A self-consistent apply record cannot be paired with an independently
	// valid runtime projection that undoes its handoff semantics.
	tampered := &CheckpointV8{wire: cloneWire(transferred.Checkpoint.wire)}
	event := &tampered.wire.History[len(tampered.wire.History)-1]
	basis, basisErr := applyHandoffBasis(applyCore{
		RunID: event.Apply.RunID, BaseBinding: event.Apply.BaseBinding, CandidateEpoch: event.Apply.CandidateEpoch,
		ReasonDigest: event.Apply.ReasonDigest, Diff: event.Apply.Diff, ProtectedDigest: event.Apply.ProtectedDigest,
	})
	if basisErr != nil {
		t.Fatal(basisErr)
	}
	for i := range event.Apply.HandoffSet {
		handoff := &event.Apply.HandoffSet[i]
		handoff.Action, handoff.Target = HandoffRetain, nil
		handoff.ID, err = handoffIdentity(handoff.Source, handoff.Action, nil, basis)
		if err != nil {
			t.Fatal(err)
		}
	}
	event.Apply.HandoffSetDigest, err = handoffSetDigest(event.Apply.HandoffSet)
	if err != nil {
		t.Fatal(err)
	}
	event.Apply.ProposalDigest, err = proposalDigest(event.Apply.applyCore)
	if err != nil {
		t.Fatal(err)
	}
	event.Apply.RecordDigest, err = applyRecordDigest(*event.Apply)
	if err != nil {
		t.Fatal(err)
	}
	event.Digest, err = historyEventDigest(*event)
	if err != nil {
		t.Fatal(err)
	}
	tampered.wire.Digest, err = checkpointDigest(tampered.wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckpointV8(tampered); err == nil {
		t.Fatal("runtime receipt that differs from its apply handoffs was accepted")
	}
	_ = source1
}
