package epochv8

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestEpochZeroToOneToTwoReplayAndFinishClaimed(t *testing.T) {
	checkpoint := testCheckpoint(t, "zero", []AuthoritySeed{
		{LocalID: "claimed", ReservationID: "claimed-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityClaimed},
		{LocalID: "rescue", ReservationID: "rescue-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed},
	})
	original := checkpoint.View()
	claimed := findAuthority(t, original.Authorities, func(authority AuthorityRecord) bool { return authority.LocalID == "claimed" })
	rescue := findAuthority(t, original.Authorities, func(authority AuthorityRecord) bool { return authority.LocalID == "rescue" })

	planOne := testPlan(t, checkpoint, "one", rescue.Identity, "rescue-1", "rescue-r1")
	first, err := Apply(checkpoint, planOne)
	if err != nil || first.Disposition != DispositionApplied {
		t.Fatalf("apply epoch one: disposition=%q err=%v", first.Disposition, err)
	}
	checkpoint = first.Checkpoint
	viewOne := checkpoint.View()
	oldRescue := findAuthority(t, viewOne.Authorities, func(authority AuthorityRecord) bool { return authority.Identity == rescue.Identity })
	successorOne := findAuthority(t, viewOne.Authorities, func(authority AuthorityRecord) bool {
		return authority.EpochID == viewOne.CurrentEpoch && authority.State == AuthorityVerifiedUnclaimed
	})
	if oldRescue.State != AuthorityHandedOff || oldRescue.Successor != successorOne.Identity || successorOne.DependsOn[0] != oldRescue.Identity {
		t.Fatalf("one-to-one handoff conservation failed: old=%+v successor=%+v", oldRescue, successorOne)
	}
	retainedClaim := findAuthority(t, viewOne.Authorities, func(authority AuthorityRecord) bool { return authority.Identity == claimed.Identity })
	if retainedClaim.EpochID != original.CurrentEpoch || retainedClaim.State != AuthorityClaimed {
		t.Fatalf("claimed authority was relabelled: %+v", retainedClaim)
	}

	replayOne, err := Apply(checkpoint, planOne)
	if err != nil || replayOne.Disposition != DispositionReplayed || replayOne.Binding != checkpoint.Binding() {
		t.Fatalf("ack-loss replay: disposition=%q err=%v", replayOne.Disposition, err)
	}

	planTwo := testPlan(t, checkpoint, "two", successorOne.Identity, "rescue-2", "rescue-r2")
	second, err := Apply(checkpoint, planTwo)
	if err != nil || second.Disposition != DispositionApplied {
		t.Fatalf("apply epoch two: disposition=%q err=%v", second.Disposition, err)
	}
	checkpoint = second.Checkpoint
	if got := checkpoint.View(); len(got.Epochs) != 3 || got.Epochs[0].Ordinal != 0 || got.Epochs[1].Ordinal != 1 || got.Epochs[2].Ordinal != 2 {
		t.Fatalf("epoch chain is not 0->1->2: %+v", got.Epochs)
	}
	lateReplay, err := Apply(checkpoint, planOne)
	if err != nil || lateReplay.Disposition != DispositionReplayed {
		t.Fatalf("historical exact replay: disposition=%q err=%v", lateReplay.Disposition, err)
	}

	terminalOld, err := FinishClaimed(checkpoint, FinishClaim{
		BaseBinding: checkpoint.Binding(), Identity: rescue.Identity, Result: FinishCompleted, EvidenceDigest: testDigest("wrong-old"),
	})
	if !errors.Is(err, ErrTerminalIdentity) || terminalOld.Checkpoint != nil {
		t.Fatalf("handed-off old identity was not terminal: result=%+v err=%v", terminalOld, err)
	}
	finish := FinishClaim{
		BaseBinding: checkpoint.Binding(), Identity: claimed.Identity, Result: FinishCompleted, EvidenceDigest: testDigest("claimed-result"),
	}
	finished, err := FinishClaimed(checkpoint, finish)
	if err != nil || finished.Disposition != DispositionApplied {
		t.Fatalf("finish claimed: disposition=%q err=%v", finished.Disposition, err)
	}
	checkpoint = finished.Checkpoint
	settled := findAuthority(t, checkpoint.View().Authorities, func(authority AuthorityRecord) bool { return authority.Identity == claimed.Identity })
	if settled.State != AuthorityCompleted || settled.EpochID != original.CurrentEpoch || !canonicalDigest(settled.TerminalRecordID) {
		t.Fatalf("finish did not preserve owner epoch: %+v", settled)
	}
	finishReplay, err := FinishClaimed(checkpoint, finish)
	if err != nil || finishReplay.Disposition != DispositionReplayed {
		t.Fatalf("finish acknowledgement replay: disposition=%q err=%v", finishReplay.Disposition, err)
	}

	encoded, err := EncodeCheckpointV8(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCheckpointV8(encoded)
	if err != nil || decoded.Binding() != checkpoint.Binding() || !reflect.DeepEqual(decoded.View(), checkpoint.View()) {
		t.Fatalf("checkpoint round trip: err=%v", err)
	}
}

func TestConcurrentPlansHaveOneCASWinnerAndStaleLoser(t *testing.T) {
	checkpoint := testCheckpoint(t, "base", []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	frontier := checkpoint.View().Authorities[0]
	planA := testPlan(t, checkpoint, "candidate-a", frontier.Identity, "next-a", "next-ra")
	planB := testPlan(t, checkpoint, "candidate-b", frontier.Identity, "next-b", "next-rb")
	if planA.ProposalDigest() == planB.ProposalDigest() {
		t.Fatal("competing proposals have the same digest")
	}
	winner, err := Apply(checkpoint, planA)
	if err != nil || winner.Disposition != DispositionApplied {
		t.Fatalf("winner: disposition=%q err=%v", winner.Disposition, err)
	}
	loser, err := Apply(winner.Checkpoint, planB)
	if err != nil || loser.Disposition != DispositionStale || loser.Checkpoint != winner.Checkpoint {
		t.Fatalf("loser: disposition=%q err=%v", loser.Disposition, err)
	}
	reverseWinner, err := Apply(checkpoint, planB)
	if err != nil || reverseWinner.Disposition != DispositionApplied {
		t.Fatalf("reverse winner: disposition=%q err=%v", reverseWinner.Disposition, err)
	}
	reverseLoser, err := Apply(reverseWinner.Checkpoint, planA)
	if err != nil || reverseLoser.Disposition != DispositionStale {
		t.Fatalf("reverse loser: disposition=%q err=%v", reverseLoser.Disposition, err)
	}
}

func TestStalePreviewClaimedBlockerAndFinishDependentFailure(t *testing.T) {
	checkpoint := testCheckpoint(t, "claimed", []AuthoritySeed{
		{LocalID: "claimed", ReservationID: "claimed-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityClaimed},
		{LocalID: "command", ReservationID: "command-r", NodeID: "work", Kind: AuthorityCommand, State: AuthorityActive, DependencyLocalIDs: []string{"claimed"}},
	})
	claimed := findAuthority(t, checkpoint.View().Authorities, func(authority AuthorityRecord) bool { return authority.LocalID == "claimed" })
	directives := retainAll(checkpoint.View().Authorities)
	for i := range directives {
		if directives[i].Source == claimed.Identity {
			directives[i] = HandoffDirective{
				Source: claimed.Identity, Action: HandoffTransfer, TargetLocalID: "next", TargetReservationID: "next-r", TargetNodeID: "work",
			}
		}
	}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "claimed-next"), Handoffs: directives,
	})
	if err != nil || !hasBlocker(preview.Blockers, BlockerClaimed) || !hasBlocker(preview.Blockers, BlockerActiveCommand) {
		t.Fatalf("claimed blocker closure: blockers=%+v err=%v", preview.Blockers, err)
	}
	staleBinding := checkpoint.Binding()
	staleBinding.Revision++
	stale, err := PreviewApply(checkpoint, ApplyDraft{BaseBinding: staleBinding, Candidate: supportedCandidate(t, "stale")})
	if err != nil || len(stale.Blockers) != 1 || stale.Blockers[0].Code != BlockerStaleBinding {
		t.Fatalf("stale preview: %+v err=%v", stale, err)
	}
	if _, err := FinishClaimed(checkpoint, FinishClaim{
		BaseBinding: checkpoint.Binding(), Identity: claimed.Identity, Result: FinishCompleted, EvidenceDigest: testDigest("evidence"),
	}); !errors.Is(err, ErrProtectedAuthority) {
		t.Fatalf("finish bypassed active dependent command: %v", err)
	}
}

func TestDirectiveOrderAndDiffCanonicalizationAreStable(t *testing.T) {
	checkpoint := testCheckpoint(t, "canonical", []AuthoritySeed{
		{LocalID: "a", ReservationID: "a-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed},
		{LocalID: "b", ReservationID: "b-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityClaimed},
	})
	view := checkpoint.View()
	a := findAuthority(t, view.Authorities, func(authority AuthorityRecord) bool { return authority.LocalID == "a" })
	directives := retainAll(view.Authorities)
	for i := range directives {
		if directives[i].Source == a.Identity {
			directives[i] = HandoffDirective{Source: a.Identity, Action: HandoffTransfer, TargetLocalID: "next", TargetReservationID: "next-r", TargetNodeID: "work"}
		}
	}
	draft := ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "canonical-next"),
		ReasonDigest: testDigest("canonical-reason"), Handoffs: directives,
	}
	forward, err := PreviewApply(checkpoint, draft)
	if err != nil || forward.Plan == nil {
		t.Fatalf("forward preview: %+v err=%v", forward, err)
	}
	slices.Reverse(draft.Handoffs)
	reverse, err := PreviewApply(checkpoint, draft)
	if err != nil || reverse.Plan == nil {
		t.Fatalf("reverse preview: %+v err=%v", reverse, err)
	}
	forwardBytes, _ := EncodeApplyPlan(forward.Plan)
	reverseBytes, _ := EncodeApplyPlan(reverse.Plan)
	if !slices.Equal(forwardBytes, reverseBytes) || forward.Plan.ProposalDigest() != reverse.Plan.ProposalDigest() {
		t.Fatal("directive order changed canonical plan")
	}
	if !canonicalDiff(forward.Diff) || forward.Diff.Digest == "" || len(forward.Diff.ChangedNodes) != 1 || forward.Diff.ChangedNodes[0] != "work" {
		t.Fatalf("semantic diff was not complete/canonical: %+v", forward.Diff)
	}
}

func TestPreviewReturnsEveryActiveAuthorityBlocker(t *testing.T) {
	kinds := []struct {
		kind AuthorityKind
		code BlockerCode
	}{
		{AuthorityCommand, BlockerActiveCommand},
		{AuthorityWait, BlockerActiveWait},
		{AuthorityTimer, BlockerActiveTimer},
		{AuthorityObligation, BlockerActiveObligation},
		{AuthorityContact, BlockerActiveContact},
		{AuthorityDispatchedSideEffect, BlockerDispatchedSideEffect},
		{AuthorityOutcome, BlockerActiveOutcome},
		{AuthorityParallel, BlockerActiveParallel},
		{AuthorityJoin, BlockerActiveJoin},
		{AuthorityPropagation, BlockerActivePropagation},
		{AuthorityDetachment, BlockerActiveDetachment},
		{AuthorityRetry, BlockerActiveRetry},
		{AuthorityRollbackForward, BlockerActiveRollbackForward},
	}
	seeds := []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}}
	for i, item := range kinds {
		seeds = append(seeds, AuthoritySeed{
			LocalID: fmt.Sprintf("active-%02d", i), ReservationID: fmt.Sprintf("active-r-%02d", i), NodeID: "work",
			Kind: item.kind, State: AuthorityActive, DependencyLocalIDs: []string{"frontier"},
		})
	}
	checkpoint := testCheckpoint(t, "blockers", seeds)
	frontier := findAuthority(t, checkpoint.View().Authorities, func(authority AuthorityRecord) bool { return authority.LocalID == "frontier" })
	directives := retainAll(checkpoint.View().Authorities)
	for i := range directives {
		if directives[i].Source == frontier.Identity {
			directives[i] = HandoffDirective{
				Source: frontier.Identity, Action: HandoffTransfer, TargetLocalID: "next", TargetReservationID: "next-r", TargetNodeID: "work",
			}
		}
	}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "changed"), ReasonDigest: testDigest("reason"), Handoffs: directives,
	})
	if err != nil || preview.Plan != nil {
		t.Fatalf("blocked preview: plan=%v err=%v", preview.Plan, err)
	}
	wantCodes := make([]BlockerCode, 0, len(kinds))
	for _, item := range kinds {
		wantCodes = append(wantCodes, item.code)
	}
	slices.Sort(wantCodes)
	gotCodes := make([]BlockerCode, 0, len(preview.Blockers))
	for _, blocker := range preview.Blockers {
		gotCodes = append(gotCodes, blocker.Code)
	}
	slices.Sort(gotCodes)
	if !reflect.DeepEqual(gotCodes, wantCodes) {
		t.Fatalf("blocker matrix mismatch\n got: %v\nwant: %v", gotCodes, wantCodes)
	}
}

