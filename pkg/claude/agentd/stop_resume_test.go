package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
// groups.members.stop behaviour exactly — single-conv variant should be
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

func TestResumeOneConv_CodexAppServerRequiresVerifiedReadiness(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	previousPaneStarts := codexNativePaneStarts
	codexNativePaneStarts = func() ([]byte, error) { return nil, nil }
	t.Cleanup(func() { codexNativePaneStarts = previousPaneStarts })
	const convID = "codex-appserver-resume-12345678"
	row := saveResumeSession(t, convID, t.TempDir(), harness.CodexName)
	require.NoError(t, db.SetSessionCodexAppServer(row.ID, true))

	oldAwait := awaitCodexAppServerReady
	awaitCodexAppServerReady = func(got string) bool {
		assert.Equal(t, convID, got)
		return false
	}
	t.Cleanup(func() { awaitCodexAppServerReady = oldAwait })

	res := resumeOneConv(convID)
	assert.Equal(t, "error", res.Action)
	assert.Contains(t, res.Detail, "did not become ready")
	assert.Equal(t, convID, rec.convID, "the resume launch still reaches the readiness gate")
	assert.True(t, rec.codexAppServer)
}

func TestHandleAgentResumeExplicitSendKeysPersistsCodexRollback(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	const convID = "codex-appserver-rollback-12345678"
	row := saveResumeSession(t, convID, t.TempDir(), harness.CodexName)
	require.NoError(t, db.SetSessionCodexAppServer(row.ID, true))
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	profile, err := db.RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	profile.Version = db.RelaunchProfileVersion
	selected := true
	profile.CodexAppServer = &selected
	fast := true
	profile.FastMode = &fast
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: "rollback-generation", LaunchID: row.ID, AgentID: agentID,
		ConvID: convID, ThreadID: convID, SocketPath: filepath.Join(t.TempDir(), "app.sock"),
		ServerPID: os.Getpid(), CodexVersion: "0.147.0", State: db.CodexAppServerDead,
		CreatedAt: row.CreatedAt.Add(-time.Second),
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/"+convID+"/resume?send_keys=1", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1, HumanTokenValid: true}))
	handleAgentResume(w, r, convID)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.False(t, rec.codexAppServer)
	recorded, err := db.RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, recorded.CodexAppServer)
	assert.False(t, *recorded.CodexAppServer)
	require.NotNil(t, recorded.FastMode)
	assert.True(t, *recorded.FastMode, "compatibility rollback must preserve unrelated relaunch intent")
}

func TestCodexDriveRollbackRestoresNativeProfileWhenSelectionWriteFails(t *testing.T) {
	setupTestDB(t)
	const convID = "codex-appserver-rollback-failure-12345678"
	row := saveResumeSession(t, convID, t.TempDir(), harness.CodexName)
	require.NoError(t, db.SetSessionCodexAppServer(row.ID, true))
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	profile, err := db.RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	selected := true
	profile.Version = db.RelaunchProfileVersion
	profile.CodexAppServer = &selected
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))
	runtime := db.CodexAppServerRuntime{
		Generation: "rollback-failure-generation", LaunchID: row.ID, AgentID: agentID,
		ConvID: convID, ThreadID: convID, SocketPath: filepath.Join(t.TempDir(), "app.sock"),
		State: db.CodexAppServerDead, CreatedAt: row.CreatedAt.Add(-time.Second),
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	native := db.CodexNativePermissionProfile{
		Generation: runtime.Generation, ProfileName: "tclaude-agent-8888888888888888",
		ProfileTOML: "saved generated profile",
	}
	require.NoError(t, db.UpsertCodexNativePermissionProfile(native))

	previousSet := setAgentCodexAppServerSelectionForConv
	previousUnregister := unregisterCodexNativePermissionProfile
	previousRestore := restoreCodexNativePermissionProfile
	setAgentCodexAppServerSelectionForConv = func(string, bool, string) error {
		return errors.New("injected selection write failure")
	}
	unregisterCodexNativePermissionProfile = func(generation string) error {
		assert.Equal(t, runtime.Generation, generation)
		return db.DeleteCodexNativePermissionProfile(generation)
	}
	restored := false
	restoreCodexNativePermissionProfile = func(generation, name, profileTOML string) error {
		restored = true
		assert.Equal(t, native.Generation, generation)
		assert.Equal(t, native.ProfileName, name)
		assert.Equal(t, native.ProfileTOML, profileTOML)
		return db.UpsertCodexNativePermissionProfile(native)
	}
	t.Cleanup(func() {
		setAgentCodexAppServerSelectionForConv = previousSet
		unregisterCodexNativePermissionProfile = previousUnregister
		restoreCodexNativePermissionProfile = previousRestore
	})

	res := resumeOneConvWithCodexRollbackLocked(convID, false)
	assert.Equal(t, "error:codex_drive_rollback", res.Action)
	assert.Contains(t, res.Detail, "injected selection write failure")
	assert.True(t, restored)
	recorded, err := db.RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, recorded.CodexAppServer)
	assert.True(t, *recorded.CodexAppServer, "failed transition must retain the app-server drive")
	restoredProfile, err := db.GetCodexNativePermissionProfile(runtime.Generation)
	require.NoError(t, err)
	require.NotNil(t, restoredProfile, "failed transition must restore the resumable native profile")
}

