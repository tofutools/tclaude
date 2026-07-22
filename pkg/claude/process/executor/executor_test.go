package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const helperMarker = "tclaude-safe-program-executor-helper"

func TestPreparePersistsBoundCommandBeforeAnyDispatch(t *testing.T) {
	setupExecutorTest(t)
	createRun(t, "run_prepare", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_prepare")
	require.Equal(t, ActionContinue, run.Action().Kind)

	dispatch, err := Prepare(run)
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	require.Equal(t, ActionDispatch, run.Action().Kind)

	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	require.NotNil(t, checkpoint.OutstandingCommand)
	assert.Equal(t, dispatch.command, *checkpoint.OutstandingCommand)

	cold := mustLoadRun(t, run.ID())
	assert.Equal(t, ActionNeedsReconcile, cold.Action().Kind)
	_, err = Prepare(cold)
	assert.ErrorIs(t, err, ErrNeedsReconcile)
	assert.Equal(t, []string{"program_prepared"}, eventKinds(t, run.ID()))
}

func TestLoadPreparedRunReusesCreationDefinitionWithoutSnapshotPrepare(t *testing.T) {
	setupExecutorTest(t)
	createRun(t, "run_creation_boundary", helperProgram(t, "success"))
	record, err := db.GetProcessRun("run_creation_boundary")
	require.NoError(t, err)
	var tmpl model.Template
	require.NoError(t, json.Unmarshal(record.TemplateSnapshotJSON, &tmpl))
	definition, err := engine.Prepare(&tmpl, map[string]string{})
	require.NoError(t, err)

	// A creation caller already prepared the exact snapshot before committing.
	// This narrow reconstruction path must consume the definition directly;
	// cold LoadRun remains responsible for decoding and preparing snapshots.
	record.TemplateSnapshotJSON = json.RawMessage(`not-json`)
	run, err := LoadPreparedRun(record, definition)
	require.NoError(t, err)
	assert.Equal(t, ActionContinue, run.Action().Kind)
}

func TestPrepareRollsBackCheckpointWhenEvidenceCannotCommit(t *testing.T) {
	setupExecutorTest(t)
	createRun(t, "run_prepare_rollback", helperProgram(t, "success"))
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER reject_program_prepared
		BEFORE INSERT ON process_run_events WHEN NEW.kind = 'program_prepared'
		BEGIN SELECT RAISE(ABORT, 'injected evidence failure'); END`)
	require.NoError(t, err)

	run := mustLoadRun(t, "run_prepare_rollback")
	_, err = Prepare(run)
	require.Error(t, err)
	record, err := db.GetProcessRun(run.ID())
	require.NoError(t, err)
	assert.Equal(t, db.InitialProcessRunStateVersion, record.StateVersion)
	var checkpoint engine.Checkpoint
	require.NoError(t, record.DecodeCheckpoint(&checkpoint))
	assert.Nil(t, checkpoint.OutstandingCommand)
	assert.Empty(t, eventKinds(t, run.ID()))
}

func TestExecutionRequiresRunAuthorizationAndKeepsShellSyntaxInert(t *testing.T) {
	setupExecutorTest(t)
	injectedPath := filepath.Join(t.TempDir(), "injected")
	commandSubstitution := "$(touch " + injectedPath + ")"
	program := helperProgram(t, "echo-args", commandSubstitution, `; echo injected`, "a b")
	program.Profile = "profile-a"
	createRun(t, "run_argv", program)
	run := mustLoadRun(t, "run_argv")
	dispatch, err := Prepare(run)
	require.NoError(t, err)

	_, err = Execute(t.Context(), run, dispatch, Authorization{RunID: "run_other", Profile: program.Profile})
	assert.ErrorIs(t, err, ErrUnauthorized)
	_, err = Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID(), Profile: "profile-b"})
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.Equal(t, ActionDispatch, run.Action().Kind, "authorization rejection must happen before dispatch consumption")

	result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID(), Profile: program.Profile})
	require.NoError(t, err)
	assert.True(t, result.Dispatched)
	assert.Equal(t, engine.ProgramSucceeded, result.Observation.Outcome)
	var got []string
	require.NoError(t, json.Unmarshal([]byte(result.Stdout), &got))
	assert.Equal(t, []string{commandSubstitution, `; echo injected`, "a b"}, got)
	assert.NoFileExists(t, injectedPath)
	assert.Equal(t, ActionContinue, run.Action().Kind)
}

func TestExecutionUsesBoundedSecretFreeEnvironment(t *testing.T) {
	setupExecutorTest(t)
	t.Setenv("TCLAUDE_SECRET_TOKEN", "must-not-leak")
	createRun(t, "run_env", helperProgram(t, "environment"))
	run := mustLoadRun(t, "run_env")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	require.NoError(t, err)
	assert.Equal(t, "unset|run_env|"+dispatch.command.ID, result.Stdout)
	assert.NotContains(t, result.Stdout, "must-not-leak")
}

func TestEnvironmentAndTimeoutBoundsFailWithoutDispatchAndBecomeDurableObservations(t *testing.T) {
	for _, test := range []struct {
		name          string
		changeProgram func(*engine.ProgramCommand)
		changeRuntime func(*testing.T)
		want          string
	}{
		{
			name: "environment",
			changeRuntime: func(t *testing.T) {
				t.Setenv("HOME", strings.Repeat("x", MaxEnvironmentValue+1))
			},
			want: "environment variable HOME exceeds",
		},
		{
			name: "timeout",
			changeProgram: func(program *engine.ProgramCommand) {
				program.Timeout = (MaxProgramTimeout + time.Second).String()
			},
			want: "program timeout must be",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupExecutorTest(t)
			program := helperProgram(t, "success")
			if test.changeProgram != nil {
				test.changeProgram(&program)
			}
			createRun(t, "run_bound_"+test.name, program)
			run := mustLoadRun(t, "run_bound_"+test.name)
			dispatch, err := Prepare(run)
			require.NoError(t, err)
			if test.changeRuntime != nil {
				test.changeRuntime(t)
			}
			result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
			require.NoError(t, err)
			assert.False(t, result.Dispatched)
			assert.Contains(t, result.Observation.Error, test.want)
			assert.Equal(t, ActionTerminal, run.Action().Kind)
			assert.Equal(t, engine.RunFailed, run.Action().Status)
		})
	}
}

func TestOutputIsTailBoundedAndPersistedWithObservation(t *testing.T) {
	setupExecutorTest(t)
	createRun(t, "run_output", helperProgram(t, "output"))
	run := mustLoadRun(t, "run_output")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	require.NoError(t, err)
	assert.Equal(t, engine.ProgramFailed, result.Observation.Outcome)
	assert.Equal(t, 7, result.Observation.ExitCode)
	assert.Len(t, result.Stdout, MaxOutputTailBytes)
	assert.Len(t, result.Stderr, MaxOutputTailBytes)
	assert.True(t, result.StdoutTruncated)
	assert.True(t, result.StderrTruncated)
	assert.Equal(t, strings.Repeat("o", MaxOutputTailBytes), result.Stdout)
	assert.Equal(t, strings.Repeat("e", MaxOutputTailBytes), result.Stderr)

	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "program_observed", events[1].Kind)
	var stored Result
	require.NoError(t, events[1].DecodePayload(&stored))
	assert.Equal(t, result, stored)
}

func TestBinaryOutputIsBoundedAndStoredAsValidText(t *testing.T) {
	setupExecutorTest(t)
	createRun(t, "run_binary_output", helperProgram(t, "binary-output"))
	run := mustLoadRun(t, "run_binary_output")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	require.NoError(t, err)
	assert.True(t, result.StdoutTruncated)
	assert.True(t, result.StderrTruncated)
	assert.True(t, utf8.ValidString(result.Stdout))
	assert.True(t, utf8.ValidString(result.Stderr))
	assert.LessOrEqual(t, len(result.Stdout), MaxOutputTailBytes)
	assert.LessOrEqual(t, len(result.Stderr), MaxOutputTailBytes)
	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	var stored Result
	require.NoError(t, events[len(events)-1].DecodePayload(&stored))
	assert.Equal(t, result, stored)
}

func TestObservationCommitFailureLeavesOutstandingCommandForReconciliation(t *testing.T) {
	setupExecutorTest(t)
	createRun(t, "run_observation_rollback", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_observation_rollback")
	dispatch, err := Prepare(run)
	require.NoError(t, err)
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER reject_program_observed
		BEFORE INSERT ON process_run_events WHEN NEW.kind = 'program_observed'
		BEGIN SELECT RAISE(ABORT, 'injected observation failure'); END`)
	require.NoError(t, err)

	result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "observation is not durable; reconciliation required")
	assert.Equal(t, engine.ProgramSucceeded, result.Observation.Outcome)
	assert.Equal(t, ActionNeedsReconcile, run.Action().Kind)
	_, err = runProgram(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	assert.ErrorIs(t, err, ErrStaleDispatch, "the spent dispatch must never become reusable")
	require.NoError(t, RecordOutcome(run, "operator:test", RecordedOutcome{
		Outcome: result.Observation.Outcome, ExitCode: result.Observation.ExitCode,
		Error: result.Observation.Error, Note: "recorded after observation commit failure",
	}))
	assert.Equal(t, ActionContinue, run.Action().Kind)
	cold := mustLoadRun(t, run.ID())
	assert.Equal(t, ActionContinue, cold.Action().Kind)
	assert.Equal(t, []string{"program_prepared", "program_outcome_recorded"}, eventKinds(t, run.ID()))
}