func TestPreviewRequiresCompleteUniqueHandoffSetAndTerminalCannotTransfer(t *testing.T) {
	checkpoint := testCheckpoint(t, "complete", []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	frontier := checkpoint.View().Authorities[0]
	missing, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "missing"), Handoffs: nil,
	})
	if err != nil || len(missing.Blockers) != 1 || missing.Blockers[0].Code != BlockerHandoffMissing {
		t.Fatalf("missing handoff classification: %+v err=%v", missing, err)
	}
	duplicateDirective := HandoffDirective{Source: frontier.Identity, Action: HandoffRetain}
	duplicate, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "duplicate"), Handoffs: []HandoffDirective{duplicateDirective, duplicateDirective},
	})
	if err != nil || !hasBlocker(duplicate.Blockers, BlockerHandoffDuplicate) {
		t.Fatalf("duplicate handoff classification: %+v err=%v", duplicate, err)
	}
	unknown, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "unknown"), Handoffs: []HandoffDirective{
			duplicateDirective, {Source: OwnerIdentity(strings.Repeat("f", 64)), Action: HandoffRetain},
		},
	})
	if err != nil || !hasBlocker(unknown.Blockers, BlockerHandoffUnknown) {
		t.Fatalf("unknown handoff classification: %+v err=%v", unknown, err)
	}

	plan := testPlan(t, checkpoint, "applied", frontier.Identity, "next", "next-r")
	applied, err := Apply(checkpoint, plan)
	if err != nil {
		t.Fatal(err)
	}
	terminalDirectives := retainAll(applied.Checkpoint.View().Authorities)
	for i := range terminalDirectives {
		if terminalDirectives[i].Source == frontier.Identity {
			terminalDirectives[i] = HandoffDirective{
				Source: frontier.Identity, Action: HandoffTransfer, TargetLocalID: "resurrect", TargetReservationID: "resurrect-r", TargetNodeID: "work",
			}
		}
	}
	terminal, err := PreviewApply(applied.Checkpoint, ApplyDraft{
		BaseBinding: applied.Binding, Candidate: supportedCandidate(t, "terminal"), Handoffs: terminalDirectives,
	})
	if err != nil || !hasBlocker(terminal.Blockers, BlockerNotTransferable) {
		t.Fatalf("terminal transfer classification: %+v err=%v", terminal, err)
	}
}

