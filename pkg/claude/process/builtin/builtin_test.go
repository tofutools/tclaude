//ci:reads docs/examples/ways-of-working.yaml

package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestEveryBuiltinValidatesWithoutWarnings(t *testing.T) {
	all, err := List()
	require.NoError(t, err)
	require.Len(t, all, 7)
	for _, b := range all {
		assert.Equal(t, b.ID, b.Parsed.Template.ID, "the file is named after its id")
		assert.NotEmpty(t, b.Name)
		assert.NotEmpty(t, b.Description)
		for _, d := range b.Parsed.Diagnostics {
			assert.NotEqual(t, model.SeverityError, d.Severity, "%s: %+v", b.ID, d)
			assert.NotEqual(t, model.SeverityWarning, d.Severity, "%s: %+v", b.ID, d)
		}
		assert.Equal(t, "lead", b.Parsed.Template.Overseer, "%s: the group's lead oversees it", b.ID)
		for id, node := range b.Parsed.Template.Nodes {
			if node.Type == model.NodeTypeEnd && node.Result == "canceled" {
				assert.Equal(t, "ask-me-first", b.ID, "%s: %s: escalation fails a run; only your own stop cancels one", b.ID, id)
			}
		}
		_, required := b.Parsed.Template.Params["task"]
		assert.True(t, required, "%s takes the task it is run for ({{params.task}})", b.ID)
	}
	_, err = Get("nope")
	assert.ErrorIs(t, err, ErrNotFound)
}

// The operator's 8-step example in the docs is the built-in plan-vet-build,
// under another id and name.
func TestPlanVetBuildIsTheDocsExample(t *testing.T) {
	b, err := Get("plan-vet-build")
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "examples", "ways-of-working.yaml"))
	require.NoError(t, err)
	docs, err := model.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, docs.Template.Nodes, b.Parsed.Template.Nodes)
	assert.Equal(t, docs.Template.Params, b.Parsed.Template.Params)
	assert.Equal(t, docs.Template.Overseer, b.Parsed.Template.Overseer)
	assert.Equal(t, docs.Template.Patience, b.Parsed.Template.Patience)
}
