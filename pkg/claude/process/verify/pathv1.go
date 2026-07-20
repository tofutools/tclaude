package verify

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// PathV1History verifies current schema-7 authority and, only for a migrated
// checkpoint, its bounded legacy evidence anchor. The historical snapshot and
// exact template are report inputs; callers must keep graph, status, and live
// routing authority on snapshot.Checkpoint.
func PathV1History(ctx context.Context, snapshot store.PathV1RunSnapshot) (Report, *store.Snapshot, *model.Template) {
	report := Report{RunID: snapshot.Run.ID, EffectiveStatus: state.RunStatusInconsistent}
	if snapshot.Checkpoint == nil {
		report.Diagnostics = []Diagnostic{{
			Layer: LayerSemantic, Severity: model.SeverityError, Code: "path_v1_invalid",
			Message: "schema-7 checkpoint is missing",
		}}
		return report, nil, nil
	}
	if _, err := pathv1.VerifyExecutionInput(ctx, snapshot.CheckpointJSON, snapshot.TemplateSource); err != nil {
		report.Diagnostics = []Diagnostic{{
			Layer: LayerSemantic, Severity: model.SeverityError, Code: "path_v1_invalid",
			Message: "schema-7 checkpoint or exact template authority is invalid",
		}}
		return report, nil, nil
	}
	status := state.RunStatus(pathv1.CurrentRunStatus(snapshot.Checkpoint))
	if !status.IsValid() {
		report.Diagnostics = []Diagnostic{{
			Layer: LayerSemantic, Severity: model.SeverityError, Code: "path_v1_invalid",
			Message: "schema-7 checkpoint status is invalid",
		}}
		return report, nil, nil
	}
	report.StoredStatus, report.EffectiveStatus = status, status
	parsed, err := model.ParseExactSource(snapshot.TemplateSource)
	if err != nil || parsed.Template == nil || parsed.Ref != snapshot.Run.TemplateRef {
		report.EffectiveStatus = state.RunStatusInconsistent
		report.Diagnostics = []Diagnostic{{
			Layer: LayerSemantic, Severity: model.SeverityError, Code: "path_v1_invalid",
			Message: "schema-7 exact template authority is invalid",
		}}
		return report, nil, nil
	}
	if snapshot.Checkpoint == nil || snapshot.Checkpoint.Execution == nil || snapshot.Checkpoint.Execution.LegacyProjection == nil {
		return report, nil, parsed.Template
	}
	if snapshot.LegacyEvidenceFailure != "" {
		code, message := "legacy_projection_evidence_unavailable", "migrated legacy evidence is unavailable"
		switch snapshot.LegacyEvidenceFailure {
		case store.PathV1LegacyEvidenceInvalid:
			code, message = "legacy_projection_evidence_invalid", "migrated legacy evidence contains an invalid record"
		case store.PathV1LegacyEvidenceResourceLimit:
			code, message = "legacy_projection_evidence_resource_limit", "migrated legacy evidence exceeds the verification resource limit"
		}
		report.EffectiveStatus = state.RunStatusInconsistent
		report.Diagnostics = []Diagnostic{{
			Layer: LayerEvidence, Severity: model.SeverityError, Code: code, Message: message,
		}}
		return report, nil, parsed.Template
	}
	if snapshot.LegacyEvidence == nil {
		report.EffectiveStatus = state.RunStatusInconsistent
		report.Diagnostics = []Diagnostic{{
			Layer: LayerEvidence, Severity: model.SeverityError, Code: "legacy_projection_evidence_missing",
			Message: "migrated legacy evidence was not loaded",
		}}
		return report, nil, parsed.Template
	}
	legacyState, err := pathv1.VerifyMigratedLegacyEvidence(
		ctx, snapshot.Checkpoint, parsed.Template,
		snapshot.LegacyEvidence.Manifest, snapshot.LegacyEvidence.NodeLogs,
	)
	if err != nil {
		report.EffectiveStatus = state.RunStatusInconsistent
		report.Diagnostics = []Diagnostic{{
			Layer: LayerEvidence, Severity: model.SeverityError, Code: "legacy_projection_invalid",
			Message: "migrated legacy evidence differs from its retained projection anchor",
		}}
		return report, nil, parsed.Template
	}
	historical := &store.Snapshot{
		Run: snapshot.Run, State: legacyState,
		Manifest: snapshot.LegacyEvidence.Manifest, NodeLogs: snapshot.LegacyEvidence.NodeLogs,
	}
	return report, historical, parsed.Template
}
