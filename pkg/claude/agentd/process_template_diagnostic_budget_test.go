package agentd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestDiagnosticsForEditorHasDefensiveWireLimit(t *testing.T) {
	diagnostics := model.Diagnostics{{
		Severity: model.SeverityError,
		Code:     "oversized",
		Path:     "nodes." + strings.Repeat("<", model.MaxTemplateDiagnosticWireBytes),
		Message:  strings.Repeat("<", model.MaxTemplateDiagnosticWireBytes),
	}}
	result := diagnosticsForEditor(diagnostics, nil)
	require.Len(t, result, 1)
	assert.Equal(t, model.DiagnosticCodeDiagnosticBudget, result[0].Code)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Less(t, len(encoded), 1024)
}
