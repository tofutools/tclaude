package processcmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// TestPrintDiagnosticsRendersPositions verifies source positions render as
// path:line:col when present, and are omitted when the diagnostic carries none.
func TestPrintDiagnosticsRendersPositions(t *testing.T) {
	var out bytes.Buffer
	printDiagnostics(&out, model.Diagnostics{
		{Severity: model.SeverityError, Code: "unknown_field", Path: "nodes.a.bogus", Message: "unknown field \"bogus\"", Line: 9, Col: 5},
		{Severity: model.SeverityError, Code: "invalid_duration", Path: "nodes.a.wait.duration", Message: "must be a positive duration"},
		{Severity: model.SeverityError, Code: "nil_template", Path: "", Message: "process template is nil"},
	})
	got := out.String()

	if !strings.Contains(got, "nodes.a.bogus:9:5:") {
		t.Fatalf("expected positioned path nodes.a.bogus:9:5, got:\n%s", got)
	}
	if strings.Contains(got, "nodes.a.wait.duration:") && strings.Contains(got, "wait.duration:0:0") {
		t.Fatalf("zero position should not render, got:\n%s", got)
	}
	if !strings.Contains(got, "nodes.a.wait.duration: must be a positive duration") {
		t.Fatalf("unpositioned diagnostic should print bare path, got:\n%s", got)
	}
	if !strings.Contains(got, "] nil_template -: process template is nil") {
		t.Fatalf("empty path should render as -, got:\n%s", got)
	}
}
