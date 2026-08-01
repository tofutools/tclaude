package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

func processRunFixture(t *testing.T, id, status string, checkpoint json.RawMessage) ProcessRunCreate {
	t.Helper()
	tmpl := storetest.Template()
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	hash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	return ProcessRunCreate{
		ID:                   id,
		TemplateRef:          model.TemplateRef(tmpl.ID, hash),
		TemplateSnapshotJSON: snapshot,
		ParamsJSON:           json.RawMessage(`{"branch":"main"}`),
		Status:               status,
		CheckpointJSON:       checkpoint,
	}
}

func processRunEvent(sequence int64, kind string) ProcessRunEvent {
	return ProcessRunEvent{
		Sequence: sequence, OccurredAt: time.Date(2026, 7, 22, 12, 0, int(sequence), 0, time.UTC),
		NodeID: "implement", Kind: kind, PayloadJSON: json.RawMessage(fmt.Sprintf(`{"sequence":%d}`, sequence)),
		Actor: "engine:agentd",
	}
}

func TestProcessRunCreateColdReadAndEvidencePagination(t *testing.T) {
	setupTestDB(t)
	checkpoint := json.RawMessage(`{"stateSchemaVersion":1,"marker":"cold"}`)
	input := processRunFixture(t, "run_cold", "running", checkpoint)
	input.InitialEvents = []ProcessRunEvent{processRunEvent(1, "run_created"), processRunEvent(2, "node_ready")}
	require.NoError(t, CreateProcessRun(input))

	run, err := GetProcessRun(input.ID)
	require.NoError(t, err)
	assert.Equal(t, input.ID, run.ID)
	assert.Equal(t, input.TemplateRef, run.TemplateRef)
	assert.Equal(t, input.TemplateSnapshotJSON, run.TemplateSnapshotJSON)
	assert.Equal(t, input.ParamsJSON, run.ParamsJSON)
	assert.Equal(t, checkpoint, run.CheckpointJSON, "cold load returns the stored checkpoint bytes")
	assert.Equal(t, InitialProcessRunStateVersion, run.StateVersion)
	assert.Equal(t, "running", run.Status)
	assert.False(t, run.CreatedAt.IsZero())
	assert.Equal(t, run.CreatedAt, run.UpdatedAt)

	events, err := ListProcessRunEvents(input.ID, 0, 1)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, int64(1), events[0].Sequence)
	events, err = ListProcessRunEvents(input.ID, events[0].Sequence, MaxProcessRunEventReadPage)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, int64(2), events[0].Sequence)
}

func TestProcessRunActiveReadIsPagedAndDoesNotReplayEvidence(t *testing.T) {
	setupTestDB(t)
	for _, item := range []struct{ id, status string }{
		{"run_a", "running"}, {"run_b", "paused"}, {"run_c", ProcessRunStatusCompleted},
		{"run_d", ProcessRunStatusFailed}, {"run_e", ProcessRunStatusCanceled},
	} {
		require.NoError(t, CreateProcessRun(processRunFixture(t, item.id, item.status, json.RawMessage(`{"marker":"`+item.id+`"}`))))
	}

	first, err := ListActiveProcessRuns("", 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, "run_a", first[0].ID)
	second, err := ListActiveProcessRuns(first[0].ID, MaxProcessRunReadPage)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "run_b", second[0].ID)

	// Manually corrupt evidence. Canonical cold-load reads must remain healthy,
	// proving neither Get nor active recovery scans/replays the history table.
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO process_run_events
		(run_id, sequence, occurred_at, node_id, kind, payload_json, actor)
		VALUES ('run_a', 1, 1784721600000000000, '', 'bad_evidence', 'not-json', '')`)
	require.NoError(t, err)
	run, err := GetProcessRun("run_a")
	require.NoError(t, err)
	assert.JSONEq(t, `{"marker":"run_a"}`, string(run.CheckpointJSON))
	_, err = ListActiveProcessRuns("", MaxProcessRunReadPage)
	require.NoError(t, err)
	_, err = ListProcessRunEvents("run_a", 0, MaxProcessRunEventReadPage)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt)
}

func TestProcessRunAggregateProjectionsDoNotMaterializeRuntimePayloads(t *testing.T) {
	setupTestDB(t)
	for i := range MaxProcessRunReadPage {
		id := fmt.Sprintf("run_summary_%02d", i)
		record := processRunFixture(t, id, "running", json.RawMessage(`{"step":1}`))
		require.NoError(t, CreateProcessRun(record))
	}

	// Give every row payload values that the full-row scanner must reject. The
	// maximum aggregate page remains readable because its projection never asks
	// SQLite to return snapshot/checkpoint/params/authorization columns.
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE process_runs SET
		template_snapshot_json = 7, params_json = 8,
		program_authorizations_json = 99, checkpoint_json = 10`)
	require.NoError(t, err)

	summaries, err := ListProcessRunSummaries("", MaxProcessRunReadPage)
	require.NoError(t, err)
	require.Len(t, summaries, MaxProcessRunReadPage)
	assert.Equal(t, "run_summary_00", summaries[0].ID)
	assert.Equal(t, "run_summary_31", summaries[len(summaries)-1].ID)
	_, err = GetProcessRun(summaries[0].ID)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt, "the full-row path still rejects the payload")
}

