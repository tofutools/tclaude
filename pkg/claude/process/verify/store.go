package verify

import (
	"context"
	"errors"
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
	report, _ := SnapshotWithPinnedTemplate(ctx, src, snapshot)
	return report
}

// SnapshotWithPinnedTemplate verifies snapshot and returns the exact template
// whose semantic identity matches snapshot.Run.TemplateRef. Legacy records
// may resolve that exact ref from src; this function never substitutes a
// mutable template head. A nil returned template means callers must not render
// a healthy graph from the run record.
func SnapshotWithPinnedTemplate(ctx context.Context, src store.Templates, snapshot store.Snapshot) (Report, *model.Template) {
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
			return report, nil
		}
		return SnapshotWithTemplate(snapshot, tmpl), tmpl
	}
	resolved, err := src.GetTemplate(ctx, snapshot.Run.TemplateRef)
	if err != nil {
		report := Snapshot(snapshot)
		code := "template_unavailable"
		message := fmt.Sprintf("could not load exact pinned template %q; template-aware invariants were not checked", snapshot.Run.TemplateRef)
		if errors.Is(err, store.ErrContentMismatch) {
			code = "pinned_template_mismatch"
			message = fmt.Sprintf("stored template content does not match pinned ref %q; template-aware invariants were not checked", snapshot.Run.TemplateRef)
		}
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Layer:    LayerLoad,
			Severity: model.SeverityError,
			Code:     code,
			Path:     "run.templateRef",
			Message:  message,
		})
		sortReportDiagnostics(report.Diagnostics)
		report.EffectiveStatus = state.RunStatusInconsistent
		return report, nil
	}
	semanticHash, hashErr := model.SemanticHash(resolved)
	if wantRef := model.TemplateRef(resolved.ID, semanticHash); hashErr != nil || strings.TrimSpace(wantRef) == "" || wantRef != snapshot.Run.TemplateRef {
		report := Snapshot(snapshot)
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Layer: LayerLoad, Severity: model.SeverityError, Code: "pinned_template_mismatch", Path: "run.templateRef",
			Message: fmt.Sprintf("stored template content does not match pinned ref %q; template-aware invariants were not checked", snapshot.Run.TemplateRef),
		})
		sortReportDiagnostics(report.Diagnostics)
		report.EffectiveStatus = state.RunStatusInconsistent
		return report, nil
	}
	return SnapshotWithTemplate(snapshot, resolved), resolved
}
