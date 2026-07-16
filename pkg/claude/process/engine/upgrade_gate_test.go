package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

type fixedMigrationAuthority struct {
	needed            pathv1.UpgradeNeeded
	coherent          pathv1.UpgradeNeeded
	calls             int
	confirmationCalls int
}

type releaseGateAdapter struct{}

func (releaseGateAdapter) Validate(processexec.Request) error { return nil }
func (releaseGateAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{Actor: "agent:agt_release1", Verdict: "pass", EvidenceRef: "artifact:release"}, nil
}

type releaseGateDeferredAdapter struct{}

func (releaseGateDeferredAdapter) Validate(processexec.Request) error { return nil }
func (releaseGateDeferredAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	panic("deferred adapter must not perform synchronously")
}
func (releaseGateDeferredAdapter) Dispatch(context.Context, processexec.Request) (processexec.DispatchResult, error) {
	return processexec.DispatchResult{ExternalRef: "release:in-flight"}, nil
}
func (releaseGateDeferredAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	return processexec.Observation{}, processexec.DeferredInFlight, nil
}

func (a *fixedMigrationAuthority) UpgradeNeeded(context.Context, string) (pathv1.UpgradeNeeded, error) {
	a.calls++
	return a.needed, nil
}

func (a *fixedMigrationAuthority) ConfirmUpgradeNeeded(_ context.Context, _ string, supplied pathv1.UpgradeNeeded) error {
	a.confirmationCalls++
	return pathv1.RequireExactUpgradeNeeded(supplied, a.coherent)
}

func newFixedMigrationAuthority(needed pathv1.UpgradeNeeded) *fixedMigrationAuthority {
	return &fixedMigrationAuthority{needed: needed, coherent: needed}
}

func TestDecideBeforePlanningUsesOnlyTypedUpgradeNeeded(t *testing.T) {
	upgrade := validUpgradeNeeded()
	drain := validUpgradeNeeded()
	drain.Reason = pathv1.UpgradeLegacyDrainRequired
	drain.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: pathv1.LegacyActiveCommand, ID: "cmd"}}
	for _, tc := range []struct {
		name   string
		needed pathv1.UpgradeNeeded
		want   PrePlanningAction
	}{
		{name: "drain", needed: drain, want: PrePlanningDrainLegacy},
		{name: "upgrade", needed: upgrade, want: PrePlanningUpgrade},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority := newFixedMigrationAuthority(tc.needed)
			decision, err := DecideBeforePlanning(t.Context(), authority, "run")
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != tc.want || authority.calls != 1 || authority.confirmationCalls != 1 {
				t.Fatalf("decision = %#v, calls = %d, confirmations = %d", decision, authority.calls, authority.confirmationCalls)
			}
		})
	}
}

func TestDecideBeforePlanningRejectsForgedAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*pathv1.UpgradeNeeded)
	}{
		{name: "partial", mutate: func(value *pathv1.UpgradeNeeded) { value.Checkpoint.Digest = "" }},
		{name: "uppercase source digest", mutate: func(value *pathv1.UpgradeNeeded) {
			value.TemplateSourceHash = strings.ToUpper(value.TemplateSourceHash)
		}},
		{name: "forged template id", mutate: func(value *pathv1.UpgradeNeeded) {
			value.TemplateRef = "Upper@sha256:" + strings.Repeat("a", 64)
		}},
		{name: "reason mismatch", mutate: func(value *pathv1.UpgradeNeeded) { value.Reason = pathv1.UpgradeLegacyDrainRequired }},
		{name: "unsorted ids", mutate: func(value *pathv1.UpgradeNeeded) {
			value.Reason = pathv1.UpgradeLegacyDrainRequired
			value.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: pathv1.LegacyActiveWait, ID: "z"}, {Kind: pathv1.LegacyActiveCommand, ID: "a"}}
		}},
		{name: "unknown kind", mutate: func(value *pathv1.UpgradeNeeded) {
			value.Reason = pathv1.UpgradeLegacyDrainRequired
			value.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: "invented", ID: "id"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needed := validUpgradeNeeded()
			tc.mutate(&needed)
			_, err := DecideBeforePlanning(t.Context(), newFixedMigrationAuthority(needed), "run")
			if err == nil {
				t.Fatal("forged authority was accepted")
			}
		})
	}
}