func TestRunnableProcessRunIDsExcludeOutstandingBeforeColdLoad(t *testing.T) {
	setupTestDB(t)
	for _, item := range []struct {
		id, status, checkpoint string
	}{
		{"run_awaiting", "running", `{"awaitingDecisions":[{"nodeId":"choose"}],
			"nodes":{"choose":"ready"}}`},
		{"run_reconcile", "running", `{"commands":[{"id":"command"}],
			"nodes":{"task":"running"}}`},
		// A branch parked on an operator resolution is blocked, not ready, so a
		// blocked-only run is excluded exactly like a decision-only one.
		{"run_blocked", "running", `{"blocked":[{"nodeId":"task"}],
			"nodes":{"task":"blocked"}}`},
		// A runnable row has a ready node and empty outbox arrays; an
		// absent-key row (run_absent) classifies its outboxes as empty via
		// COALESCE(json_array_length(...), 0) = 0 too.
		{"run_runnable", "running", `{"commands":[],"awaitingDecisions":[],
			"nodes":{"task":"ready"}}`},
		{"run_absent", "running", `{"nodes":{"task":"ready"}}`},
		{"run_terminal", ProcessRunStatusCompleted, `{"nodes":{"end":"done"}}`},
	} {
		record := processRunFixture(t, item.id, item.status, json.RawMessage(item.checkpoint))
		require.NoError(t, CreateProcessRun(record))
	}

	// Recovery needs only IDs and the checkpoint JSON predicate. A corrupt
	// snapshot would make LoadRun fail, but it must not be selected here.
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE process_runs SET template_snapshot_json = 7`)
	require.NoError(t, err)

	for range 2 {
		ids, next, err := ListRunnableProcessRunIDs("", MaxProcessRunReadPage)
		require.NoError(t, err)
		assert.Equal(t, []string{"run_absent", "run_runnable"}, ids,
			"repeated recovery pages must keep reconciliation-, decision-, and operator-blocked rows before LoadRun")
		assert.Empty(t, next)
	}
}

// TestRunnableProcessRunIDsIncludeReadyBranchBesideAwaitedDecision covers
// fan-out: an awaited decision no longer means the run is quiescent, because a
// sibling branch can hold a ready task that only a resume will plan. Excluding
// such a run would strand that branch until an unrelated event resumed it.
func TestRunnableProcessRunIDsIncludeReadyBranchBesideAwaitedDecision(t *testing.T) {
	setupTestDB(t)
	for _, item := range []struct {
		id, checkpoint string
	}{
		// Only the awaited decision is ready: genuinely quiescent, still excluded
		// so the periodic fallback does not reload it every tick.
		{"run_parked", `{"awaitingDecisions":[{"nodeId":"decide-b"}],
			"nodes":{"decide-b":"ready","fork":"done","join":"pending","start":"done"}}`},
		// A sibling branch task is ready beside the awaited decision: pushable.
		{"run_ready_branch", `{"awaitingDecisions":[{"nodeId":"decide-b"}],
			"nodes":{"decide-a":"done","decide-b":"ready","fork":"done","join":"pending",
			"start":"done","task-a":"ready"}}`},
		// Both branches parked on their own decisions: nothing to push.
		{"run_two_parked", `{"awaitingDecisions":[{"nodeId":"decide-a"},{"nodeId":"decide-b"}],
			"nodes":{"decide-a":"ready","decide-b":"ready","fork":"done","start":"done"}}`},
		// An outstanding command still excludes the run regardless of ready nodes.
		{"run_zz_command", `{"commands":[{"id":"command"}],
			"awaitingDecisions":[{"nodeId":"decide-b"}],
			"nodes":{"decide-b":"ready","task-a":"running","task-c":"ready"}}`},
	} {
		record := processRunFixture(t, item.id, "running", json.RawMessage(item.checkpoint))
		require.NoError(t, CreateProcessRun(record))
	}

	ids, _, err := ListRunnableProcessRunIDs("", MaxProcessRunReadPage)
	require.NoError(t, err)
	assert.Equal(t, []string{"run_ready_branch"}, ids,
		"only the run with work an engine pass can push is resumable")
}

// TestRunnableProcessRunIDsExcludeBlockedOnlyRuns is the parked-branch half of
// the same rule: a branch waiting on an operator resolution is blocked rather
// than ready, so a blocked-only run must not be reloaded and re-prepared every
// minute — while a blocked branch beside live work still resumes normally.
func TestRunnableProcessRunIDsExcludeBlockedOnlyRuns(t *testing.T) {
	setupTestDB(t)
	for _, item := range []struct {
		id, checkpoint string
	}{
		// Parked on an operator and nothing else: quiescent, so excluded.
		{"run_blocked_only", `{"blocked":[{"nodeId":"parked"}],
			"nodes":{"fork":"done","join":"pending","live":"done","parked":"blocked","start":"done"}}`},
		// Two parked branches, still nothing an engine pass could push.
		{"run_blocked_pair", `{"blocked":[{"nodeId":"a"},{"nodeId":"b"}],
			"nodes":{"a":"blocked","b":"blocked","fork":"done","start":"done"}}`},
		// A sibling task is ready beside the parked branch: pushable.
		{"run_blocked_with_ready", `{"blocked":[{"nodeId":"parked"}],
			"nodes":{"fork":"done","join":"pending","live":"ready","parked":"blocked","start":"done"}}`},
		// Parked beside an awaited decision: the decision consumes the only
		// ready node, so there is still nothing to push.
		{"run_blocked_with_decision", `{"blocked":[{"nodeId":"parked"}],
			"awaitingDecisions":[{"nodeId":"decide"}],
			"nodes":{"decide":"ready","fork":"done","parked":"blocked","start":"done"}}`},
	} {
		record := processRunFixture(t, item.id, "running", json.RawMessage(item.checkpoint))
		require.NoError(t, CreateProcessRun(record))
	}

	for range 2 {
		ids, _, err := ListRunnableProcessRunIDs("", MaxProcessRunReadPage)
		require.NoError(t, err)
		assert.Equal(t, []string{"run_blocked_with_ready"}, ids,
			"repeated sweeps must leave parked runs alone")
	}
}

func TestRunnableProcessRunIDsAdvanceByExaminedRowsNotMatches(t *testing.T) {
	setupTestDB(t)
	for i := range MaxProcessRunReadPage + 1 {
		checkpoint := json.RawMessage(`{"commands":[{"id":"command"}],"nodes":{"task":"running"}}`)
		if i == MaxProcessRunReadPage {
			checkpoint = json.RawMessage(`{"nodes":{"task":"ready"}}`)
		}
		id := fmt.Sprintf("run_recovery_%02d", i)
		require.NoError(t, CreateProcessRun(processRunFixture(t, id, "running", checkpoint)))
	}

	ids, next, err := ListRunnableProcessRunIDs("", MaxProcessRunReadPage)
	require.NoError(t, err)
	assert.Empty(t, ids)
	assert.Equal(t, "run_recovery_31", next,
		"cursor must advance across a full raw page even when no row is runnable")

	ids, next, err = ListRunnableProcessRunIDs(next, MaxProcessRunReadPage)
	require.NoError(t, err)
	assert.Equal(t, []string{"run_recovery_32"}, ids)
	assert.Empty(t, next)
}

func TestProcessRunTransitionRollbackAndVersionCAS(t *testing.T) {
	setupTestDB(t)
	input := processRunFixture(t, "run_atomic", "running", json.RawMessage(`{"step":1}`))
	input.InitialEvents = []ProcessRunEvent{processRunEvent(1, "created")}
	require.NoError(t, CreateProcessRun(input))

	_, err := TransitionProcessRun(input.ID, ProcessRunTransition{
		ExpectedStateVersion: 1,
		Status:               "failed",
		CheckpointJSON:       json.RawMessage(`{"step":999}`),
		Events:               []ProcessRunEvent{processRunEvent(1, "duplicate")},
	})
	assert.ErrorIs(t, err, ErrProcessRunEventSequence)
	run, err := GetProcessRun(input.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), run.StateVersion)
	assert.Equal(t, "running", run.Status)
	assert.JSONEq(t, `{"step":1}`, string(run.CheckpointJSON), "event failure rolls the checkpoint update back")

	version, err := TransitionProcessRun(input.ID, ProcessRunTransition{
		ExpectedStateVersion: 1,
		Status:               "running",
		CheckpointJSON:       json.RawMessage(`{"step":2}`),
		Events:               []ProcessRunEvent{processRunEvent(2, "advanced")},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), version)

	_, err = TransitionProcessRun(input.ID, ProcessRunTransition{
		ExpectedStateVersion: 1,
		Status:               "failed",
		CheckpointJSON:       json.RawMessage(`{"step":3}`),
	})
	assert.ErrorIs(t, err, ErrProcessRunVersionConflict)
	var conflict *ProcessRunVersionConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, int64(2), conflict.Actual)
}

func TestProcessRunTransitionCanAssignEvidenceSequenceAtomically(t *testing.T) {
	setupTestDB(t)
	input := processRunFixture(t, "run_auto_sequence", "running", json.RawMessage(`{"step":1}`))
	input.InitialEvents = []ProcessRunEvent{processRunEvent(7, "created")}
	require.NoError(t, CreateProcessRun(input))

	event := processRunEvent(0, "advanced")
	_, err := TransitionProcessRun(input.ID, ProcessRunTransition{
		ExpectedStateVersion: 1,
		Status:               "running",
		CheckpointJSON:       json.RawMessage(`{"step":2}`),
		Events:               []ProcessRunEvent{event},
	})
	require.NoError(t, err)

	events, err := ListProcessRunEvents(input.ID, 0, MaxProcessRunEventReadPage)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, []int64{7, 8}, []int64{events[0].Sequence, events[1].Sequence})
}

func TestProcessRunTransitionRollsBackPartialEvidenceAppend(t *testing.T) {
	setupTestDB(t)
	input := processRunFixture(t, "run_partial_rollback", "running", json.RawMessage(`{"step":1}`))
	require.NoError(t, CreateProcessRun(input))
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TRIGGER reject_second_process_event
		BEFORE INSERT ON process_run_events WHEN NEW.sequence = 2
		BEGIN SELECT RAISE(ABORT, 'injected event failure'); END`)
	require.NoError(t, err)

	_, err = TransitionProcessRun(input.ID, ProcessRunTransition{
		ExpectedStateVersion: 1,
		Status:               "failed",
		CheckpointJSON:       json.RawMessage(`{"step":2}`),
		Events: []ProcessRunEvent{
			processRunEvent(1, "first_would_insert"),
			processRunEvent(2, "injected_failure"),
		},
	})
	require.Error(t, err)

	run, err := GetProcessRun(input.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), run.StateVersion)
	assert.Equal(t, "running", run.Status)
	assert.JSONEq(t, `{"step":1}`, string(run.CheckpointJSON))
	events, err := ListProcessRunEvents(input.ID, 0, MaxProcessRunEventReadPage)
	require.NoError(t, err)
	assert.Empty(t, events, "the event inserted before the failure must roll back too")
}

