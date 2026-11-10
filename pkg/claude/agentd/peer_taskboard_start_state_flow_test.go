package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A linked node starts a new task in any column as a local caller does, in
// one create, under its peer grants: write for the task (blocked or logged
// as done), and assign on top for an assignee (in progress).
func TestPeerTaskboardCreateStartState(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	fh.f.HaveGroup("crew")
	h := &peerProcessHarness{fedHarness: fh}
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	create := func(body map[string]any) (int, string) {
		t.Helper()
		rec := testharness.Serve(view, testharness.JSONRequest(t, http.MethodPost, "/v1/groups/crew/taskboard/tasks", body))
		return rec.Code, rec.Body.String()
	}
	const member = "tb-peer-start-1111"
	fh.f.HaveConvWithTitle(member, "builder")
	_, _, err = db.EnsureAgentForConv(member, "test")
	require.NoError(t, err)
	fh.f.HaveMember("crew", member)

	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	code, body := create(map[string]any{"title": "stuck", "status": "blocked", "block_reason": "waiting on bob"})
	require.Equal(t, http.StatusCreated, code, body)
	var got tbTask
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, "blocked", got.Status)
	assert.Equal(t, "waiting on bob", got.BlockReason)
	code, body = create(map[string]any{"title": "already done", "status": "done"})
	require.Equal(t, http.StatusCreated, code, body)

	// An assignee takes taskboard.assign, as an assign does.
	code, body = create(map[string]any{"title": "for builder", "to": member})
	assert.Equal(t, http.StatusForbidden, code, body)
	h.grant(t, agentd.PermTaskboardAssign, "group=crew")
	code, body = create(map[string]any{"title": "for builder", "to": member})
	require.Equal(t, http.StatusCreated, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, "next", got.Stage, "in progress")
	assert.Contains(t, body, `"assignee"`)
}
