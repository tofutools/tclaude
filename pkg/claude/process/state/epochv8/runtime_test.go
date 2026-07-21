package epochv8

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

func TestRuntimeWitnessRejectsCoherentForgeries(t *testing.T) {
	started := runtimeStartedFixture(t, "runtime-witness-forgery")

	t.Run("coherently_rehashed_allowed_kind_mint", func(t *testing.T) {
		assertRehashedRuntimeMintRejected(t, started.Checkpoint, RuntimeAdvanceHead)
	})
	t.Run("route_submode_substitution", func(t *testing.T) {
		forged := rehashLastRuntimeReceipt(t, started.Checkpoint, RuntimeAdvanceHead, func(receipt *RuntimeReceipt) {
			receipt.ExecutionWitness.RouteObservation.Mode = "pending_route"
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "execution witness replay") {
			t.Fatalf("coherently rehashed route witness forgery was accepted: %v", err)
		}
	})
	t.Run("kind_substitution", func(t *testing.T) {
		forged := rehashLastRuntimeReceipt(t, started.Checkpoint, RuntimeAdvanceHead, func(receipt *RuntimeReceipt) {
			receipt.PathTransitionKind = pathv1.TransitionParallelAny
			receipt.ExecutionWitness.Kind = pathv1.TransitionParallelAny
			receipt.ExecutionWitness.RouteObservation = nil
			receipt.ExecutionWitness.Empty = &pathv1.EmptyExecutionWitnessV1{}
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "execution witness replay") {
			t.Fatalf("transition kind substitution was accepted: %v", err)
		}
	})
	t.Run("pre_binding_splice", func(t *testing.T) {
		forged := rehashLastRuntimeReceipt(t, started.Checkpoint, RuntimeAdvanceHead, func(receipt *RuntimeReceipt) {
			receipt.ExecutionWitness.Pre = receipt.ExecutionWitness.Post
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "pre-binding mismatch") {
			t.Fatalf("witness pre-binding splice was accepted: %v", err)
		}
	})
	t.Run("advance_owner_substitution", func(t *testing.T) {
		forged := rehashLastRuntimeReceipt(t, started.Checkpoint, RuntimeAdvanceHead, func(receipt *RuntimeReceipt) {
			receipt.Owner = alternateSameEpochAuthority(t, receipt)
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "owner differs from lineage") {
			t.Fatalf("advance owner substitution was accepted: %v", err)
		}
	})
	t.Run("execution_witness_bound", func(t *testing.T) {
		forged := rehashLastRuntimeReceipt(t, started.Checkpoint, RuntimeAdvanceHead, func(receipt *RuntimeReceipt) {
			receipt.ExecutionWitness.RouteObservation.Mode = strings.Repeat("x", pathv1.MaxExecutionWitnessBytes)
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "execution_witness_bytes") {
			t.Fatalf("oversized execution witness was accepted: %v", err)
		}
	})
}

func alternateSameEpochAuthority(t *testing.T, receipt *RuntimeReceipt) OwnerIdentity {
	t.Helper()
	for _, authority := range receipt.After {
		if authority.EpochID == receipt.EpochID && authority.Identity != receipt.Owner {
			return authority.Identity
		}
	}
	t.Fatal("alternate same-epoch authority is absent")
	return ""
}

func TestRuntimeWitnessCumulativeBudgetNearLimit(t *testing.T) {
	remaining := pathv1.MaxCheckpointBytes - 1
	got, err := accumulateRuntimeWitnessBytes(remaining, 1)
	if err != nil || got != pathv1.MaxCheckpointBytes {
		t.Fatalf("exact cumulative witness limit: got=%d err=%v", got, err)
	}
	if _, err := accumulateRuntimeWitnessBytes(remaining, 2); err == nil || !strings.Contains(err.Error(), "runtime_witness_bytes") {
		t.Fatalf("cumulative witness overflow was accepted: %v", err)
	}
}

func TestRuntimeGenesisWitnessRejectsNodeSemanticPreimageMismatch(t *testing.T) {
	attached := runtimeAttachedFixture(t, "runtime-genesis-forgery")
	forged := rehashLastRuntimeReceipt(t, attached.Checkpoint, RuntimeAttachGenesis, func(receipt *RuntimeReceipt) {
		var tmpl model.Template
		if err := json.Unmarshal(receipt.GenesisWitness.Template, &tmpl); err != nil {
			t.Fatal(err)
		}
		node := tmpl.Nodes["start"]
		node.Name = "forged semantic node"
		tmpl.Nodes["start"] = node
		canonical, err := model.CanonicalSemanticJSON(&tmpl)
		if err != nil {
			t.Fatal(err)
		}
		receipt.GenesisWitness.Template = canonical
	})
	if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "semantic") {
		t.Fatalf("node semantic preimage mismatch was accepted: %v", err)
	}
}

func runtimeAttachedFixture(t *testing.T, runID string) RuntimeTransitionResult {
	t.Helper()
	source := testTemplateSource(runID)
	checkpoint, err := Initialize(runID, supportedCandidate(t, runID), []AuthoritySeed{{
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
	return attached
}

func runtimeStartedFixture(t *testing.T, runID string) RuntimeTransitionResult {
	t.Helper()
	source := testTemplateSource(runID)
	attached := runtimeAttachedFixture(t, runID)
	input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePath(t, attached.Artifact.Checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	start, err := pathv1.AdvanceExclusiveStart(t.Context(), input, aggregate.Authority.Genesis.OutputPathID)
	if err != nil {
		t.Fatal(err)
	}
	started, err := AdvanceHead(t.Context(), attached.Checkpoint, attached.ArtifactJSON, source, start)
	if err != nil {
		t.Fatal(err)
	}
	return started
}

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
	assertRehashedRuntimeMintRejected(t, current.Checkpoint, RuntimeAdvanceHead)
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
	assertRehashedRuntimeDropRejected(t, current.Checkpoint, RuntimeClaimExternal)
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
	assertRehashedRuntimeMintRejected(t, current.Checkpoint, RuntimeFinishClaimed)
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
		t.Run(decision+"_outer_decision_substitution", func(t *testing.T) {
			forged := rehashLastRuntimeReceipt(t, terminal.Checkpoint, RuntimeSettlement, func(receipt *RuntimeReceipt) {
				if receipt.Decision == "skip" {
					receipt.Decision = "cancel"
				} else {
					receipt.Decision = "skip"
				}
				resolution := pathv1.BlockResolution{NodeID: receipt.NodeID, BlockedAttempt: receipt.BlockedAttempt, Decision: receipt.Decision, Actor: receipt.Actor, Reason: receipt.Reason, EvidenceRef: receipt.EvidenceRef, Timestamp: receipt.Timestamp}
				var digestErr error
				receipt.ResolutionDigest, digestErr = pathv1.ValidateBlockResolution(resolution)
				if digestErr != nil {
					t.Fatal(digestErr)
				}
			})
			if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "provenance differs from typed witness") {
				t.Fatalf("outer settlement decision substitution was accepted: %v", err)
			}
		})
		t.Run(decision+"_outer_metadata_substitution", func(t *testing.T) {
			forged := rehashLastRuntimeReceipt(t, terminal.Checkpoint, RuntimeSettlement, func(receipt *RuntimeReceipt) {
				receipt.Actor, receipt.Reason, receipt.EvidenceRef = "human:forged", "forged reason", "ticket:forged"
				resolution := pathv1.BlockResolution{NodeID: receipt.NodeID, BlockedAttempt: receipt.BlockedAttempt, Decision: receipt.Decision, Actor: receipt.Actor, Reason: receipt.Reason, EvidenceRef: receipt.EvidenceRef, Timestamp: receipt.Timestamp}
				var digestErr error
				receipt.ResolutionDigest, digestErr = pathv1.ValidateBlockResolution(resolution)
				if digestErr != nil {
					t.Fatal(digestErr)
				}
			})
			if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "provenance differs from typed witness") {
				t.Fatalf("outer settlement metadata substitution was accepted: %v", err)
			}
		})
		t.Run(decision+"_owner_substitution", func(t *testing.T) {
			forged := rehashLastRuntimeReceipt(t, terminal.Checkpoint, RuntimeSettlement, func(receipt *RuntimeReceipt) {
				receipt.Owner = alternateSameEpochAuthority(t, receipt)
			})
			if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "owner differs from lineage") {
				t.Fatalf("nonretry settlement owner substitution was accepted: %v", err)
			}
		})
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
	assertRehashedRuntimeDropRejected(t, rescued.Checkpoint, RuntimeSettlement)
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

func assertRehashedRuntimeMintRejected(t *testing.T, checkpoint *CheckpointV8, kind RuntimeTransitionKind) {
	t.Helper()
	forged := rehashLastRuntimeReceipt(t, checkpoint, kind, func(receipt *RuntimeReceipt) {
		authority := AuthorityRecord{
			EpochID: receipt.EpochID, LocalID: "command.forged", ReservationID: "forged", NodeID: "work",
			Kind: AuthorityCommand, State: AuthorityCompleted, DependsOn: []OwnerIdentity{},
		}
		var err error
		authority.Identity, err = authorityIdentity(checkpoint.wire.Anchor.RunID, authority)
		if err != nil {
			t.Fatal(err)
		}
		sealTerminal(&authority, "forged", authority.LocalID, string(authority.State))
		receipt.After = append(receipt.After, authority)
		sortAuthorities(receipt.After)
	})
	if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "typed witness projection") {
		t.Fatalf("rehashed %s mint forgery was not rejected semantically: %v", kind, err)
	}
}

func assertRehashedRuntimeDropRejected(t *testing.T, checkpoint *CheckpointV8, kind RuntimeTransitionKind) {
	t.Helper()
	last := checkpoint.wire.History[len(checkpoint.wire.History)-1].Runtime
	if last == nil || last.Kind != kind {
		t.Fatalf("last runtime receipt is %v, want %s", last, kind)
	}
	for _, candidate := range last.Before {
		if candidate.Identity == last.Owner || runtimeAuthorityReferenced(last.After, candidate.Identity) {
			continue
		}
		forged := rehashLastRuntimeReceipt(t, checkpoint, kind, func(receipt *RuntimeReceipt) {
			receipt.After = slices.DeleteFunc(receipt.After, func(authority AuthorityRecord) bool { return authority.Identity == candidate.Identity })
		})
		if err := VerifyCheckpointV8(forged); err != nil && strings.Contains(err.Error(), "typed witness projection") {
			return
		}
	}
	t.Fatalf("could not construct semantic %s drop forgery", kind)
}

func runtimeAuthorityReferenced(authorities []AuthorityRecord, identity OwnerIdentity) bool {
	for _, authority := range authorities {
		if authority.Successor == identity || slices.Contains(authority.DependsOn, identity) {
			return true
		}
	}
	return false
}

func rehashLastRuntimeReceipt(t *testing.T, checkpoint *CheckpointV8, kind RuntimeTransitionKind, mutate func(*RuntimeReceipt)) *CheckpointV8 {
	t.Helper()
	wire := cloneWire(checkpoint.wire)
	event := &wire.History[len(wire.History)-1]
	if event.Runtime == nil || event.Runtime.Kind != kind {
		t.Fatalf("last runtime receipt is %v, want %s", event.Runtime, kind)
	}
	mutate(event.Runtime)
	var err error
	event.Runtime.ID, err = runtimeReceiptIdentity(*event.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	event.Digest, err = historyEventDigest(*event)
	if err != nil {
		t.Fatal(err)
	}
	wire.Authorities = cloneAuthorities(event.Runtime.After)
	wire.RuntimeBinding = event.Runtime.PostRuntime
	wire.Digest, err = checkpointDigest(wire)
	if err != nil {
		t.Fatal(err)
	}
	return &CheckpointV8{wire: wire}
}

func TestRuntimeReplayRejectsTemporaryAuthorityDropMintAndRestore(t *testing.T) {
	finished := runtimeFinishedFixture(t, "runtime-history-forgery")
	claimIndex, finishIndex := -1, -1
	for i := range finished.wire.History {
		receipt := finished.wire.History[i].Runtime
		if receipt == nil {
			continue
		}
		switch receipt.Kind {
		case RuntimeClaimExternal:
			claimIndex = i
		case RuntimeFinishClaimed:
			finishIndex = i
		}
	}
	if claimIndex < 0 || finishIndex <= claimIndex {
		t.Fatal("claim/finish receipts are absent")
	}
	claim := finished.wire.History[claimIndex].Runtime
	var conserved OwnerIdentity
	for _, authority := range claim.Before {
		if authority.Identity != claim.Owner && !runtimeAuthorityReferenced(claim.After, authority.Identity) {
			conserved = authority.Identity
			break
		}
	}
	if conserved == "" {
		t.Fatal("conserved authority for temporary drop forgery is absent")
	}

	t.Run("drop_then_restore", func(t *testing.T) {
		forged := rehashRuntimeReceipts(t, finished, []int{claimIndex, finishIndex}, func(index int, receipt *RuntimeReceipt) {
			if index == claimIndex {
				receipt.After = slices.DeleteFunc(receipt.After, func(authority AuthorityRecord) bool { return authority.Identity == conserved })
			} else {
				receipt.Before = slices.DeleteFunc(receipt.Before, func(authority AuthorityRecord) bool { return authority.Identity == conserved })
			}
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "typed witness projection") {
			t.Fatalf("temporary authority drop/restore was not rejected semantically: %v", err)
		}
	})

	t.Run("mint_then_remove", func(t *testing.T) {
		forgedAuthority := AuthorityRecord{
			EpochID: claim.EpochID, LocalID: "forged.temporary", ReservationID: "forged.temporary", NodeID: "work",
			Kind: AuthorityRetry, State: AuthorityCompleted, DependsOn: []OwnerIdentity{},
		}
		var err error
		forgedAuthority.Identity, err = authorityIdentity(finished.wire.Anchor.RunID, forgedAuthority)
		if err != nil {
			t.Fatal(err)
		}
		sealTerminal(&forgedAuthority, "forged", forgedAuthority.LocalID, string(forgedAuthority.State))
		forged := rehashRuntimeReceipts(t, finished, []int{claimIndex, finishIndex}, func(index int, receipt *RuntimeReceipt) {
			if index == claimIndex {
				receipt.After = append(receipt.After, forgedAuthority)
				sortAuthorities(receipt.After)
			} else {
				receipt.Before = append(receipt.Before, forgedAuthority)
				sortAuthorities(receipt.Before)
			}
		})
		if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "typed witness projection") {
			t.Fatalf("temporary authority mint/removal was not rejected semantically: %v", err)
		}
	})
}

func rehashRuntimeReceipts(t *testing.T, checkpoint *CheckpointV8, indices []int, mutate func(int, *RuntimeReceipt)) *CheckpointV8 {
	t.Helper()
	wire := cloneWire(checkpoint.wire)
	for _, index := range indices {
		event := &wire.History[index]
		if event.Runtime == nil {
			t.Fatalf("history event %d has no runtime receipt", index)
		}
		mutate(index, event.Runtime)
		var err error
		event.Runtime.ID, err = runtimeReceiptIdentity(*event.Runtime)
		if err != nil {
			t.Fatal(err)
		}
		event.Digest, err = historyEventDigest(*event)
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	wire.Digest, err = checkpointDigest(wire)
	if err != nil {
		t.Fatal(err)
	}
	return &CheckpointV8{wire: wire}
}

func runtimeFinishedFixture(t *testing.T, runID string) *CheckpointV8 {
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
	observed, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{Outcome: "pass", Actor: "human:operator"}, false)
	if err != nil {
		t.Fatal(err)
	}
	current, err = FinishClaimedHead(t.Context(), current.Checkpoint, current.ArtifactJSON, source, observed, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return current.Checkpoint
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

func TestAuditedSettlementRejectsNilTransition(t *testing.T) {
	source := testTemplateSource("nil settlement")
	checkpoint, err := Initialize("runtime-nil-settlement", supportedCandidate(t, "nil settlement"), []AuthoritySeed{{
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
	if _, err := AuditedSettlement(t.Context(), attached.Checkpoint, attached.ArtifactJSON, source, nil); err == nil {
		t.Fatal("nil audited settlement transition was accepted")
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

func TestRuntimeAdvanceReplaySearchesPastRetainReceipt(t *testing.T) {
	source0 := testTemplateSource("replay epoch zero")
	checkpoint, err := Initialize("runtime-replay-retain", supportedCandidate(t, "replay epoch zero"), []AuthoritySeed{{
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
	input, err := pathv1.VerifyExecutionInput(t.Context(), attached.Artifact.Checkpoint, source0)
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
	advanced, err := AdvanceHead(t.Context(), attached.Checkpoint, attached.ArtifactJSON, source0, transition)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewApply(advanced.Checkpoint, ApplyDraft{
		BaseBinding: advanced.Checkpoint.Binding(), Candidate: supportedCandidate(t, "replay epoch one"),
		Handoffs: retainAll(advanced.Checkpoint.View().ProtectedAuthorities),
	})
	if err != nil || preview.Plan == nil {
		t.Fatalf("retain preview: %+v, %v", preview, err)
	}
	retained, err := ApplyRetainHead(t.Context(), advanced.Checkpoint, advanced.ArtifactJSON, source0, preview.Plan)
	if err != nil {
		t.Fatal(err)
	}
	wantCheckpoint, err := EncodeCheckpointV8(retained.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := AdvanceHead(t.Context(), retained.Checkpoint, retained.ArtifactJSON, source0, transition)
	if err != nil {
		t.Fatal(err)
	}
	gotCheckpoint, err := EncodeCheckpointV8(replayed.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Disposition != DispositionReplayed || !bytes.Equal(gotCheckpoint, wantCheckpoint) || !bytes.Equal(replayed.ArtifactJSON, retained.ArtifactJSON) {
		t.Fatal("lost-ack transition replay after retain was not byte-exact")
	}
	if _, err := exactRuntimeReplay(retained.Checkpoint, retained.Artifact, retained.ArtifactJSON, RuntimeAdvanceHead, transition.Kind(), strings.Repeat("f", 64), ""); err == nil {
		t.Fatal("nonmatching transition authority sharing the retained binding was accepted")
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

func TestMergeRuntimeHistoryTerminalizesOnlyActivatedReservationAuthorities(t *testing.T) {
	reservation := AuthorityRecord{
		Identity: "reservation-outcome", EpochID: "epoch", LocalID: "reservation.path-reservation",
		ReservationID: "reservation.path-reservation", NodeID: "left", Kind: AuthorityOutcome, State: AuthorityActive,
		DependsOn: []OwnerIdentity{},
	}
	join := AuthorityRecord{
		Identity: "reservation-join", EpochID: "epoch", LocalID: "reservation.join-reservation",
		ReservationID: "reservation.join-reservation", NodeID: "join", Kind: AuthorityJoin, State: AuthorityActive,
		DependsOn: []OwnerIdentity{},
	}
	command := AuthorityRecord{
		Identity: "missing-command", EpochID: "epoch", LocalID: "command.missing", ReservationID: "path-reservation",
		NodeID: "left", Kind: AuthorityCommand, State: AuthorityActive, DependsOn: []OwnerIdentity{},
	}
	malformed := AuthorityRecord{
		Identity: "malformed-reservation", EpochID: "epoch", LocalID: "reservation.malformed", ReservationID: "different",
		NodeID: "left", Kind: AuthorityOutcome, State: AuthorityActive, DependsOn: []OwnerIdentity{},
	}
	terminal := AuthorityRecord{
		Identity: "old-terminal", EpochID: "epoch", LocalID: "path.old", ReservationID: "old",
		NodeID: "old", Kind: AuthorityOutcome, State: AuthorityCompleted, DependsOn: []OwnerIdentity{}, TerminalRecordID: "sealed",
	}

	merged := mergeRuntimeHistory([]AuthorityRecord{reservation, join, command, malformed, terminal}, nil)
	if len(merged) != 3 {
		t.Fatalf("merged authority count = %d, want 3: %+v", len(merged), merged)
	}
	for _, identity := range []OwnerIdentity{reservation.Identity, join.Identity} {
		authority, ok := authorityByID(merged, identity)
		if !ok || authority.State != AuthorityCompleted || authority.TerminalRecordID == "" {
			t.Fatalf("activated reservation %q was not terminalized exactly: %+v", identity, authority)
		}
	}
	if _, ok := authorityByID(merged, command.Identity); ok {
		t.Fatal("unrelated missing active command was synthesized")
	}
	if _, ok := authorityByID(merged, malformed.Identity); ok {
		t.Fatal("malformed reservation relationship was synthesized")
	}
	if got, ok := authorityByID(merged, terminal.Identity); !ok || !reflect.DeepEqual(got, terminal) {
		t.Fatalf("historical terminal was not byte-identical: %+v", got)
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

func TestRuntimeRetainReceiptBindsOriginalLineageAcrossEpoch(t *testing.T) {
	runID := "runtime-retain-owner-binding"
	source := testTemplateSource(runID)
	started := runtimeStartedFixture(t, runID)
	preview, err := PreviewApply(started.Checkpoint, ApplyDraft{
		BaseBinding: started.Checkpoint.Binding(), Candidate: supportedCandidate(t, "runtime-retain-next"),
		Handoffs: retainAll(started.Checkpoint.View().ProtectedAuthorities),
	})
	if err != nil || preview.Plan == nil {
		t.Fatalf("retain preview: %+v, %v", preview, err)
	}
	retained, err := ApplyRetainHead(t.Context(), started.Checkpoint, started.ArtifactJSON, source, preview.Plan)
	if err != nil {
		t.Fatal(err)
	}
	forged := rehashLastRuntimeReceipt(t, retained.Checkpoint, RuntimeApplyRetain, func(receipt *RuntimeReceipt) {
		receipt.Owner = alternateSameEpochAuthority(t, receipt)
	})
	if err := VerifyCheckpointV8(forged); err == nil || !strings.Contains(err.Error(), "retain runtime lineage witness") {
		t.Fatalf("retain owner substitution across epoch change was accepted: %v", err)
	}
}