func TestProcessRunConcurrentTransitionsHaveOneWinner(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, CreateProcessRun(processRunFixture(t, "run_race", "running", json.RawMessage(`{"winner":0}`))))

	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index := 1; index <= 2; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := TransitionProcessRun("run_race", ProcessRunTransition{
				ExpectedStateVersion: 1,
				Status:               "running",
				CheckpointJSON:       json.RawMessage(fmt.Sprintf(`{"winner":%d}`, index)),
				Events:               []ProcessRunEvent{processRunEvent(int64(index), "race")},
			})
			results <- err
		}(index)
	}
	wg.Wait()
	close(results)
	var succeeded, conflicted int
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrProcessRunVersionConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected transition result: %v", err)
		}
	}
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, conflicted)
}

func TestProcessRunDuplicatesFailClearly(t *testing.T) {
	setupTestDB(t)
	input := processRunFixture(t, "run_duplicate", "running", json.RawMessage(`{"step":1}`))
	require.NoError(t, CreateProcessRun(input))
	assert.ErrorIs(t, CreateProcessRun(input), ErrProcessRunExists)

	_, err := TransitionProcessRun(input.ID, ProcessRunTransition{
		ExpectedStateVersion: 1, Status: "running", CheckpointJSON: json.RawMessage(`{"step":2}`),
		Events: []ProcessRunEvent{processRunEvent(1, "first")},
	})
	require.NoError(t, err)
	_, err = TransitionProcessRun(input.ID, ProcessRunTransition{
		ExpectedStateVersion: 2, Status: "running", CheckpointJSON: json.RawMessage(`{"step":3}`),
		Events: []ProcessRunEvent{processRunEvent(1, "duplicate")},
	})
	assert.ErrorIs(t, err, ErrProcessRunEventSequence)
}

