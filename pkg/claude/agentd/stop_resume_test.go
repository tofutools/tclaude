package agentd

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func saveResumeSession(t *testing.T, convID, cwd, harnessName string) *db.SessionRow {
	t.Helper()
	captured, err := resumeprovenance.Capture(cwd)
	require.NoError(t, err)
	encoded, err := resumeprovenance.Encode(captured)
	require.NoError(t, err)
	row := &db.SessionRow{
		ID: "resume-" + convID, ConvID: convID, Cwd: cwd, Status: "exited",
		Harness: harnessName, ResumeProvenance: encoded,
	}
	require.NoError(t, db.SaveSession(row))
	return row
}

func physicalTestPath(t *testing.T, path string) string {
	t.Helper()
	physical, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return physical
}

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
	physicalCwd := physicalTestPath(t, cwd)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: gID, ConvID: "worker-conv-id-12345678",
	})
	saveResumeSession(t, "worker-conv-id-12345678", cwd, harness.DefaultName)
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
	assert.Equal(t, physicalCwd, rec.cwd)
}

func TestHandleAgentResume_GroupOwnershipAuthority(t *testing.T) {
	const (
		target = "worker-conv-id-12345678"
		owner  = "owner-conv-id-123456789"
	)

	requestAsOwner := func(t *testing.T, denyResume, unrelatedOwner bool) (*httptest.ResponseRecorder, *recordingResumeSpawner) {
		t.Helper()
		setupTestDB(t)
		rec := installRecordingResumeSpawner(t)
		cwd := t.TempDir()
		targetGroupID, err := db.CreateAgentGroup("target-team", "")
		require.NoError(t, err)
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
			GroupID: targetGroupID, ConvID: target,
		}))
		ownerGroupID := targetGroupID
		if unrelatedOwner {
			ownerGroupID, err = db.CreateAgentGroup("other-team", "")
			require.NoError(t, err)
		}
		require.NoError(t, db.AddAgentGroupOwner(ownerGroupID, owner, "test"))
		if denyResume {
			require.NoError(t, db.SetAgentPermissionOverride(
				owner, PermAgentResume, db.PermEffectDeny, "test"))
		}
		saveResumeSession(t, target, cwd, harness.DefaultName)

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/agent/"+target+"/resume", nil)
		r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{
			PID: 1, HasClaudeAncestor: true, ConvID: owner,
		}))
		handleAgentByConv(w, r)
		return w, rec
	}

	t.Run("owner of target group resumes without slug", func(t *testing.T) {
		w, rec := requestAsOwner(t, false, false)
		require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
		assert.Equal(t, target, rec.convID)
	})

	t.Run("explicit deny beats ownership", func(t *testing.T) {
		w, rec := requestAsOwner(t, true, false)
		assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
		assert.Empty(t, rec.convID, "denied resume must not launch the target")
	})

	t.Run("ownership does not cross group boundaries", func(t *testing.T) {
		w, rec := requestAsOwner(t, false, true)
		assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
		assert.Empty(t, rec.convID, "unrelated ownership must not launch the target")
	})
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

func TestStopOneConvWithIntent_FailedKillClearsAttribution(t *testing.T) {
	w := testharness.New(t)
	prevTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = prevTmux })
	const (
		convID    = "failed-stop-conv-12345678"
		sessionID = "failed-stop-session"
		tmuxName  = "failed-stop-tmux"
		eventID   = "evt_1234567890abcdef12345678"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: tmuxName,
		Status: "working", CreatedAt: time.Now(),
	}))
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID,
		"11111111111111111111111111111111"))
	w.Tmux.MarkAlive(tmuxName)
	w.Tmux.FailNextCommand("kill-pane")

	res := stopOneConvWithIntent(convID, true, db.AgentExitActionForceStop, eventID)
	assert.Equal(t, "error", res.Action)

	d, err := db.Open()
	require.NoError(t, err)
	var action, related, generation string
	require.NoError(t, d.QueryRow(`SELECT exit_intent, exit_intent_event_id,
		exit_intent_generation FROM sessions WHERE id = ?`, sessionID).
		Scan(&action, &related, &generation))
	assert.Empty(t, action)
	assert.Empty(t, related)
	assert.Empty(t, generation)
}

