//go:build linux || darwin

package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// TCL-608 capstone crash semantics (A1). The window between an adapter side
// effect and its durable observation is irreducible: the engine never claims
// exactly-once external effects across it. What these tests pin instead:
//
//   - A claim that crashed BEFORE any external dispatch is redispatched safely
//     after restart, exactly once, through the idempotent adapter contract
//     (ReconcileDeferred discovers nothing external -> Dispatch).
//   - A crash AFTER a durable observed/settled transition never re-invokes the
//     adapter and never duplicates durable authority or history.
//   - Any ambiguous delivery window stays governed by the adapter contract:
//     a recovered claim is only ever resolved via Reconcile/ReconcileDeferred
//     discovery, and a claimed command that is not externally discoverable
//     fails closed ("refusing to perform it again") rather than re-Performing.
//
// The load-bearing invariant is no duplicate durable authority/history and
// correct restart rediscovery over an expired crash lease.

// capstoneRedispatchAdapter is a deferred adapter whose external system holds
// nothing until Dispatch is called; afterwards reconciliation discovers a
// passed observation. Perform must never run for a recovered claim.
type capstoneRedispatchAdapter struct {
	mu         sync.Mutex
	performs   int
	dispatches int
	reconciles int
}

func (a *capstoneRedispatchAdapter) Validate(processexec.Request) error { return nil }

func (a *capstoneRedispatchAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.performs++
	return processexec.Observation{}, errors.New("capstone deferred adapter must never perform synchronously")
}

func (a *capstoneRedispatchAdapter) Dispatch(context.Context, processexec.Request) (processexec.DispatchResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dispatches++
	return processexec.DispatchResult{ExternalRef: "capstone:redispatched"}, nil
}

func (a *capstoneRedispatchAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconciles++
	if a.dispatches == 0 {
		return processexec.Observation{}, processexec.DeferredMissing, nil
	}
	return processexec.Observation{Actor: "agent:agt_capstone", Verdict: "pass", EvidenceRef: "artifact:capstone"}, processexec.DeferredObserved, nil
}

type capstoneCountingAdapter struct {
	mu    sync.Mutex
	calls int
}

func (a *capstoneCountingAdapter) Validate(processexec.Request) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return nil
}

func (a *capstoneCountingAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return processexec.Observation{Actor: "agent:agt_capstone", Verdict: "pass"}, nil
}

type capstonePromptAdapter struct {
	mu      sync.Mutex
	prompts []string
}

func (a *capstonePromptAdapter) Validate(processexec.Request) error { return nil }

func (a *capstonePromptAdapter) Perform(_ context.Context, request processexec.Request) (processexec.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prompts = append(a.prompts, request.Performer.Prompt)
	return processexec.Observation{Actor: "agent:agt_capstone", Verdict: "pass"}, nil
}

func TestEpochV8CapstoneCrashBeforeDispatchRedispatchesOnceOnRestart(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	frozen := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	t.Cleanup(fs.SetNowForTest(func() time.Time { return frozen }))
	record, source := putEpochV8Template(t, fs, "epoch-capstone-crash-claim", "capstone work")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-capstone-crash-claim", TemplateRef: record.Ref}, source)
	require.NoError(t, err)
	lease, err := fs.AcquireEngineLease(t.Context(), "epoch-capstone-crash-claim", "crashed-engine", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
	require.NoError(t, err)
	claim := capstonePlanGenesisClaim(t, attached.Artifact.Checkpoint, source)
	claimed, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
	require.NoError(t, err)
	require.Equal(t, epochv8.DispositionApplied, claimed.Disposition)
	// Crash boundary: the engine dies here with a durable claim, no external
	// dispatch, and an unreleased lease. Restart happens after the TTL expired.

	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	t.Cleanup(restarted.SetNowForTest(func() time.Time { return frozen.Add(2 * time.Minute) }))
	adapter := &capstoneRedispatchAdapter{}
	host := engine.New(restarted, "restarted-engine", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})

	first, err := host.Tick(t.Context())
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Empty(t, first[0].Error)
	assert.False(t, first[0].LeaseContended, "expired crash lease must not block restart rediscovery")
	assert.Equal(t, state.RunStatusRunning, first[0].Status)
	assert.Equal(t, 1, adapter.dispatches, "recovered undispatched claim redispatches exactly once")
	assert.Equal(t, 0, adapter.performs, "recovered claim must never be blind-performed")

	second, err := host.Tick(t.Context())
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Empty(t, second[0].Error)
	assert.Equal(t, state.RunStatusCompleted, second[0].Status)
	assert.Equal(t, 1, adapter.dispatches)
	assert.Equal(t, 0, adapter.performs)
	assert.GreaterOrEqual(t, adapter.reconciles, 2, "each recovery round rediscovers through the adapter contract")

	snapshot, err := restarted.LoadEpochV8RunView(t.Context(), "epoch-capstone-crash-claim")
	require.NoError(t, err)
	view := snapshot.Checkpoint.View()
	assert.Equal(t, 1, capstoneCountReceipts(view, epochv8.RuntimeClaimExternal), "claim authority is not duplicated across the crash")
	assert.Equal(t, 1, capstoneCountReceipts(view, epochv8.RuntimeFinishClaimed), "finish authority is recorded exactly once")
	workFrontiers := 0
	for _, authority := range view.Authorities {
		if authority.Kind == epochv8.AuthorityFrontier && authority.NodeID == "work" {
			workFrontiers++
			assert.Equal(t, epochv8.AuthorityCompleted, authority.State)
		}
	}
	assert.Equal(t, 1, workFrontiers, "exactly one terminal authority for the crashed node attempt")
}