func TestProcessRunJSONIsStrict(t *testing.T) {
	setupTestDB(t)
	for name, checkpoint := range map[string]json.RawMessage{
		"malformed":  []byte(`{"x":`),
		"trailing":   []byte(`{} {}`),
		"non-object": []byte(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			err := CreateProcessRun(processRunFixture(t, "run_"+strings.ReplaceAll(name, "-", "_"), "running", checkpoint))
			assert.ErrorIs(t, err, ErrProcessRunInvalid)
		})
	}

	input := processRunFixture(t, "run_strict", "running", json.RawMessage(`{"known":1,"unknown":2}`))
	require.NoError(t, CreateProcessRun(input))
	run, err := GetProcessRun(input.ID)
	require.NoError(t, err)
	var checkpoint struct {
		Known int `json:"known"`
	}
	err = run.DecodeCheckpoint(&checkpoint)
	assert.ErrorIs(t, err, ErrProcessRunInvalid)
	assert.Contains(t, err.Error(), "unknown field")

	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE process_runs SET checkpoint_json = '{} trailing' WHERE id = ?`, input.ID)
	require.NoError(t, err)
	_, err = GetProcessRun(input.ID)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt)
}

func TestProcessRunJSONRejectsDuplicatesAndInvalidUTF8AcrossSurfaces(t *testing.T) {
	invalidUTF8 := json.RawMessage{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
	cases := map[string]json.RawMessage{
		"nested duplicate":    json.RawMessage(`{"outer":{"x":1,"x":2}}`),
		"duplicate known key": json.RawMessage(`{"known":1,"known":2}`),
		"invalid UTF-8":       invalidUTF8,
	}
	decoders := map[string]func(json.RawMessage) error{
		"checkpoint": func(data json.RawMessage) error {
			run := ProcessRun{CheckpointJSON: data}
			var dst struct {
				Known int            `json:"known"`
				Outer map[string]int `json:"outer"`
				X     string         `json:"x"`
			}
			return run.DecodeCheckpoint(&dst)
		},
		"params": func(data json.RawMessage) error {
			run := ProcessRun{ParamsJSON: data}
			var dst map[string]any
			return run.DecodeParams(&dst)
		},
		"evidence payload": func(data json.RawMessage) error {
			event := ProcessRunEvent{PayloadJSON: data}
			var dst map[string]any
			return event.DecodePayload(&dst)
		},
	}
	for surface, decode := range decoders {
		for name, data := range cases {
			t.Run(surface+"/"+name, func(t *testing.T) {
				assert.ErrorIs(t, decode(data), ErrProcessRunInvalid)
			})
		}
	}

	setupTestDB(t)
	for name, mutate := range map[string]func(*ProcessRunCreate){
		"checkpoint": func(input *ProcessRunCreate) {
			input.CheckpointJSON = json.RawMessage(`{"known":1,"known":2}`)
		},
		"params": func(input *ProcessRunCreate) {
			input.ParamsJSON = json.RawMessage(`{"outer":{"x":1,"x":2}}`)
		},
		"evidence payload": func(input *ProcessRunCreate) {
			event := processRunEvent(1, "created")
			event.PayloadJSON = invalidUTF8
			input.InitialEvents = []ProcessRunEvent{event}
		},
	} {
		t.Run("create/"+name, func(t *testing.T) {
			input := processRunFixture(t, "run_strict_"+strings.ReplaceAll(name, " ", "_"), "running", json.RawMessage(`{}`))
			mutate(&input)
			assert.ErrorIs(t, CreateProcessRun(input), ErrProcessRunInvalid)
		})
	}
}

func TestProcessRunEventTimestampUnixNanoYearBoundaries(t *testing.T) {
	setupTestDB(t)
	for _, year := range []int{1678, 2262} {
		t.Run(fmt.Sprintf("accept_%d", year), func(t *testing.T) {
			input := processRunFixture(t, fmt.Sprintf("run_time_%d", year), "running", json.RawMessage(`{}`))
			event := processRunEvent(1, "created")
			event.OccurredAt = time.Date(year, 1, 2, 3, 4, 5, 6, time.UTC)
			input.InitialEvents = []ProcessRunEvent{event}
			require.NoError(t, CreateProcessRun(input))
			stored, err := ListProcessRunEvents(input.ID, 0, 1)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			assert.True(t, stored[0].OccurredAt.Equal(event.OccurredAt))
		})
	}
	for _, year := range []int{-1, 0, 1677, 2263, 9999, 10000} {
		t.Run(fmt.Sprintf("reject_%d", year), func(t *testing.T) {
			id := fmt.Sprintf("run_time_reject_%d", year)
			input := processRunFixture(t, id, "running", json.RawMessage(`{}`))
			event := processRunEvent(1, "created")
			event.OccurredAt = time.Date(year, 1, 2, 3, 4, 5, 6, time.UTC)
			input.InitialEvents = []ProcessRunEvent{event}
			assert.ErrorIs(t, CreateProcessRun(input), ErrProcessRunInvalid)
			_, err := GetProcessRun(id)
			assert.ErrorIs(t, err, ErrProcessRunNotFound, "validation must fail before the transaction inserts the run")
		})
	}
}

func TestProcessRunReadsRejectOversizedCorruptRowsBeforeScanningContent(t *testing.T) {
	setupTestDB(t)
	snapshotInput := processRunFixture(t, "run_oversized_a_snapshot", "running", json.RawMessage(`{}`))
	require.NoError(t, CreateProcessRun(snapshotInput))
	input := processRunFixture(t, "run_oversized_b_checkpoint", "running", json.RawMessage(`{}`))
	input.InitialEvents = []ProcessRunEvent{processRunEvent(1, "created")}
	require.NoError(t, CreateProcessRun(input))

	d, err := Open()
	require.NoError(t, err)
	conn, err := d.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA ignore_check_constraints = ON`)
	require.NoError(t, err)
	overSnapshot := `{"x":"` + strings.Repeat("x", MaxProcessRunTemplateSnapshotBytes) + `"}`
	_, err = conn.ExecContext(t.Context(), `UPDATE process_runs SET template_snapshot_json = ? WHERE id = ?`, overSnapshot, snapshotInput.ID)
	require.NoError(t, err)
	overCheckpoint := `{"x":"` + strings.Repeat("x", MaxProcessRunCheckpointBytes) + `"}`
	_, err = conn.ExecContext(t.Context(), `UPDATE process_runs SET checkpoint_json = ? WHERE id = ?`, overCheckpoint, input.ID)
	require.NoError(t, err)
	overPayload := `{"x":"` + strings.Repeat("x", MaxProcessRunEventPayloadBytes) + `"}`
	_, err = conn.ExecContext(t.Context(), `UPDATE process_run_events SET payload_json = ? WHERE run_id = ?`, overPayload, input.ID)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	_, err = GetProcessRun(snapshotInput.ID)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt)
	_, err = GetProcessRun(input.ID)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt)
	_, err = ListActiveProcessRuns("", MaxProcessRunReadPage)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt)
	_, err = ListProcessRunEvents(input.ID, 0, MaxProcessRunEventReadPage)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt)
}