func TestHandleAgentResumeExplicitSendKeysDoesNotChangeOnlineTarget(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	const convID = "codex-online-rollback-12345678"
	row := saveResumeSession(t, convID, t.TempDir(), harness.CodexName)
	row.Status = session.StatusIdle
	row.TmuxSession = "codex-online-pane"
	require.NoError(t, db.SaveSession(row))
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	profile, err := db.RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	selected := true
	profile.CodexAppServer = &selected
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: "online-rollback-generation", LaunchID: row.ID, AgentID: agentID,
		ConvID: convID, ThreadID: convID, SocketPath: filepath.Join(t.TempDir(), "app.sock"),
		State: db.CodexAppServerDead, CreatedAt: row.CreatedAt.Add(-time.Second),
	}))
	tmux := &commandRecordingTmux{}
	previousTmux := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/"+convID+"/resume?send_keys=1", nil)
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 1, HumanTokenValid: true}))
	handleAgentResume(w, r, convID)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	recorded, err := db.RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, recorded.CodexAppServer)
	assert.True(t, *recorded.CodexAppServer, "online target posture must remain unchanged")
	assert.Zero(t, rec.resumeCalls)
	for _, command := range tmux.snapshot() {
		assert.NotEqual(t, "kill-session", command[0], "rollback eligibility must not tear down an online pane")
	}
}

func TestStopCodexAppServerTerminalRuntimeNeverSignalsRetainedPID(t *testing.T) {
	setupTestDB(t)
	const convID = "terminal-runtime-12345678"
	runtime := db.CodexAppServerRuntime{
		Generation: "terminal-generation", LaunchID: "terminal-launch", AgentID: "agent",
		ConvID: convID, ThreadID: convID, SocketPath: filepath.Join(t.TempDir(), "app.sock"),
		ServerPID: 424242, State: db.CodexAppServerDead,
	}
	require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	previousSignal := signalCodexAppServerProcess
	signals := 0
	signalCodexAppServerProcess = func(int, syscall.Signal) error { signals++; return nil }
	t.Cleanup(func() { signalCodexAppServerProcess = previousSignal })

	stopCodexAppServerRuntimeForConv(convID)
	assert.Zero(t, signals, "a terminal row's retained PID may have been recycled")
}

func TestCodexDriveRollbackApprovalDescribesDurableDriveChange(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const (
		target = "rollback-approval-target-12345678"
		caller = "rollback-approval-caller-12345678"
	)
	_, _, err := db.EnsureAgentForConv(target, "target")
	require.NoError(t, err)
	_, _, err = db.EnsureAgentForConv(caller, "caller")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(caller, PermAgentResume, "test"))
	previousApproval := RequestHumanApprovalImpl
	var captured *approvalRequest
	RequestHumanApprovalImpl = func(req *approvalRequest, _ string) bool {
		captured = req
		return false
	}
	t.Cleanup(func() { RequestHumanApprovalImpl = previousApproval })

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/"+target+"/resume?send_keys=1", nil)
	r.Header.Set("X-Tclaude-Ask-Human", "5s")
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{
		PID: 1, HasClaudeAncestor: true, ConvID: caller,
	}))
	handleAgentResume(w, r, target)
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, captured)
	assert.Contains(t, captured.bodyLabel, "app-server → send-keys")
	assert.Contains(t, captured.bodyPreview, "Durably change")
	assert.Contains(t, captured.bodyPreview, "tear down")
	assert.NotContains(t, captured.bodyPreview, "working-directory")
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
				owner, PermGroupsMembersResume, db.PermEffectDeny, "test"))
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