func TestEpochV8CapstoneCrashAfterSettledTransitionDoesNotReexecute(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	frozen := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)
	t.Cleanup(fs.SetNowForTest(func() time.Time { return frozen }))
	record, source := putEpochV8Template(t, fs, "epoch-capstone-crash-settled", "settled work")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-capstone-crash-settled", TemplateRef: record.Ref}, source)
	require.NoError(t, err)
	lease, err := fs.AcquireEngineLease(t.Context(), "epoch-capstone-crash-settled", "crashed-engine", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
	require.NoError(t, err)
	claim := capstonePlanGenesisClaim(t, attached.Artifact.Checkpoint, source)
	claimed, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
	require.NoError(t, err)
	observation := capstoneObserveRecoveredAttempt(t, claimed.Artifact.Checkpoint, source, "pass")
	finished, err := fs.AppendEpochV8FinishClaimed(t.Context(), lease, observation, observation.PostBinding().Digest)
	require.NoError(t, err)
	input, err := pathv1.VerifyExecutionInput(t.Context(), finished.Artifact.Checkpoint, source)
	require.NoError(t, err)
	route, err := pathv1.AdvanceExclusiveRoute(t.Context(), input)
	require.NoError(t, err)
	_, err = fs.AppendEpochV8Advance(t.Context(), lease, route)
	require.NoError(t, err)
	// Crash boundary: the node's observation and routing are durably settled;
	// the engine dies before finishing the run and never releases its lease.

	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	t.Cleanup(restarted.SetNowForTest(func() time.Time { return frozen.Add(2 * time.Minute) }))
	adapter := &capstoneCountingAdapter{}
	host := engine.New(restarted, "restarted-engine", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	results, err := host.Tick(t.Context())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Empty(t, results[0].Error)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	assert.Equal(t, 0, adapter.calls, "durably settled work is never re-executed after restart")

	snapshot, err := restarted.LoadEpochV8RunView(t.Context(), "epoch-capstone-crash-settled")
	require.NoError(t, err)
	view := snapshot.Checkpoint.View()
	assert.Equal(t, 1, capstoneCountReceipts(view, epochv8.RuntimeClaimExternal))
	assert.Equal(t, 1, capstoneCountReceipts(view, epochv8.RuntimeFinishClaimed))
	work := findEpochOwner(t, snapshot.Checkpoint, store.EpochV8InitialFrontierLocalID)
	assert.Equal(t, epochv8.AuthorityCompleted, work.State)
}

// A2: forward execution after an approved sole-bare-frontier transfer. Only
// the atomic handed_off successor executes; no backward re-entry exists.
func TestEpochV8CapstoneTransferredEpochExecutesForward(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	record0, _ := putEpochV8Template(t, fs, "epoch-capstone-forward", "old prompt")
	run, err := engine.Instantiate(t.Context(), fs, engine.InstantiateRequest{TemplateRef: record0.Ref, RunID: "epoch-capstone-forward-run"})
	require.NoError(t, err)
	engineLease, err := fs.AcquireEngineLease(t.Context(), run.ID, "engine", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), engineLease)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engineLease))
	initial := findEpochOwner(t, attached.Checkpoint, store.EpochV8InitialFrontierLocalID)
	require.Equal(t, epochv8.AuthorityVerifiedUnclaimed, initial.State, "frontier must stay unclaimed before the transfer")

	_, source1 := putEpochV8Template(t, fs, "epoch-capstone-forward", "new prompt")
	plan := previewEpochV8Apply(t, attached.Checkpoint, source1, "")
	maintenance, err := fs.AcquireMaintenanceLease(t.Context(), run.ID, "maintainer", time.Minute)
	require.NoError(t, err)
	_, err = fs.PublishEpochV8TransferAuthorized(t.Context(), maintenance, plan, source1, nil, epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("d", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "human:operator", AppliedAt: "2026-07-21T09:40:00Z",
	})
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance))

	adapter := &capstonePromptAdapter{}
	host := engine.New(fs, "capstone-engine", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	results, err := host.Tick(t.Context())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Empty(t, results[0].Error)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	assert.Equal(t, []string{"new prompt"}, adapter.prompts, "only the transferred epoch's node executes, with its own prompt")

	snapshot, err := fs.LoadEpochV8RunView(t.Context(), run.ID)
	require.NoError(t, err)
	view := snapshot.Checkpoint.View()
	assert.Len(t, view.Epochs, 2)
	require.NotNil(t, snapshot.Runtime)
	assert.Equal(t, plan.CandidateEpoch().ID, snapshot.Runtime.EpochID, "runtime head runs under the candidate epoch")
	oldFrontier := findEpochOwner(t, snapshot.Checkpoint, store.EpochV8InitialFrontierLocalID)
	assert.Equal(t, epochv8.AuthorityHandedOff, oldFrontier.State)
	successor := findEpochOwner(t, snapshot.Checkpoint, "next-frontier")
	assert.Equal(t, oldFrontier.Successor, successor.Identity)
	assert.Equal(t, 1, capstoneDependentCount(view, oldFrontier.Identity), "handed-off frontier has exactly one successor")
	assert.Equal(t, plan.CandidateEpoch().ID, successor.EpochID)
	assert.Equal(t, epochv8.AuthorityCompleted, successor.State)
}