func TestProcessRunTemplateSnapshotIsPinnedAtCreationAndAuthoritativeAtRuntime(t *testing.T) {
	setupTestDB(t)
	input := processRunFixture(t, "run_template", "running", json.RawMessage(`{"step":1}`))
	require.NoError(t, CreateProcessRun(input))

	unknown := append(json.RawMessage(nil), input.TemplateSnapshotJSON[:len(input.TemplateSnapshotJSON)-1]...)
	unknown = append(unknown, []byte(`,"unknown":true}`)...)
	bad := input
	bad.ID = "run_unknown_template"
	bad.TemplateSnapshotJSON = unknown
	assert.ErrorIs(t, CreateProcessRun(bad), ErrProcessRunInvalid)

	bad = input
	bad.ID = "run_wrong_ref"
	bad.TemplateRef += "0"
	assert.ErrorIs(t, CreateProcessRun(bad), ErrProcessRunInvalid)

	var edited model.Template
	require.NoError(t, json.Unmarshal(input.TemplateSnapshotJSON, &edited))
	edited.Name = "user-edited runtime definition"
	encodedEdit, err := json.Marshal(edited)
	require.NoError(t, err)
	editedSnapshot := append(json.RawMessage(" \n"), encodedEdit...)
	newHash, err := model.SemanticHash(&edited)
	require.NoError(t, err)
	require.NotEqual(t, input.TemplateRef, model.TemplateRef(edited.ID, newHash))

	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE process_runs SET template_snapshot_json = ? WHERE id = ?`, string(editedSnapshot), input.ID)
	require.NoError(t, err)
	run, err := GetProcessRun(input.ID)
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(editedSnapshot), run.TemplateSnapshotJSON)
	active, err := ListActiveProcessRuns("", MaxProcessRunReadPage)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, json.RawMessage(editedSnapshot), active[0].TemplateSnapshotJSON)

	var runtimeTemplate model.Template
	require.NoError(t, strictjson.Decode(run.TemplateSnapshotJSON, &runtimeTemplate))
	assert.Equal(t, edited.Name, runtimeTemplate.Name)

	_, err = d.Exec(`UPDATE process_runs SET template_snapshot_json = ? WHERE id = ?`, string(unknown), input.ID)
	require.NoError(t, err)
	run, err = GetProcessRun(input.ID)
	require.NoError(t, err, "raw detail reads leave template decoding to the reconstruction boundary")
	assert.Error(t, strictjson.Decode(run.TemplateSnapshotJSON, &runtimeTemplate))
}

func TestProcessRunBoundsExactAndPlusOne(t *testing.T) {
	setupTestDB(t)
	exact := json.RawMessage(`{"x":"` + strings.Repeat("x", MaxProcessRunCheckpointBytes-8) + `"}`)
	require.Len(t, exact, MaxProcessRunCheckpointBytes)
	require.NoError(t, CreateProcessRun(processRunFixture(t, "run_exact_bound", "running", exact)))

	over := json.RawMessage(`{"x":"` + strings.Repeat("x", MaxProcessRunCheckpointBytes-7) + `"}`)
	require.Len(t, over, MaxProcessRunCheckpointBytes+1)
	err := CreateProcessRun(processRunFixture(t, "run_over_bound", "running", over))
	assert.ErrorIs(t, err, ErrProcessRunInvalid)

	events := make([]ProcessRunEvent, MaxProcessRunEventsPerTransition+1)
	for index := range events {
		events[index] = processRunEvent(int64(index+1), "bounded")
	}
	_, err = TransitionProcessRun("run_exact_bound", ProcessRunTransition{
		ExpectedStateVersion: 1, Status: "running", CheckpointJSON: exact, Events: events,
	})
	assert.ErrorIs(t, err, ErrProcessRunInvalid)
	_, err = ListActiveProcessRuns("", MaxProcessRunReadPage+1)
	assert.ErrorIs(t, err, ErrProcessRunInvalid)
	_, err = ListProcessRunEvents("run_exact_bound", 0, MaxProcessRunEventReadPage+1)
	assert.ErrorIs(t, err, ErrProcessRunInvalid)
}

func TestDeleteProcessRunsWithoutCheckpointVersionRemovesOnlyIncompatibleRuns(t *testing.T) {
	setupTestDB(t)
	outdated := processRunFixture(t, "run_v1", "running", json.RawMessage(`{"version":1,"cursor":"old"}`))
	outdated.InitialEvents = []ProcessRunEvent{processRunEvent(1, "created")}
	require.NoError(t, CreateProcessRun(outdated))
	unversioned := processRunFixture(t, "run_unversioned", "running", json.RawMessage(`{"cursor":"older"}`))
	require.NoError(t, CreateProcessRun(unversioned))
	current := processRunFixture(t, "run_v2", "running", json.RawMessage(`{"version":2,"cursor":"new"}`))
	current.InitialEvents = []ProcessRunEvent{processRunEvent(1, "created")}
	require.NoError(t, CreateProcessRun(current))
	_, _, err := CreateProcessSnippet("Keep", "keep", `{"kind":"keep"}`)
	require.NoError(t, err)

	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), storetest.Template())
	require.NoError(t, err)

	wiped, err := DeleteProcessRunsWithoutCheckpointVersion(2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), wiped)
	_, err = GetProcessRun("run_v1")
	assert.ErrorIs(t, err, ErrProcessRunNotFound)
	_, err = GetProcessRun("run_unversioned")
	assert.ErrorIs(t, err, ErrProcessRunNotFound)

	kept, err := GetProcessRun("run_v2")
	require.NoError(t, err)
	assert.Equal(t, current.CheckpointJSON, kept.CheckpointJSON)
	events, err := ListProcessRunEvents("run_v2", 0, MaxProcessRunEventReadPage)
	require.NoError(t, err)
	assert.Len(t, events, 1, "compatible run evidence must survive the version wipe")

	d, err := Open()
	require.NoError(t, err)
	var orphaned int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM process_run_events WHERE run_id != 'run_v2'`).Scan(&orphaned))
	assert.Zero(t, orphaned, "incompatible run evidence must cascade away with its checkpoint")
	library, err := ListProcessSnippets()
	require.NoError(t, err)
	require.Len(t, library.Snippets, 1)
	_, err = fs.GetTemplate(t.Context(), record.Ref)
	require.NoError(t, err, "checkpoint version wipe must not touch filesystem template data")
}