func TestCrashBoundariesNeverSilentlyRerunOutstandingCommand(t *testing.T) {
	setupExecutorTest(t)

	t.Run("before dispatch", func(t *testing.T) {
		createRun(t, "run_crash_before", helperProgram(t, "success"))
		run := mustLoadRun(t, "run_crash_before")
		_, err := Prepare(run)
		require.NoError(t, err)
		assert.Equal(t, ActionNeedsReconcile, mustLoadRun(t, run.ID()).Action().Kind)
	})

	t.Run("after execution before observation", func(t *testing.T) {
		createRun(t, "run_crash_after", helperProgram(t, "success"))
		run := mustLoadRun(t, "run_crash_after")
		dispatch, err := Prepare(run)
		require.NoError(t, err)
		result, err := runProgram(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
		require.NoError(t, err)
		require.Equal(t, engine.ProgramSucceeded, result.Observation.Outcome)
		assert.Equal(t, ActionNeedsReconcile, mustLoadRun(t, run.ID()).Action().Kind)
		_, err = runProgram(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
		assert.ErrorIs(t, err, ErrStaleDispatch)
	})

	t.Run("during execution", func(t *testing.T) {
		dir := t.TempDir()
		readyPath := filepath.Join(dir, "ready")
		pidPath := filepath.Join(dir, "descendant.pid")
		program := helperProgram(t, "descendant", readyPath, pidPath)
		program.Timeout = "30s"
		createRun(t, "run_crash_during", program)
		run := mustLoadRun(t, "run_crash_during")
		dispatch, err := Prepare(run)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		type programResponse struct {
			result Result
			err    error
		}
		done := make(chan programResponse, 1)
		go func() {
			result, err := runProgram(ctx, run, dispatch, Authorization{RunID: run.ID()})
			done <- programResponse{result: result, err: err}
		}()
		descendantPID := waitForHelper(t, readyPath, pidPath)
		t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
		cancel()
		response := <-done
		require.NoError(t, response.err)
		require.True(t, response.result.Canceled)
		waitForProcessExit(t, descendantPID)
		assert.Equal(t, ActionNeedsReconcile, mustLoadRun(t, run.ID()).Action().Kind)
	})

	t.Run("after durable observation", func(t *testing.T) {
		createRun(t, "run_crash_durable", helperProgram(t, "success"))
		run := mustLoadRun(t, "run_crash_durable")
		dispatch, err := Prepare(run)
		require.NoError(t, err)
		_, err = Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
		require.NoError(t, err)
		cold := mustLoadRun(t, run.ID())
		assert.Equal(t, ActionContinue, cold.Action().Kind)
		terminal, err := Prepare(cold)
		require.NoError(t, err)
		assert.Nil(t, terminal)
		assert.Equal(t, ActionTerminal, cold.Action().Kind)
		assert.Equal(t, engine.RunCompleted, cold.Action().Status)
	})
}

func TestExplicitReconciliationDecisionsAreDurableAndStaleOutcomesLoseCAS(t *testing.T) {
	setupExecutorTest(t)
	createRun(t, "run_reconcile", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_reconcile")
	_, err := Prepare(run)
	require.NoError(t, err)

	cold := mustLoadRun(t, run.ID())
	_, err = Reissue(cold, "")
	assert.ErrorIs(t, err, ErrInvalidActor)
	dispatch, err := Reissue(cold, "operator:johan")
	require.NoError(t, err)
	require.NotNil(t, dispatch)
	assert.Equal(t, ActionDispatch, cold.Action().Kind)
	assert.Equal(t, []string{"program_prepared", "program_reissued"}, eventKinds(t, run.ID()))

	// A crash immediately after the durable reissue decision is ambiguous too.
	winner := mustLoadRun(t, run.ID())
	stale := mustLoadRun(t, run.ID())
	assert.Equal(t, ActionNeedsReconcile, winner.Action().Kind)
	require.NoError(t, RecordOutcome(winner, "operator:johan", RecordedOutcome{
		Outcome: engine.ProgramSucceeded, ExitCode: 0, Note: "verified outside tclaude",
	}))
	assert.Equal(t, ActionContinue, winner.Action().Kind)
	err = RecordOutcome(stale, "operator:johan", RecordedOutcome{
		Outcome: engine.ProgramFailed, ExitCode: 9, Error: "late duplicate",
	})
	assert.ErrorIs(t, err, db.ErrProcessRunVersionConflict)
	assert.Equal(t, []string{"program_prepared", "program_reissued", "program_outcome_recorded"}, eventKinds(t, run.ID()))

	fresh := mustLoadRun(t, run.ID())
	assert.Equal(t, ActionContinue, fresh.Action().Kind)
	err = RecordOutcome(fresh, "operator:johan", RecordedOutcome{Outcome: engine.ProgramSucceeded})
	assert.ErrorIs(t, err, ErrNoReconciliation)
}

func TestGracefulCancellationWaitsKillsAndRecordsFailure(t *testing.T) {
	setupExecutorTest(t)
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	pidPath := filepath.Join(dir, "descendant.pid")
	program := helperProgram(t, "descendant", readyPath, pidPath)
	program.Timeout = "30s"
	createRun(t, "run_cancel", program)
	run := mustLoadRun(t, "run_cancel")
	dispatch, err := Prepare(run)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	type response struct {
		result Result
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := Execute(ctx, run, dispatch, Authorization{RunID: run.ID()})
		done <- response{result: result, err: err}
	}()
	descendantPID := waitForHelper(t, readyPath, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
	cancel()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.True(t, got.result.Canceled)
		assert.False(t, got.result.TimedOut)
		assert.Equal(t, engine.ProgramFailed, got.result.Observation.Outcome)
		assert.Contains(t, got.result.Observation.Error, "program canceled")
	case <-time.After(10 * time.Second):
		t.Fatal("executor did not return after graceful cancellation")
	}
	waitForProcessExit(t, descendantPID)
	cold := mustLoadRun(t, run.ID())
	assert.Equal(t, ActionTerminal, cold.Action().Kind)
	assert.Equal(t, engine.RunFailed, cold.Action().Status)
}

func TestProcessGroupCleanupFailureIsAuditableAndCannotRecordSuccess(t *testing.T) {
	setupExecutorTest(t)
	originalKill := killProgramProcessGroup
	killProgramProcessGroup = func(int) error { return syscall.EPERM }
	t.Cleanup(func() { killProgramProcessGroup = originalKill })
	createRun(t, "run_cleanup_failure", helperProgram(t, "success"))
	run := mustLoadRun(t, "run_cleanup_failure")
	dispatch, err := Prepare(run)
	require.NoError(t, err)

	result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
	require.NoError(t, err)
	assert.Equal(t, engine.ProgramFailed, result.Observation.Outcome)
	assert.Contains(t, result.Observation.Error, "process-group cleanup")
	assert.Contains(t, result.CleanupError, "operation not permitted")
	assert.Equal(t, engine.RunFailed, mustLoadRun(t, run.ID()).Action().Status)

	events, err := db.ListProcessRunEvents(run.ID(), 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	var stored Result
	require.NoError(t, events[len(events)-1].DecodePayload(&stored))
	assert.Equal(t, result, stored)
}

func TestProgramTimeoutKillsDescendantAndRecordsTimeout(t *testing.T) {
	setupExecutorTest(t)
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	pidPath := filepath.Join(dir, "descendant.pid")
	program := helperProgram(t, "descendant", readyPath, pidPath)
	program.Timeout = "1s"
	createRun(t, "run_timeout", program)
	run := mustLoadRun(t, "run_timeout")
	dispatch, err := Prepare(run)
	require.NoError(t, err)

	type response struct {
		result Result
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := Execute(t.Context(), run, dispatch, Authorization{RunID: run.ID()})
		done <- response{result: result, err: err}
	}()
	descendantPID := waitForHelper(t, readyPath, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
	got := <-done
	require.NoError(t, got.err)
	assert.True(t, got.result.TimedOut)
	assert.Equal(t, engine.ProgramFailed, got.result.Observation.Outcome)
	waitForProcessExit(t, descendantPID)
}

func TestProgramExecutorHelperProcess(t *testing.T) {
	args := flag.Args()
	if len(args) < 2 || args[0] != helperMarker {
		return
	}
	switch args[1] {
	case "success":
		_, _ = fmt.Fprint(os.Stdout, "ok")
	case "echo-args":
		_ = json.NewEncoder(os.Stdout).Encode(args[2:])
	case "environment":
		_, _ = fmt.Fprintf(os.Stdout, "%s|%s|%s",
			envOr("TCLAUDE_SECRET_TOKEN", "unset"),
			os.Getenv("TCLAUDE_PROCESS_RUN_ID"), os.Getenv("TCLAUDE_PROCESS_COMMAND_ID"))
	case "output":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 127)+strings.Repeat("o", MaxOutputTailBytes))
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("y", 127)+strings.Repeat("e", MaxOutputTailBytes))
		os.Exit(7)
	case "binary-output":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte{0xff, 'o'}, MaxOutputTailBytes+127))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{0xfe, 'e'}, MaxOutputTailBytes+127))
	case "descendant":
		if len(args) != 4 {
			os.Exit(91)
		}
		descendant := osexec.Command("sleep", "3600")
		descendant.Stdout, descendant.Stderr = os.Stdout, os.Stderr
		if err := descendant.Start(); err != nil {
			os.Exit(92)
		}
		_ = os.WriteFile(args[3], []byte(strconv.Itoa(descendant.Process.Pid)), 0o600)
		_ = os.WriteFile(args[2], []byte("ready"), 0o600)
		_ = descendant.Wait()
		os.Exit(93)
	default:
		os.Exit(90)
	}
	os.Exit(0)
}

