package agentd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

const (
	processRuntimeReleaseBenchmark = "TCLAUDE_PROCESS_RELEASE_BENCHMARK"
	processRuntimeBudget           = 10 * time.Millisecond
)

// BenchmarkProcessRuntimeSequentialMVP measures the production daemon,
// executor, engine, and SQLite path without HTTP/CLI transport or real program
// wall time. Run the release sample with production fsync and a fixed sample
// count so p50/p95 are comparable:
//
//	TCLAUDE_TEST_KEEP_FSYNC=1 TCLAUDE_PROCESS_RELEASE_BENCHMARK=1 \
//	  go test ./pkg/claude/agentd -run '^$' \
//	  -bench '^BenchmarkProcessRuntimeSequentialMVP$' \
//	  -benchtime=100x -count=1 -v
//
// The opt-in release gate enforces the required five-node p50 budget. The
// ten-node result is reported as the preferred M1 stretch goal. Ordinary CI
// does not run an environment-sensitive wall-clock assertion.
func BenchmarkProcessRuntimeSequentialMVP(b *testing.B) {
	b.StopTimer()
	home := b.TempDir()
	b.Setenv("HOME", home)
	// A benchmark must never silently inherit the suite's synchronous(OFF)
	// fast path. Reset after setting the opt-out so every pooled connection is
	// opened with the production DSN.
	b.Setenv("TCLAUDE_TEST_KEEP_FSYNC", "1")
	db.ResetForTest()
	b.Cleanup(db.Close)

	root := filepath.Join(home, ".tclaude", "process-runtime-benchmark")
	b.Cleanup(SetProcessStoreRootForTest(root))
	previousRuns := processRuns
	processRuns = newProcessRunManager()
	b.Cleanup(func() {
		processRuns.wg.Wait()
		processRuns = previousRuns
	})
	b.Cleanup(executor.SetProgramPerformForTest(noopBenchmarkProgram))

	if err := config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}); err != nil {
		b.Fatal(err)
	}
	database, err := db.Open()
	if err != nil {
		b.Fatal(err)
	}
	sqlite := readBenchmarkSQLiteConfig(b, database)
	if sqlite.JournalMode != "wal" || sqlite.Synchronous == 0 || sqlite.ForeignKeys != 1 || sqlite.BusyTimeout != 5000 {
		b.Fatalf("benchmark requires production SQLite durability; got %+v", sqlite)
	}

	fs, err := store.NewFS(root)
	if err != nil {
		b.Fatal(err)
	}
	for _, tasks := range []int{3, 8} {
		if _, err := fs.PutTemplate(b.Context(), processRuntimeBenchmarkTemplate(tasks)); err != nil {
			b.Fatal(err)
		}
	}
	b.Logf("environment: hardware=%q os=%s/%s cpus=%d go=%s sqlite=%+v",
		benchmarkCPUModel(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), sqlite)
	b.Log("contract: starts immediately before run creation/first cold sweep; ends after the terminal checkpoint and final evidence commit; excludes fixture, HTTP/CLI, and external performer wall time")

	b.Run("five_nodes", func(b *testing.B) {
		benchmarkWarmProcessRuns(b, 3, true)
	})
	b.Run("ten_nodes", func(b *testing.B) {
		benchmarkWarmProcessRuns(b, 8, false)
	})
	b.Run("cold_reload_five_nodes", func(b *testing.B) {
		benchmarkColdProcessRuns(b, 3)
	})
}

type processRuntimeBenchmarkSample struct {
	total  time.Duration
	create time.Duration
	drive  time.Duration
}

func benchmarkWarmProcessRuns(b *testing.B, tasks int, enforceBudget bool) {
	b.ReportAllocs()
	samples := make([]processRuntimeBenchmarkSample, b.N)
	var last *executor.Run
	b.ResetTimer()
	for i := range b.N {
		started := time.Now()
		run, dispatch, err := createProcessRun(context.Background(), processRunCreateRequest{
			TemplateID: benchmarkTemplateID(tasks), AuthorizeProgramProfiles: []string{"benchmark"},
		}, "benchmark")
		created := time.Now()
		if err != nil {
			b.Fatal(err)
		}
		began, err := processRuns.beginCreated(run, dispatch)
		if err != nil {
			b.Fatal(err)
		}
		if !began {
			b.Fatal("newly created benchmark run was not started")
		}
		processRuns.wg.Wait()
		finished := time.Now()
		if run.Action().Kind != executor.ActionTerminal || run.Action().Status != engine.RunCompleted {
			b.Fatalf("run did not complete: %+v", run.Action())
		}
		samples[i] = processRuntimeBenchmarkSample{
			total: finished.Sub(started), create: created.Sub(started), drive: finished.Sub(created),
		}
		last = run
	}
	b.StopTimer()
	verifyProcessRuntimeBenchmarkRun(b, last, tasks)
	reportProcessRuntimeBenchmark(b, samples, tasks, true)
	if enforceBudget && os.Getenv(processRuntimeReleaseBenchmark) == "1" {
		if p50 := benchmarkPercentile(sampleDurations(samples, func(s processRuntimeBenchmarkSample) time.Duration { return s.total }), 0.50); p50 > processRuntimeBudget {
			b.Fatalf("required five-node p50 budget exceeded: %s > %s", p50, processRuntimeBudget)
		}
	}
}

