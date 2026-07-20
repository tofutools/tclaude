package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
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

type releaseGateDeferredAdapter struct {
	reconciliations int
}

type progressedMigrationAdapter struct {
	reconciliations int
	nudges          int
}

type countingReleaseAdapter struct{ calls int }

func (a *countingReleaseAdapter) Validate(processexec.Request) error {
	a.calls++
	return nil
}

func (a *countingReleaseAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	a.calls++
	return processexec.Observation{Actor: "agent:agt_release1", Verdict: "pass"}, nil
}

func (*releaseGateDeferredAdapter) Validate(processexec.Request) error { return nil }
func (*releaseGateDeferredAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	panic("deferred adapter must not perform synchronously")
}
func (*releaseGateDeferredAdapter) Dispatch(context.Context, processexec.Request) (processexec.DispatchResult, error) {
	return processexec.DispatchResult{ExternalRef: "release:in-flight"}, nil
}
func (a *releaseGateDeferredAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	a.reconciliations++
	if a.reconciliations == 1 {
		return processexec.Observation{}, processexec.DeferredInFlight, nil
	}
	return processexec.Observation{Actor: "agent:agt_release1", Verdict: "pass", EvidenceRef: "artifact:release"}, processexec.DeferredObserved, nil
}

func (*progressedMigrationAdapter) Validate(request processexec.Request) error {
	if request.Input.NodeID == "recover" {
		return fmt.Errorf("fixture stops before recovery dispatch")
	}
	return nil
}

func (*progressedMigrationAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	panic("progressed migration fixture is deferred")
}

func (*progressedMigrationAdapter) Dispatch(context.Context, processexec.Request) (processexec.DispatchResult, error) {
	return processexec.DispatchResult{ExternalRef: "agent:agt_migrationworker", Assignee: "agent:agt_migrationworker"}, nil
}

func (a *progressedMigrationAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	a.reconciliations++
	if a.reconciliations < 3 {
		return processexec.Observation{}, processexec.DeferredInFlight, nil
	}
	return processexec.Observation{
		Actor: "agent:agt_migrationworker", Verdict: "fail", EvidenceRef: "artifact:migration-failure",
		EvidenceHash: strings.Repeat("a", 64), ExternalRef: "agent:agt_migrationworker",
	}, processexec.DeferredObserved, nil
}

func (a *progressedMigrationAdapter) Contact(context.Context, processexec.Request, bool) error {
	a.nudges++
	return nil
}

func (*progressedMigrationAdapter) Activity(context.Context, processexec.Request, time.Time) (processexec.Activity, error) {
	return processexec.Activity{}, nil
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
	adapter := &releaseGateDeferredAdapter{}
	adapters := map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter}
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
	if adapter.reconciliations != 1 {
		t.Fatalf("release drain reconciliations = %d, want first in-flight observation", adapter.reconciliations)
	}

	results, err = enabled.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("release observation tick = %#v, %v", results, err)
	}
	if results[0].Status != state.RunStatusCompleted {
		t.Fatalf("release observation status = %q, want settled completion", results[0].Status)
	}
	schema, err = fs.RunStateSchemaVersion(t.Context(), "run-v6-drain")
	if err != nil || schema != state.StateSchemaVersion {
		t.Fatalf("observed legacy run schema = %d, %v; migrated during drain", schema, err)
	}
	if adapter.reconciliations != 2 {
		t.Fatalf("release drain reconciliations = %d, want later observed result", adapter.reconciliations)
	}
	// The drain is now complete and future parity migration has exact authority.
	// Today's initializer deliberately accepts only pristine v6 checkpoints, so
	// this progressed terminal history must remain v6 without being re-driven.
	readiness, err := fs.UpgradeNeeded(t.Context(), "run-v6-drain")
	if err != nil || readiness.Reason != pathv1.UpgradeMigrationRequired || len(readiness.ActiveLegacyIDs) != 0 {
		t.Fatalf("post-drain readiness = %#v, %v; want quiescent migration authority", readiness, err)
	}

	results, err = enabled.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("post-drain release tick = %#v, %v", results, err)
	}
	schema, err = fs.RunStateSchemaVersion(t.Context(), "run-v6-drain")
	if err != nil || schema != state.StateSchemaVersion {
		t.Fatalf("progressed terminal run schema = %d, %v; want intentional v6 fallback", schema, err)
	}
	if results[0].Status != state.RunStatusCompleted || adapter.reconciliations != 2 {
		t.Fatalf("post-drain release tick changed settled result: result = %#v, reconciliations = %d", results[0], adapter.reconciliations)
	}

	results, err = enabled.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCompleted {
		t.Fatalf("repeat v6 fallback tick = %#v, %v", results, err)
	}
	schema, err = fs.RunStateSchemaVersion(t.Context(), "run-v6-drain")
	if err != nil || schema != state.StateSchemaVersion || adapter.reconciliations != 2 {
		t.Fatalf("repeat tick was not idempotent: schema = %d, err = %v, reconciliations = %d", schema, err, adapter.reconciliations)
	}
}