func TestDecideBeforePlanningRejectsForgedCheckpointAdminProvenance(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(*pathv1.UpgradeNeeded)
		rebind           bool
		rebindResolution bool
	}{
		{
			name: "resolution omitted from active ids",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
		},
		{
			name: "cross-run record",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.RunID = "other-run"
			},
			rebind: true,
		},
		{
			name: "positive legacy event sequence rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.EventSeq = 1
			},
			rebind: true,
		},
		{
			name: "missing admin type",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.AdminType = ""
			},
			rebind: true,
		},
		{
			name: "missing actor",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Actor = ""
			},
			rebind: true,
		},
		{
			name: "missing timestamp",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Timestamp = ""
			},
			rebind: true,
		},
		{
			name: "block-resolution type without payload rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "zero blocked attempt rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.BlockedAttempt = 0
			},
			rebindResolution: true,
		},
		{
			name: "invalid resolution actor rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Actor = "operator"
			},
			rebindResolution: true,
		},
		{
			name: "engine resolution actor rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Actor = "engine:forged"
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution node rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.NodeID = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution reason rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Reason = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution evidence rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.EvidenceRef = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution timestamp rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Timestamp = ""
			},
			rebindResolution: true,
		},
		{
			name: "wrong resolution admin type rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.AdminType = "admin_repair_recorded"
			},
			rebind: true,
		},
		{
			name: "repair type swap clears resolution authority rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "admin_repair_recorded"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "programs-allowed type swap clears resolution authority rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "admin_programs_allowed"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "unknown nonresolution admin type rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "invented"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "outer actor mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Actor = "human:other"
			},
			rebind: true,
		},
		{
			name: "outer reason mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.ReasonCode = "other"
			},
			rebind: true,
		},
		{
			name: "outer evidence mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.EvidenceRef = "ticket:other"
			},
			rebind: true,
		},
		{
			name: "outer timestamp mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Timestamp = "2026-07-15T00:00:01Z"
			},
			rebind: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			needed := validUpgradeNeededWithCheckpointAdmin(t)
			test.mutate(&needed)
			if test.rebindResolution {
				rebindCheckpointAdminResolution(t, &needed)
			} else if test.rebind {
				rebindCheckpointAdminIdentity(t, &needed)
			}
			if _, err := DecideBeforePlanning(t.Context(), newFixedMigrationAuthority(needed), "run"); err == nil {
				t.Fatal("forged checkpoint admin provenance was accepted")
			}
		})
	}
}

func TestDecideBeforePlanningRequiresCoherentSourceConfirmation(t *testing.T) {
	coherent := validUpgradeNeededWithCheckpointAdmin(t)
	forged := coherent
	forged.ActiveLegacyIDs = nil
	forged.CheckpointAdminRecords = nil
	forged.Reason = pathv1.UpgradeMigrationRequired
	if err := pathv1.ValidateUpgradeNeeded(forged); err != nil {
		t.Fatalf("total-omission forgery should be structurally valid: %v", err)
	}
	authority := &fixedMigrationAuthority{needed: forged, coherent: coherent}
	decision, err := DecideBeforePlanning(t.Context(), authority, "run")
	if err == nil {
		t.Fatalf("total-omission forgery selected decision %#v", decision)
	}
	if authority.calls != 1 || authority.confirmationCalls != 1 || decision.Action == PrePlanningUpgrade {
		t.Fatalf("decision = %#v, calls = %d, confirmations = %d", decision, authority.calls, authority.confirmationCalls)
	}
}

type migrationCapableStore struct {
	store.Store
	calls int
}

func (s *migrationCapableStore) UpgradeNeeded(context.Context, string) (pathv1.UpgradeNeeded, error) {
	s.calls++
	return pathv1.UpgradeNeeded{}, nil
}

func TestLiveV6HostDoesNotCallDormantMigrationAuthority(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "terminal", Start: "end", Nodes: map[string]model.Node{"end": {Type: model.NodeTypeEnd}}}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := state.New("run", record.Ref, record.Ref, []state.NodeInit{{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusCompleted}})
	checkpoint.Status = state.RunStatusCompleted
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "run", TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}
	capable := &migrationCapableStore{Store: fs}
	host := New(capable, "test:legacy-host", nil)
	_, _ = host.Tick(t.Context())
	if capable.calls != 0 {
		t.Fatalf("live v6 host called dormant migration authority %d times", capable.calls)
	}
}

func TestEnabledHostMigratesAndCompletesExclusiveSchema7Run(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "release-host", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":   {Type: model.NodeTypeEnd, Result: "completed"},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := state.New("run-v7-host", record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	checkpoint.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "run-v7-host", TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}
	host := New(fs, "test:release-host", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: releaseGateAdapter{}})
	if err := host.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	results, err := host.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCompleted {
		t.Fatalf("enabled host tick = %#v, %v", results, err)
	}
	schema, err := fs.RunStateSchemaVersion(t.Context(), "run-v7-host")
	if err != nil || schema != pathv1.CheckpointStateSchemaVersion {
		t.Fatalf("run schema = %d, %v", schema, err)
	}
	view, err := fs.LoadPathV1RunView(t.Context(), "run-v7-host")
	if err != nil || pathv1.CurrentRunStatus(view.Checkpoint) != "completed" {
		t.Fatalf("schema-7 checkpoint = %#v, %v", view.Checkpoint, err)
	}
}

