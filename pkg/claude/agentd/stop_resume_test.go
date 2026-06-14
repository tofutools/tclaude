package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
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

// Resume on a target whose conv-id resolves and has resumable metadata should
// attempt the spawn. The spawner is faked here so the test does not fork a
// real `tclaude session new` subprocess.
func TestHandleAgentResume_AttemptsSpawnForOfflineTarget(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-conv-id-12345678",
	})
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      "worker-conv-id-12345678",
		ProjectPath: "/tmp/worker",
		IndexedAt:   time.Now(),
	}))
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentResume, "<test>"), "grant")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/resume", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 1, HasClaudeAncestor: true, ConvID: "manager"}))
	handleAgentByConv(w, r)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode")
	assert.Equal(t, "resumed", resp["action"], "full body=%s", w.Body.String())
	assert.Equal(t, "worker-conv-id-12345678", rec.convID)
	assert.Equal(t, "/tmp/worker", rec.cwd)
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

func TestResumeOneConv_OrphanWithoutSessionOrIndexErrors(t *testing.T) {
	setupTestDB(t)
	res := resumeOneConv("orphan-conv-id-12345678")
	assert.Equal(t, "error", res.Action, "action")
	assert.Contains(t, res.Detail, "no resumable session metadata")
}

func TestResumeOneConv_UsesConvIndexWhenSessionMissing(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	const convID = "codex-conv-id-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectPath: "/tmp/codex-work",
		Harness:     harness.CodexName,
		IndexedAt:   time.Now(),
	}))

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, convID, rec.convID)
	assert.Equal(t, "/tmp/codex-work", rec.cwd)
	assert.Equal(t, harness.CodexName, rec.harness)
	assert.Equal(t, harness.CodexAgentProfile, rec.sandbox)
	assert.Equal(t, "never", rec.approval)
}

func TestResumeOneConv_UsesCodexNativeStoreWhenTclaudeCacheMissing(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	cwd := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	const convID = "019ec663-3bef-7c41-abf8-ad956ed94a01"
	cx := testharness.NewCodexSimWithID(t, home, convID, cwd)
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteThreadRow(testharness.CodexThreadSeed{
		Cwd:       cwd,
		CreatedAt: cx.CreatedUnix(),
		UpdatedAt: cx.CreatedUnix(),
	}))

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, convID, rec.convID)
	assert.Equal(t, cwd, rec.cwd)
	assert.Equal(t, harness.CodexName, rec.harness)
}

type recordingResumeSpawner struct {
	convID, cwd, effort, model, harness, sandbox, approval string
	autoReview                                             bool
}

func installRecordingResumeSpawner(t *testing.T) *recordingResumeSpawner {
	t.Helper()
	rec := &recordingResumeSpawner{}
	prev := Spawn
	Spawn = rec
	t.Cleanup(func() { Spawn = prev })
	return rec
}

func (s *recordingResumeSpawner) SpawnNew(label, cwd, effort, model, harness, sandbox, approval string, autoReview, trustDir bool) error {
	return nil
}

func (s *recordingResumeSpawner) SpawnResume(convID, cwd, effort, model, harness, sandbox, approval string, autoReview bool) error {
	s.convID = convID
	s.cwd = cwd
	s.effort = effort
	s.model = model
	s.harness = harness
	s.sandbox = sandbox
	s.approval = approval
	s.autoReview = autoReview
	return nil
}

func TestResumeOneConv_ConvIndexProjectDirFallback(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	const convID = "claude-conv-id-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     convID,
		ProjectDir: "/tmp/claude-project",
		IndexedAt:  time.Now(),
	}))

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, "/tmp/claude-project", rec.cwd)
	assert.True(t, rec.harness == "" || strings.EqualFold(rec.harness, harness.DefaultName))
}