func TestTransferConservationRejectsSuccessorReservationReuse(t *testing.T) {
	checkpoint := testCheckpoint(t, "reuse", []AuthoritySeed{
		{LocalID: "a", ReservationID: "a-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed},
		{LocalID: "b", ReservationID: "b-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed},
	})
	directives := make([]HandoffDirective, 0, 2)
	for i, authority := range checkpoint.View().ProtectedAuthorities {
		directives = append(directives, HandoffDirective{
			Source: authority.Identity, Action: HandoffTransfer, TargetLocalID: fmt.Sprintf("next-%d", i),
			TargetReservationID: "reused-r", TargetNodeID: "work",
		})
	}
	if _, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "reuse-next"), Handoffs: directives,
	}); err == nil || !strings.Contains(err.Error(), "reservation is reused") {
		t.Fatalf("successor reservation reuse was not rejected: %v", err)
	}
}

func TestPreviewRejectsDuplicateProposedLogicalFrontier(t *testing.T) {
	checkpoint := testCheckpoint(t, "duplicate-target-frontier", []AuthoritySeed{
		{LocalID: "a", ReservationID: "a-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed},
		{LocalID: "b", ReservationID: "b-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed},
	})
	directives := make([]HandoffDirective, 0, 2)
	for i, authority := range checkpoint.wire.Authorities {
		directives = append(directives, HandoffDirective{
			Source: authority.Identity, Action: HandoffTransfer,
			TargetLocalID: "shared", TargetReservationID: fmt.Sprintf("shared-r-%d", i), TargetNodeID: "work",
		})
	}
	draft := ApplyDraft{BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "duplicate-target-next"), Handoffs: directives}
	forward, err := PreviewApply(checkpoint, draft)
	if err != nil || forward.Plan != nil || len(forward.Blockers) != 1 || forward.Blockers[0].Code != BlockerHandoffDuplicate {
		t.Fatalf("duplicate proposed logical frontier: preview=%+v err=%v", forward, err)
	}
	slices.Reverse(draft.Handoffs)
	reverse, err := PreviewApply(checkpoint, draft)
	if err != nil || reverse.Plan != nil || !reflect.DeepEqual(reverse.Blockers, forward.Blockers) {
		t.Fatalf("duplicate target blocker is unstable: forward=%+v reverse=%+v err=%v", forward.Blockers, reverse.Blockers, err)
	}

	validDirectives := slices.Clone(directives)
	for i := range validDirectives {
		validDirectives[i].TargetLocalID = fmt.Sprintf("shared-%d", i)
	}
	valid, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "duplicate-target-defense"), Handoffs: validDirectives,
	})
	if err != nil || valid.Plan == nil {
		t.Fatalf("build defense plan: preview=%+v err=%v", valid, err)
	}
	forged := cloneApplyCore(valid.Plan.core)
	firstTarget := forged.HandoffSet[0].Target
	second := &forged.HandoffSet[1]
	second.Target.LocalID = firstTarget.LocalID
	second.Target.NodeID = firstTarget.NodeID
	second.Target.Identity, _ = authorityIdentity(forged.RunID, *second.Target)
	basis, _ := applyHandoffBasis(forged)
	second.ID, _ = handoffIdentity(second.Source, second.Action, second.Target, basis)
	forged.HandoffSetDigest, _ = handoffSetDigest(forged.HandoffSet)
	forged.ProposalDigest, _ = proposalDigest(forged)
	if err := validateApplyCoreStatic(forged.RunID, forged); err == nil || !strings.Contains(err.Error(), "duplicate a logical frontier") {
		t.Fatalf("static validation accepted duplicate proposed logical frontier: %v", err)
	}
	dependencies, _ := newAuthorityDependencyIndex(checkpoint.wire.Authorities)
	if _, err := applyHandoffSet(forged.RunID, checkpoint.wire.Authorities, forged.HandoffSet, dependencies); err == nil ||
		!strings.Contains(err.Error(), "duplicate a logical frontier") {
		t.Fatalf("apply defense accepted duplicate proposed logical frontier: %v", err)
	}
}

