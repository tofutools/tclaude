package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
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
		&peer{PID: 1, HumanTokenValid: true}))
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

	// A real, existing launch dir: resume now refuses to relaunch into a
	// vanished cwd, so the recorded path must exist for the spawn to proceed.
	cwd := t.TempDir()
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-conv-id-12345678",
	})
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      "worker-conv-id-12345678",
		ProjectPath: cwd,
		IndexedAt:   time.Now(),
	}))
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentResume, "<test>"), "grant")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/w/resume", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 1, HumanTokenValid: true}))
	handleAgentByConv(w, r)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode")
	assert.Equal(t, "resumed", resp["action"], "full body=%s", w.Body.String())
	assert.Equal(t, "worker-conv-id-12345678", rec.convID)
	assert.Equal(t, cwd, rec.cwd)
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

	cwd := t.TempDir()
	const convID = "codex-conv-id-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectPath: cwd,
		Harness:     harness.CodexName,
		IndexedAt:   time.Now(),
	}))

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, convID, rec.convID)
	assert.Equal(t, cwd, rec.cwd)
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
	require.NoError(t, exec.Command("git", "init", "-q", cwd).Run())
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
	assert.Empty(t, rec.codexGitCommonDir,
		"proofless human relaunch lets the child derive its own repository root")
	assert.False(t, rec.codexGitCommonDirPinned)
}

// A resume whose recorded launch dir was deleted must NOT spawn into the
// vanished cwd (that wedges the agent at startup). It reports
// `error:missing_cwd` with the path in Detail so the caller can offer to
// recreate it, and creates nothing on its own.
func TestResumeOneConv_MissingCwdReportsErrorAndDoesNotSpawn(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	// Parent exists (writable temp), leaf is gone — the deleted-worktree shape.
	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const convID = "gone-conv-id-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectPath: gone,
		IndexedAt:   time.Now(),
	}))

	res := resumeOneConv(convID)
	assert.Equal(t, "error:missing_cwd", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, gone, res.Detail,
		"Detail must carry the missing path so the dialog can name it and recreate it")
	assert.Empty(t, rec.convID, "resume must not spawn a child into a vanished cwd")
	assert.NoDirExists(t, gone, "resume without the recreate opt-in must not create the dir")
}

// With the recreate opt-in, resume recreates the deleted launch dir empty
// and then relaunches into it — the "recreate the local dir so the agent
// can start" path the dashboard confirm and `--recreate-dir` drive.
func TestResumeOneConvRecreate_RecreatesMissingCwdThenSpawns(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const convID = "recr-conv-id-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectPath: gone,
		IndexedAt:   time.Now(),
	}))

	res := resumeOneConvRecreate(convID, true)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.DirExists(t, gone, "the recreate opt-in must create the empty launch dir")
	assert.Equal(t, gone, rec.cwd, "the resumed agent must launch into the recreated dir")
}

// End-to-end over the daemon mux: POST /v1/agent/{conv}/resume answers
// `error:missing_cwd` and creates nothing when the launch dir is gone;
// re-POSTing with ?recreate=1 (what the CLI's --recreate-dir and the
// dashboard's confirm send) recreates the dir empty and resumes.
func TestHandleAgentResume_RecreateParamCreatesMissingDir(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const convID = "httpr-conv-id-12345678"
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: convID})
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: convID, ProjectPath: gone, IndexedAt: time.Now(),
	}))
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentResume, "<test>"), "grant")

	resumePost := func(query string) map[string]any {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/agent/"+convID+"/resume"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), peerKey{},
			&peer{PID: 1, HumanTokenValid: true}))
		handleAgentByConv(w, r)
		require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "decode")
		return resp
	}

	// Without the opt-in: missing_cwd, nothing created, nothing spawned.
	resp := resumePost("")
	assert.Equal(t, "error:missing_cwd", resp["action"], "resp=%v", resp)
	assert.Equal(t, gone, resp["detail"], "resp=%v", resp)
	assert.NoDirExists(t, gone)
	assert.Empty(t, rec.convID, "the plain resume must not spawn")

	// With ?recreate=1: the dir is recreated empty and the agent resumes.
	resp = resumePost("?recreate=1")
	assert.Equal(t, "resumed", resp["action"], "resp=%v", resp)
	assert.DirExists(t, gone)
	assert.Equal(t, gone, rec.cwd)
}

func TestHandleAgentResume_AgentCannotRecreateMissingDir(t *testing.T) {
	setupTestDB(t)
	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const convID = "agent-recreate-target-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: convID, ProjectPath: gone, IndexedAt: time.Now(),
	}))
	require.NoError(t, db.GrantAgentPermission("manager", PermAgentResume, "<test>"))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/"+convID+"/resume?recreate=1", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{},
		&peer{PID: 1, HasClaudeAncestor: true, ConvID: "manager"}))
	handleAgentByConv(w, r)
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "recreate_dir_restricted")
	assert.NoDirExists(t, gone)
}

type recordingResumeSpawner struct {
	convID, cwd, cwdWriteProof, effort, model, harness, sandbox, approval, codexGitCommonDir string
	autoReview, codexGitCommonDirPinned                                                      bool
}

func installRecordingResumeSpawner(t *testing.T) *recordingResumeSpawner {
	t.Helper()
	rec := &recordingResumeSpawner{}
	prev := Spawn
	Spawn = rec
	t.Cleanup(func() { Spawn = prev })
	return rec
}

func (s *recordingResumeSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	return nil
}

func (s *recordingResumeSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	s.convID = args.ConvID
	s.cwd = args.Cwd
	s.cwdWriteProof = args.CwdWriteProof
	s.effort = args.Effort
	s.model = args.Model
	s.harness = args.Harness
	s.sandbox = args.Sandbox
	s.approval = args.Approval
	s.autoReview = args.AutoReview
	s.codexGitCommonDir = args.CodexGitCommonDir
	s.codexGitCommonDirPinned = args.CodexGitCommonDirPinned
	return nil
}

func TestResumeOneConv_ConvIndexProjectDirFallback(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	cwd := t.TempDir()
	const convID = "claude-conv-id-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:     convID,
		ProjectDir: cwd,
		IndexedAt:  time.Now(),
	}))

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, cwd, rec.cwd)
	assert.True(t, rec.harness == "" || strings.EqualFold(rec.harness, harness.DefaultName))
}

func TestResumeOneConvWithGrant_OnlineProofSnapshotCannotBecomeLaunch(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	const convID = "online-at-proof-conv-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: convID, ProjectPath: t.TempDir(), IndexedAt: time.Now(),
	}))

	// The sentinel is recorded by requireResumeWriteProofs while the member is
	// online. At execution time this test has no live tmux row, modelling the
	// online→offline transition that can occur behind an earlier bulk resume.
	res := resumeOneConvWithGrant(convID, false, &resumeGrant{SkipOnline: true}, "", nil)
	require.Equal(t, "skipped:already_online", res.Action, "detail=%s", res.Detail)
	assert.Empty(t, rec.convID, "proof-time-online member must not turn into an unproved launch")
}

func TestResumeOneConvWithGrant_DoesNotPassAnotherMembersProof(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	const convID = "unproved-group-member-conv-12345678"
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: convID, ProjectPath: t.TempDir(), IndexedAt: time.Now(),
	}))

	res := resumeOneConvWithGrant(convID, false, nil, "other-member-proof", nil)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Empty(t, rec.cwdWriteProof,
		"member without a cwd grant must not receive another member's group proof")
}
