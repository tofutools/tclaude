package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// bridgeRawPostToMux serves the CLI's raw uploads from h as the operator.
func bridgeRawPostToMux(t *testing.T, h http.Handler) {
	t.Helper()
	prev := agent.DaemonPostRawWithOptionsImpl
	agent.DaemonPostRawWithOptionsImpl = func(path, contentType string, body []byte, _ http.Header, out any, _ agent.DaemonOpts) error {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		rec := testharness.Serve(h, agentd.AsHumanPeer(req))
		if rec.Code >= 400 {
			var e struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			return &agent.DaemonError{Status: rec.Code, Code: e.Code, Msg: e.Error, Raw: rec.Body.Bytes()}
		}
		if out != nil && rec.Body.Len() > 0 {
			return json.Unmarshal(rec.Body.Bytes(), out)
		}
		return nil
	}
	t.Cleanup(func() { agent.DaemonPostRawWithOptionsImpl = prev })
}

// The CLI's files on a task: add --attach lands the task with its files,
// show lists them, attach adds more, attachments --save writes them out
// (never over a file), and detach takes one off by name.
func TestTaskboardAttachmentsCLI(t *testing.T) {
	h := newTaskboardFlow(t)
	crew := h.f.HaveGroup("crew")
	bridgeAgentClientToMux(t, h.f.Mux)
	bridgeRawPostToMux(t, h.f.Mux)
	src := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(src, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	shot, notes, extra := write("shot.png", string(tbPNG)), write("notes.md", "# notes"), write("extra.txt", "more")
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}

	rc, out, errOut := run("add", "crew", "fix the layout", "--attach", shot, "--attach", notes)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "shot.png")
	tasks, err := db.ListGroupTasks(crew.ID, db.GroupTaskListOpts{})
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	id := tasks[0].ID

	rc, out, errOut = run("show", id)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "files:   2")
	assert.Contains(t, out, "notes.md")

	rc, out, errOut = run("attach", id, extra)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "extra.txt")
	rc, _, errOut = run("attach", id, write("page.html", "<p>"))
	assert.NotEqual(t, 0, rc, "a type off the allow-list is refused")
	assert.Contains(t, errOut, "type not allowed")

	dest := filepath.Join(t.TempDir(), "out")
	rc, out, errOut = run("attachments", id, "--save", dest)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	got, err := os.ReadFile(filepath.Join(dest, "shot.png"))
	require.NoError(t, err)
	assert.Equal(t, tbPNG, got)
	rc, _, errOut = run("attachments", id, "--save", dest)
	assert.NotEqual(t, 0, rc, "an existing file is never overwritten")
	assert.Contains(t, errOut, "exists")

	rc, out, errOut = run("detach", id, "NOTES.md")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	rc, out, errOut = run("attachments", id)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.NotContains(t, out, "notes.md")
	assert.Contains(t, out, "extra.txt")
	rc, _, errOut = run("detach", id, "nope.txt")
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, errOut, "no file")
}
