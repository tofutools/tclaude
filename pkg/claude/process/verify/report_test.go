package verify_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
	"github.com/tofutools/tclaude/pkg/claude/process/verify"
)

func TestSnapshotReportsCleanRun(t *testing.T) {
	fixture := storetest.BuildInitializedFixture(t)
	entry := storetest.LogEntry(fixture.RunID, "implement", 0)
	if _, err := fixture.Store.Append(t.Context(), fixture.RunID, 0, []evidence.LogEntry{entry}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if err != nil {
		t.Fatal(err)
	}

	report := verify.Snapshot(snapshot)
	if report.HasErrors() {
		t.Fatalf("unexpected diagnostics: %#v", report.Diagnostics)
	}
	if report.EffectiveStatus != snapshot.State.Status {
		t.Fatalf("effective status = %q, want %q", report.EffectiveStatus, snapshot.State.Status)
	}
	if report.Dirty {
		t.Fatal("clean snapshot should not be dirty")
	}
}

func TestSnapshotReportsEvidenceCrash(t *testing.T) {
	fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)

	if !report.HasErrors() {
		t.Fatal("expected errors")
	}
	if report.EffectiveStatus != state.RunStatusInconsistent {
		t.Fatalf("effective status = %q", report.EffectiveStatus)
	}
	if !hasDiagnostic(report, verify.LayerEvidence, "state_behind_manifest") {
		t.Fatalf("expected state_behind_manifest, got %#v", report.Diagnostics)
	}
	if report.Dirty {
		t.Fatal("evidence crash should be inconsistent, not dirty")
	}
}

func TestStoreRunReportsTornTailReadError(t *testing.T) {
	fixture := storetest.BuildCrashFixture(t, storetest.CrashTornFinalLogLine)
	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)

	if !hasDiagnosticAt(report, verify.LayerEvidence, "read_torn_tail", "nodes/implement/log.jsonl:line.1") {
		t.Fatalf("expected read_torn_tail, got %#v", report.Diagnostics)
	}
}

func TestStoreRunReportsManifestTornTailPath(t *testing.T) {
	fixture := storetest.BuildInitializedFixture(t)
	manifestPath := filepath.Join(fixture.Root, "runs", fixture.RunID, "manifest.jsonl")
	if err := os.WriteFile(manifestPath, []byte(`{"schemaVersion":1,"seq":1`), 0o644); err != nil {
		t.Fatal(err)
	}

	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)
	if !hasDiagnosticAt(report, verify.LayerEvidence, "read_torn_tail", "manifest.jsonl:line.1") {
		t.Fatalf("expected manifest read_torn_tail, got %#v", report.Diagnostics)
	}
}

func TestSnapshotReportsSemanticDirtyState(t *testing.T) {
	fixture := storetest.BuildSemanticViolationFixture(t)
	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)

	if !hasDiagnostic(report, verify.LayerSemantic, "running_attempt_without_command_or_actor") {
		t.Fatalf("expected semantic diagnostic, got %#v", report.Diagnostics)
	}
	if report.EffectiveStatus != state.RunStatusDirty {
		t.Fatalf("effective status = %q", report.EffectiveStatus)
	}
	if !report.Dirty {
		t.Fatal("semantic violation with matching evidence anchors should be dirty")
	}
}