func TestStopOneConv_DefaultWrapperArmsForceStopIntent(t *testing.T) {
	w := testharness.New(t)
	prevTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = prevTmux })
	const (
		convID     = "default-force-stop-conv-12345678"
		sessionID  = "default-force-stop-session"
		tmuxName   = "default-force-stop-tmux"
		generation = "23232323232323232323232323232323"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: tmuxName,
		Status: "working", CreatedAt: time.Now(),
	}))
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
	w.Tmux.MarkAlive(tmuxName)

	res := stopOneConv(convID, true)
	assert.Equal(t, "killed", res.Action)
	database, err := db.Open()
	require.NoError(t, err)
	var action, intentGeneration string
	require.NoError(t, database.QueryRow(`SELECT exit_intent, exit_intent_generation
		FROM sessions WHERE id = ?`, sessionID).Scan(&action, &intentGeneration))
	assert.Equal(t, db.AgentExitActionForceStop, action)
	assert.Equal(t, generation, intentGeneration)
}

// resumeOneConv must report `skipped:no_conv_id` when called with an
// empty conv-id. This mirrors the bulk groups.members.resume placeholder
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
	assert.Contains(t, res.Detail, "no trustworthy recovery target")
	assert.Contains(t, res.Detail, "conversation no longer exists")
}

func TestResumeOneConv_StaleConvIndexWithoutHarnessConversationFails(t *testing.T) {
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
	assert.Contains(t, res.Detail, "conversation no longer exists")
	assert.Empty(t, rec.convID, "a stale conv_index row must not become launch authority")
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

	res := resumeOneConvRecreate(convID, true)
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
	convID, cwd, cwdWriteProof, effort, model, harness, sandbox, sandboxImplementation, approval,
	askUserQuestionTimeout, codexGitCommonDir, codexStateRoot string
	autoReview, remoteControl, autoMemory, codexGitCommonDirPinned, codexAppServer,
	codexAppServerExistingThread bool
	effectiveSandbox      *sandboxpolicy.Snapshot
	spawnErr              error
	resumeCalls, newCalls int
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
	s.newCalls++
	return nil
}

func (s *recordingResumeSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	s.resumeCalls++
	s.convID = args.ConvID
	s.cwd = args.Cwd
	s.cwdWriteProof = args.CwdWriteProof
	s.effort = args.Effort
	s.model = args.Model
	s.harness = args.Harness
	s.sandbox = args.Sandbox
	s.sandboxImplementation = args.SandboxImplementation
	s.approval = args.Approval
	s.autoReview = args.AutoReview
	s.askUserQuestionTimeout = args.AskUserQuestionTimeout
	s.remoteControl = args.RemoteControl
	s.autoMemory = args.AutoMemory
	s.codexGitCommonDir = args.CodexGitCommonDir
	s.codexGitCommonDirPinned = args.CodexGitCommonDirPinned
	s.codexAppServer = args.CodexAppServer
	s.codexAppServerExistingThread = args.CodexAppServerExistingThread
	s.codexStateRoot = args.CodexStateRoot
	s.effectiveSandbox = args.EffectiveSandbox
	return s.spawnErr
}

