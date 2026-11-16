package agentd

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/builtin"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const processBuiltinViewsFixture = "jstest/process-builtin-views.json"

// builtinFixtureView is the part of a built-in's edit view the step list
// reads: what the dashboard gets from GET /v1/process/builtins/{id}.
type builtinFixtureView struct {
	Template *model.Template `json:"template"`
	Edges    []model.Edge    `json:"edges"`
	Layout   *model.Layout   `json:"layout,omitempty"`
}

// TestProcessBuiltinViewsFixture keeps jstest/process-builtin-views.json, the
// built-ins as the editor receives them, in step with the built-ins. The
// step-list jstest round-trips each one (graph → ☰ Steps → graph,
// tk_bd629fb5). Regenerate with UPDATE_PROCESS_BUILTIN_VIEWS=1.
func TestProcessBuiltinViewsFixture(t *testing.T) {
	views := map[string]builtinFixtureView{}
	list, err := builtin.List()
	require.NoError(t, err)
	for i := range list {
		b := &list[i]
		v := processBuiltinView(b)
		views[b.ID] = builtinFixtureView{Template: v.Template, Edges: v.Edges, Layout: v.Layout}
	}
	want, err := json.MarshalIndent(map[string]any{"builtins": views}, "", "  ")
	require.NoError(t, err)
	want = append(want, '\n')
	if os.Getenv("UPDATE_PROCESS_BUILTIN_VIEWS") != "" {
		require.NoError(t, os.WriteFile(processBuiltinViewsFixture, want, 0o644))
	}
	got, err := os.ReadFile(processBuiltinViewsFixture)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got),
		"the built-ins changed: regenerate %s with UPDATE_PROCESS_BUILTIN_VIEWS=1 go test ./pkg/claude/agentd/ -run TestProcessBuiltinViewsFixture", processBuiltinViewsFixture)
}

// TestProcessBuiltinViewsSaveOrderFree is the other half of the step list's
// round trip: the jstest shows graph → steps → graph gives back the same
// nodes and edges, possibly in another order, and here a view's saved YAML
// does not depend on that order. Together: the YAML is identical.
func TestProcessBuiltinViewsSaveOrderFree(t *testing.T) {
	raw, err := os.ReadFile(processBuiltinViewsFixture)
	require.NoError(t, err)
	var fixture struct {
		Builtins map[string]json.RawMessage `json:"builtins"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Builtins)
	canonical := func(data []byte, reverse bool) string {
		var view processTemplateEditView
		require.NoError(t, json.Unmarshal(data, &view))
		view.layoutPresent = true
		if reverse {
			slices.Reverse(view.Edges)
		}
		parsed, err := parseProcessEditView(&view)
		require.NoError(t, err)
		require.Empty(t, parsed.Diagnostics)
		out, err := model.CanonicalYAML(parsed.Template)
		require.NoError(t, err)
		return string(out)
	}
	for id, data := range fixture.Builtins {
		t.Run(id, func(t *testing.T) {
			b, err := builtin.Get(id)
			require.NoError(t, err)
			source, err := model.CanonicalYAML(b.Parsed.Template)
			require.NoError(t, err)
			require.Equal(t, string(source), canonical(data, false), "the fixture view saves as the built-in")
			require.Equal(t, string(source), canonical(data, true), "edge order changes the saved YAML")
		})
	}
}
