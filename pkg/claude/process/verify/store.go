package verify

import (
	"context"
	"fmt"
	"strings"

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
	tmpl := snapshot.Run.Template
	if tmpl != nil {
		semanticHash, hashErr := model.SemanticHash(tmpl)
		wantRef := model.TemplateRef(tmpl.ID, semanticHash)
		if hashErr != nil || strings.TrimSpace(wantRef) == "" || wantRef != snapshot.Run.TemplateRef {
			report := Snapshot(snapshot)
			message := fmt.Sprintf("embedded run template does not match pinned ref %q", snapshot.Run.TemplateRef)
			if hashErr != nil {
				message += ": " + hashErr.Error()
			}
			report.Diagnostics = append(report.Diagnostics, Diagnostic{
				Layer: LayerLoad, Severity: model.SeverityError, Code: "embedded_template_mismatch", Path: "run.template", Message: message,
			})
			sortReportDiagnostics(report.Diagnostics)
			report.EffectiveStatus = state.RunStatusInconsistent
			return report
		}
		return SnapshotWithTemplate(snapshot, tmpl)
	}
	tmpl, err = src.GetTemplate(ctx, snapshot.Run.TemplateRef)
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
