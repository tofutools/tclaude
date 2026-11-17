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
	requireEditCasesRunnable(t, "jstest/process-mini-edit-cases.json")
}

// TestReferenceProcessesAreRunnable: the operator's two reference
// implementation processes, as jstest/process-reference-recipes.test.mjs
// builds them with the inline editor's operations, save without an error and
// execute (tk_c324d5a4). process_reference_recipes_flow_test.go runs them.
func TestReferenceProcessesAreRunnable(t *testing.T) {
	requireEditCasesRunnable(t, "jstest/process-reference-recipes.json")
}

func requireEditCasesRunnable(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
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
