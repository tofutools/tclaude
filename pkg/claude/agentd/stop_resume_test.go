package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Ensures that stopping a conv that has no live tmux session returns
// the idempotent `skipped:already_offline` sentinel rather than a
// 503 error or a 200 with an empty action. Mirrors the bulk
// groups.stop behaviour exactly — single-conv variant should be
// indistinguishable from a one-member group stop.
func TestHandleAgentStop_SkipsOfflineTarget(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-conv-id-12345678",
	})
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentStop, "<test>"), "grant")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/stop", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 1, HasClaudeAncestor: true, ConvID: "manager"}))
	handleAgentByConv(w, r)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode")
	assert.Equal(t, "skipped:already_offline", resp["action"],
		"action; full body=%s", w.Body.String())
}

// Without the agent.stop slug AND no group ownership, a cross-agent
// stop must 403. This locks in that the dispatcher's auth gate is
// active for the new verb (it'd be easy to forget the
// requireCrossAgentPermission call when copy-pasting a handler).
func TestHandleAgentStop_NoSlugDenies(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-conv-id-12345678",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/stop", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 1, HasClaudeAncestor: true, ConvID: "stranger"}))
	handleAgentByConv(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
}

// Resume on a target whose conv-id resolves but isn't online should
// attempt the spawn. We can't fully exercise the spawn in a unit
// test (no real tmux), so the outcome is either `resumed` (if the
// spawn subprocess started) or `error` (if `tclaude` isn't in $PATH).
// Either way it must NOT be the offline-skip sentinel — that's the
// stop semantics, not resume's.
func TestHandleAgentResume_AttemptsSpawnForOfflineTarget(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-conv-id-12345678",
	})
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentResume, "<test>"), "grant")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/resume", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 1, HasClaudeAncestor: true, ConvID: "manager"}))
	handleAgentByConv(w, r)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode")
	action, _ := resp["action"].(string)
	switch action {
	case "resumed", "error":
		// Acceptable — resumed means the subprocess started; error means
		// the spawn helper rejected the call (e.g. tclaude binary missing
		// in test sandbox).
	case "skipped:already_offline", "skipped:already_online", "skipped:no_conv_id":
		t.Errorf("unexpected sentinel %q; resume should attempt spawn for offline target with valid conv-id",
			action)
	default:
		t.Errorf("unexpected action %q; full body=%s", action, w.Body.String())
	}
}

// stopOneConv is the helper shared between the bulk and single-conv
// stop paths. Locks in the offline-skip sentinel here so future
// refactors don't accidentally change the contract that both
// handlers rely on.
func TestStopOneConv_OfflineConvSkips(t *testing.T) {
	setupTestDB(t)
	res := stopOneConv("nonexistent-conv-id", false)
	assert.Equal(t, "skipped:already_offline", res.Action, "action")
	assert.Equal(t, "nonexistent-conv-id", res.ConvID, "ConvID should round-trip input")
}

// resumeOneConv must report `skipped:no_conv_id` when called with an
// empty conv-id. This mirrors the bulk groups.resume placeholder
// handling — without a conv-id we have no .jsonl to resume from.
func TestResumeOneConv_EmptyConvIDSkips(t *testing.T) {
	setupTestDB(t)
	res := resumeOneConv("")
	assert.Equal(t, "skipped:no_conv_id", res.Action, "action")
}