// A2 lineage: epoch 0 -> 1 -> 2 attached via two sole-bare-frontier transfers
// of still-unclaimed frontiers, then forward execution completes under the
// final epoch. Each consumed frontier keeps its single atomic successor.
func TestEpochV8CapstoneSequentialTransfersKeepAtomicHandoffLineage(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	record0, source0 := putEpochV8Template(t, fs, "epoch-capstone-lineage", "lineage zero")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-capstone-lineage", TemplateRef: record0.Ref}, source0)
	require.NoError(t, err)
	engineLease, err := fs.AcquireEngineLease(t.Context(), "epoch-capstone-lineage", "engine", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), engineLease)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engineLease))

	maintenance, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-capstone-lineage", "maintainer", time.Minute)
	require.NoError(t, err)
	_, source1 := putEpochV8Template(t, fs, "epoch-capstone-lineage", "lineage one")
	plan1 := previewEpochV8Apply(t, attached.Checkpoint, source1, "")
	transferred1, err := fs.PublishEpochV8TransferAuthorized(t.Context(), maintenance, plan1, source1, nil, epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("e", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "human:operator", AppliedAt: "2026-07-21T09:50:00Z",
	})
	require.NoError(t, err)
	_, source2 := putEpochV8Template(t, fs, "epoch-capstone-lineage", "lineage two")
	plan2 := previewEpochV8Apply(t, transferred1.Checkpoint, source2, "")
	_, err = fs.PublishEpochV8TransferAuthorized(t.Context(), maintenance, plan2, source2, nil, epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("f", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "human:operator", AppliedAt: "2026-07-21T09:51:00Z",
	})
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance))

	adapter := &capstonePromptAdapter{}
	host := engine.New(fs, "capstone-engine", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	results, err := host.Tick(t.Context())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Empty(t, results[0].Error)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	assert.Equal(t, []string{"lineage two"}, adapter.prompts)

	snapshot, err := fs.LoadEpochV8RunView(t.Context(), "epoch-capstone-lineage")
	require.NoError(t, err)
	view := snapshot.Checkpoint.View()
	assert.Len(t, view.Epochs, 3)
	require.NotNil(t, snapshot.Runtime)
	assert.Equal(t, plan2.CandidateEpoch().ID, snapshot.Runtime.EpochID)
	first := findEpochOwner(t, snapshot.Checkpoint, store.EpochV8InitialFrontierLocalID)
	second := findEpochOwner(t, snapshot.Checkpoint, "next-frontier")
	third := findEpochOwner(t, snapshot.Checkpoint, "next-frontier-2")
	assert.Equal(t, epochv8.AuthorityHandedOff, first.State)
	assert.Equal(t, second.Identity, first.Successor)
	assert.Equal(t, 1, capstoneDependentCount(view, first.Identity))
	assert.Equal(t, epochv8.AuthorityHandedOff, second.State)
	assert.Equal(t, third.Identity, second.Successor)
	assert.Equal(t, 1, capstoneDependentCount(view, second.Identity))
	assert.Equal(t, plan1.CandidateEpoch().ID, second.EpochID)
	assert.Equal(t, plan2.CandidateEpoch().ID, third.EpochID)
	assert.Equal(t, epochv8.AuthorityCompleted, third.State)
}