func TestEnabledHostDrainsActiveLegacyCommandBeforeMigration(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "release-drain", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":   {Type: model.NodeTypeEnd, Result: "completed"},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := state.New("run-v6-drain", record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	checkpoint.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "run-v6-drain", TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}
	adapters := map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: releaseGateDeferredAdapter{}}
	legacy := New(fs, "test:legacy-dispatch", adapters)
	results, err := legacy.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("legacy dispatch tick = %#v, %v", results, err)
	}
	enabled := New(fs, "test:release-drain", adapters)
	if err := enabled.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	results, err = enabled.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("release drain tick = %#v, %v", results, err)
	}
	schema, err := fs.RunStateSchemaVersion(t.Context(), "run-v6-drain")
	if err != nil || schema != state.StateSchemaVersion {
		t.Fatalf("active legacy run schema = %d, %v; migrated before drain", schema, err)
	}
}

func TestEnabledHostRecoversAfterCommittedAmbiguousInitialization(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "release-ambiguous", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":   {Type: model.NodeTypeEnd, Result: "completed"},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := state.New("run-v7-ambiguous", record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	checkpoint.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "run-v7-ambiguous", TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}
	injected := fmt.Errorf("lost initialization acknowledgement")
	restore := fs.SetPathV1InitializeHooksForTest(nil, func() error { return injected })
	host := New(fs, "test:release-ambiguous", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: releaseGateAdapter{}})
	if err := host.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	results, err := host.Tick(t.Context())
	restore()
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCompleted {
		t.Fatalf("ambiguous initialization recovery = %#v, %v", results, err)
	}
	schema, err := fs.RunStateSchemaVersion(t.Context(), "run-v7-ambiguous")
	if err != nil || schema != pathv1.CheckpointStateSchemaVersion {
		t.Fatalf("recovered schema = %d, %v", schema, err)
	}
}

func TestEnabledHostFallsBackToLegacyForProgressedQuiescentRun(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "release-progressed", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":   {Type: model.NodeTypeEnd, Result: "completed"},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-v6-progressed"
	checkpoint := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	checkpoint.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "work", 0)}); err != nil {
		t.Fatal(err)
	}

	host := New(fs, "test:release-progressed", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: releaseGateAdapter{}})
	if err := host.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	results, err := host.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCompleted {
		t.Fatalf("progressed legacy fallback tick = %#v, %v", results, err)
	}
	schema, err := fs.RunStateSchemaVersion(t.Context(), runID)
	if err != nil || schema != state.StateSchemaVersion {
		t.Fatalf("progressed legacy schema = %d, %v", schema, err)
	}
}

func TestEnabledHostKeepsUnsupportedCanceledEndOnLegacySchema(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "release-canceled", Start: "done",
		Nodes: map[string]model.Node{"done": {Type: model.NodeTypeEnd, Result: "canceled"}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := state.New("run-v6-canceled", record.Ref, record.Ref, []state.NodeInit{{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusReady}})
	checkpoint.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "run-v6-canceled", TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}
	host := New(fs, "test:release-canceled", nil)
	if err := host.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	results, err := host.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCanceled {
		t.Fatalf("canceled legacy tick = %#v, %v", results, err)
	}
	schema, err := fs.RunStateSchemaVersion(t.Context(), "run-v6-canceled")
	if err != nil || schema != state.StateSchemaVersion {
		t.Fatalf("canceled legacy schema = %d, %v", schema, err)
	}
}