func TestDuplicateDirectivesDoNotSuppressDependentBlockers(t *testing.T) {
	checkpoint := testCheckpoint(t, "duplicate-budget", []AuthoritySeed{
		{LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed},
		{LocalID: "command", ReservationID: "command-r", NodeID: "work", Kind: AuthorityCommand, State: AuthorityActive, DependencyLocalIDs: []string{"frontier"}},
	})
	frontier := findAuthority(t, checkpoint.wire.Authorities, func(authority AuthorityRecord) bool {
		return authority.Kind == AuthorityFrontier
	})
	command := findAuthority(t, checkpoint.wire.Authorities, func(authority AuthorityRecord) bool {
		return authority.Kind == AuthorityCommand
	})
	transfer := HandoffDirective{
		Source: frontier.Identity, Action: HandoffTransfer,
		TargetLocalID: "next", TargetReservationID: "next-r", TargetNodeID: "work",
	}
	directives := make([]HandoffDirective, MaxBlockers+2)
	for i := 0; i <= MaxBlockers; i++ {
		directives[i] = transfer
	}
	directives[len(directives)-1] = HandoffDirective{Source: command.Identity, Action: HandoffRetain}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "duplicate-budget-next"), Handoffs: directives,
	})
	if err != nil || preview.Plan != nil || len(preview.Blockers) != 2 ||
		!hasBlocker(preview.Blockers, BlockerHandoffDuplicate) || !hasBlocker(preview.Blockers, BlockerActiveCommand) {
		t.Fatalf("duplicate directives suppressed unique blockers: preview=%+v err=%v", preview, err)
	}
}

func TestSourceBlockersDoNotConsumeDuplicateDependentBudget(t *testing.T) {
	seeds := make([]AuthoritySeed, 0, MaxBlockers+1)
	seeds = append(seeds, AuthoritySeed{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work",
		Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	})
	for i := 0; i < MaxBlockers-1; i++ {
		seeds = append(seeds, AuthoritySeed{
			LocalID: fmt.Sprintf("command-%04d", i), ReservationID: fmt.Sprintf("command-r-%04d", i), NodeID: "work",
			Kind: AuthorityCommand, State: AuthorityActive, DependencyLocalIDs: []string{"frontier"},
		})
	}
	seeds = append(seeds, AuthoritySeed{
		LocalID: "wait", ReservationID: "wait-r", NodeID: "work",
		Kind: AuthorityWait, State: AuthorityActive, DependencyLocalIDs: []string{"frontier"},
	})
	checkpoint := testCheckpoint(t, "overlapping-blocker-budget", seeds)
	directives := make([]HandoffDirective, 0, len(seeds))
	for _, authority := range checkpoint.wire.Authorities {
		if authority.Kind == AuthorityWait {
			directives = append(directives, HandoffDirective{Source: authority.Identity, Action: HandoffRetain})
			continue
		}
		directives = append(directives, HandoffDirective{
			Source: authority.Identity, Action: HandoffTransfer,
			TargetLocalID:       "next-" + authority.LocalID,
			TargetReservationID: "next-" + authority.ReservationID,
			TargetNodeID:        "work",
		})
	}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "overlapping-blocker-budget-next"), Handoffs: directives,
	})
	if err != nil || preview.Plan != nil || len(preview.Blockers) != MaxBlockers || !hasBlocker(preview.Blockers, BlockerActiveWait) {
		t.Fatalf("cross-phase duplicates suppressed distinct blocker: blockers=%d activeWait=%t err=%v",
			len(preview.Blockers), hasBlocker(preview.Blockers, BlockerActiveWait), err)
	}
	activeCommands := 0
	for _, blocker := range preview.Blockers {
		if blocker.Code == BlockerActiveCommand {
			activeCommands++
		}
	}
	if activeCommands != MaxBlockers-1 {
		t.Fatalf("active command blockers=%d, want %d", activeCommands, MaxBlockers-1)
	}
}

func TestTransferRejectsHistoricalMaterializationReuse(t *testing.T) {
	checkpoint := testCheckpoint(t, "reuse-epoch-zero", []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	frontier := checkpoint.View().Authorities[0]
	legalRebind, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "reuse-legal-rebind"),
		Handoffs: []HandoffDirective{{
			Source: frontier.Identity, Action: HandoffTransfer,
			TargetLocalID: frontier.LocalID, TargetReservationID: "frontier-r-fresh", TargetNodeID: frontier.NodeID,
		}},
	})
	if err != nil || legalRebind.Plan == nil || len(legalRebind.Blockers) != 0 {
		t.Fatalf("atomic logical-frontier rebind with fresh reservation: preview=%+v err=%v", legalRebind, err)
	}
	legalApplied, err := Apply(checkpoint, legalRebind.Plan)
	if err != nil || legalApplied.Disposition != DispositionApplied {
		t.Fatalf("apply atomic logical-frontier rebind: result=%+v err=%v", legalApplied, err)
	}

	directReuse, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "reuse-direct"),
		Handoffs: []HandoffDirective{{
			Source: frontier.Identity, Action: HandoffTransfer,
			TargetLocalID: frontier.LocalID, TargetReservationID: frontier.ReservationID, TargetNodeID: frontier.NodeID,
		}},
	})
	if err == nil || directReuse.Plan != nil || !strings.Contains(err.Error(), "historical reservation") {
		t.Fatalf("epoch-zero materialization reuse was accepted: preview=%+v err=%v", directReuse, err)
	}

	first, err := Apply(checkpoint, testPlan(t, checkpoint, "reuse-one", frontier.Identity, "frontier-1", "frontier-r-1"))
	if err != nil {
		t.Fatal(err)
	}
	frontierOne := findAuthority(t, first.Checkpoint.View().Authorities, func(authority AuthorityRecord) bool {
		return authority.State == AuthorityVerifiedUnclaimed
	})
	second, err := Apply(first.Checkpoint, testPlan(t, first.Checkpoint, "reuse-two", frontierOne.Identity, "frontier-2", "frontier-r-2"))
	if err != nil {
		t.Fatal(err)
	}
	frontierTwo := findAuthority(t, second.Checkpoint.View().Authorities, func(authority AuthorityRecord) bool {
		return authority.State == AuthorityVerifiedUnclaimed
	})
	afterInterveningReservation, err := PreviewApply(second.Checkpoint, ApplyDraft{
		BaseBinding: second.Binding, Candidate: supportedCandidate(t, "reuse-three-reservation"),
		Handoffs: []HandoffDirective{{
			Source: frontierTwo.Identity, Action: HandoffTransfer,
			TargetLocalID: "frontier-3", TargetReservationID: frontier.ReservationID, TargetNodeID: "work",
		}},
	})
	if err == nil || afterInterveningReservation.Plan != nil || !strings.Contains(err.Error(), "historical reservation") {
		t.Fatalf("intervening-epoch reservation reuse was accepted: preview=%+v err=%v", afterInterveningReservation, err)
	}
	afterIntervening, err := PreviewApply(second.Checkpoint, ApplyDraft{
		BaseBinding: second.Binding, Candidate: supportedCandidate(t, "reuse-three"),
		Handoffs: []HandoffDirective{{
			Source: frontierTwo.Identity, Action: HandoffTransfer,
			TargetLocalID: frontier.LocalID, TargetReservationID: "frontier-r-3", TargetNodeID: frontier.NodeID,
		}},
	})
	if err == nil || afterIntervening.Plan != nil || !strings.Contains(err.Error(), "historical logical frontier") {
		t.Fatalf("intervening-epoch logical-frontier reentry was accepted: preview=%+v err=%v", afterIntervening, err)
	}
}