// A3 (runtime-attached lattice): claimed/dispatched work under epoch N is not
// refused by a later apply; it stays executable/settleable only under its
// immutable owner epoch N, and the consumed unclaimed frontier accepts no
// command/observation/second handoff after its atomic transfer.
//
// With a runtime attached, a mixed apply that transfers one frontier while
// other work is claimed cannot even seal a plan: the claimed authority is a
// stable BlockerClaimed and ApplyTransferHead additionally requires a bare
// active closure. The mixed contract itself is pinned at the unattached
// checkpoint lattice in TestEpochV8CapstoneMixedRunPinsImmutableOwnerEpoch.
func TestEpochV8CapstoneOwnerEpochConservationAcrossRetainThenTransfer(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	record0, source0 := putEpochV8HandoffTemplate(t, fs, "epoch-capstone-owner", "old owner work")
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-capstone-owner", TemplateRef: record0.Ref}, source0)
	require.NoError(t, err)
	epochZero := initialized.Checkpoint.View().OriginalEpoch
	lease1, err := fs.AcquireEngineLease(t.Context(), "epoch-capstone-owner", "engine", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease1)
	require.NoError(t, err)
	claim := capstonePlanGenesisClaim(t, attached.Artifact.Checkpoint, source0)
	claimed, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease1, claim)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease1))
	claimedFrontier := findEpochOwner(t, claimed.Checkpoint, store.EpochV8InitialFrontierLocalID)
	require.Equal(t, epochv8.AuthorityClaimed, claimedFrontier.State)

	_, source1 := putEpochV8Template(t, fs, "epoch-capstone-owner", "retained owner")
	classification1, err := epochv8.ClassifyTemplateSource(source1)
	require.NoError(t, err)

	// A transfer directive naming the claimed authority is a stable blocker;
	// no plan seals, so epoch N+1 can never consume claimed epoch-N work.
	transferClaimed := make([]epochv8.HandoffDirective, 0, len(claimed.Checkpoint.View().ProtectedAuthorities))
	for _, authority := range claimed.Checkpoint.View().ProtectedAuthorities {
		directive := epochv8.HandoffDirective{Source: authority.Identity, Action: epochv8.HandoffRetain}
		if authority.Identity == claimedFrontier.Identity {
			directive = epochv8.HandoffDirective{
				Source: authority.Identity, Action: epochv8.HandoffTransfer,
				TargetLocalID: "stolen-frontier", TargetReservationID: "stolen-reservation", TargetNodeID: "work",
			}
		}
		transferClaimed = append(transferClaimed, directive)
	}
	refused, err := epochv8.PreviewApply(claimed.Checkpoint, epochv8.ApplyDraft{
		BaseBinding: claimed.Checkpoint.Binding(), Candidate: classification1.Candidate(), Handoffs: transferClaimed,
	})
	require.NoError(t, err)
	assert.Nil(t, refused.Plan)
	assert.Contains(t, refused.Blockers, epochv8.Blocker{Code: epochv8.BlockerClaimed, AuthorityID: claimedFrontier.Identity})

	// Retain-apply epoch 1: the claimed epoch-0 command is retained, not
	// refused and not relabeled.
	retain := make([]epochv8.HandoffDirective, 0, len(claimed.Checkpoint.View().ProtectedAuthorities))
	for _, authority := range claimed.Checkpoint.View().ProtectedAuthorities {
		retain = append(retain, epochv8.HandoffDirective{Source: authority.Identity, Action: epochv8.HandoffRetain})
	}
	preview1, err := epochv8.PreviewApply(claimed.Checkpoint, epochv8.ApplyDraft{
		BaseBinding: claimed.Checkpoint.Binding(), Candidate: classification1.Candidate(), Handoffs: retain,
	})
	require.NoError(t, err)
	require.Empty(t, preview1.Blockers)
	maintenance1, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-capstone-owner", "maintainer", time.Minute)
	require.NoError(t, err)
	retained, err := fs.PublishEpochV8RetainAuthorized(t.Context(), maintenance1, preview1.Plan, source1, nil, epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("b", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "human:operator", AppliedAt: "2026-07-21T10:00:00Z",
	})
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance1))
	assert.Equal(t, claimed.Artifact.Digest, retained.Artifact.Digest, "retain apply leaves the owner runtime untouched")
	retainedFrontier := findEpochOwner(t, retained.Checkpoint, store.EpochV8InitialFrontierLocalID)
	assert.Equal(t, epochv8.AuthorityClaimed, retainedFrontier.State)
	assert.Equal(t, epochZero, retainedFrontier.EpochID, "claimed work keeps its immutable owner epoch")
	assert.NotEqual(t, epochZero, retained.Checkpoint.View().CurrentEpoch)

	// (a) The old claimed command finishes under owner epoch N; the receipt
	// records epoch N, never the current epoch N+1.
	lease2, err := fs.AcquireEngineLease(t.Context(), "epoch-capstone-owner", "engine", time.Minute)
	require.NoError(t, err)
	observation := capstoneObserveRecoveredAttempt(t, retained.Artifact.Checkpoint, source0, "pass")
	finished, err := fs.AppendEpochV8FinishClaimed(t.Context(), lease2, observation, digestText("old owner result"))
	require.NoError(t, err)
	finishReceipt := capstoneLastRuntimeReceipt(t, finished.Checkpoint.View())
	assert.Equal(t, epochv8.RuntimeFinishClaimed, finishReceipt.Kind)
	assert.Equal(t, claimedFrontier.Identity, finishReceipt.Owner)
	assert.Equal(t, epochZero, finishReceipt.EpochID, "finish receipt records the immutable owner epoch")
	assert.NotEqual(t, finished.Checkpoint.View().CurrentEpoch, finishReceipt.EpochID)

	input, err := pathv1.VerifyExecutionInput(t.Context(), finished.Artifact.Checkpoint, source0)
	require.NoError(t, err)
	route, err := pathv1.AdvanceExclusiveRoute(t.Context(), input)
	require.NoError(t, err)
	routed, err := fs.AppendEpochV8Advance(t.Context(), lease2, route)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease2))
	frontiers := capstoneVerifiedFrontiers(routed.Checkpoint.View())
	require.Len(t, frontiers, 1)
	consumed := frontiers[0]
	assert.Equal(t, epochZero, consumed.EpochID, "forward execution continues under the owner epoch")

	// Build a claim against the pre-transfer runtime head but do not append it
	// yet: it becomes the late claim/observation aimed at the consumed frontier.
	staleInput, err := pathv1.VerifyExecutionInput(t.Context(), routed.Artifact.Checkpoint, source0)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePathV1(t, routed.Artifact.Checkpoint))
	require.NoError(t, err)
	livePathID := ""
	for _, path := range aggregate.Routing.Paths {
		if path.Kind == pathv1.PathActivationOutput && path.State == pathv1.PathLive {
			livePathID = path.ID
		}
	}
	require.NotEmpty(t, livePathID)
	stalePlan, err := pathv1.PlanExclusiveAttempt(t.Context(), staleInput, livePathID, 1, nil)
	require.NoError(t, err)
	staleClaim, err := pathv1.ClaimExclusiveAttempt(t.Context(), staleInput, stalePlan)
	require.NoError(t, err)

	// Transfer the separate bare verified-unclaimed frontier to epoch 2.
	_, source2 := putEpochV8Template(t, fs, "epoch-capstone-owner", "transferred owner")
	plan2 := previewEpochV8Apply(t, routed.Checkpoint, source2, "")
	maintenance2, err := fs.AcquireMaintenanceLease(t.Context(), "epoch-capstone-owner", "maintainer", time.Minute)
	require.NoError(t, err)
	transferred, err := fs.PublishEpochV8TransferAuthorized(t.Context(), maintenance2, plan2, source2, nil, epochv8.ApplyAuthorization{
		HandoffDirectiveDigest: strings.Repeat("c", 64), ReasonCode: epochv8.ApplyReasonUnlock,
		Actor: "human:operator", AppliedAt: "2026-07-21T10:10:00Z",
	})
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseMaintenanceLease(t.Context(), maintenance2))
	after := transferred.Checkpoint.View()
	consumedAfter := capstoneAuthorityByIdentity(t, after, consumed.Identity)
	assert.Equal(t, epochv8.AuthorityHandedOff, consumedAfter.State)
	require.NotEmpty(t, consumedAfter.Successor)
	assert.Equal(t, 1, capstoneDependentCount(after, consumed.Identity), "consumed frontier has exactly one atomic successor")
	successor := capstoneAuthorityByIdentity(t, after, consumedAfter.Successor)
	assert.Equal(t, plan2.CandidateEpoch().ID, successor.EpochID)
	assert.Equal(t, epochv8.AuthorityVerifiedUnclaimed, successor.State)

	// (c) The consumed frontier accepts no late claim/observation: the claim
	// built against the pre-transfer head is rejected and mutates nothing.
	lease3, err := fs.AcquireEngineLease(t.Context(), "epoch-capstone-owner", "engine", time.Minute)
	require.NoError(t, err)
	_, err = fs.AppendEpochV8ClaimExternal(t.Context(), lease3, staleClaim)
	assert.Error(t, err, "late claim addressed to the consumed frontier must be rejected")
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease3))
	reloaded, err := fs.LoadEpochV8RunView(t.Context(), "epoch-capstone-owner")
	require.NoError(t, err)
	assert.Equal(t, transferred.Checkpoint.Binding(), reloaded.Checkpoint.Binding(), "rejected late claim publishes nothing")

	// ...and no second handoff: a transfer directive naming the handed-off
	// frontier is a stable blocker.
	secondDirectives := make([]epochv8.HandoffDirective, 0, len(after.ProtectedAuthorities))
	for _, authority := range after.ProtectedAuthorities {
		directive := epochv8.HandoffDirective{Source: authority.Identity, Action: epochv8.HandoffRetain}
		if authority.Identity == consumed.Identity {
			directive = epochv8.HandoffDirective{
				Source: authority.Identity, Action: epochv8.HandoffTransfer,
				TargetLocalID: "again-frontier", TargetReservationID: "again-reservation", TargetNodeID: "work",
			}
		}
		secondDirectives = append(secondDirectives, directive)
	}
	secondPreview, err := epochv8.PreviewApply(transferred.Checkpoint, epochv8.ApplyDraft{
		BaseBinding: transferred.Checkpoint.Binding(), Candidate: classification1.Candidate(), Handoffs: secondDirectives,
	})
	require.NoError(t, err)
	assert.Nil(t, secondPreview.Plan)
	assert.Contains(t, secondPreview.Blockers, epochv8.Blocker{Code: epochv8.BlockerNotTransferable, AuthorityID: consumed.Identity})

	assert.Equal(t, 1, capstoneCountReceipts(after, epochv8.RuntimeFinishClaimed), "the old claimed command settled exactly once")
}

