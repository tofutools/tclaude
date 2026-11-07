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

// A linked node (bob) moves a task between two groups of this node, as the
// fleet view's drag does on a peer's board (tcl-0r0obl): taskboard.transfer
// on the target group, and taskboard.manage on the source for a move it does
// not hold; a clone needs only a read of the source.
func TestPeerTaskboardMoveToGroup(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	fh.f.HaveGroup("crew")
	fh.f.HaveGroup("ops")
	h := &peerProcessHarness{fedHarness: fh}
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(view, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "hop", "stage": "next"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var task tbTask
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &task))
	path := "/v1/taskboard/tasks/" + task.ID + "/move-to-group"

	// Without any grant on the source the task does not exist for bob.
	code, body := call("POST", path, map[string]any{"group": "ops"})
	assert.Equal(t, http.StatusNotFound, code, body)

	// Reading crew, but no transfer on ops: ops answers as a missing group
	// does, so group names do not leak.
	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	code, body = call("POST", path, map[string]any{"group": "ops", "clone": true})
	assert.Equal(t, http.StatusNotFound, code, body)
	code, missing := call("POST", path, map[string]any{"group": "nope", "clone": true})
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, missing, strings.Replace(body, "ops", "nope", 1), "the same answer as a missing group")

	// Transfer on ops: a clone goes through on read alone. Transfer does not
	// show ops' board.
	h.grant(t, agentd.PermTaskboardTransfer, "group=ops")
	code, ctx := call("GET", "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, code, ctx)
	assert.Contains(t, ctx, `"group":"crew"`)
	assert.NotContains(t, ctx, `"group":"ops"`, "transfer alone does not read the board")
	code, body = call("POST", path, map[string]any{"group": "ops", "clone": true})
	require.Equal(t, http.StatusOK, code, body)

	// A move also takes manage on crew.
	code, body = call("POST", path, map[string]any{"group": "ops"})
	assert.Equal(t, http.StatusForbidden, code, body)
	h.grant(t, agentd.PermTaskboardManage, "group=crew")
	code, body = call("POST", path, map[string]any{"group": "ops"})
	require.Equal(t, http.StatusOK, code, body)
	var out struct {
		Group string `json:"group"`
		From  string `json:"from"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, "ops", out.Group)
	assert.Equal(t, "crew", out.From)
}