func TestEnabledHostDrainsMigratesAndResumesProgressedLegacyRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "release-progressed-migration", Start: "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work", Contact: &model.ContactSchedule{
					Cadence: "5m", Budget: 3, EscalationTarget: "human:operator",
				}},
				Next: model.Next{"pass": "done", "fail": "recover"},
			},
			"recover": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "recover"}, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":    {Type: model.NodeTypeEnd, Result: "completed"},
			"failed":  {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-v6-progressed-migration"
	checkpoint := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "recover", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	checkpoint.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	legacyAdapter := &progressedMigrationAdapter{}
	legacyHost := New(fs, "test:progressed-legacy", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: legacyAdapter})
	legacyHost.Now = func() time.Time { return now }
	results, err := legacyHost.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || legacyAdapter.reconciliations != 0 {
		t.Fatalf("legacy dispatch tick = %#v, err=%v adapter=%#v", results, err, legacyAdapter)
	}
	now = now.Add(6 * time.Minute)
	results, err = legacyHost.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || legacyAdapter.reconciliations != 1 || legacyAdapter.nudges != 1 {
		t.Fatalf("legacy contact tick = %#v, err=%v adapter=%#v", results, err, legacyAdapter)
	}
	now = now.Add(time.Minute)
	results, err = legacyHost.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || legacyAdapter.reconciliations != 2 {
		t.Fatalf("legacy second reconcile tick = %#v, err=%v adapter=%#v", results, err, legacyAdapter)
	}
	now = now.Add(time.Minute)
	results, err = legacyHost.Tick(t.Context())
	if err != nil || len(results) != 1 || !strings.Contains(results[0].Error, "fixture stops before recovery dispatch") || legacyAdapter.reconciliations != 3 {
		t.Fatalf("legacy settle tick = %#v, err=%v adapter=%#v", results, err, legacyAdapter)
	}

	legacySnapshot, err := fs.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if legacySnapshot.State.Status != state.RunStatusRunning || legacySnapshot.State.Nodes["work"].Status != state.NodeStatusFailed || legacySnapshot.State.Nodes["recover"].Status != state.NodeStatusReady {
		t.Fatalf("quiescent legacy frontier = %#v", legacySnapshot.State.Nodes)
	}
	var legacyContact state.ContactState
	for _, contact := range legacySnapshot.State.Contacts {
		legacyContact = contact
	}
	if legacyContact.Used != 1 || !legacyContact.Paused || legacyContact.PauseReason != "performer observed" {
		t.Fatalf("settled legacy contact = %#v", legacyContact)
	}
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	if err != nil || proof.Reason != pathv1.UpgradeMigrationRequired || len(proof.ActiveLegacyIDs) != 0 {
		t.Fatalf("quiescent migration proof = %#v, %v", proof, err)
	}
	stateBefore, err := os.ReadFile(filepath.Join(root, "runs", runID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, "runs", runID, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	workLogBefore, err := os.ReadFile(filepath.Join(root, "runs", runID, "nodes", "work", "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	recoverLogBefore, err := os.ReadFile(filepath.Join(root, "runs", runID, "nodes", "recover", "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	forged, err := state.Decode(stateBefore)
	if err != nil {
		t.Fatal(err)
	}
	forgedRecover := forged.Nodes["recover"]
	forgedRecover.Status = state.NodeStatusPending
	forged.Nodes["recover"] = forgedRecover
	forgedFailed := forged.Nodes["failed"]
	forgedFailed.Status = state.NodeStatusReady
	forged.Nodes["failed"] = forgedFailed
	forgedBytes, err := state.Encode(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), forgedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	forgedProof, err := fs.UpgradeNeeded(t.Context(), runID)
	if err != nil || forgedProof.Reason != pathv1.UpgradeMigrationRequired {
		t.Fatalf("structurally valid forged frontier proof = %#v, err=%v", forgedProof, err)
	}
	countingAdapter := &countingReleaseAdapter{}
	forgedHost := New(fs, "test:forged-progressed-v7", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: countingAdapter})
	if err := forgedHost.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	forgedResults, forgedErr := forgedHost.Tick(t.Context())
	if forgedErr == nil && len(forgedResults) == 1 && forgedResults[0].Error == "" {
		t.Fatalf("forged retained frontier was acknowledged: %#v", forgedResults)
	}
	if countingAdapter.calls != 0 {
		t.Fatalf("forged retained frontier invoked adapter %d time(s)", countingAdapter.calls)
	}
	if err := os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), stateBefore, 0o644); err != nil {
		t.Fatal(err)
	}

	enabled := New(fs, "test:progressed-v7", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: releaseGateAdapter{}})
	if err := enabled.EnableExclusiveV7(); err != nil {
		t.Fatal(err)
	}
	injectedCommitFailure := errors.New("injected progressed migration commit failure")
	restoreInitializeHook := fs.SetPathV1InitializeHooksForTest(func() error { return injectedCommitFailure }, nil)
	failedResults, failedErr := enabled.Tick(t.Context())
	restoreInitializeHook()
	if failedErr == nil && len(failedResults) == 1 && failedResults[0].Error == "" {
		t.Fatalf("progressed migration commit failure was acknowledged: %#v", failedResults)
	}
	for path, want := range map[string][]byte{
		filepath.Join(root, "runs", runID, "state.json"):                    stateBefore,
		filepath.Join(root, "runs", runID, "manifest.jsonl"):                manifestBefore,
		filepath.Join(root, "runs", runID, "nodes", "work", "log.jsonl"):    workLogBefore,
		filepath.Join(root, "runs", runID, "nodes", "recover", "log.jsonl"): recoverLogBefore,
	} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("progressed commit failure changed %s: err=%v", path, err)
		}
	}
	results, err = enabled.Tick(t.Context())
	if err != nil || len(results) != 1 || results[0].Error != "" || results[0].Status != state.RunStatusCompleted {
		t.Fatalf("progressed migration/resume tick = %#v, %v", results, err)
	}
	if schema, err := fs.RunStateSchemaVersion(t.Context(), runID); err != nil || schema != pathv1.CheckpointStateSchemaVersion {
		t.Fatalf("migrated schema = %d, %v", schema, err)
	}
	view, err := fs.LoadPathV1RunView(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if pathv1.CurrentRunStatus(view.Checkpoint) != "completed" || view.Checkpoint.Execution == nil || view.Checkpoint.Execution.LegacyProjection == nil {
		t.Fatalf("migrated checkpoint = %#v", view.Checkpoint)
	}
	if len(view.Checkpoint.Execution.Aggregate.Contacts) != 1 {
		t.Fatalf("migrated contacts = %#v", view.Checkpoint.Execution.Aggregate.Contacts)
	}
	migrationAdminRecords := 0
	for _, record := range view.Checkpoint.Execution.Aggregate.AdminRecords {
		if record.AdminType == pathv1.LegacyProjectionAdminType {
			migrationAdminRecords++
		}
	}
	if migrationAdminRecords != 1 {
		t.Fatalf("progressed migration retry admin records = %d", migrationAdminRecords)
	}
	for _, contact := range view.Checkpoint.Execution.Aggregate.Contacts {
		if contact.Used != 1 || contact.Budget != 3 || contact.Provenance != pathv1.ContactProvenanceLegacyProjection || contact.LegacyPauseReason != "performer observed" {
			t.Fatalf("migrated contact = %#v", contact)
		}
		if marker := view.Checkpoint.Execution.Aggregate.SideEffects[contact.ID]; marker.State != pathv1.ContactStateCompleted {
			t.Fatalf("migrated contact marker = %#v", marker)
		}
	}
	if legacyAdapter.reconciliations != 3 || legacyAdapter.nudges != 1 {
		t.Fatalf("projection or v7 resume touched legacy adapter: %#v", legacyAdapter)
	}
	history, err := fs.LoadPathV1RunHistoryView(t.Context(), runID)
	if err != nil || history.LegacyEvidence == nil {
		t.Fatalf("load migrated history view: evidence=%#v err=%v", history.LegacyEvidence, err)
	}
	var historyReadBytes, previousHistoryReadBytes int64
	var previousHistoryReadName string
	restoreHistoryReadHook := fs.SetViewerIOHooksForTest(func(name string, bytesRead int64) {
		if name != previousHistoryReadName || bytesRead <= previousHistoryReadBytes {
			historyReadBytes += bytesRead
		} else {
			historyReadBytes += bytesRead - previousHistoryReadBytes
		}
		previousHistoryReadName, previousHistoryReadBytes = name, bytesRead
	}, nil)
	_, err = fs.LoadPathV1RunHistoryView(t.Context(), runID)
	restoreHistoryReadHook()
	if err != nil || historyReadBytes < 2 {
		t.Fatalf("measure migrated history reads: bytes=%d err=%v", historyReadBytes, err)
	}
	restoreHistoryLimits := fs.SetViewerResourceLimitsForTest(16<<20, historyReadBytes-1, 100_000, 4_096)
	limitedHistory, err := fs.LoadPathV1RunHistoryView(t.Context(), runID)
	restoreHistoryLimits()
	if err != nil || limitedHistory.Checkpoint == nil || limitedHistory.LegacyEvidence != nil ||
		limitedHistory.LegacyEvidenceFailure != store.PathV1LegacyEvidenceResourceLimit {
		t.Fatalf("over-budget migrated history = %#v, err=%v", limitedHistory, err)
	}
	limitedEnvelope, err := processview.BuildCurrentPathV1Envelope(t.Context(), limitedHistory)
	if err != nil || limitedEnvelope.Run.StoredStatus != state.RunStatusCompleted || limitedEnvelope.Run.EffectiveStatus != state.RunStatusCompleted ||
		limitedEnvelope.ViewerV2.ExactTopology == nil || len(limitedEnvelope.Report.Nodes) != 0 || len(limitedEnvelope.Verification.Diagnostics) != 1 {
		t.Fatalf("over-budget migrated viewer = %#v, err=%v", limitedEnvelope, err)
	}
	verification, historical, _ := processverify.PathV1History(t.Context(), history)
	if verification.HasErrors() || historical == nil || historical.State.Status != state.RunStatusRunning {
		t.Fatalf("composed migrated verification = %#v historical=%#v", verification, historical)
	}
	envelope, err := processview.BuildCurrentPathV1Envelope(t.Context(), history)
	if err != nil || envelope.Run.StoredStatus != state.RunStatusCompleted || envelope.Run.EffectiveStatus != state.RunStatusCompleted ||
		envelope.Graph != nil || len(envelope.Report.Nodes) == 0 || envelope.ViewerV2.StateSchemaVersion != pathv1.CheckpointStateSchemaVersion ||
		envelope.ViewerV2.ExactTopology == nil {
		t.Fatalf("composed migrated viewer = %#v, err=%v", envelope, err)
	}
	viewerJSON, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"artifact:migration-failure", strings.Repeat("a", 64), "agent:agt_migrationworker"} {
		if bytes.Contains(viewerJSON, []byte(private)) {
			t.Fatalf("migrated viewer leaked private legacy evidence %q: %s", private, viewerJSON)
		}
	}
	tampered := history
	tampered.LegacyEvidence = &store.PathV1LegacyEvidence{
		Manifest: append([]evidence.ManifestEntry(nil), history.LegacyEvidence.Manifest...),
		NodeLogs: history.LegacyEvidence.NodeLogs,
	}
	tampered.LegacyEvidence.Manifest[0].Checksum = strings.Repeat("b", 64)
	tamperedVerification, tamperedHistorical, _ := processverify.PathV1History(t.Context(), tampered)
	if !tamperedVerification.HasErrors() || tamperedHistorical != nil {
		t.Fatalf("tampered migrated verification = %#v historical=%#v", tamperedVerification, tamperedHistorical)
	}
	tamperedEnvelope, err := processview.BuildCurrentPathV1Envelope(t.Context(), tampered)
	if err != nil || tamperedEnvelope.Run.StoredStatus != state.RunStatusCompleted || tamperedEnvelope.Run.EffectiveStatus != state.RunStatusCompleted ||
		tamperedEnvelope.Graph != nil || len(tamperedEnvelope.Report.Nodes) != 0 || len(tamperedEnvelope.Verification.Diagnostics) != 1 ||
		tamperedEnvelope.Verification.Diagnostics[0].Severity != model.SeverityError {
		t.Fatalf("tampered migrated viewer = %#v, err=%v", tamperedEnvelope, err)
	}
	var replayReadBytes, previousReadBytes int64
	var previousReadName string
	restoreReadHook := fs.SetViewerIOHooksForTest(func(name string, bytesRead int64) {
		if name != previousReadName || bytesRead <= previousReadBytes {
			replayReadBytes += bytesRead
		} else {
			replayReadBytes += bytesRead - previousReadBytes
		}
		previousReadName, previousReadBytes = name, bytesRead
	}, nil)
	replayed, err := fs.InitializePathV1(t.Context(), runID, proof)
	restoreReadHook()
	if err != nil || replayed.Disposition != pathv1.InitializationAlreadyApplied || replayReadBytes < 2 {
		t.Fatalf("measure migrated replay reads = %#v, bytes=%d, err=%v", replayed, replayReadBytes, err)
	}
	restoreLimits := fs.SetViewerResourceLimitsForTest(16<<20, replayReadBytes-1, 100_000, 4_096)
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	restoreLimits()
	var overBudget *store.ExecutionViewOverBudgetError
	if !errors.As(err, &overBudget) || overBudget.Limit != "total_bytes" {
		t.Fatalf("migrated replay aggregate budget = %#v, err=%v", overBudget, err)
	}
	for path, want := range map[string][]byte{
		filepath.Join(root, "runs", runID, "manifest.jsonl"):                manifestBefore,
		filepath.Join(root, "runs", runID, "nodes", "work", "log.jsonl"):    workLogBefore,
		filepath.Join(root, "runs", runID, "nodes", "recover", "log.jsonl"): recoverLogBefore,
	} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("legacy evidence changed at %s: err=%v", path, err)
		}
	}
	manifestPath := filepath.Join(root, "runs", runID, "manifest.jsonl")
	if err := os.WriteFile(manifestPath, append(append([]byte(nil), manifestBefore...), '{'), 0o644); err != nil {
		t.Fatal(err)
	}
	malformedHistory, err := fs.LoadPathV1RunHistoryView(t.Context(), runID)
	if err != nil || malformedHistory.Checkpoint == nil || malformedHistory.LegacyEvidence != nil ||
		malformedHistory.LegacyEvidenceFailure != store.PathV1LegacyEvidenceInvalid {
		t.Fatalf("malformed migrated history = %#v, err=%v", malformedHistory, err)
	}
	malformedEnvelope, err := processview.BuildCurrentPathV1Envelope(t.Context(), malformedHistory)
	if err != nil || malformedEnvelope.Run.StoredStatus != state.RunStatusCompleted || malformedEnvelope.Run.EffectiveStatus != state.RunStatusCompleted ||
		malformedEnvelope.ViewerV2.ExactTopology == nil || len(malformedEnvelope.Report.Nodes) != 0 || len(malformedEnvelope.Verification.Diagnostics) != 1 {
		t.Fatalf("malformed migrated viewer = %#v, err=%v", malformedEnvelope, err)
	}
	if _, err := fs.InitializePathV1(t.Context(), runID, proof); err == nil {
		t.Fatal("initializer replay accepted malformed migrated evidence")
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
	for legacySchema := 1; legacySchema <= pathv1.LegacyMaxSchemaVersion; legacySchema++ {
		t.Run(fmt.Sprintf("schema-%d", legacySchema), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "store")
			fs, err := store.NewFS(root)
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
			runID := fmt.Sprintf("run-v%d-progressed", legacySchema)
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
			progressed, err := fs.LoadRun(t.Context(), runID)
			if err != nil {
				t.Fatal(err)
			}
			progressed.State.StateSchemaVersion = legacySchema
			encoded, err := state.Encode(progressed.State)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), encoded, 0o644); err != nil {
				t.Fatal(err)
			}
			if schema, err := fs.RunStateSchemaVersion(t.Context(), runID); err != nil || schema != legacySchema {
				t.Fatalf("fixture schema = %d, %v", schema, err)
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
			if err != nil || schema <= 0 || schema > pathv1.LegacyMaxSchemaVersion {
				t.Fatalf("progressed run left legacy compatibility: schema = %d, %v", schema, err)
			}
		})
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

func TestExclusiveV7EligibilityAdmitsNoFailTaskButKeepsEndOnlyOnV6(t *testing.T) {
	passOnlyTask := &model.Template{Start: "work", Nodes: map[string]model.Node{
		"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done"}},
		"done": {Type: model.NodeTypeEnd, Result: "completed"},
	}}
	if !exclusiveV7Eligible(passOnlyTask) {
		t.Fatal("exclusive task without a failure edge must use schema-7 terminal-failure parity")
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