// A3 (mixed run, unattached checkpoint lattice): a run holding claimed work
// under epoch N plus a separate verified-unclaimed frontier. The transfer of
// the unclaimed frontier to N+1 does not refuse, relabel, or consume the
// claimed epoch-N command, and the consumed frontier is dead to further
// commands, observations, and handoffs.
func TestEpochV8CapstoneMixedRunPinsImmutableOwnerEpoch(t *testing.T) {
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	_, source0 := putEpochV8Template(t, fs, "epoch-capstone-mixed", "mixed zero")
	_, source1 := putEpochV8Template(t, fs, "epoch-capstone-mixed", "mixed one")
	classification0, err := epochv8.ClassifyTemplateSource(source0)
	require.NoError(t, err)
	classification1, err := epochv8.ClassifyTemplateSource(source1)
	require.NoError(t, err)
	checkpoint, err := epochv8.Initialize("epoch-capstone-mixed-run", classification0.Candidate(), []epochv8.AuthoritySeed{
		{LocalID: "claimed-work", ReservationID: "claimed-reservation", NodeID: "work", Kind: epochv8.AuthorityFrontier, State: epochv8.AuthorityClaimed},
		{LocalID: "open-frontier", ReservationID: "open-reservation", NodeID: "work", Kind: epochv8.AuthorityFrontier, State: epochv8.AuthorityVerifiedUnclaimed},
	})
	require.NoError(t, err)
	epochZero := checkpoint.View().OriginalEpoch
	claimedBefore := findEpochOwner(t, checkpoint, "claimed-work")
	openBefore := findEpochOwner(t, checkpoint, "open-frontier")

	preview, err := epochv8.PreviewApply(checkpoint, epochv8.ApplyDraft{
		BaseBinding: checkpoint.Binding(), Candidate: classification1.Candidate(),
		Handoffs: []epochv8.HandoffDirective{
			{Source: claimedBefore.Identity, Action: epochv8.HandoffRetain},
			{Source: openBefore.Identity, Action: epochv8.HandoffTransfer, TargetLocalID: "next-frontier", TargetReservationID: "next-reservation", TargetNodeID: "work"},
		},
	})
	require.NoError(t, err)
	require.Empty(t, preview.Blockers, "claimed work must not refuse a transfer of a separate unclaimed frontier")
	require.NotNil(t, preview.Plan)
	applied, err := epochv8.Apply(checkpoint, preview.Plan)
	require.NoError(t, err)
	require.Equal(t, epochv8.DispositionApplied, applied.Disposition)
	view := applied.Checkpoint.View()
	assert.Len(t, view.Epochs, 2)
	assert.Equal(t, preview.Plan.CandidateEpoch().ID, view.CurrentEpoch)

	claimedAfter := findEpochOwner(t, applied.Checkpoint, "claimed-work")
	assert.Equal(t, epochv8.AuthorityClaimed, claimedAfter.State, "claimed epoch-N work survives the mixed apply untouched")
	assert.Equal(t, epochZero, claimedAfter.EpochID, "claimed work is never relabeled to epoch N+1")
	consumed := findEpochOwner(t, applied.Checkpoint, "open-frontier")
	assert.Equal(t, epochv8.AuthorityHandedOff, consumed.State)
	successor := findEpochOwner(t, applied.Checkpoint, "next-frontier")
	assert.Equal(t, consumed.Successor, successor.Identity)
	assert.Equal(t, preview.Plan.CandidateEpoch().ID, successor.EpochID)
	assert.Equal(t, epochv8.AuthorityVerifiedUnclaimed, successor.State)
	assert.True(t, slices.Contains(successor.DependsOn, consumed.Identity))
	assert.Equal(t, 1, capstoneDependentCount(view, consumed.Identity), "consumed frontier has exactly one successor")

	// (a) The claimed command settles only under its immutable owner epoch N.
	finished, err := epochv8.FinishClaimed(applied.Checkpoint, epochv8.FinishClaim{
		BaseBinding: applied.Binding, Identity: claimedAfter.Identity,
		Result: epochv8.FinishCompleted, EvidenceDigest: digestText("mixed owner evidence"),
	})
	require.NoError(t, err)
	require.Equal(t, epochv8.DispositionApplied, finished.Disposition)
	finishedView := finished.Checkpoint.View()
	var receipt *epochv8.FinishReceipt
	for _, event := range finishedView.History {
		if event.Finish != nil {
			receipt = event.Finish
		}
	}
	require.NotNil(t, receipt)
	assert.Equal(t, claimedAfter.Identity, receipt.Identity)
	assert.Equal(t, epochZero, receipt.OwnerEpochID, "finish receipt records owner epoch N")
	assert.NotEqual(t, finishedView.CurrentEpoch, receipt.OwnerEpochID, "finish never relabels work to the current epoch")

	// (b) Epoch N+1 identities cannot claim, acknowledge, or relabel the old
	// command: finishing the N+1 frontier as if it owned the claimed work is
	// invalid, and the consumed frontier is a terminal identity.
	_, err = epochv8.FinishClaimed(finished.Checkpoint, epochv8.FinishClaim{
		BaseBinding: finished.Binding, Identity: successor.Identity,
		Result: epochv8.FinishCompleted, EvidenceDigest: digestText("stolen acknowledgement"),
	})
	assert.ErrorIs(t, err, epochv8.ErrInvalid)
	_, err = epochv8.FinishClaimed(finished.Checkpoint, epochv8.FinishClaim{
		BaseBinding: finished.Binding, Identity: consumed.Identity,
		Result: epochv8.FinishCompleted, EvidenceDigest: digestText("late consumed observation"),
	})
	assert.ErrorIs(t, err, epochv8.ErrTerminalIdentity)

	// (c) The consumed frontier accepts no second handoff.
	secondPreview, err := epochv8.PreviewApply(finished.Checkpoint, epochv8.ApplyDraft{
		BaseBinding: finished.Binding, Candidate: classification1.Candidate(),
		Handoffs: []epochv8.HandoffDirective{
			{Source: successor.Identity, Action: epochv8.HandoffRetain},
			{Source: consumed.Identity, Action: epochv8.HandoffTransfer, TargetLocalID: "again-frontier", TargetReservationID: "again-reservation", TargetNodeID: "work"},
		},
	})
	require.NoError(t, err)
	assert.Nil(t, secondPreview.Plan)
	assert.Contains(t, secondPreview.Blockers, epochv8.Blocker{Code: epochv8.BlockerNotTransferable, AuthorityID: consumed.Identity})
}