func setupExecutorTest(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
}

func helperProgram(t *testing.T, mode string, args ...string) engine.ProgramCommand {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	return engine.ProgramCommand{
		Run:  executable,
		Args: append([]string{"-test.run=^TestProgramExecutorHelperProcess$", "--", helperMarker, mode}, args...),
	}
}

func createRun(t *testing.T, runID string, program engine.ProgramCommand) {
	t.Helper()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "executor-test", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "task"}},
			"task": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerProgram, Profile: program.Profile,
					Run: program.Run, Args: append([]string(nil), program.Args...), Timeout: program.Timeout,
				},
				Next: model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	definition, err := engine.Prepare(tmpl, map[string]string{})
	require.NoError(t, err)
	checkpoint, err := engine.Initialize(runID, definition)
	require.NoError(t, err)
	checkpointJSON, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	hash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	require.NoError(t, db.CreateProcessRun(db.ProcessRunCreate{
		ID: runID, TemplateRef: model.TemplateRef(tmpl.ID, hash),
		TemplateSnapshotJSON: snapshot, ParamsJSON: json.RawMessage(`{}`),
		Status: string(checkpoint.Status), CheckpointJSON: checkpointJSON,
	}))
}

func mustLoadRun(t *testing.T, runID string) *Run {
	t.Helper()
	run, err := LoadRun(runID)
	require.NoError(t, err)
	return run
}

func eventKinds(t *testing.T, runID string) []string {
	t.Helper()
	events, err := db.ListProcessRunEvents(runID, 0, db.MaxProcessRunEventReadPage)
	require.NoError(t, err)
	kinds := make([]string, len(events))
	for index := range events {
		kinds[index] = events[index].Kind
	}
	return kinds
}

func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func waitForHelper(t *testing.T, readyPath, pidPath string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ready, readyErr := os.ReadFile(readyPath)
		pidBytes, pidErr := os.ReadFile(pidPath)
		if readyErr == nil && string(ready) == "ready" && pidErr == nil {
			pid, err := strconv.Atoi(string(pidBytes))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("program helper did not become ready")
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived executor cleanup", pid)
}