func TestSnapshotRejectsEscalationDecisionThatBypassedResolution(t *testing.T) {
	tmpl := &model.Template{Nodes: map[string]model.Node{
		"implement": {
			Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"},
			Checks: []model.Step{{ID: "tests", Performer: model.Performer{Kind: model.PerformerProgram, Run: "true"}}},
			Next:   model.Next{"fail": "escalate"},
		},
		"escalate": {
			Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "retry?"},
			Next: model.Next{"retry": "implement", "cancel": "canceled"},
		},
		"canceled": {Type: model.NodeTypeEnd, Result: "canceled"},
	}}
	st := &state.State{
		StateSchemaVersion: state.StateSchemaVersion, RunID: "run", Status: state.RunStatusCanceled,
		Nodes: map[string]state.NodeState{
			"implement": {
				Type: model.NodeTypeTask, Status: state.NodeStatusBlocked, BlockedAttempt: 2, BlockedNodeID: "implement.test.tests",
			},
			"escalate": {
				Type: model.NodeTypeDecision, Status: state.NodeStatusCompleted, Attempt: 2, PoisonedNodeID: "implement.test.tests",
				ChosenEdge: "cancel", Decisions: []state.DecisionRecord{{Actor: "human:johan", Verdict: "cancel", EvidenceRef: "human-message:42", Timestamp: time.Now()}},
			},
			"canceled": {Type: model.NodeTypeEnd, Status: state.NodeStatusCompleted},
		},
	}
	report := verify.SnapshotWithTemplate(store.Snapshot{Run: store.RunRecord{ID: "run"}, State: st}, tmpl)
	if !hasDiagnostic(report, verify.LayerSemantic, "escalation_decision_bypassed_resolution") {
		t.Fatalf("manual decision-edge bypass verified cleanly: %#v", report.Diagnostics)
	}
}

func TestStoreRunReportsInvalidTamperedEnums(t *testing.T) {
	fixture := storetest.BuildInitializedFixture(t)
	snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.State.Status = "bogus_run_status"
	node := snapshot.State.Nodes["implement"]
	node.Status = "bogus_node_status"
	snapshot.State.Nodes["implement"] = node
	data, err := state.Encode(snapshot.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.Root, "runs", fixture.RunID, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)
	if !hasDiagnostic(report, verify.LayerSemantic, "invalid_run_status") {
		t.Fatalf("expected invalid_run_status, got %#v", report.Diagnostics)
	}
	if !hasDiagnostic(report, verify.LayerSemantic, "invalid_node_status") {
		t.Fatalf("expected invalid_node_status, got %#v", report.Diagnostics)
	}
	if report.EffectiveStatus != state.RunStatusDirty {
		t.Fatalf("effective status = %q", report.EffectiveStatus)
	}
}

func TestStoreRunRejectsTamperedEmbeddedTemplate(t *testing.T) {
	fixture := storetest.BuildInitializedFixture(t)
	snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Template == nil {
		t.Fatal("new run did not pin its template")
	}
	snapshot.Run.Template.Name = "tampered after instantiation"
	data, err := json.MarshalIndent(snapshot.Run, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(fixture.Root, "runs", fixture.RunID, "run.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)
	if !hasDiagnostic(report, verify.LayerLoad, "embedded_template_mismatch") || report.EffectiveStatus != state.RunStatusInconsistent {
		t.Fatalf("tampered embedded template report = %#v", report)
	}
	tampered, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, resolved := verify.SnapshotWithPinnedTemplate(t.Context(), fixture.Store, tampered)
	if resolved != nil {
		t.Fatal("a mismatched embedded template must not be returned for graph rendering")
	}
}

func TestStoreRunLegacyRecordFallsBackToTemplateLibrary(t *testing.T) {
	fixture := storetest.BuildInitializedFixture(t)
	snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Run.Template = nil
	data, err := json.MarshalIndent(snapshot.Run, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(fixture.Root, "runs", fixture.RunID, "run.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)
	if report.HasErrors() {
		t.Fatalf("legacy template fallback diagnostics = %#v", report.Diagnostics)
	}
	legacy, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, resolved := verify.SnapshotWithPinnedTemplate(t.Context(), fixture.Store, legacy)
	if resolved == nil {
		t.Fatal("legacy run did not resolve its exact pinned template")
	}
	hash, err := model.SemanticHash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.TemplateRef(resolved.ID, hash); got != legacy.Run.TemplateRef {
		t.Fatalf("resolved ref = %q, want %q", got, legacy.Run.TemplateRef)
	}
}

func hasDiagnostic(report verify.Report, layer verify.Layer, code string) bool {
	return hasDiagnosticAt(report, layer, code, "")
}

func hasDiagnosticAt(report verify.Report, layer verify.Layer, code, path string) bool {
	for _, diag := range report.Diagnostics {
		if diag.Layer == layer && diag.Severity == model.SeverityError && diag.Code == code && (path == "" || diag.Path == path) {
			return true
		}
	}
	return false
}
