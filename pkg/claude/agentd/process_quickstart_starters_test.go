package agentd

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
)

// TestQuickStartStartersAreRunnable keeps the Groups › Processes quick-start
// starters (process-quickstart.js) honest: each one, as the dashboard builds
// it for a fixed roster, must parse with no diagnostics and be executable by
// the engine. jstest/process-quickstart.test.mjs asserts the dashboard still
// builds exactly this fixture.
func TestQuickStartStartersAreRunnable(t *testing.T) {
	raw, err := os.ReadFile("jstest/process-quickstart-starters.json")
	require.NoError(t, err)
	var fixture struct {
		Starters map[string]*processTemplateEditView `json:"starters"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Starters)
	keys := make([]string, 0, len(fixture.Starters))
	for key := range fixture.Starters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			view := fixture.Starters[key]
			view.Template.ID = key // the store mints the id on save
			parsed, err := parseProcessEditView(view)
			require.NoError(t, err)
			require.Empty(t, parsed.Diagnostics, "a starter must arrive valid")
			params := map[string]string{}
			for name := range parsed.Template.Params {
				params[name] = "x"
			}
			_, err = engine.Prepare(parsed.Template, params)
			require.NoError(t, err, "a starter must be executable as built")
		})
	}
}
