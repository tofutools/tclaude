package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A move or clone across nodes (tcl-0r0obl): a linked node (bob) creates the
// task on a board here with transfer-in under taskboard.transfer, and drops
// the original here with moved-away under taskboard.manage (a task it does
// not hold).
func TestPeerTaskboardTransferAcrossNodes(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	fh.f.HaveGroup("crew")
	h := &peerProcessHarness{fedHarness: fh}
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(view, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	in := map[string]any{"title": "from bob's board", "body": "the brief", "ref": "https://example.com/7", "stage": "next",
		"origin": map[string]any{"node": "bob", "group": "web", "task": "tk_00000abc"}}

	// transfer-in: write alone is not enough; taskboard.transfer is.
	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	code, body := call("POST", "/v1/groups/crew/taskboard/transfer-in", in)
	assert.Equal(t, http.StatusForbidden, code, body)
	h.grant(t, agentd.PermTaskboardTransfer, "group=crew")
	code, body = call("POST", "/v1/groups/crew/taskboard/transfer-in", in)
	require.Equal(t, http.StatusCreated, code, body)
	var landed tbTask
	require.NoError(t, json.Unmarshal([]byte(body), &landed))
	assert.Equal(t, "next", landed.Stage)
	require.NotNil(t, landed.Ref)
	assert.Equal(t, "https://example.com/7", landed.Ref.URL)
	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/taskboard/tasks/"+landed.ID+"/events", nil)
	assert.Contains(t, rec.Body.String(), "moved from bob/web tk_00000abc", "its history names where it came from")
	code, _ = call("POST", "/v1/groups/crew/taskboard/transfer-in", map[string]any{"title": "no origin"})
	assert.Equal(t, http.StatusBadRequest, code)

	// moved-away: the source side of a move from here to bob. A task the peer
	// does not hold takes taskboard.manage; write is not enough.
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "leaving", "stage": "next"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var leaving tbTask
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &leaving))
	away := map[string]any{"to": map[string]any{"node": "bob", "group": "web", "task": "tk_00000def"}}
	code, body = call("POST", "/v1/taskboard/tasks/"+leaving.ID+"/moved-away", away)
	assert.Equal(t, http.StatusForbidden, code, body)
	h.grant(t, agentd.PermTaskboardManage, "group=crew")
	code, body = call("POST", "/v1/taskboard/tasks/"+leaving.ID+"/moved-away", away)
	require.Equal(t, http.StatusOK, code, body)
	var gone tbTask
	require.NoError(t, json.Unmarshal([]byte(body), &gone))
	assert.Equal(t, "dropped", gone.Status)
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/taskboard/tasks/"+leaving.ID+"/events", nil)
	assert.True(t, strings.Contains(rec.Body.String(), "moved to bob/web tk_00000def"), rec.Body.String())
	// Only a live task moves.
	code, _ = call("POST", "/v1/taskboard/tasks/"+leaving.ID+"/moved-away", away)
	assert.Equal(t, http.StatusConflict, code)
}
