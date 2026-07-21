package worklist

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
)

func epochV8TestAuthority(localID string, kind epochv8.AuthorityKind, authorityState epochv8.AuthorityState) epochv8.AuthorityRecord {
	return epochv8.AuthorityRecord{
		EpochID: "owner-epoch", LocalID: localID, ReservationID: "res-1", NodeID: "work",
		Kind: kind, State: authorityState,
	}
}

func epochV8TestRecord(localID string, kind epochv8.AuthorityKind, authorityState epochv8.AuthorityState) epochV8Record {
	return epochV8Record{localID: localID, kind: kind, state: authorityState, reservationID: "res-1", nodeID: "work"}
}

func TestCrossCheckEpochV8ExactAgreementPasses(t *testing.T) {
	records := []epochV8Record{
		epochV8TestRecord("command.c1", epochv8.AuthorityCommand, epochv8.AuthorityActive),
		{
			localID: "effect.w1", kind: epochv8.AuthorityWait, state: epochv8.AuthorityActive,
			reservationID: "res-1", nodeID: "work",
			alsoCovers: []epochV8Expectation{{localID: "command.c2", kind: epochv8.AuthorityCommand, state: epochv8.AuthorityActive, reservationID: "res-1", nodeID: "work"}},
		},
	}
	authorities := []epochv8.AuthorityRecord{
		epochV8TestAuthority("command.c1", epochv8.AuthorityCommand, epochv8.AuthorityActive),
		epochV8TestAuthority("command.c2", epochv8.AuthorityCommand, epochv8.AuthorityActive),
		epochV8TestAuthority("effect.w1", epochv8.AuthorityWait, epochv8.AuthorityActive),
		// Routing authorities are never items and never need coverage.
		epochV8TestAuthority("path.p1", epochv8.AuthorityFrontier, epochv8.AuthorityVerifiedUnclaimed),
		// Terminal work-bearing authorities need no runtime record either.
		epochV8TestAuthority("command.old", epochv8.AuthorityCommand, epochv8.AuthorityCompleted),
	}
	assert.NoError(t, crossCheckEpochV8(records, authorities, "owner-epoch"))
}

func TestCrossCheckEpochV8FailsClosedInBothDirections(t *testing.T) {
	base := epochV8TestAuthority("command.c1", epochv8.AuthorityCommand, epochv8.AuthorityActive)
	record := epochV8TestRecord("command.c1", epochv8.AuthorityCommand, epochv8.AuthorityActive)

	// Zero matches: the record has no authority.
	assert.Error(t, crossCheckEpochV8([]epochV8Record{record}, nil, "owner-epoch"))
	// Multiple matches for one local id are ambiguous.
	assert.Error(t, crossCheckEpochV8([]epochV8Record{record}, []epochv8.AuthorityRecord{base, base}, "owner-epoch"))
	// Field disagreement: state, kind, node, reservation, and epoch each fail.
	for _, tampered := range []func(epochv8.AuthorityRecord) epochv8.AuthorityRecord{
		func(a epochv8.AuthorityRecord) epochv8.AuthorityRecord { a.State = epochv8.AuthorityCompleted; return a },
		func(a epochv8.AuthorityRecord) epochv8.AuthorityRecord { a.Kind = epochv8.AuthorityWait; return a },
		func(a epochv8.AuthorityRecord) epochv8.AuthorityRecord { a.NodeID = "other"; return a },
		func(a epochv8.AuthorityRecord) epochv8.AuthorityRecord { a.ReservationID = "res-2"; return a },
		func(a epochv8.AuthorityRecord) epochv8.AuthorityRecord { a.EpochID = "other-epoch"; return a },
	} {
		assert.Error(t, crossCheckEpochV8([]epochV8Record{record}, []epochv8.AuthorityRecord{tampered(base)}, "owner-epoch"))
	}
	// Duplicate records for one authority are ambiguous.
	assert.Error(t, crossCheckEpochV8([]epochV8Record{record, record}, []epochv8.AuthorityRecord{base}, "owner-epoch"))
	// Reverse direction: an active work-bearing authority with no runtime
	// record fails the whole projection instead of being silently dropped.
	uncovered := epochV8TestAuthority("command.c9", epochv8.AuthorityCommand, epochv8.AuthorityActive)
	assert.Error(t, crossCheckEpochV8([]epochV8Record{record}, []epochv8.AuthorityRecord{base, uncovered}, "owner-epoch"))
	claimed := epochV8TestAuthority("effect.o9", epochv8.AuthorityObligation, epochv8.AuthorityClaimed)
	assert.Error(t, crossCheckEpochV8([]epochV8Record{record}, []epochv8.AuthorityRecord{base, claimed}, "owner-epoch"))
	// An active work-bearing authority on a different epoch is outside the
	// runtime head and is not silently relabeled into this projection.
	foreign := epochV8TestAuthority("command.f1", epochv8.AuthorityCommand, epochv8.AuthorityActive)
	foreign.EpochID = "other-epoch"
	assert.NoError(t, crossCheckEpochV8([]epochV8Record{record}, []epochv8.AuthorityRecord{base, foreign}, "owner-epoch"))
}