// resumeOneConv must report `skipped:no_conv_id` when called with an
// empty conv-id. This mirrors the bulk groups.resume placeholder
// handling — without a conv-id we have no .jsonl to resume from.
func TestResumeOneConv_EmptyConvIDSkips(t *testing.T) {
	setupTestDB(t)
	res := resumeOneConv("")
	assert.Equal(t, "skipped:no_conv_id", res.Action, "action")
}

func TestResumeOneConv_RetiredAgentSkips(t *testing.T) {
	setupTestDB(t)
	const convID = "retired-resume-conv-12345678"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	_, err = db.RetireAgent(convID, "test", "done")
	require.NoError(t, err)

	res := resumeOneConv(convID)
	assert.Equal(t, "skipped:not_active_agent", res.Action)
	assert.Contains(t, res.Detail, "retired")
}

func TestResumeOneConv_OrphanWithoutSessionOrIndexErrors(t *testing.T) {
	setupTestDB(t)
	res := resumeOneConv("orphan-conv-id-12345678")
	assert.Equal(t, "error", res.Action, "action")
	assert.Contains(t, res.Detail, "no resumable session row")
}

func TestResumeOneConv_ConvIndexWithoutProvenanceFailsClosed(t *testing.T) {
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
	require.Equal(t, "error", res.Action, "detail=%s", res.Detail)
	assert.Contains(t, res.Detail, "no resumable session row")
	assert.Empty(t, rec.convID)
}

func TestResumeOneConv_CodexNativeStoreWithoutProvenanceFailsClosed(t *testing.T) {
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
	require.Equal(t, "error", res.Action, "detail=%s", res.Detail)
	assert.Contains(t, res.Detail, "no resumable session row")
	assert.Empty(t, rec.convID)
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
	require.NoError(t, os.MkdirAll(gone, 0o755))
	physicalGone := physicalTestPath(t, gone)
	saveResumeSession(t, convID, gone, harness.DefaultName)
	require.NoError(t, os.Remove(gone))
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: convID, ProjectPath: gone, IndexedAt: time.Now(),
	}))

	res := resumeOneConv(convID)
	assert.Equal(t, "error:missing_cwd", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, physicalGone, res.Detail,
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
	require.NoError(t, os.MkdirAll(gone, 0o755))
	physicalGone := physicalTestPath(t, gone)
	saveResumeSession(t, convID, gone, harness.DefaultName)
	require.NoError(t, os.Remove(gone))

	res := resumeOneConvWithTrustRoot(convID, true)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.DirExists(t, gone, "the recreate opt-in must create the empty launch dir")
	assert.Equal(t, physicalGone, rec.cwd, "the resumed agent must launch into the recreated dir")
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
	require.NoError(t, os.MkdirAll(gone, 0o755))
	physicalGone := physicalTestPath(t, gone)
	saveResumeSession(t, convID, gone, harness.DefaultName)
	require.NoError(t, os.Remove(gone))
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
	assert.Equal(t, physicalGone, resp["detail"], "resp=%v", resp)
	assert.NoDirExists(t, gone)
	assert.Empty(t, rec.convID, "the plain resume must not spawn")

	// With ?recreate=1: the dir is recreated empty and the agent resumes.
	resp = resumePost("?recreate=1")
	assert.Equal(t, "resumed", resp["action"], "resp=%v", resp)
	assert.DirExists(t, gone)
	assert.Equal(t, physicalGone, rec.cwd)
}