// A4: a restart between the human verdict and the settlement append. The
// settlement token is a pure function of the checkpoint binding, so the token
// minted before the crash stays honored after it, and the settlement applies
// exactly once (Applied, then Replayed).
func TestEpochV8CapstoneSettlementCrashBetweenVerdictAndAppendReplaysExactlyOnce(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, source := putEpochV8Template(t, fs, "epoch-capstone-settlement", "fail then rescue")
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "epoch-capstone-settlement", TemplateRef: record.Ref}, source)
	require.NoError(t, err)
	lease, err := fs.AcquireEngineLease(t.Context(), "epoch-capstone-settlement", "engine", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
	require.NoError(t, err)
	claim := capstonePlanGenesisClaim(t, attached.Artifact.Checkpoint, source)
	claimed, err := fs.AppendEpochV8ClaimExternal(t.Context(), lease, claim)
	require.NoError(t, err)
	observation := capstoneObserveRecoveredAttempt(t, claimed.Artifact.Checkpoint, source, "fail")
	_, err = fs.AppendEpochV8FinishClaimed(t.Context(), lease, observation, digestText("failed attempt"))
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease))

	settlement := pathv1.AuditedSettlementInput{
		Decision: "retry", Actor: "human:operator", Reason: "approved capstone rescue",
		EvidenceRef: "ticket:TCL-608", Timestamp: time.Date(2026, 7, 21, 10, 30, 0, 0, time.UTC),
	}
	var preflight processexec.EpochV8SettlementPreflight
	require.NoError(t, fs.WithEpochV8ExecutionView(t.Context(), "epoch-capstone-settlement", func(view store.EpochV8ExecutionView) error {
		var preflightErr error
		preflight, preflightErr = processexec.PreflightEpochV8AuditedSettlement(t.Context(), view, "", settlement)
		return preflightErr
	}))
	require.Len(t, preflight.Token, 64)

	// The human verdict exists; the daemon crashes before the settlement
	// checkpoint publication becomes durable.
	crash := errors.New("crash before settlement checkpoint publication")
	restore := fs.SetEpochV8PublishHooksForTest(nil, nil, func() error { return crash }, nil)
	_, err = fs.AppendEpochV8Settlement(t.Context(), "epoch-capstone-settlement", preflight.Transition)
	restore()
	assert.ErrorIs(t, err, crash)

	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	var replayed processexec.EpochV8SettlementPreflight
	require.NoError(t, restarted.WithEpochV8ExecutionView(t.Context(), "epoch-capstone-settlement", func(view store.EpochV8ExecutionView) error {
		var preflightErr error
		replayed, preflightErr = processexec.PreflightEpochV8AuditedSettlement(t.Context(), view, preflight.Token, settlement)
		return preflightErr
	}), "the token minted before the restart must still be honored")
	assert.Equal(t, preflight.Token, replayed.Token)

	applied, err := restarted.AppendEpochV8Settlement(t.Context(), "epoch-capstone-settlement", replayed.Transition)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionApplied, applied.Disposition)
	again, err := restarted.AppendEpochV8Settlement(t.Context(), "epoch-capstone-settlement", replayed.Transition)
	require.NoError(t, err)
	assert.Equal(t, epochv8.DispositionReplayed, again.Disposition, "replaying the settlement applies exactly once")

	view := applied.Checkpoint.View()
	frontiers := capstoneVerifiedFrontiers(view)
	require.Len(t, frontiers, 1, "retry settlement creates exactly one new verified frontier")
	receipt := capstoneLastRuntimeReceipt(t, view)
	assert.Equal(t, epochv8.RuntimeSettlement, receipt.Kind)
	assert.Equal(t, frontiers[0].Identity, receipt.Owner)
	assert.Equal(t, "retry", receipt.Decision)
	assert.Equal(t, "human:operator", receipt.Actor)
	assert.Equal(t, "approved capstone rescue", receipt.Reason)
	assert.Equal(t, "ticket:TCL-608", receipt.EvidenceRef)
	assert.Equal(t, "work", receipt.NodeID)
	assert.Equal(t, uint64(1), receipt.BlockedAttempt)
	assert.Len(t, receipt.ResolutionDigest, 64)
	assert.NotEmpty(t, receipt.Timestamp)
}