func benchmarkColdProcessRuns(b *testing.B, tasks int) {
	b.ReportAllocs()
	samples := make([]processRuntimeBenchmarkSample, b.N)
	var lastRunID string
	for i := range b.N {
		// Persist the active fixture before the measured cold-start interval.
		b.StopTimer()
		fixtureRunID, err := createColdProcessRuntimeBenchmarkFixture(context.Background(), tasks)
		if err != nil {
			b.Fatal(err)
		}
		lastRunID = fixtureRunID

		b.StartTimer()
		started := time.Now()
		sweepProcessRuns()
		processRuns.wg.Wait()
		finished := time.Now()
		b.StopTimer()
		samples[i] = processRuntimeBenchmarkSample{total: finished.Sub(started), drive: finished.Sub(started)}
		verifyProcessRuntimeBenchmarkRunID(b, lastRunID, tasks)
	}
	reportProcessRuntimeBenchmark(b, samples, tasks, false)
}

func noopBenchmarkProgram(_ context.Context, _ string, command engine.Command) (executor.Result, error) {
	now := time.Now().UTC()
	return executor.Result{
		Observation: engine.ProgramObservation{
			CommandID: command.ID, NodeID: command.NodeID,
			Outcome: engine.ProgramSucceeded, ExitCode: 0,
		},
		StartedAt: now, FinishedAt: now, Dispatched: true,
	}, nil
}

func processRuntimeBenchmarkTemplate(tasks int) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "task-01"}},
		"end":   {Type: model.NodeTypeEnd, Result: "success"},
	}
	for i := 1; i <= tasks; i++ {
		id := fmt.Sprintf("task-%02d", i)
		next := "end"
		if i < tasks {
			next = fmt.Sprintf("task-%02d", i+1)
		}
		nodes[id] = model.Node{
			Type: model.NodeTypeTask, Next: model.Next{"next": next},
			Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "benchmark", Run: "benchmark-noop"},
		}
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: benchmarkTemplateID(tasks), Start: "start", Nodes: nodes,
	}
}

func createColdProcessRuntimeBenchmarkFixture(ctx context.Context, tasks int) (string, error) {
	tmpl := processRuntimeBenchmarkTemplate(tasks)
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return "", err
	}
	head, err := fs.GetTemplateHead(ctx, tmpl.ID)
	if err != nil {
		return "", err
	}
	definition, err := engine.Prepare(tmpl, map[string]string{})
	if err != nil {
		return "", err
	}
	runID := db.NewProcessRunID()
	checkpoint, err := engine.Initialize(runID, definition)
	if err != nil {
		return "", err
	}
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	if err != nil {
		return "", err
	}
	checkpointJSON, err := json.Marshal(checkpoint)
	if err != nil {
		return "", err
	}
	if err := db.CreateProcessRun(db.ProcessRunCreate{
		ID: runID, TemplateRef: head.Ref, TemplateSnapshotJSON: snapshot,
		ParamsJSON: json.RawMessage(`{}`), ProgramAuthorizationsJSON: json.RawMessage(`["benchmark"]`),
		Status: string(checkpoint.Status), CheckpointJSON: checkpointJSON,
		InitialEvents: []db.ProcessRunEvent{{
			Sequence: 1, OccurredAt: time.Now().UTC(), Kind: "run_created",
			PayloadJSON: json.RawMessage(`{}`), Actor: "benchmark",
		}},
	}); err != nil {
		return "", err
	}
	return runID, nil
}

func benchmarkTemplateID(tasks int) string { return fmt.Sprintf("benchmark-%02d-tasks", tasks) }

func verifyProcessRuntimeBenchmarkRun(b *testing.B, run *executor.Run, tasks int) {
	b.Helper()
	if run == nil {
		b.Fatal("benchmark produced no run")
	}
	verifyProcessRuntimeBenchmarkRunID(b, run.ID(), tasks)
}