func TestResumeOneConv_CodexCrashRecoveryKeepsPersistedStateRootAndThread(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	home := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "codex-state")
	require.NoError(t, os.MkdirAll(stateRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stateRoot, "config.toml"),
		[]byte("service_tier = \"fast\"\n"), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "daemon-wrong-state"))
	const convID = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	row := saveResumeSession(t, convID, t.TempDir(), harness.CodexName)
	row.Status = session.StatusExited
	require.NoError(t, db.SaveSession(row))

	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	profile, err := db.RecordedLaunchPostureForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	profile.Version = db.RelaunchProfileVersion
	source := codexStateRootSourceCodexHome
	profile.CodexStateRoot = &stateRoot
	profile.CodexStateRootSource = &source
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, convID, rec.convID, "recovery must resume the original thread id")
	assert.Equal(t, stateRoot, rec.codexStateRoot, "wrapper must use the original Codex store")
	assert.True(t, rec.codexAppServerExistingThread,
		"durable recovery must select the exact existing-thread bootstrap contract")
	assert.Equal(t, 1, rec.resumeCalls)
	assert.Zero(t, rec.newCalls, "recovery must not create an empty replacement generation")
	updated, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Nil(t, updated.FastMode, "resume must keep inherited mode unpinned")
	require.NotNil(t, updated.FastModeAtLaunch)
	assert.True(t, *updated.FastModeAtLaunch,
		"resume refreshes the dashboard baseline from the launch's main config")
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

func TestResumeOneConv_TemporaryOffDisablesTclaudeOuterLayer(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)

	const convID = "temporary-off-tclaude-layer-12345678"
	row := saveResumeSession(t, convID, t.TempDir(), harness.DefaultName)
	normalMode := harness.ClaudeSandboxOn
	normalImplementation := string(sandboxpolicy.ImplementationTclaudeLayer)
	approval := "default"
	row.HarnessBuiltinMode = normalMode
	row.SandboxImplementation = normalImplementation
	row.ApprovalPolicy = approval
	require.NoError(t, db.SaveSession(row))

	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, HarnessBuiltinMode: &normalMode,
		SandboxImplementation: &normalImplementation, ApprovalPolicy: &approval,
	}))
	override := harness.ClaudeSandboxOff
	require.NoError(t, db.SetTemporaryHarnessBuiltinMode(
		agentID, normalMode, normalImplementation, "", &override,
	))

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Equal(t, harness.ClaudeSandboxOff, rec.sandbox)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin),
		rec.sandboxImplementation,
		"temporary off must omit the tclaude outer wrapper as well as disabling Claude's sandbox")
}

func TestResumeOneConv_DoesNotRequireFilesystemPin(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	const convID = "unproved-group-member-conv-12345678"
	saveResumeSession(t, convID, t.TempDir(), harness.DefaultName)

	res := resumeOneConv(convID)
	require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
	assert.Empty(t, rec.cwdWriteProof,
		"an authorized lifecycle continuation must not re-prove its inherited cwd")
}

func TestResumeOneConv_UsesDurableProfilesAfterAllSessionsArePruned(t *testing.T) {
	t.Cleanup(SetWaitTimingsForTest(100*time.Millisecond, time.Millisecond))
	t.Cleanup(WaitForBackgroundForTest)
	for _, tc := range []struct {
		name, harnessName, sandbox, approval, model, effort, askTimeout string
		remoteControl, autoMemory                                       bool
	}{
		{
			name: "claude", harnessName: harness.DefaultName,
			sandbox: harness.ClaudeSandboxOn, approval: "auto",
			model: "claude-sonnet-4-6", effort: "high", askTimeout: "5m",
			remoteControl: true, autoMemory: true,
		},
		{
			name: "codex", harnessName: harness.CodexName,
			sandbox: harness.SandboxWorkspaceWrite, approval: harness.ApprovalUntrusted,
			model: "gpt-5-codex", effort: "high",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			rec := installRecordingResumeSpawner(t)
			convID := "pruned-profile-" + tc.name + "-12345678"
			sessionID := "pruned-profile-session-" + tc.name
			cwd := t.TempDir()
			physicalCwd := physicalTestPath(t, cwd)
			_, _, err := db.EnsureAgentForConv(convID, "test")
			require.NoError(t, err)
			row := saveResumeSession(t, convID, cwd, tc.harnessName)
			originalSessionID := row.ID
			row.ID = sessionID
			row.HarnessBuiltinMode = tc.sandbox
			row.ApprovalPolicy = tc.approval
			row.AskUserQuestionTimeout = tc.askTimeout
			require.NoError(t, db.SaveSession(row))
			require.NoError(t, db.UpdateSessionModelID(sessionID, tc.model))
			require.NoError(t, db.UpdateSessionEffort(sessionID, tc.effort))
			require.NoError(t, db.SetSessionRemoteControl(sessionID, tc.remoteControl))
			require.NoError(t, db.SetSessionAutoMemory(sessionID, tc.autoMemory))
			require.NoError(t, db.DeleteSession(originalSessionID))
			require.NoError(t, db.DeleteSession(sessionID))
			rows, err := db.FindSessionsByConvID(convID)
			require.NoError(t, err)
			require.Empty(t, rows, "test setup must prune every process-history row")

			res := resumeOneConv(convID)
			require.Equal(t, "resumed", res.Action, "detail=%s", res.Detail)
			assert.Equal(t, physicalCwd, rec.cwd)
			assert.Equal(t, tc.harnessName, rec.harness)
			assert.Equal(t, tc.sandbox, rec.sandbox)
			assert.Equal(t, tc.approval, rec.approval)
			assert.Equal(t, tc.model, rec.model)
			assert.Equal(t, tc.effort, rec.effort)
			assert.Equal(t, tc.askTimeout, rec.askUserQuestionTimeout)
			assert.Equal(t, tc.remoteControl, rec.remoteControl)
			assert.Equal(t, tc.autoMemory, rec.autoMemory)
		})
	}
}

