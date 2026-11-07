package agentd_test

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The CLI's --peer, end to end through this node's sender path: the operator
// runs `tclaude agent taskboard … --peer bob`, this node forwards it to bob
// through its peer proxy (and records the outbound audit row), and the
// scripted bob answers each request with the real receiving side,
// PeerViewHandler, serving this same node's boards under the grants given to
// bob. So the CLI, the sender proxy and the receiver's rules all run.
func TestPeerTaskboardCLI(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: true}
	require.NoError(t, config.Save(cfg))
	crew := fh.f.HaveGroup("crew")
	const member = "tb-cli-peer-member-1111"
	fh.f.HaveConvWithTitle(member, "builder")
	_, _, err = db.EnsureAgentForConv(member, "test")
	require.NoError(t, err)
	fh.f.HaveMember("crew", member)
	h := &peerProcessHarness{fedHarness: fh}
	bridgeAgentClientToMux(t, fh.f.Mux)
	receiver := agentd.PeerViewHandler(fh.peer.id.ID())

	// run runs one CLI invocation and answers the peer view it opens with
	// the receiving side.
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

	// Without a grant on crew, the peer refuses.
	rc, out, errOut := run("ls", "crew", "--peer", "bob")
	assert.NotEqual(t, 0, rc, "out=%s err=%s", out, errOut)

	// A write grant scoped to crew: add, list and assign through --peer.
	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	rc, out, errOut = run("add", "crew", "fix the flake", "--peer", "bob", "--next")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "fix the flake")
	tasks, err := db.ListGroupTasks(crew.ID, db.GroupTaskListOpts{})
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	id := tasks[0].ID
	assert.Contains(t, tasks[0].CreatedBy, "peer: ")

	rc, out, errOut = run("ls", "crew", "--peer", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "untrusted content", "a peer's answer is marked as remote content")
	assert.Contains(t, out, "fix the flake")

	rc, out, errOut = run("assign", id, "--to", member, "--peer", "bob")
	assert.NotEqual(t, 0, rc, "write does not assign: out=%s err=%s", out, errOut)
	h.grant(t, agentd.PermTaskboardAssign, "group=crew")
	rc, out, errOut = run("assign", id, "--to", member, "--peer", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "assigned")
	got, err := db.GetGroupTask(id)
	require.NoError(t, err)
	assert.NotEmpty(t, got.AssigneeID)

	// The sender recorded each outbound write, with its route, as its
	// operator's.
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "federation.peer_view.out"})
	require.NoError(t, err)
	var routes []string
	for _, row := range rows {
		routes = append(routes, row.Detail)
	}
	assert.Contains(t, routes, "phase=result peer="+fh.peer.id.ID()+" route=POST /v1/groups/{name}/taskboard/tasks")
	assert.Contains(t, routes, "phase=result peer="+fh.peer.id.ID()+" route=POST /v1/taskboard/tasks/{id}/assign")
}
