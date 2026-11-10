package agentd_test

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The CLI's comments: add (with a file and a key), ls --save, edit, rm, and
// activity (history its alias) reading the stream.
func TestTaskboardCommentsCLI(t *testing.T) {
	h := newTaskboardFlow(t)
	crew := h.f.HaveGroup("crew")
	bridgeAgentClientToMux(t, h.f.Mux)
	bridgeRawPostToMux(t, h.f.Mux)
	src := t.TempDir()
	shot := filepath.Join(src, "shot.png")
	require.NoError(t, os.WriteFile(shot, tbPNG, 0o600))
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}

	rc, out, errOut := run("add", "crew", "fix the layout")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	tasks, err := db.ListGroupTasks(crew.ID, db.GroupTaskListOpts{})
	require.NoError(t, err)
	id := tasks[0].ID

	rc, out, errOut = run("comment", "add", id, "see **this**", "--attach", shot, "--key", "k-1")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "shot.png")
	rc, out, errOut = run("comment", "add", id, "see **this**", "--key", "k-1")
	require.Equal(t, 0, rc, "the same key and body is the same comment: out=%s err=%s", out, errOut)
	rc, _, _ = run("comment", "add", id)
	assert.NotEqual(t, 0, rc, "a comment needs a body")

	note := filepath.Join(src, "note.md")
	require.NoError(t, os.WriteFile(note, []byte("second\nline two"), 0o600))
	rc, out, errOut = run("comment", "add", id, "--file", note)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)

	rc, out, errOut = run("comment", "ls", id)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "see **this**")
	assert.Contains(t, out, "      line two")
	comments, err := db.ListGroupTaskComments(id)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	first := "#" + tbID(comments[0].ID)

	dir := filepath.Join(t.TempDir(), "out")
	rc, out, errOut = run("comment", "ls", id, "--save", dir)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	got, err := os.ReadFile(filepath.Join(dir, "comment-"+tbID(comments[0].ID), "shot.png"))
	require.NoError(t, err)
	assert.Equal(t, tbPNG, got)

	rc, out, errOut = run("comment", "edit", id, first, "see that")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	rc, out, errOut = run("comment", "rm", id, "#"+tbID(comments[1].ID))
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)

	rc, out, errOut = run("history", id)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "see that")
	assert.Contains(t, out, "[edited")
	assert.Contains(t, out, "[deleted")
	assert.Contains(t, out, "comment_edited")
	assert.NotContains(t, out, "line two", "a deleted comment's body is gone")

	rc, out, errOut = run("show", id)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "comments: 1")

	// --body is the argument as a flag, as agent message takes it; one way only.
	rc, out, errOut = run("comment", "add", id, "--body", "by flag")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	rc, _, errOut = run("comment", "add", id, "an arg", "--body", "and a flag")
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, errOut, "one way")
	rc, out, errOut = run("comment", "ls", id)
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "by flag")
}

// Comments through --peer: the receiver's rules gate them on
// taskboard.write, and read them under taskboard.read.
func TestPeerTaskboardCommentsCLI(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	crew := fh.f.HaveGroup("crew")
	h := &peerProcessHarness{fedHarness: fh}
	bridgeAgentClientToMux(t, fh.f.Mux)
	receiver := agentd.PeerViewHandler(fh.peer.id.ID())
	task, err := db.CreateGroupTask(db.NewGroupTask{GroupID: crew.ID, Title: "talk it over", Actor: "human:operator"})
	require.NoError(t, err)

	opened := 0
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		done := make(chan int, 1)
		go func() { done <- agent.RunTaskboardCLI(args, stdout, stderr) }()
		conn := acceptOutboundPeerView(t, fh.peer, opened)
		opened++
		wire := readPeerViewWire(t, conn)
		req := httptest.NewRequest(wire.Method, wire.URI, bytes.NewReader(wire.Body))
		for k, v := range wire.Header {
			req.Header[k] = v
		}
		rec := testharness.Serve(receiver, req)
		writePeerViewWire(t, conn, peerViewWire{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()})
		rc := <-done
		_ = conn.Close()
		return rc, stdout.String(), stderr.String()
	}

	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	rc, out, errOut := run("comment", "add", task.ID, "from afar", "--peer", "bob")
	assert.NotEqual(t, 0, rc, "read does not comment: out=%s err=%s", out, errOut)

	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	rc, out, errOut = run("comment", "add", task.ID, "from afar", "--peer", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	comments, err := db.ListGroupTaskComments(task.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	// Authorship keys on the node's instance id, not its changeable name.
	assert.Equal(t, "peer:"+fh.peer.id.ID(), comments[0].Actor)

	rc, out, errOut = run("comment", "edit", task.ID, "#"+tbID(comments[0].ID), "from afar, edited", "--peer", "bob")
	require.Equal(t, 0, rc, "the peer edits its own comment: out=%s err=%s", out, errOut)

	rc, out, errOut = run("activity", task.ID, "--peer", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "from afar, edited")
	assert.Contains(t, out, "the operator of linked node", "the author is named, not numbered")
	assert.Contains(t, out, "untrusted content")
}
