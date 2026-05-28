package workflow

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tmplFS(workflowYAML, mmd string, nodes map[string]string) fstest.MapFS {
	m := fstest.MapFS{
		"workflow.yaml": &fstest.MapFile{Data: []byte(workflowYAML)},
		"flow.mmd":      &fstest.MapFile{Data: []byte(mmd)},
	}
	for id, body := range nodes {
		m["nodes/"+id+".yaml"] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

func TestLoadFS_Valid(t *testing.T) {
	fsys := tmplFS(
		"name: greet\nentry: a\n",
		"flowchart TD\n a[Start] --> b[End]\n",
		map[string]string{
			"a": "executor:\n  kind: human\n  instructions: do a\n",
			"b": "executor:\n  kind: tool\n  run: echo hi\n",
		},
	)
	tmpl, err := LoadFS(fsys, "user:greet", SourceUser, "")
	require.NoError(t, err)
	assert.Equal(t, "greet", tmpl.Name)
	assert.Equal(t, SourceUser, tmpl.Source)
	assert.Equal(t, []string{"a"}, tmpl.Entry)
	assert.Len(t, tmpl.Nodes, 2)
	assert.Equal(t, map[string]bool{"a->b|": true}, edgeSet(tmpl.Edges))
	assert.Equal(t, "Start", tmpl.DisplayLabel("a")) // mermaid text fallback
}

func TestLoadFS_EntryComputedFromSources(t *testing.T) {
	fsys := tmplFS(
		"name: t\n", // no entry declared
		"flowchart TD\n a --> b\n b --> c\n",
		map[string]string{
			"a": "executor: {kind: human}\n",
			"b": "executor: {kind: human}\n",
			"c": "executor: {kind: human}\n",
		},
	)
	tmpl, err := LoadFS(fsys, "t", SourceUser, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, tmpl.Entry)
}

func TestLoadFS_EnumBranchValid(t *testing.T) {
	fsys := tmplFS(
		"name: t\nentry: review\n",
		"flowchart TD\n review{Review} -->|approved| ship\n review -->|changes| fix\n",
		map[string]string{
			"review": "executor: {kind: human}\nverify:\n  kind: enum\n  values: [approved, changes]\n",
			"ship":   "executor: {kind: human}\n",
			"fix":    "executor: {kind: human}\n",
		},
	)
	_, err := LoadFS(fsys, "t", SourceUser, "")
	require.NoError(t, err)
}

// loadProblems loads a deliberately broken template and returns the aggregated
// problem list.
func loadProblems(t *testing.T, workflowYAML, mmd string, nodes map[string]string) []string {
	t.Helper()
	_, err := LoadFS(tmplFS(workflowYAML, mmd, nodes), "bad", SourceUser, "")
	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	return ve.Problems
}

func assertAnyContains(t *testing.T, problems []string, substr string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Errorf("no problem contained %q; problems were: %v", substr, problems)
}

func TestLoadFS_ValidationProblems(t *testing.T) {
	t.Run("missing node yaml", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a --> b\n",
			map[string]string{"a": "executor: {kind: human}\n"})
		assertAnyContains(t, p, "no nodes/b.yaml")
	})

	t.Run("orphan node yaml", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a --> b\n",
			map[string]string{
				"a": "executor: {kind: human}\n",
				"b": "executor: {kind: human}\n",
				"c": "executor: {kind: human}\n",
			})
		assertAnyContains(t, p, "nodes/c.yaml has no matching node")
	})

	t.Run("unknown executor kind", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: wizard}\n"})
		assertAnyContains(t, p, "unknown executor.kind")
	})

	t.Run("ai needs prompt", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: ai}\n"})
		assertAnyContains(t, p, "ai executor needs a prompt")
	})

	t.Run("tool needs run", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: tool}\n"})
		assertAnyContains(t, p, "tool executor needs a run command")
	})

	t.Run("enum needs values", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: human}\nverify: {kind: enum}\n"})
		assertAnyContains(t, p, "enum verification needs a non-empty values list")
	})

	t.Run("format needs valid regex", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: human}\nverify:\n  kind: format\n  pattern: \"[unclosed\"\n"})
		assertAnyContains(t, p, "not a valid regex")
	})

	t.Run("bad edge label", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a -->|weird| b\n",
			map[string]string{
				"a": "executor: {kind: human}\n",
				"b": "executor: {kind: human}\n",
			})
		assertAnyContains(t, p, "is not valid here")
	})

	t.Run("fail edge without on_fail continue", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a -->|fail| b\n",
			map[string]string{
				"a": "executor: {kind: human}\n",
				"b": "executor: {kind: human}\n",
			})
		assertAnyContains(t, p, "the fail edge is dead")
	})

	t.Run("undefined entry", func(t *testing.T) {
		p := loadProblems(t, "name: t\nentry: zzz\n", "flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: human}\n"})
		assertAnyContains(t, p, `entry node "zzz" is not declared`)
	})

	t.Run("pure cycle no entry", func(t *testing.T) {
		p := loadProblems(t, "name: t\n", "flowchart TD\n a --> b\n b --> a\n",
			map[string]string{
				"a": "executor: {kind: human}\n",
				"b": "executor: {kind: human}\n",
			})
		assertAnyContains(t, p, "no entry node")
	})

	t.Run("missing name", func(t *testing.T) {
		p := loadProblems(t, "description: no name\n", "flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: human}\n"})
		assertAnyContains(t, p, "name is required")
	})

	t.Run("duplicate param", func(t *testing.T) {
		p := loadProblems(t,
			"name: t\nparams:\n  - name: x\n  - name: x\n",
			"flowchart TD\n a\n",
			map[string]string{"a": "executor: {kind: human}\n"})
		assertAnyContains(t, p, `duplicate param "x"`)
	})
}

func TestLoadFS_ReportsMultipleProblems(t *testing.T) {
	p := loadProblems(t, "description: no name\n", "flowchart TD\n a --> b\n",
		map[string]string{"a": "executor: {kind: ai}\n"}) // missing name, missing b.yaml, ai no prompt
	assert.GreaterOrEqual(t, len(p), 2, "expected several problems, got: %v", p)
}

func TestParam_IsRequired(t *testing.T) {
	yes := true
	no := false
	assert.True(t, Param{Name: "a"}.IsRequired())                         // default
	assert.False(t, Param{Name: "a", Default: "x"}.IsRequired())          // has default
	assert.True(t, Param{Name: "a", Required: &yes}.IsRequired())         // explicit
	assert.False(t, Param{Name: "a", Required: &no}.IsRequired())         // explicit
	assert.False(t, Param{Name: "a", Required: &no, Default: ""}.IsRequired())
}
