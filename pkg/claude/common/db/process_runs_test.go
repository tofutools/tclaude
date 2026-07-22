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
		VALUES ('run_a', 1, '2026-07-22T12:00:00Z', '', 'bad_evidence', 'not-json', '')`)
	require.NoError(t, err)
	run, err := GetProcessRun("run_a")
	require.NoError(t, err)
	assert.JSONEq(t, `{"marker":"run_a"}`, string(run.CheckpointJSON))
	_, err = ListActiveProcessRuns("", MaxProcessRunReadPage)
	require.NoError(t, err)
	_, err = ListProcessRunEvents("run_a", 0, MaxProcessRunEventReadPage)
	assert.ErrorIs(t, err, ErrProcessRunCorrupt)
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

func TestProcessRunEventTimestampRFC3339YearBoundaries(t *testing.T) {
	setupTestDB(t)
	for _, year := range []int{0, 9999} {
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
	for _, year := range []int{-1, 10000} {
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
