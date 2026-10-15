package agentd

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// TestMiniEditCasesAreRunnable keeps the Groups › Processes inline mini
// editor's graph operations (process-mini-edit.js) honest: each operation, as
// jstest/process-mini-edit.test.mjs asserts the dashboard performs it on a
// quick-start starter, must leave a template with no error diagnostics that
// the engine can execute.
func TestMiniEditCasesAreRunnable(t *testing.T) {
	raw, err := os.ReadFile("jstest/process-mini-edit-cases.json")
	require.NoError(t, err)
	var fixture struct {
		Cases map[string]*processTemplateEditView `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Cases)
	keys := make([]string, 0, len(fixture.Cases))
	for key := range fixture.Cases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			view := fixture.Cases[key]
			view.Template.ID = key
			parsed, err := parseProcessEditView(view)
			require.NoError(t, err)
			for _, diagnostic := range parsed.Diagnostics {
				require.NotEqual(t, model.SeverityError, diagnostic.Severity, "%s: %s", diagnostic.Path, diagnostic.Message)
			}
			params := map[string]string{}
			for name := range parsed.Template.Params {
				params[name] = "x"
			}
			_, err = engine.Prepare(parsed.Template, params)
			require.NoError(t, err, "an edited process must stay executable")
		})
	}
}