func TestWipeProcessRuntimeDataPreservesTemplateAuthoringAndOtherDBData(t *testing.T) {
	setupTestDB(t)
	input := processRunFixture(t, "run_wipe", "running", json.RawMessage(`{"step":1}`))
	input.InitialEvents = []ProcessRunEvent{processRunEvent(1, "created")}
	require.NoError(t, CreateProcessRun(input))
	_, _, err := CreateProcessSnippet("Keep", "keep", `{"kind":"keep"}`)
	require.NoError(t, err)

	root := t.TempDir()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), storetest.Template())
	require.NoError(t, err)

	wiped, err := WipeProcessRuntimeData()
	require.NoError(t, err)
	assert.Equal(t, int64(1), wiped)
	_, err = GetProcessRun(input.ID)
	assert.ErrorIs(t, err, ErrProcessRunNotFound)

	d, err := Open()
	require.NoError(t, err)
	var events int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM process_run_events`).Scan(&events))
	assert.Zero(t, events)
	library, err := ListProcessSnippets()
	require.NoError(t, err)
	require.Len(t, library.Snippets, 1)
	_, err = fs.GetTemplate(t.Context(), record.Ref)
	require.NoError(t, err, "SQLite runtime wipe must not touch filesystem template data")
	_, err = os.Stat(root)
	require.NoError(t, err)
}