func TestResumeOneConv_RestoresPreviousSandboxSnapshotWhenLaunchFails(t *testing.T) {
	setupTestDB(t)
	rec := installRecordingResumeSpawner(t)
	rec.spawnErr = errors.New("launch reservation lost")
	const convID = "failed-resume-sandbox-conv-12345678"
	row := saveResumeSession(t, convID, t.TempDir(), harness.DefaultName)
	row.HarnessBuiltinMode = harness.ClaudeSandboxOn
	require.NoError(t, db.SaveSession(row))

	oldDeny, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	newDeny, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:        "changing-policy",
		Environment: []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "old"}},
		Filesystem:  []db.SandboxFilesystemGrant{{Path: oldDeny, Access: sandboxpolicy.AccessDeny}},
	})
	require.NoError(t, err)
	oldEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name:        "changing-policy",
		Environment: []sandboxpolicy.EnvironmentEntry{{Name: "POLICY_VERSION", Value: "old"}},
		Filesystem:  []sandboxpolicy.FilesystemGrant{{Path: oldDeny, Access: sandboxpolicy.AccessDeny}},
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
	profile.Filesystem = []db.SandboxFilesystemGrant{{Path: newDeny, Access: sandboxpolicy.AccessDeny}}
	require.NoError(t, db.UpdateSandboxProfile(profile))

	res := resumeOneConv(convID)
	require.Equal(t, "error", res.Action)
	assert.Contains(t, res.Detail, "launch reservation lost")
	require.NotNil(t, rec.effectiveSandbox)
	assert.Equal(t, "new", rec.effectiveSandbox.Effective.Environment[0].Value)
	// The current operator-authored profile is passed to the attempted launch;
	// rules removed since the previous launch are not restored.
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{
		{Path: newDeny, Access: sandboxpolicy.AccessDeny},
	}, sortedGrants(rec.effectiveSandbox.Effective.Filesystem, newDeny, oldDeny))

	persisted, err := db.AgentEffectiveSandboxConfigForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, "old", persisted.Effective.Environment[0].Value,
		"a failed launch must not commit policy for a pane that never started")
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{{Path: oldDeny, Access: sandboxpolicy.AccessDeny}}, persisted.Effective.Filesystem,
		"a failed launch restores the exact previous snapshot rather than persisting the newly resolved profile")
}

// sortedGrants returns the grants in the caller's stated order so an assertion
// does not depend on canonical path sorting of two temp dirs.
func sortedGrants(in []sandboxpolicy.FilesystemGrant, order ...string) []sandboxpolicy.FilesystemGrant {
	out := make([]sandboxpolicy.FilesystemGrant, 0, len(in))
	for _, path := range order {
		for _, grant := range in {
			if grant.Path == path {
				out = append(out, grant)
			}
		}
	}
	return out
}
