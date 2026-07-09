package verify_test

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
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

	if !hasDiagnostic(report, verify.LayerEvidence, "read_torn_tail") {
		t.Fatalf("expected read_torn_tail, got %#v", report.Diagnostics)
	}
}

func TestSnapshotReportsSemanticDirtyState(t *testing.T) {
	fixture := storetest.BuildSemanticViolationFixture(t)
	report := verify.StoreRun(t.Context(), fixture.Store, fixture.RunID)

	if !hasDiagnostic(report, verify.LayerSemantic, "running_attempt_without_command_or_actor") {
		t.Fatalf("expected semantic diagnostic, got %#v", report.Diagnostics)
	}
	if report.EffectiveStatus != state.RunStatusInconsistent {
		t.Fatalf("effective status = %q", report.EffectiveStatus)
	}
	if !report.Dirty {
		t.Fatal("semantic violation with matching evidence anchors should be dirty")
	}
}

func hasDiagnostic(report verify.Report, layer verify.Layer, code string) bool {
	for _, diag := range report.Diagnostics {
		if diag.Layer == layer && diag.Severity == model.SeverityError && diag.Code == code {
			return true
		}
	}
	return false
}