func TestVerifierRejectsRehashedHistoricalMaterializationReuse(t *testing.T) {
	checkpoint := testCheckpoint(t, "reuse-replay-zero", []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	source := checkpoint.View().Authorities[0]
	applied, err := Apply(checkpoint, testPlan(t, checkpoint, "reuse-replay-one", source.Identity, "next", "next-r"))
	if err != nil {
		t.Fatal(err)
	}

	forged := cloneWire(applied.Checkpoint.wire)
	record := forged.History[0].Apply
	handoff := &record.HandoffSet[0]
	target := cloneAuthority(*handoff.Target)
	target.LocalID = source.LocalID
	target.ReservationID = source.ReservationID
	target.NodeID = source.NodeID
	target.Identity, _ = authorityIdentity(forged.Anchor.RunID, target)
	handoff.Target = &target
	basis, _ := applyHandoffBasis(record.applyCore)
	handoff.ID, _ = handoffIdentity(handoff.Source, handoff.Action, handoff.Target, basis)
	record.HandoffSetDigest, _ = handoffSetDigest(record.HandoffSet)
	record.ProposalDigest, _ = proposalDigest(record.applyCore)
	record.RecordDigest, _ = applyRecordDigest(*record)
	forged.History[0].Digest, _ = historyEventDigest(forged.History[0])
	source.State = AuthorityHandedOff
	source.Successor = target.Identity
	source.TerminalRecordID = handoff.ID
	forged.Authorities = []AuthorityRecord{source, target}
	sortAuthorities(forged.Authorities)
	forged.Digest, _ = checkpointDigest(forged)
	if err := VerifyCheckpointV8(&CheckpointV8{wire: forged}); err == nil || !strings.Contains(err.Error(), "historical reservation") {
		t.Fatalf("rehashed historical materialization reuse was accepted: %v", err)
	}
}

func TestFrozenEligibilityClassifier(t *testing.T) {
	supported, err := ClassifyTemplateSource(testTemplateSource("supported"))
	if err != nil || supported.MatrixVersion != EligibilityMatrixVersion || supported.Status != EligibilitySupported || supported.Reason != EligibilityReasonSupported || supported.Candidate() == nil {
		t.Fatalf("supported classification: %+v err=%v", supported, err)
	}
	programSource := strings.Replace(string(testTemplateSource("program")), "kind: agent\n      prompt: program", "kind: program\n      run: true", 1)
	program, err := ClassifyTemplateSource([]byte(programSource))
	if err != nil || program.Status != EligibilityUnsupported || program.Reason != EligibilityReasonProgram || program.Candidate() != nil {
		t.Fatalf("program classification: %+v err=%v", program, err)
	}
	endOnly := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: end-only
start: done
nodes:
  done:
    type: end
    result: completed
`)
	end, err := ClassifyTemplateSource(endOnly)
	if err != nil || end.Status != EligibilityUnsupported || end.Reason != EligibilityReasonEnd {
		t.Fatalf("end-only classification: %+v err=%v", end, err)
	}
	unknown := append(testTemplateSource("unknown"), []byte("unknownField: nope\n")...)
	invalid, err := ClassifyTemplateSource(unknown)
	if err != nil || invalid.Status != EligibilityUnsupported || invalid.Reason != EligibilityReasonInvalidTemplate {
		t.Fatalf("unknown-field classification: %+v err=%v", invalid, err)
	}
	overBudget, err := ClassifyTemplateSource(make([]byte, model.MaxProcessTemplateSourceBytes+1))
	if err != nil || overBudget.Status != EligibilityUnsupported || overBudget.Reason != EligibilityReasonInvalidTemplate {
		t.Fatalf("over-budget source classification: %+v err=%v", overBudget, err)
	}

	candidate := supported.Candidate()
	candidate.epoch.RequiredCapabilities = append(candidate.epoch.RequiredCapabilities, Capability("program_v1"))
	if _, err := Initialize("capability-escalation", candidate, nil); err == nil {
		t.Fatal("candidate capability escalation was accepted")
	}
}

func TestFrozenEligibilityReasonUsesSortedNodePrecedence(t *testing.T) {
	template := &model.Template{
		Start: "z-wait",
		Nodes: map[string]model.Node{
			"a-program": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerProgram,
				},
			},
			"z-wait": {
				Type: model.NodeTypeWait,
			},
		},
	}
	for range 100 {
		if got := classifyProductionPathV1(template); got != EligibilityReasonProgram {
			t.Fatalf("classification reason = %q, want sorted-node reason %q", got, EligibilityReasonProgram)
		}
	}
}

func TestStrictCanonicalCheckpointAndPlanInputs(t *testing.T) {
	checkpoint := testCheckpoint(t, "json", []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	checkpointJSON, err := EncodeCheckpointV8(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	unknown := append([]byte(`{"unknown":1,`), checkpointJSON[1:]...)
	if _, err := DecodeCheckpointV8(unknown); err == nil {
		t.Fatal("unknown checkpoint field was accepted")
	}
	duplicate := append([]byte(`{"stateSchemaVersion":8,`), checkpointJSON[1:]...)
	if _, err := DecodeCheckpointV8(duplicate); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("duplicate checkpoint field: %v", err)
	}
	whitespace := append([]byte(" "), checkpointJSON...)
	if _, err := DecodeCheckpointV8(whitespace); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("noncanonical checkpoint whitespace: %v", err)
	}
	if _, err := DecodeCheckpointV8(append(checkpointJSON, []byte("{}")...)); err == nil {
		t.Fatal("trailing checkpoint value was accepted")
	}
	if _, err := DecodeCheckpointV8(make([]byte, MaxCheckpointBytes+1)); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("over-budget checkpoint: %v", err)
	}
	tampered := slices.Clone(checkpointJSON)
	index := strings.Index(string(tampered), checkpoint.wire.Anchor.OriginalEpoch.TemplateSourceDigest)
	if index < 0 {
		t.Fatal("source digest missing from checkpoint")
	}
	tampered[index] = differentHex(tampered[index])
	if _, err := DecodeCheckpointV8(tampered); err == nil {
		t.Fatal("tampered checkpoint digest was accepted")
	}

	frontier := checkpoint.View().Authorities[0]
	plan := testPlan(t, checkpoint, "plan-json", frontier.Identity, "next", "next-r")
	planJSON, err := EncodeApplyPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	decodedPlan, err := DecodeApplyPlan(planJSON)
	if err != nil || decodedPlan.ProposalDigest() != plan.ProposalDigest() {
		t.Fatalf("plan round trip: err=%v", err)
	}
	planUnknown := append([]byte(`{"unknown":1,`), planJSON[1:]...)
	if _, err := DecodeApplyPlan(planUnknown); err == nil {
		t.Fatal("unknown plan field was accepted")
	}
	planTampered := slices.Clone(planJSON)
	index = strings.Index(string(planTampered), plan.ProposalDigest())
	planTampered[index] = differentHex(planTampered[index])
	if _, err := DecodeApplyPlan(planTampered); err == nil {
		t.Fatal("tampered proposal digest was accepted")
	}
	if _, err := DecodeApplyPlan(make([]byte, MaxApplyPlanBytes+1)); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("over-budget plan: %v", err)
	}
}

func TestSensitiveSourceReasonEvidenceAndAuditProvenanceNeverPersist(t *testing.T) {
	const (
		secretSource   = "source-secret-should-not-persist"
		secretReason   = "reason-secret-should-not-persist"
		secretEvidence = "evidence-secret-should-not-persist"
	)
	checkpoint := testCheckpoint(t, secretSource, []AuthoritySeed{{
		LocalID: "claimed", ReservationID: "claimed-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityClaimed,
	}})
	checkpointBytes, err := EncodeCheckpointV8(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(checkpointBytes), secretSource) {
		t.Fatal("template source bytes persisted in checkpoint")
	}
	planPreview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "next-source-secret"),
		ReasonDigest: testDigest(secretReason), Handoffs: retainAll(checkpoint.View().ProtectedAuthorities),
	})
	if err != nil || planPreview.Plan == nil {
		t.Fatalf("preview: %+v err=%v", planPreview, err)
	}
	planBytes, err := EncodeApplyPlan(planPreview.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planBytes), secretReason) || strings.Contains(string(planBytes), "next-source-secret") {
		t.Fatal("source or reason bytes persisted in apply plan")
	}
	applied, err := Apply(checkpoint, planPreview.Plan)
	if err != nil {
		t.Fatal(err)
	}
	claimed := applied.Checkpoint.View().Authorities[0]
	finished, err := FinishClaimed(applied.Checkpoint, FinishClaim{
		BaseBinding: applied.Binding, Identity: claimed.Identity, Result: FinishCompleted, EvidenceDigest: testDigest(secretEvidence),
	})
	if err != nil {
		t.Fatal(err)
	}
	finishedBytes, err := EncodeCheckpointV8(finished.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{secretSource, secretReason, secretEvidence, "actor", "timestamp"} {
		if strings.Contains(string(finishedBytes), secret) {
			t.Fatalf("untrusted provenance or sensitive bytes %q persisted", secret)
		}
	}
}

func TestGraphAndAuthorityCycleAndReentryRejection(t *testing.T) {
	candidate := supportedCandidate(t, "cycle")
	graph := cloneGraph(candidate.epoch.Graph)
	graph.Edges = append(graph.Edges, GraphEdge{From: "work", Outcome: "again", To: "start"})
	slices.SortFunc(graph.Edges, compareGraphEdge)
	graph.Digest, _ = graphDigest(graph)
	if err := validateGraph(graph, candidate.epoch.RequiredCapabilities); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("local cycle was not rejected: %v", err)
	}

	checkpoint := testCheckpoint(t, "composed", []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	frontier := checkpoint.View().Authorities[0]
	applied, err := Apply(checkpoint, testPlan(t, checkpoint, "composed-next", frontier.Identity, "next", "next-r"))
	if err != nil {
		t.Fatal(err)
	}
	authorities := cloneAuthorities(applied.Checkpoint.View().Authorities)
	old := findAuthority(t, authorities, func(authority AuthorityRecord) bool { return authority.State == AuthorityHandedOff })
	for i := range authorities {
		if authorities[i].Identity == old.Successor {
			authorities[i].EpochID = old.EpochID
		}
	}
	if err := validateComposedGraph(applied.Checkpoint.View().Epochs, authorities); err == nil || !strings.Contains(err.Error(), "re-enters") {
		t.Fatalf("backward epoch re-entry was not rejected: %v", err)
	}

	cycleAuthorities := cloneAuthorities(checkpoint.View().Authorities)
	copy := cycleAuthorities[0]
	copy.LocalID = "second"
	copy.ReservationID = "second-r"
	copy.Identity, _ = authorityIdentity(checkpoint.View().RunID, copy)
	cycleAuthorities = append(cycleAuthorities, copy)
	cycleAuthorities[0].DependsOn = []OwnerIdentity{copy.Identity}
	cycleAuthorities[1].DependsOn = []OwnerIdentity{cycleAuthorities[0].Identity}
	sortAuthorities(cycleAuthorities)
	if err := validateAuthorities(checkpoint.View().RunID, checkpoint.View().Epochs, cycleAuthorities, false); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("authority cycle was not rejected: %v", err)
	}
}

func TestVerifierRejectsRehashedHistoryAndSummaryTampering(t *testing.T) {
	checkpoint := testCheckpoint(t, "tamper-0", []AuthoritySeed{{
		LocalID: "frontier", ReservationID: "frontier-r", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	frontier := checkpoint.View().Authorities[0]
	first, err := Apply(checkpoint, testPlan(t, checkpoint, "tamper-1", frontier.Identity, "next-1", "next-r1"))
	if err != nil {
		t.Fatal(err)
	}
	frontier = findAuthority(t, first.Checkpoint.View().Authorities, func(authority AuthorityRecord) bool {
		return authority.EpochID == first.Checkpoint.View().CurrentEpoch && authority.State == AuthorityVerifiedUnclaimed
	})
	second, err := Apply(first.Checkpoint, testPlan(t, first.Checkpoint, "tamper-2", frontier.Identity, "next-2", "next-r2"))
	if err != nil {
		t.Fatal(err)
	}

	reordered := cloneWire(second.Checkpoint.wire)
	reordered.History[0], reordered.History[1] = reordered.History[1], reordered.History[0]
	reordered.Digest, _ = checkpointDigest(reordered)
	if err := VerifyCheckpointV8(&CheckpointV8{wire: reordered}); err == nil {
		t.Fatal("rehashed reordered history was accepted")
	}

	forgedHead := cloneWire(second.Checkpoint.wire)
	forgedHead.CurrentEpochID = forgedHead.Epochs[0].ID
	forgedHead.Digest, _ = checkpointDigest(forgedHead)
	if err := VerifyCheckpointV8(&CheckpointV8{wire: forgedHead}); err == nil {
		t.Fatal("rehashed forged current epoch was accepted")
	}

	resurrected := cloneWire(second.Checkpoint.wire)
	for i := range resurrected.Authorities {
		if resurrected.Authorities[i].State == AuthorityHandedOff {
			resurrected.Authorities[i].State = AuthorityVerifiedUnclaimed
			resurrected.Authorities[i].Successor = ""
			resurrected.Authorities[i].TerminalRecordID = ""
			break
		}
	}
	resurrected.Digest, _ = checkpointDigest(resurrected)
	if err := VerifyCheckpointV8(&CheckpointV8{wire: resurrected}); err == nil {
		t.Fatal("rehashed authority resurrection was accepted")
	}
}

func TestRepeatedEpochTransferProperty(t *testing.T) {
	checkpoint := testCheckpoint(t, "property-0", []AuthoritySeed{{
		LocalID: "frontier-0", ReservationID: "frontier-r-0", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed,
	}})
	frontier := checkpoint.View().Authorities[0]
	for i := 1; i <= 24; i++ {
		plan := testPlan(t, checkpoint, fmt.Sprintf("property-%d", i), frontier.Identity,
			fmt.Sprintf("frontier-%d", i), fmt.Sprintf("frontier-r-%d", i))
		result, err := Apply(checkpoint, plan)
		if err != nil || result.Disposition != DispositionApplied {
			t.Fatalf("epoch %d: disposition=%q err=%v", i, result.Disposition, err)
		}
		checkpoint = result.Checkpoint
		frontier = findAuthority(t, checkpoint.View().Authorities, func(authority AuthorityRecord) bool {
			return authority.EpochID == checkpoint.View().CurrentEpoch && authority.State == AuthorityVerifiedUnclaimed
		})
		if err := VerifyCheckpointV8(checkpoint); err != nil {
			t.Fatalf("epoch %d verification: %v", i, err)
		}
	}
	view := checkpoint.View()
	if len(view.Epochs) != 25 || len(view.History) != 24 || len(view.Authorities) != 25 {
		t.Fatalf("property chain cardinality: epochs=%d history=%d authorities=%d", len(view.Epochs), len(view.History), len(view.Authorities))
	}
	for _, authority := range view.Authorities {
		if authority.Identity == frontier.Identity {
			continue
		}
		if authority.State != AuthorityHandedOff {
			t.Fatalf("old authority resurrected: %+v", authority)
		}
	}
}

func TestPublicOutputsEnforceCanonicalWireBudgets(t *testing.T) {
	candidate := supportedCandidate(t, "wire-budget")

	t.Run("initialize", func(t *testing.T) {
		if _, err := Initialize("wire-initialize", candidate, authoritySeeds(30_000, false)); !errors.Is(err, ErrOverBudget) {
			t.Fatalf("oversized initialization checkpoint: %v", err)
		}
	})

	t.Run("preview", func(t *testing.T) {
		checkpoint, err := Initialize("wire-preview", candidate, authoritySeeds(25_000, false))
		if err != nil {
			t.Fatal(err)
		}
		directives := make([]HandoffDirective, 0, len(checkpoint.wire.Authorities))
		for i, authority := range checkpoint.wire.Authorities {
			directives = append(directives, HandoffDirective{
				Source: authority.Identity, Action: HandoffTransfer,
				TargetLocalID: fmt.Sprintf("next-%05d", i), TargetReservationID: fmt.Sprintf("next-r-%05d", i), TargetNodeID: "work",
			})
		}
		preview, err := PreviewApply(checkpoint, ApplyDraft{
			BaseBinding: checkpoint.Binding(), Candidate: candidate, Handoffs: directives,
		})
		if !errors.Is(err, ErrOverBudget) || preview.Plan != nil {
			t.Fatalf("oversized apply plan: preview=%+v err=%v", preview, err)
		}
	})

	t.Run("apply", func(t *testing.T) {
		checkpoint, err := Initialize("wire-apply", candidate, authoritySeeds(20_000, false))
		if err != nil {
			t.Fatal(err)
		}
		preview, err := PreviewApply(checkpoint, ApplyDraft{
			BaseBinding: checkpoint.Binding(), Candidate: candidate,
			Handoffs: retainAll(checkpoint.wire.Authorities),
		})
		if err != nil || preview.Plan == nil {
			t.Fatalf("bounded apply plan: preview=%+v err=%v", preview, err)
		}
		if _, err := EncodeApplyPlan(preview.Plan); err != nil {
			t.Fatalf("preview returned an unencodable plan: %v", err)
		}
		result, err := Apply(checkpoint, preview.Plan)
		if !errors.Is(err, ErrOverBudget) || result.Checkpoint != nil {
			t.Fatalf("oversized applied checkpoint: result=%+v err=%v", result, err)
		}
	})

	t.Run("finish_exact_boundary", func(t *testing.T) {
		// These fixed cardinalities deliberately pin the canonical encoding
		// boundary. Any wire-shape change must make the budget decision explicit.
		boundary, err := Initialize("r"+strings.Repeat("x", 152), candidate, authoritySeeds(29_848, true))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := EncodeCheckpointV8(boundary)
		if err != nil || len(encoded) != MaxCheckpointBytes {
			t.Fatalf("checkpoint boundary bytes=%d err=%v, want %d", len(encoded), err, MaxCheckpointBytes)
		}
		claimed := findAuthority(t, boundary.wire.Authorities, func(authority AuthorityRecord) bool {
			return authority.LocalID == "a-00000"
		})
		result, err := FinishClaimed(boundary, FinishClaim{
			BaseBinding: boundary.Binding(), Identity: claimed.Identity,
			Result: FinishCompleted, EvidenceDigest: testDigest("wire-finish"),
		})
		if !errors.Is(err, ErrOverBudget) || result.Checkpoint != nil {
			t.Fatalf("finish crossed checkpoint wire boundary: result=%+v err=%v", result, err)
		}
	})
}

func TestDependencyIndexHighCardinalityWorkIsNearLinear(t *testing.T) {
	const authoritiesCount = 5_000
	checkpoint, err := Initialize("dependency-work", supportedCandidate(t, "dependency-work"), authoritySeeds(authoritiesCount, false))
	if err != nil {
		t.Fatal(err)
	}
	authorities := checkpoint.wire.Authorities
	index, err := newAuthorityDependencyIndex(authorities)
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[OwnerIdentity]struct{}, len(authorities))
	for _, authority := range authorities {
		if blocker, blocked := transferSourceBlocker(authority); blocked {
			t.Fatalf("independent frontier unexpectedly blocked: %+v", blocker)
		}
		sources[authority.Identity] = struct{}{}
	}
	blockers := newBlockerAccumulator(MaxBlockers)
	index.addActiveDependentBlockers(sources, blockers)
	if got := blockers.canonical(); len(got) != 0 {
		t.Fatalf("independent frontiers have dependent blockers: %+v", got)
	}
	if index.buildAuthorityVisits != authoritiesCount || index.buildDependencyVisits != 0 || index.descendantVisits != 0 {
		t.Fatalf("dependency work is not linear for independent authorities: authorities=%d dependencies=%d descendants=%d",
			index.buildAuthorityVisits, index.buildDependencyVisits, index.descendantVisits)
	}

	directives := make([]HandoffDirective, 0, len(authorities))
	for i, authority := range authorities {
		directives = append(directives, HandoffDirective{
			Source: authority.Identity, Action: HandoffTransfer,
			TargetLocalID: fmt.Sprintf("next-%04d", i), TargetReservationID: fmt.Sprintf("next-r-%04d", i), TargetNodeID: "work",
		})
	}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, "dependency-work-next"), Handoffs: directives,
	})
	if err != nil || preview.Plan == nil || len(preview.Blockers) != 0 {
		t.Fatalf("high-cardinality preview: blockers=%+v err=%v", preview.Blockers, err)
	}
}

func testCheckpoint(t *testing.T, prompt string, seeds []AuthoritySeed) *CheckpointV8 {
	t.Helper()
	checkpoint, err := Initialize("run", supportedCandidate(t, prompt), seeds)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func supportedCandidate(t *testing.T, prompt string) *TemplateCandidate {
	t.Helper()
	classification, err := ClassifyTemplateSource(testTemplateSource(prompt))
	if err != nil || classification.Status != EligibilitySupported {
		t.Fatalf("candidate %q: classification=%+v err=%v", prompt, classification, err)
	}
	return classification.Candidate()
}

func testTemplateSource(prompt string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: protocol-test
start: start
nodes:
  start:
    type: start
    next: work
  work:
    type: task
    performer:
      kind: agent
      prompt: %s
    next:
      pass: done
  done:
    type: end
    result: completed
`, prompt))
}

func testPlan(t *testing.T, checkpoint *CheckpointV8, prompt string, transfer OwnerIdentity, targetLocalID, targetReservationID string) *ApplyPlan {
	t.Helper()
	directives := retainAll(checkpoint.View().ProtectedAuthorities)
	found := false
	for i := range directives {
		if directives[i].Source != transfer {
			continue
		}
		directives[i] = HandoffDirective{
			Source: transfer, Action: HandoffTransfer, TargetLocalID: targetLocalID,
			TargetReservationID: targetReservationID, TargetNodeID: "work",
		}
		found = true
	}
	if !found {
		t.Fatalf("transfer source %q not found", transfer)
	}
	preview, err := PreviewApply(checkpoint, ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: supportedCandidate(t, prompt),
		ReasonDigest: testDigest("reason-" + prompt), Handoffs: directives,
	})
	if err != nil || preview.Plan == nil || len(preview.Blockers) != 0 {
		t.Fatalf("preview %q: blockers=%+v err=%v", prompt, preview.Blockers, err)
	}
	return preview.Plan
}

func retainAll(authorities []AuthorityRecord) []HandoffDirective {
	result := make([]HandoffDirective, 0, len(authorities))
	for _, authority := range authorities {
		result = append(result, HandoffDirective{Source: authority.Identity, Action: HandoffRetain})
	}
	return result
}

func findAuthority(t *testing.T, authorities []AuthorityRecord, predicate func(AuthorityRecord) bool) AuthorityRecord {
	t.Helper()
	for _, authority := range authorities {
		if predicate(authority) {
			return authority
		}
	}
	t.Fatal("authority not found")
	return AuthorityRecord{}
}

func hasBlocker(blockers []Blocker, code BlockerCode) bool {
	for _, blocker := range blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}

func testDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func differentHex(value byte) byte {
	if value == '0' {
		return '1'
	}
	return '0'
}

func authoritySeeds(count int, claimedFirst bool) []AuthoritySeed {
	seeds := make([]AuthoritySeed, count)
	for i := range seeds {
		state := AuthorityVerifiedUnclaimed
		if claimedFirst && i == 0 {
			state = AuthorityClaimed
		}
		seeds[i] = AuthoritySeed{
			LocalID: fmt.Sprintf("a-%05d", i), ReservationID: fmt.Sprintf("r-%05d", i),
			NodeID: "work", Kind: AuthorityFrontier, State: state,
		}
	}
	return seeds
}
