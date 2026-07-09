package verify

import (
	"context"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// RunSource is the store surface StoreRun needs: the run snapshot plus the
// pinned template for template-aware invariants.
type RunSource interface {
	store.Runs
	store.Templates
}

func StoreRun(ctx context.Context, src RunSource, runID string) Report {
	snapshot, err := src.LoadRun(ctx, runID)
	if err != nil {
		return LoadError(runID, err)
	}
	tmpl, err := src.GetTemplate(ctx, snapshot.Run.TemplateRef)
	if err != nil {
		report := Snapshot(snapshot)
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Layer:    LayerLoad,
			Severity: model.SeverityError,
			Code:     "template_unavailable",
			Path:     "run.templateRef",
			Message:  fmt.Sprintf("could not load pinned template %q: %v; template-aware invariants were not checked", snapshot.Run.TemplateRef, err),
		})
		sortReportDiagnostics(report.Diagnostics)
		report.EffectiveStatus = state.RunStatusInconsistent
		return report
	}
	return SnapshotWithTemplate(snapshot, tmpl)
}
