package agentd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

// A tracker ref with no stored link is linked as the board is read whenever
// its tracker's link can be known, and says why when it cannot
// (taskboard_reflinks.go).

type tbLinkRef struct {
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Label    string `json:"label"`
	Local    bool   `json:"local"`
	ID       string `json:"id"`
	LinkNote string `json:"link_note"`
}

type tbLinkTask struct {
	ID           string      `json:"id"`
	Refs         []tbLinkRef `json:"refs"`
	ResolveNotes []string    `json:"resolve_notes"`
}

func (h *taskboardHarness) linkTask(method, path string, body any, want int) tbLinkTask {
	h.t.Helper()
	rec := h.call("", method, path, body)
	require.Equal(h.t, want, rec.Code, "body=%s", rec.Body.String())
	var out tbLinkTask
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// boardRefs is every task's refs on crew's board, by task id.
func (h *taskboardHarness) boardRefs() map[string][]tbLinkRef {
	h.t.Helper()
	rec := h.call("", http.MethodGet, "/v1/groups/crew/taskboard", nil)
	require.Equal(h.t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out struct {
		Tasks []tbLinkTask `json:"tasks"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	refs := map[string][]tbLinkRef{}
	for _, t := range out.Tasks {
		refs[t.ID] = t.Refs
	}
	return refs
}

// awbCLIAt makes the awb CLI report a remote server at server, or fail.
func awbCLIAt(t *testing.T, server string) {
	t.Cleanup(agentd.SetAWBCLIStatusForTest(func(context.Context) ([]byte, error) {
		if server == "" {
			return nil, errors.New("no awb CLI on the daemon's PATH")
		}
		return []byte(`{"connection":{"mode":"remote","server":"` + server + `","ui":"` + server + `#/workspaces"}}`), nil
	}))
}

// With no awb proxy configured, an awb id stored without a link is linked on
// the server the awb CLI is configured for, as the board is read; until a
// server is known it says why, and a resolve says so too.
func TestTaskboardAWBRefLinkedFromTheAWBCLI(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	awbCLIAt(t, "")

	created := h.linkTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "runner seam", "ref": "awb:tcl-8ab4sp",
			"add_refs": []string{"https://other.example/#/issues/tcl-zzzzzz"}}, http.StatusCreated)
	require.Len(t, created.Refs, 2)
	assert.Empty(t, created.Refs[0].URL, "no awb server is known")
	assert.Contains(t, created.Refs[0].LinkNote, "no awb server URL is configured")
	assert.Contains(t, created.Refs[0].LinkNote, "no awb CLI", "and why the awb CLI gave none")

	// A resolve with no server to link on says why; it is never silent.
	edited := h.linkTask(http.MethodPatch, "/v1/taskboard/tasks/"+created.ID,
		map[string]any{"resolve_ref": true}, http.StatusOK)
	require.Len(t, edited.ResolveNotes, 1, "%+v", edited)
	assert.Contains(t, edited.ResolveNotes[0], "tcl-8ab4sp: no awb server URL is configured")

	// The awb CLI now knows its server: the stored ref is linked on read,
	// and the one stored with its own link keeps it.
	awbCLIAt(t, "https://awb.example/awb/")
	refs := h.boardRefs()[created.ID]
	require.Len(t, refs, 2)
	assert.Equal(t, "https://awb.example/awb/#/issues/tcl-8ab4sp", refs[0].URL)
	assert.Empty(t, refs[0].LinkNote)
	assert.True(t, refs[0].Local, "linked on this node's awb server, so a move sends its id")
	assert.Equal(t, "tcl-8ab4sp", refs[0].ID)
	assert.Equal(t, "https://other.example/#/issues/tcl-zzzzzz", refs[1].URL, "a stored link wins")
	assert.False(t, refs[1].Local)

	// A resolve now stores the link.
	edited = h.linkTask(http.MethodPatch, "/v1/taskboard/tasks/"+created.ID,
		map[string]any{"resolve_ref": true}, http.StatusOK)
	assert.Empty(t, edited.ResolveNotes)
	awbCLIAt(t, "")
	refs = h.boardRefs()[created.ID]
	assert.Equal(t, "https://awb.example/awb/#/issues/tcl-8ab4sp", refs[0].URL, "the resolved link was stored")
}

// A Linear id has no link until its team's workspace is known; any link of
// one of the team's issues teaches it, and then every link-less ref of that
// team is linked on read.
func TestTaskboardLinearRefLinkedOnceItsWorkspaceIsKnown(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")

	bare := h.linkTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "checkout", "ref": "LIN-12"}, http.StatusCreated)
	require.Len(t, bare.Refs, 1)
	assert.Empty(t, bare.Refs[0].URL)
	assert.Contains(t, bare.Refs[0].LinkNote, "Linear workspace of team LIN is not known yet")

	other := h.linkTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "other team", "ref": "SHOP-3"}, http.StatusCreated)

	h.linkTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "cart", "ref": "https://linear.app/acme/issue/LIN-7/cart-total"}, http.StatusCreated)
	refs := h.boardRefs()
	assert.Equal(t, "https://linear.app/acme/issue/LIN-12", refs[bare.ID][0].URL)
	assert.Empty(t, refs[bare.ID][0].LinkNote)
	assert.Empty(t, refs[other.ID][0].URL, "another team's workspace is not assumed")
	assert.Contains(t, refs[other.ID][0].LinkNote, "team SHOP")
}

// owner/repo#n is a GitHub link as given.
func TestTaskboardGitHubRefIsLinked(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	created := h.linkTask(http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "flake", "ref": "owner/repo#7"}, http.StatusCreated)
	refs := h.boardRefs()[created.ID]
	require.Len(t, refs, 1)
	assert.Equal(t, "github", refs[0].Kind)
	assert.Equal(t, "https://github.com/owner/repo/issues/7", refs[0].URL)
	assert.Empty(t, refs[0].LinkNote)
}

// A linked node reading this node's board gets each ref linked as this node
// links it (its awb server, not the reader's), through the real peer path.
func TestPeerTaskboardRefLinkedByTheOwningNode(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	crew := fh.f.HaveGroup("crew")
	h := &peerProcessHarness{fedHarness: fh}
	bridgeAgentClientToMux(t, fh.f.Mux)
	receiver := agentd.PeerViewHandler(fh.peer.id.ID())
	awbCLIAt(t, "")
	task, err := db.CreateGroupTask(db.NewGroupTask{GroupID: crew.ID, Title: "runner seam",
		RefKind: "awb", RefLabel: "tcl-8ab4sp", Actor: "human:operator"})
	require.NoError(t, err)
	awbCLIAt(t, "https://awb.example/awb/")
	h.grant(t, agentd.PermTaskboardRead, "group=crew")

	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	done := make(chan int, 1)
	go func() { done <- agent.RunTaskboardCLI([]string{"show", task.ID, "--peer", "bob", "--json"}, stdout, stderr) }()
	conn := acceptOutboundPeerView(t, fh.peer, 0)
	wire := readPeerViewWire(t, conn)
	req := httptest.NewRequest(wire.Method, wire.URI, bytes.NewReader(wire.Body))
	for k, v := range wire.Header {
		req.Header[k] = v
	}
	rec := testharness.Serve(receiver, req)
	writePeerViewWire(t, conn, peerViewWire{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()})
	rc := <-done
	_ = conn.Close()
	require.Equal(t, 0, rc, "out=%s err=%s", stdout, stderr)
	assert.Contains(t, stdout.String(), `"url": "https://awb.example/awb/#/issues/tcl-8ab4sp"`)
}
