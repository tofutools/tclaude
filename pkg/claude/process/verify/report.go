package verify

import (
	"errors"
	"fmt"
	"sort"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

type Layer string

const (
	LayerLoad     Layer = "load"
	LayerEvidence Layer = "evidence"
	LayerSemantic Layer = "semantic"
)

type Diagnostic struct {
	Layer    Layer          `json:"layer"`
	Severity model.Severity `json:"severity"`
	Code     string         `json:"code"`
	Path     string         `json:"path,omitempty"`
	Message  string         `json:"message"`
}

type Report struct {
	RunID           string          `json:"runId,omitempty"`
	StoredStatus    state.RunStatus `json:"storedStatus,omitempty"`
	EffectiveStatus state.RunStatus `json:"effectiveStatus"`
	Dirty           bool            `json:"dirty,omitempty"`
	Diagnostics     []Diagnostic    `json:"diagnostics,omitempty"`
}

func (r Report) HasErrors() bool {
	for _, diag := range r.Diagnostics {
		if diag.Severity == model.SeverityError {
			return true
		}
	}
	return false
}

func (r Report) DiagnosticsForLayer(layer Layer) []Diagnostic {
	out := make([]Diagnostic, 0, len(r.Diagnostics))
	for _, diag := range r.Diagnostics {
		if diag.Layer == layer {
			out = append(out, diag)
		}
	}
	return out
}

func Snapshot(snapshot store.Snapshot) Report {
	return SnapshotWithTemplate(snapshot, nil)
}

// SnapshotWithTemplate verifies a run snapshot including template-aware
// invariants (recorded compound expansions must match what the pinned
// template derives). A nil template skips only the template-aware layer.
func SnapshotWithTemplate(snapshot store.Snapshot, tmpl *model.Template) Report {
	report := Report{RunID: snapshot.Run.ID}
	if snapshot.State != nil {
		report.StoredStatus = snapshot.State.Status
		report.EffectiveStatus = snapshot.State.Status
	}
	if report.EffectiveStatus == "" {
		report.EffectiveStatus = state.RunStatusInconsistent
	}

	evidenceDiagnostics := append(
		evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs),
		evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest)...,
	)
	report.Diagnostics = appendDiagnostics(report.Diagnostics, LayerEvidence, evidenceDiagnostics)

	semanticDiagnostics := state.CheckInvariants(snapshot.State)
	if tmpl != nil {
		semanticDiagnostics = append(semanticDiagnostics, state.CheckTemplateInvariants(snapshot.State, tmpl)...)
	}
	report.Diagnostics = appendDiagnostics(report.Diagnostics, LayerSemantic, semanticDiagnostics)

	sortReportDiagnostics(report.Diagnostics)
	if !evidenceDiagnostics.HasErrors() && semanticDiagnostics.HasErrors() {
		report.Dirty = true
	}
	if report.HasErrors() {
		report.EffectiveStatus = state.RunStatusInconsistent
		if report.Dirty {
			report.EffectiveStatus = state.RunStatusDirty
		}
	}
	return report
}

func LoadError(runID string, err error) Report {
	report := Report{
		RunID:           runID,
		EffectiveStatus: state.RunStatusInconsistent,
	}
	if err == nil {
		return report
	}
	report.Diagnostics = append(report.Diagnostics, diagnosticForLoadError(err))
	return report
}

func appendDiagnostics(dst []Diagnostic, layer Layer, diagnostics model.Diagnostics) []Diagnostic {
	for _, diag := range diagnostics {
		message := diag.Message
		if layer == LayerEvidence {
			message = explainEvidenceDiagnostic(diag)
		}
		dst = append(dst, Diagnostic{
			Layer:    layer,
			Severity: diag.Severity,
			Code:     diag.Code,
			Path:     diag.Path,
			Message:  message,
		})
	}
	return dst
}

func diagnosticForLoadError(err error) Diagnostic {
	var readErr *evidence.ReadError
	if errors.As(err, &readErr) {
		switch readErr.Kind {
		case evidence.ReadErrorTornTail:
			return Diagnostic{
				Layer:    LayerEvidence,
				Severity: model.SeverityError,
				Code:     "read_torn_tail",
				Path:     readErrorPath(readErr),
				Message:  fmt.Sprintf("evidence JSONL final line is torn at line %d: %s; likely crash or truncation during append, repair required", readErr.Line, readErrorCause(readErr)),
			}
		case evidence.ReadErrorMalformed:
			return Diagnostic{
				Layer:    LayerEvidence,
				Severity: model.SeverityError,
				Code:     "read_malformed",
				Path:     readErrorPath(readErr),
				Message:  fmt.Sprintf("evidence JSONL is malformed at line %d: %s; file content is corrupt or was edited, repair required", readErr.Line, readErrorCause(readErr)),
			}
		default:
			return Diagnostic{
				Layer:    LayerEvidence,
				Severity: model.SeverityError,
				Code:     "read_error",
				Path:     readErrorPath(readErr),
				Message:  fmt.Sprintf("evidence JSONL read error at line %d: %v", readErr.Line, err),
			}
		}
	}
	return Diagnostic{
		Layer:    LayerLoad,
		Severity: model.SeverityError,
		Code:     "load_error",
		Message:  fmt.Sprintf("could not load run snapshot: %v; this is a store/load error, not evidence corruption", err),
	}
}

func readErrorPath(readErr *evidence.ReadError) string {
	if readErr == nil {
		return "jsonl.line.0"
	}
	line := fmt.Sprintf("line.%d", readErr.Line)
	if readErr.File != "" {
		return readErr.File + ":" + line
	}
	return "jsonl." + line
}

func readErrorCause(readErr *evidence.ReadError) string {
	if readErr == nil || readErr.Err == nil {
		return "unknown read error"
	}
	return readErr.Err.Error()
}

func explainEvidenceDiagnostic(diag model.Diagnostic) string {
	switch diag.Code {
	case "state_behind_manifest":
		return diag.Message + "; crash between manifest append and state checkpoint write, repair can replay manifest-backed entries"
	case "state_ahead_of_manifest":
		return diag.Message + "; state checkpoint references evidence that is not in the manifest, manual repair required"
	case "state_checksum_mismatch":
		return diag.Message + "; state anchor does not match manifest head, manual repair required"
	case "log_ahead_of_manifest":
		return diag.Message + "; crash between node log append and manifest append, repair must index or truncate the log tail after review"
	case "manifest_ahead_of_log":
		return diag.Message + "; manifest references an event missing from its owning log, manual repair required"
	default:
		return diag.Message
	}
}

func sortReportDiagnostics(diagnostics []Diagnostic) {
	layerOrder := map[Layer]int{
		LayerLoad:     0,
		LayerEvidence: 1,
		LayerSemantic: 2,
	}
	sort.SliceStable(diagnostics, func(i, j int) bool {
		leftLayer, rightLayer := layerOrder[diagnostics[i].Layer], layerOrder[diagnostics[j].Layer]
		if leftLayer != rightLayer {
			return leftLayer < rightLayer
		}
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		if diagnostics[i].Path != diagnostics[j].Path {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
}
