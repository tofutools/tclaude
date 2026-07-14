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

// ExactTemplateSource is the non-recovering immutable-template read used by
// the viewer boundary. It deliberately excludes authoring/head operations.
type ExactTemplateSource interface {
	GetTemplateExact(ctx context.Context, ref string) (*model.Template, error)
}

// SnapshotWithExactPinnedTemplate verifies snapshot and returns only the exact
// immutable template matching TemplateRef. Legacy lookup never substitutes a
// mutable head and never runs attributed-save recovery.
func SnapshotWithExactPinnedTemplate(ctx context.Context, src ExactTemplateSource, snapshot store.Snapshot) (Report, *model.Template, error) {
	tmpl := snapshot.Run.Template
	if tmpl != nil {
		semanticHash, hashErr := model.SemanticHash(tmpl)
		wantRef := model.TemplateRef(tmpl.ID, semanticHash)
		if hashErr != nil || strings.TrimSpace(wantRef) == "" || wantRef != snapshot.Run.TemplateRef {
			report := Snapshot(snapshot)
			report.Diagnostics = append(report.Diagnostics, Diagnostic{
				Layer: LayerLoad, Severity: model.SeverityError, Code: "embedded_template_mismatch", Path: "run.template",
				Message: "embedded run template does not match its pinned ref",
			})
			sortReportDiagnostics(report.Diagnostics)
			report.EffectiveStatus = state.RunStatusInconsistent
			return report, nil, nil
		}
		return SnapshotWithTemplate(snapshot, tmpl), tmpl, nil
	}
	resolved, err := src.GetTemplateExact(ctx, snapshot.Run.TemplateRef)
	if err != nil {
		dataFailure := errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrTemplateSavePending) || errors.Is(err, store.ErrContentMismatch)
		if !dataFailure {
			return Report{}, nil, err
		}
		report := Snapshot(snapshot)
		code := "template_unavailable"
		message := "the exact pinned template is unavailable"
		if errors.Is(err, store.ErrContentMismatch) {
			code = "pinned_template_mismatch"
			message = "stored template content does not match its pinned ref"
		}
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Layer: LayerLoad, Severity: model.SeverityError, Code: code, Path: "run.templateRef", Message: message,
		})
		sortReportDiagnostics(report.Diagnostics)
		report.EffectiveStatus = state.RunStatusInconsistent
		return report, nil, nil
	}
	semanticHash, hashErr := model.SemanticHash(resolved)
	if wantRef := model.TemplateRef(resolved.ID, semanticHash); hashErr != nil || strings.TrimSpace(wantRef) == "" || wantRef != snapshot.Run.TemplateRef {
		report := Snapshot(snapshot)
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Layer: LayerLoad, Severity: model.SeverityError, Code: "pinned_template_mismatch", Path: "run.templateRef",
			Message: "stored template content does not match its pinned ref",
		})
		sortReportDiagnostics(report.Diagnostics)
		report.EffectiveStatus = state.RunStatusInconsistent
		return report, nil, nil
	}
	return SnapshotWithTemplate(snapshot, resolved), resolved, nil
}