func capstonePlanGenesisClaim(t *testing.T, checkpointJSON, source []byte) *pathv1.ExecutionTransition {
	t.Helper()
	input, err := pathv1.VerifyExecutionInput(t.Context(), checkpointJSON, source)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(mustDecodePathV1(t, checkpointJSON))
	require.NoError(t, err)
	plan, err := pathv1.PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	require.NoError(t, err)
	claim, err := pathv1.ClaimExclusiveAttempt(t.Context(), input, plan)
	require.NoError(t, err)
	return claim
}

func capstoneObserveRecoveredAttempt(t *testing.T, checkpointJSON, source []byte, outcome string) *pathv1.ExecutionTransition {
	t.Helper()
	input, err := pathv1.VerifyExecutionInput(t.Context(), checkpointJSON, source)
	require.NoError(t, err)
	recovered, found, err := pathv1.RecoverExclusiveAttempt(t.Context(), input)
	require.NoError(t, err)
	require.True(t, found)
	observation, err := pathv1.ObserveExclusiveAttempt(t.Context(), input, recovered, pathv1.ExclusiveObservation{
		Outcome: outcome, Actor: "human:operator",
	}, false)
	require.NoError(t, err)
	return observation
}

func capstoneCountReceipts(view epochv8.CheckpointView, kind epochv8.RuntimeTransitionKind) int {
	count := 0
	for _, event := range view.History {
		if event.Runtime != nil && event.Runtime.Kind == kind {
			count++
		}
	}
	return count
}

