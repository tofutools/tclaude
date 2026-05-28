package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTemplate scaffolds a minimal valid template dir under parent and returns
// its path. dirName is the on-disk directory (the discovery name); wfName is the
// name inside workflow.yaml.
func writeTemplate(t *testing.T, parent, dirName, wfName string) string {
	t.Helper()
	dir := filepath.Join(parent, dirName)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nodes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte("name: "+wfName+"\nentry: a\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "flow.mmd"), []byte("flowchart TD\n a --> b\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nodes", "a.yaml"), []byte("executor: {kind: human}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nodes", "b.yaml"), []byte("executor: {kind: human}\n"), 0o644))
	return dir
}

func TestResolve_ProjectQualifiedAndBare(t *testing.T) {
	parent := t.TempDir()
	writeTemplate(t, parent, "foo", "foo-wf")

	qual, err := Resolve("project:foo", parent)
	require.NoError(t, err)
	assert.Equal(t, "foo-wf", qual.Name)
	assert.Equal(t, SourceProject, qual.Source)

	bare, err := Resolve("foo", parent)
	require.NoError(t, err)
	assert.Equal(t, SourceProject, bare.Source)
}

func TestResolve_Example(t *testing.T) {
	tmpl, err := Resolve("example:implement-microservice")
	require.NoError(t, err)
	assert.Equal(t, "implement-microservice", tmpl.Name)
	assert.Equal(t, SourceExample, tmpl.Source)
}

func TestResolve_NotFound(t *testing.T) {
	_, err := Resolve("does-not-exist", t.TempDir())
	assert.Error(t, err)
}

func TestList_IncludesExample(t *testing.T) {
	entries := List() // no project dirs
	var found *ListEntry
	for i := range entries {
		if entries[i].Ref == "example:implement-microservice" {
			found = &entries[i]
		}
	}
	require.NotNil(t, found, "List() should surface the embedded example")
	assert.Equal(t, SourceExample, found.Source)
	assert.Equal(t, 6, found.NodeCount)
	assert.Empty(t, found.Err)
}

func TestList_ProjectShadowsExample(t *testing.T) {
	parent := t.TempDir()
	// A project template whose dir name collides with the example.
	writeTemplate(t, parent, "implement-microservice", "my-override")

	entries := List(parent)
	var sawProjectOverride, sawExampleVersion bool
	for _, e := range entries {
		switch e.Ref {
		case "project:implement-microservice":
			sawProjectOverride = true
			assert.Equal(t, "my-override", e.Name)
		case "example:implement-microservice":
			sawExampleVersion = true
		}
	}
	assert.True(t, sawProjectOverride, "project template should be listed")
	assert.False(t, sawExampleVersion, "example should be shadowed by the project template")
}

func TestList_BrokenTemplateReportsErr(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "broken")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// Has a workflow.yaml (so it's a candidate) but no flow.mmd (so it fails to load).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte("name: broken\n"), 0o644))

	entries := List(parent)
	var broken *ListEntry
	for i := range entries {
		if entries[i].Ref == "project:broken" {
			broken = &entries[i]
		}
	}
	require.NotNil(t, broken)
	assert.NotEmpty(t, broken.Err, "a template that fails to load should report its error")
}

func TestSplitRef(t *testing.T) {
	cases := []struct {
		ref       string
		source    Source
		name      string
		qualified bool
	}{
		{"example:foo", SourceExample, "foo", true},
		{"user:bar", SourceUser, "bar", true},
		{"project:baz", SourceProject, "baz", true},
		{"plainname", "", "plainname", false},
		{"unknown:thing", "", "unknown:thing", false}, // unrecognised prefix → bare
	}
	for _, c := range cases {
		t.Run(c.ref, func(t *testing.T) {
			source, name, qualified := splitRef(c.ref)
			assert.Equal(t, c.source, source)
			assert.Equal(t, c.name, name)
			assert.Equal(t, c.qualified, qualified)
		})
	}
}

func TestIsTemplateDir(t *testing.T) {
	parent := t.TempDir()
	assert.False(t, isTemplateDir(parent))
	writeTemplate(t, parent, "ok", "ok")
	assert.True(t, isTemplateDir(filepath.Join(parent, "ok")))
}

func TestUserDir(t *testing.T) {
	if ud := UserDir(); ud != "" {
		assert.Equal(t, "workflows", filepath.Base(ud))
	}
}