func verifyProcessRuntimeBenchmarkRunID(b *testing.B, runID string, tasks int) {
	b.Helper()
	record, err := db.GetProcessRun(runID)
	if err != nil {
		b.Fatal(err)
	}
	if record.Status != string(engine.RunCompleted) {
		b.Fatalf("persisted run status = %q, want completed", record.Status)
	}
	events, err := db.ListProcessRunEvents(runID, 0, db.MaxProcessRunEventReadPage)
	if err != nil {
		b.Fatal(err)
	}
	if want := 2*tasks + 2; len(events) != want {
		b.Fatalf("evidence rows = %d, want %d", len(events), want)
	}
	if events[len(events)-1].Kind != "engine_advanced" {
		b.Fatalf("final evidence = %q, want engine_advanced", events[len(events)-1].Kind)
	}
}

func reportProcessRuntimeBenchmark(b *testing.B, samples []processRuntimeBenchmarkSample, tasks int, warm bool) {
	b.Helper()
	totals := sampleDurations(samples, func(s processRuntimeBenchmarkSample) time.Duration { return s.total })
	b.ReportMetric(float64(benchmarkPercentile(totals, 0.50).Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(benchmarkPercentile(totals, 0.95).Nanoseconds()), "p95-ns")
	b.ReportMetric(float64(tasks), "tasks/op")
	b.ReportMetric(float64(2*tasks+2), "evidence/op")
	if warm {
		creates := sampleDurations(samples, func(s processRuntimeBenchmarkSample) time.Duration { return s.create })
		drives := sampleDurations(samples, func(s processRuntimeBenchmarkSample) time.Duration { return s.drive })
		b.ReportMetric(float64(benchmarkPercentile(creates, 0.50).Nanoseconds()), "create-p50-ns")
		b.ReportMetric(float64(benchmarkPercentile(drives, 0.50).Nanoseconds()), "drive-p50-ns")
		b.ReportMetric(float64(tasks+1), "tx/op")
		b.ReportMetric(float64(4*tasks+3), "sqlstmt/op")
		b.ReportMetric(0, "aggregate-read/op")
		return
	}
	// Cold startup performs one bounded ID-only active-page read and one exact
	// checkpoint load, followed by the same per-transition SQL as a warm drive.
	b.ReportMetric(float64(tasks+1), "tx/op")
	b.ReportMetric(float64(4*tasks+5), "sqlstmt/op")
	b.ReportMetric(1, "aggregate-read/op")
	b.ReportMetric(1, "exact-read/op")
}

func sampleDurations(samples []processRuntimeBenchmarkSample, pick func(processRuntimeBenchmarkSample) time.Duration) []time.Duration {
	durations := make([]time.Duration, len(samples))
	for i := range samples {
		durations[i] = pick(samples[i])
	}
	return durations
}

func benchmarkPercentile(samples []time.Duration, percentile float64) time.Duration {
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := int(math.Ceil(percentile*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	return ordered[index]
}

type benchmarkSQLiteConfig struct {
	Version             string
	JournalMode         string
	Synchronous         int
	ForeignKeys         int
	BusyTimeout         int
	PageSize            int
	WALAutoCheckpoint   int
	FullFSync           int
	CheckpointFullFSync int
}

func readBenchmarkSQLiteConfig(b *testing.B, database *sql.DB) benchmarkSQLiteConfig {
	b.Helper()
	var result benchmarkSQLiteConfig
	queries := []struct {
		query string
		dst   any
	}{
		{"SELECT sqlite_version()", &result.Version},
		{"PRAGMA journal_mode", &result.JournalMode},
		{"PRAGMA synchronous", &result.Synchronous},
		{"PRAGMA foreign_keys", &result.ForeignKeys},
		{"PRAGMA busy_timeout", &result.BusyTimeout},
		{"PRAGMA page_size", &result.PageSize},
		{"PRAGMA wal_autocheckpoint", &result.WALAutoCheckpoint},
		{"PRAGMA fullfsync", &result.FullFSync},
		{"PRAGMA checkpoint_fullfsync", &result.CheckpointFullFSync},
	}
	for _, query := range queries {
		if err := database.QueryRow(query.query).Scan(query.dst); err != nil {
			b.Fatalf("%s: %v", query.query, err)
		}
	}
	result.JournalMode = strings.ToLower(result.JournalMode)
	return result
}

func benchmarkCPUModel() string {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if value, ok := strings.CutPrefix(line, "model name\t: "); ok {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	if runtime.GOOS == "darwin" {
		if data, err := osexec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return runtime.GOARCH
}