func capstoneLastRuntimeReceipt(t *testing.T, view epochv8.CheckpointView) epochv8.RuntimeReceipt {
	t.Helper()
	for i := len(view.History) - 1; i >= 0; i-- {
		if view.History[i].Runtime != nil {
			return *view.History[i].Runtime
		}
	}
	t.Fatal("no runtime receipt found")
	return epochv8.RuntimeReceipt{}
}

func capstoneVerifiedFrontiers(view epochv8.CheckpointView) []epochv8.AuthorityRecord {
	result := make([]epochv8.AuthorityRecord, 0, 1)
	for _, authority := range view.Authorities {
		if authority.Kind == epochv8.AuthorityFrontier && authority.State == epochv8.AuthorityVerifiedUnclaimed {
			result = append(result, authority)
		}
	}
	return result
}

func capstoneDependentCount(view epochv8.CheckpointView, identity epochv8.OwnerIdentity) int {
	count := 0
	for _, authority := range view.Authorities {
		if slices.Contains(authority.DependsOn, identity) {
			count++
		}
	}
	return count
}

func capstoneAuthorityByIdentity(t *testing.T, view epochv8.CheckpointView, identity epochv8.OwnerIdentity) epochv8.AuthorityRecord {
	t.Helper()
	for _, authority := range view.Authorities {
		if authority.Identity == identity {
			return authority
		}
	}
	t.Fatalf("authority %q not found", identity)
	return epochv8.AuthorityRecord{}
}
