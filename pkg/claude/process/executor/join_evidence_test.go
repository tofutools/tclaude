package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// wideMixedJoinAnyTemplate is the shape that proves join history can never be
// the reason a valid transition is refused. One fork settles its whole branch
// set in a single pass:
//
//   - directBranches edges go straight into the join: any reducer, so that one
//     pass produces one winner and directBranches-1 late arrivals;
//   - decisionBranches edges park a branch on a human each, so the SAME pass
//     also produces one decision_awaited row per branch;
//   - the reducer is a program task, so the pass also plans one command.
//
// Authoring accepts it — the fork reduces at exactly one complete structural
// reducer, and every count is inside the normalized degree and node ceilings —
// so the very first executor.Prepare has to commit all of that evidence at once.
func wideMixedJoinAnyTemplate(directBranches, decisionBranches int) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
		"join": {
			Type: model.NodeTypeTask, Join: model.JoinAny,
			Performer: &model.Performer{Kind: model.PerformerProgram, Run: "true"},
			Next:      model.Next{model.DefaultOutcome: "end"},
		},
		"end": {Type: model.NodeTypeEnd},
	}
	fork := model.Next{}
	for i := range directBranches {
		fork[fmt.Sprintf("direct%03d", i)] = "join"
	}
	for i := range decisionBranches {
		node := fmt.Sprintf("decide%03d", i)
		fork[fmt.Sprintf("branch%03d", i)] = node
		nodes[node] = model.Node{
			Type:      model.NodeTypeDecision,
			Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Continue?"},
			Next:      model.Next{"yes": "join", "no": "join"},
		}
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: fork}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "wide-mixed-join-any",
		Start: "start", Nodes: nodes,
	}
}