func TestExclusiveV7EligibilityRejectsInvalidWaitAuthority(t *testing.T) {
	tests := []struct {
		name string
		wait *model.WaitConfig
		want bool
	}{
		{name: "nil", wait: nil},
		{name: "empty", wait: &model.WaitConfig{}},
		{name: "ambiguous", wait: &model.WaitConfig{Signal: "deploy", Duration: "1m"}},
		{name: "zero duration", wait: &model.WaitConfig{Duration: "0s"}},
		{name: "negative duration", wait: &model.WaitConfig{Duration: "-1s"}},
		{name: "malformed duration", wait: &model.WaitConfig{Duration: "later"}},
		{name: "malformed until", wait: &model.WaitConfig{Until: "tomorrow"}},
		{name: "signal", wait: &model.WaitConfig{Signal: "deploy"}, want: true},
		{name: "duration", wait: &model.WaitConfig{Duration: "1m"}, want: true},
		{name: "until", wait: &model.WaitConfig{Until: "2026-07-16T12:00:00Z"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := &model.Template{Nodes: map[string]model.Node{
				"wait": {Type: model.NodeTypeWait, Wait: tt.wait},
			}}
			if got := exclusiveV7Eligible(tmpl); got != tt.want {
				t.Fatalf("exclusiveV7Eligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExclusiveV7EligibilityKeepsUnsupportedTerminalShapesOnV6(t *testing.T) {
	passOnlyTask := &model.Template{Start: "work", Nodes: map[string]model.Node{
		"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done"}},
		"done": {Type: model.NodeTypeEnd, Result: "completed"},
	}}
	if exclusiveV7Eligible(passOnlyTask) {
		t.Fatal("task without a failure edge must remain on v6 until terminal-failure parity is supported")
	}
	endOnly := &model.Template{Start: "done", Nodes: map[string]model.Node{
		"done": {Type: model.NodeTypeEnd, Result: "completed"},
	}}
	if exclusiveV7Eligible(endOnly) {
		t.Fatal("end-only entry template must remain on v6 until direct end activation is supported")
	}
}

func validUpgradeNeeded() pathv1.UpgradeNeeded {
	return pathv1.UpgradeNeeded{
		Reason: pathv1.UpgradeMigrationRequired, RunID: "run", LegacyStateSchema: 6,
		Checkpoint:  pathv1.CheckpointBinding{Digest: strings.Repeat("b", 64)},
		TemplateRef: "demo@sha256:" + strings.Repeat("a", 64), TemplateSourceHash: strings.Repeat("c", 64),
	}
}

func validUpgradeNeededWithCheckpointAdmin(t *testing.T) pathv1.UpgradeNeeded {
	t.Helper()
	resolution := pathv1.BlockResolution{
		NodeID: "review", BlockedAttempt: 2, Decision: "skip", Actor: "human:operator",
		Reason: "waived", EvidenceRef: "ticket:TCL-507", Timestamp: "2026-07-15T00:00:00Z",
	}
	digest, err := pathv1.ValidateBlockResolution(resolution)
	if err != nil {
		t.Fatal(err)
	}
	record := pathv1.PathV1AdminRecord{
		RunID: "run", AdminType: "block_resolution_recorded", Actor: resolution.Actor,
		ReasonCode: resolution.Reason, EvidenceRef: resolution.EvidenceRef,
		Timestamp: resolution.Timestamp, ResolutionDigest: digest,
	}
	record.ID, err = pathv1.LegacyAdminRecordIdentity(record)
	if err != nil {
		t.Fatal(err)
	}
	needed := validUpgradeNeeded()
	needed.Reason = pathv1.UpgradeLegacyDrainRequired
	needed.Checkpoint.Generation = 12
	checkpointID, err := pathv1.CheckpointLegacyAdminRecordIdentity(needed.Checkpoint, record)
	if err != nil {
		t.Fatal(err)
	}
	needed.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: pathv1.LegacyActiveBlockResolution, ID: checkpointID}}
	needed.CheckpointAdminRecords = []pathv1.CheckpointLegacyAdminRecord{{
		ID: checkpointID, LegacyID: record.ID, Record: record, Resolution: &resolution,
	}}
	return needed
}

func rebindCheckpointAdminIdentity(t *testing.T, needed *pathv1.UpgradeNeeded) {
	t.Helper()
	admin := &needed.CheckpointAdminRecords[0]
	oldID := admin.ID
	legacyID, err := pathv1.LegacyAdminRecordIdentity(admin.Record)
	if err != nil {
		t.Fatal(err)
	}
	admin.Record.ID = legacyID
	admin.LegacyID = legacyID
	admin.ID, err = pathv1.CheckpointLegacyAdminRecordIdentity(needed.Checkpoint, admin.Record)
	if err != nil {
		t.Fatal(err)
	}
	for i := range needed.ActiveLegacyIDs {
		if needed.ActiveLegacyIDs[i] == (pathv1.LegacyActiveID{Kind: pathv1.LegacyActiveBlockResolution, ID: oldID}) {
			needed.ActiveLegacyIDs[i].ID = admin.ID
		}
	}
}

func rebindCheckpointAdminResolution(t *testing.T, needed *pathv1.UpgradeNeeded) {
	t.Helper()
	admin := &needed.CheckpointAdminRecords[0]
	admin.Record.Actor = admin.Resolution.Actor
	admin.Record.ReasonCode = admin.Resolution.Reason
	admin.Record.EvidenceRef = admin.Resolution.EvidenceRef
	admin.Record.Timestamp = admin.Resolution.Timestamp
	var err error
	admin.Record.ResolutionDigest, err = pathv1.BlockResolutionIdentity(*admin.Resolution)
	if err != nil {
		t.Fatal(err)
	}
	rebindCheckpointAdminIdentity(t, needed)
}