func TestHandleAgentResume_AgentCannotRecreateMissingDir(t *testing.T) {
	setupTestDB(t)
	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const convID = "agent-recreate-target-12345678"
	require.NoError(t, os.MkdirAll(gone, 0o755))
	saveResumeSession(t, convID, gone, harness.DefaultName)
	require.NoError(t, os.Remove(gone))
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
	effectiveSandbox                                                                         *sandboxpolicy.Snapshot
	spawnErr                                                                                 error
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
	s.effectiveSandbox = args.EffectiveSandbox
	return s.spawnErr
}

func TestResumeOneConv_SessionProvenanceUsesClaudeHarness(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	cwd := t.TempDir()
	physicalCwd := physicalTestPath(t, cwd)
	const convID = "claude-conv-id-12345678"
	saveResumeSession(t, convID, cwd, harness.DefaultName)

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, physicalCwd, rec.cwd)
	assert.True(t, rec.harness == "" || strings.EqualFold(rec.harness, harness.DefaultName))
}

func TestResumeOneConv_UsesDaemonOwnedFilesystemPin(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	const convID = "unproved-group-member-conv-12345678"
	saveResumeSession(t, convID, t.TempDir(), harness.DefaultName)

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.NotEmpty(t, rec.cwdWriteProof,
		"daemon must bind the verified target cwd through the child launch")
}

func TestResumeOneConv_RestoresPreviousSandboxSnapshotWhenLaunchFails(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	rec.spawnErr = errors.New("launch reservation lost")
	const convID = "failed-resume-sandbox-conv-12345678"
	row := saveResumeSession(t, convID, t.TempDir(), harness.DefaultName)
	row.SandboxMode = harness.ClaudeSandboxOn
	require.NoError(t, db.SaveSession(row))

	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:        "changing-policy",
		Environment: []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "old"}},
		ReadBaselineExclusions: []string{
			sandboxpolicy.ReadExclusionHome,
			sandboxpolicy.ReadExclusionSSH,
		},
	})
	require.NoError(t, err)
	oldEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name:        "changing-policy",
		Environment: []sandboxpolicy.EnvironmentEntry{{Name: "POLICY_VERSION", Value: "old"}},
		ReadBaselineExclusions: []string{
			sandboxpolicy.ReadExclusionHome,
			sandboxpolicy.ReadExclusionSSH,
		},
	}})
	require.NoError(t, err)
	previous := sandboxpolicy.NewSnapshot(oldEffective, []sandboxpolicy.AppliedProfile{{
		Scope: sandboxpolicy.ScopeExplicit, ID: profileID, Name: "changing-policy",
	}})
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &previous))
	profile, err := db.GetSandboxProfileByID(profileID)
	require.NoError(t, err)
	profile.Environment[0].Value = "new"
	profile.ReadBaselineExclusions = []string{sandboxpolicy.ReadExclusionCloud}
	require.NoError(t, db.UpdateSandboxProfile(profile))

	res := resumeOneConv(convID)
	require.Equal(t, "error", res.Action)
	assert.Contains(t, res.Detail, "launch reservation lost")
	require.NotNil(t, rec.effectiveSandbox)
	assert.Equal(t, "new", rec.effectiveSandbox.Effective.Environment[0].Value)
	assert.Equal(t, []string{sandboxpolicy.ReadExclusionCloud}, rec.effectiveSandbox.Effective.ReadBaselineExclusions)
	assert.Equal(t, map[string][]sandboxpolicy.ProfileSource{
		sandboxpolicy.ReadExclusionCloud: {{Scope: sandboxpolicy.ScopeExplicit, Profile: "changing-policy"}},
	}, rec.effectiveSandbox.Effective.Provenance.ReadBaselineExclusions)

	persisted, err := db.AgentEffectiveSandboxConfigForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, "old", persisted.Effective.Environment[0].Value,
		"a failed launch must not commit policy for a pane that never started")
	assert.Equal(t, []string{sandboxpolicy.ReadExclusionHome, sandboxpolicy.ReadExclusionSSH}, persisted.Effective.ReadBaselineExclusions,
		"a failed launch restores the exact previous snapshot rather than persisting the newly resolved profile")
}