// TestWideMixedPassKeepsJoinEvidenceInsideTheTransactionLimit is the regression
// for a join cap chosen without looking at the rest of the commit: the join rows
// have to be budgeted against the events already in the same transaction, or an
// authoring-valid, engine-eligible template cannot take its FIRST transition.
//
// The numbers are chosen so that ANY join cap picked without looking at the
// rest of the commit overflows: 224 decision obligations plus one planned
// command leaves 31 slots, so even a fixed 32-row join cap commits 257 rows and
// the store refuses the transition. Budgeted, it commits exactly 256 and says
// what it left out.
func TestWideMixedPassKeepsJoinEvidenceInsideTheTransactionLimit(t *testing.T) {
	setupExecutorTest(t)
	const directBranches, decisionBranches = 33, 224
	tmpl := wideMixedJoinAnyTemplate(directBranches, decisionBranches)
	createRunFromTemplate(t, "run_wide_mixed", tmpl)

	run := mustLoadRun(t, "run_wide_mixed")
	dispatch, err := Prepare(run)
	require.NoError(t, err, "optional join history must never refuse a valid transition")
	require.NotNil(t, dispatch, "the winner's downstream route is still planned")

	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	require.LessOrEqual(t, len(events), db.MaxProcessRunEventsPerTransition)
	// The reader's page is the same size as the transaction limit, so a second
	// page is what proves the commit did not exceed it.
	overflow, err := db.ListProcessRunEvents(run.ID(), events[len(events)-1].Sequence, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	require.Empty(t, overflow, "the whole commit fits in one transaction-sized page")

	counts := map[string]int{}
	for _, event := range events {
		counts[event.Kind]++
	}
	assert.Equal(t, decisionBranches, counts["decision_awaited"],
		"every parked branch keeps its own obligation row")
	assert.Equal(t, 1, counts["program_prepared"])
	assert.Equal(t, 1, counts["join_arrivals_truncated"],
		"an incomplete join history has to say so")
	assert.Equal(t, 1, counts["join_won"], "the one winner is never the row that gets dropped")
	// Whatever room was left went to arrivals, and the batch lands exactly on
	// the limit rather than under it.
	assert.Equal(t, db.MaxProcessRunEventsPerTransition, len(events))
	assert.Equal(t, db.MaxProcessRunEventsPerTransition-decisionBranches-2,
		counts["join_won"]+counts["join_arrival_late"])

	// Causal order on the initial direct-fork pass: the arrival that won the
	// reducer is recorded before the command that winning it planned.
	won, prepared := -1, -1
	for index, event := range events {
		switch {
		case event.Kind == "join_won" && won < 0:
			won = index
		case event.Kind == "program_prepared" && prepared < 0:
			prepared = index
		}
	}
	require.GreaterOrEqual(t, won, 0)
	require.GreaterOrEqual(t, prepared, 0)
	assert.Less(t, won, prepared,
		"history must not claim the downstream command was prepared before the join was won")

	// The run is genuinely executable afterwards: the winner decided the join
	// and its downstream command is the one that was planned.
	assert.Equal(t, "join", dispatch.Command().NodeID)
	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Equal(t, engine.NodeRunning, checkpoint.Nodes["join"])
	winners := 0
	for outcome, disposition := range checkpoint.Edges["fork"] {
		if disposition == engine.EdgeArrived && tmpl.Nodes["fork"].Next[outcome] == "join" {
			winners++
		}
	}
	assert.Equal(t, 1, winners, "exactly one direct branch won the reducer")
}

// TestJoinEvidenceBudgetHandlesTheLastFewSlots pins the arithmetic at the edges
// the wide test cannot reach: no room at all, room for exactly one row, and
// room for one arrival with nothing left over.
func TestJoinEvidenceBudgetHandlesTheLastFewSlots(t *testing.T) {
	setupExecutorTest(t)
	definition, err := engine.Prepare(wideMixedJoinAnyTemplate(3, 2), map[string]string{})
	require.NoError(t, err)
	before, err := engine.Initialize("run_budget", definition)
	require.NoError(t, err)
	after, err := engine.AdvanceUntilQuiescent(before, definition)
	require.NoError(t, err)
	require.Len(t, definition.JoinArrivals(before, after), 3, "fixture must settle three arrivals")

	assert.Empty(t, joinEvidence(definition, before, after, 0), "no room records nothing")
	assert.Empty(t, joinEvidence(definition, before, after, -1), "an overfull commit underflows nothing")

	one := joinEvidence(definition, before, after, 1)
	require.Len(t, one, 1)
	assert.Equal(t, "join_arrivals_truncated", one[0].Kind,
		"a single slot buys the honest summary, not one arrival out of three")

	two := joinEvidence(definition, before, after, 2)
	require.Len(t, two, 2)
	assert.Equal(t, "join_won", two[0].Kind)
	assert.Equal(t, "join_arrivals_truncated", two[1].Kind)

	exact := joinEvidence(definition, before, after, 3)
	require.Len(t, exact, 3, "room for every arrival needs no summary row")
	for _, event := range exact {
		assert.NotEqual(t, "join_arrivals_truncated", event.Kind)
	}
	assert.Len(t, joinEvidence(definition, before, after, 99), 3, "spare room adds nothing")
}

func TestCommitEvidenceKeepsTimestampsCausalAfterReordering(t *testing.T) {
	setupExecutorTest(t)
	definition, err := engine.Prepare(wideMixedJoinAnyTemplate(2, 0), map[string]string{})
	require.NoError(t, err)
	before, err := engine.Initialize("run_causal_times", definition)
	require.NoError(t, err)
	after, err := engine.AdvanceUntilQuiescent(before, definition)
	require.NoError(t, err)

	earlier := time.Now().UTC().Add(-time.Hour)
	events := commitEvidence(definition, before, after,
		[]db.ProcessRunEvent{{Kind: "program_observed", OccurredAt: earlier}},
		[]db.ProcessRunEvent{{Kind: "program_prepared", OccurredAt: earlier}}, true)
	require.GreaterOrEqual(t, len(events), 3)
	assert.Equal(t, "program_observed", events[0].Kind)
	assert.Equal(t, "join_won", events[1].Kind)
	assert.Equal(t, "program_prepared", events[len(events)-1].Kind)
	assert.Equal(t, earlier, events[0].OccurredAt, "an already-causal timestamp is unchanged")
	assert.Equal(t, events[len(events)-2].OccurredAt, events[len(events)-1].OccurredAt,
		"an inverted successor inherits the preceding causal timestamp")
	for index := 1; index < len(events); index++ {
		assert.Falsef(t, events[index].OccurredAt.Before(events[index-1].OccurredAt),
			"event %d (%s) predates event %d (%s)",
			index, events[index].Kind, index-1, events[index-1].Kind)
	}
}