func TestAssignEpochV8PresentationIDsRepeatedAttemptSignalContactCollisions(t *testing.T) {
	build := func() []Item {
		return []Item{
			// Same node, same epoch, same attempt, same kind: the directed
			// repeated-attempt/multi-record collision. Two contacts or two
			// signals on one node project exactly like this.
			{Kind: KindWaiting, Node: "hold", Attempt: 1},
			{Kind: KindWaiting, Node: "hold", Attempt: 1},
			{Kind: KindWaiting, Node: "hold", Attempt: 2},
			{Kind: KindAgentObligation, Node: "hold", Attempt: 1},
			{Kind: KindWaiting, Node: "other", Attempt: 1},
		}
	}
	first := build()
	assignEpochV8PresentationIDs("run-1", 3, first)
	ids := make(map[string]struct{}, len(first))
	for _, item := range first {
		require.True(t, strings.HasPrefix(item.ID, "wi8_"), item.ID)
		_, duplicate := ids[item.ID]
		require.False(t, duplicate, "presentation ids must be unique within one response: %s", item.ID)
		ids[item.ID] = struct{}{}
	}
	// Determinism: the same coherent projection yields the same identities.
	second := build()
	assignEpochV8PresentationIDs("run-1", 3, second)
	for index := range first {
		assert.Equal(t, first[index].ID, second[index].ID)
	}
	// A different owner epoch ordinal is a different identity space.
	third := build()
	assignEpochV8PresentationIDs("run-1", 4, third)
	assert.NotEqual(t, first[0].ID, third[0].ID)
}

func TestEpochV8TimelineBoundsAndMultiEventIdentity(t *testing.T) {
	epochs := []epochv8.TemplateEpoch{{ID: "e0", Ordinal: 0}, {ID: "e1", Ordinal: 1}}
	history := make([]epochv8.HistoryEvent, 0, 70)
	for index := range 70 {
		history = append(history, epochv8.HistoryEvent{
			Revision: uint64(index + 1), Kind: epochv8.HistoryRuntime,
			Runtime: &epochv8.RuntimeReceipt{EpochID: "e1"},
		})
	}
	view := epochv8.CheckpointView{Epochs: epochs, History: history}
	events, total, truncated := epochV8Timeline(view)
	assert.Equal(t, 70, total)
	assert.True(t, truncated)
	require.Len(t, events, maxEpochV8TimelineEvents)
	// Newest-first and revision-distinct: multiple events on one node/epoch
	// stay individually addressable through their event sequence.
	assert.Equal(t, uint64(70), events[0].Revision)
	seen := make(map[uint64]struct{}, len(events))
	for _, event := range events {
		_, duplicate := seen[event.Revision]
		require.False(t, duplicate)
		seen[event.Revision] = struct{}{}
		assert.Equal(t, uint64(1), event.EpochOrdinal)
	}
}
