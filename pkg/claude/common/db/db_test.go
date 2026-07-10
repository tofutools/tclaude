package db

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ResetForTest()
}

func TestOpenAndMigrate(t *testing.T) {
	setupTestDB(t)
	db, err := Open()
	require.NoError(t, err, "Open")
	require.NotNil(t, db, "Open returned nil db")

	// Verify schema version
	var ver int
	require.NoError(t, db.QueryRow("SELECT version FROM schema_version").Scan(&ver), "schema_version query")
	require.Equal(t, currentVersion, ver, "expected version %d, got %d", currentVersion, ver)
}

func TestSessionCRUD(t *testing.T) {
	setupTestDB(t)

	now := time.Now().Truncate(time.Millisecond)
	s := &SessionRow{
		ID:            "test-001",
		TmuxSession:   "tmux-001",
		PID:           12345,
		Cwd:           "/tmp/project",
		ConvID:        "conv-abc",
		Status:        "idle",
		SubagentCount: 1,
		CreatedAt:     now,
	}

	// Save
	require.NoError(t, SaveSession(s), "SaveSession")

	// Load
	loaded, err := LoadSession("test-001")
	require.NoError(t, err, "LoadSession")
	assert.Equal(t, "conv-abc", loaded.ConvID, "ConvID")
	assert.Equal(t, 12345, loaded.PID, "PID")

	// FindByConvID
	found, err := FindSessionByConvID("conv-abc")
	require.NoError(t, err, "FindSessionByConvID")
	if assert.NotNil(t, found, "FindSessionByConvID returned nil") {
		assert.Equal(t, "test-001", found.ID, "FindSessionByConvID id")
	}

	// FindByConvID (miss)
	notFound, err := FindSessionByConvID("nonexistent")
	require.NoError(t, err, "FindSessionByConvID miss")
	assert.Nil(t, notFound, "expected nil for nonexistent conv ID")

	// Exists
	exists, err := SessionExists("test-001")
	require.NoError(t, err, "SessionExists")
	assert.True(t, exists, "expected session to exist")

	// List
	all, err := ListSessions()
	require.NoError(t, err, "ListSessions")
	assert.Len(t, all, 1, "ListSessions count")

	// Delete
	require.NoError(t, DeleteSession("test-001"), "DeleteSession")
	exists, _ = SessionExists("test-001")
	assert.False(t, exists, "session should not exist after delete")
}

func TestCleanupOldExited(t *testing.T) {
	setupTestDB(t)

	// Create an "exited" session with old timestamp
	old := &SessionRow{
		ID:        "old-exited",
		Status:    "exited",
		CreatedAt: time.Now().Add(-48 * time.Hour),
		UpdatedAt: time.Now().Add(-48 * time.Hour),
	}
	// Save manually to set UpdatedAt in the past
	db, _ := Open()
	_, err := db.Exec(`INSERT INTO sessions
		(id, tmux_session, pid, cwd, conv_id, status, status_detail, auto_registered, created_at, updated_at)
		VALUES (?, '', 0, '', '', 'exited', '', 0, ?, ?)`,
		old.ID, old.CreatedAt.Format(time.RFC3339Nano), old.UpdatedAt.Format(time.RFC3339Nano))
	require.NoError(t, err, "insert old session")

	// Create a fresh "exited" session
	fresh := &SessionRow{
		ID:        "fresh-exited",
		Status:    "exited",
		CreatedAt: time.Now(),
	}
	require.NoError(t, SaveSession(fresh), "SaveSession fresh")

	// Cleanup with 24h threshold
	deleted, err := CleanupOldExited(24 * time.Hour)
	require.NoError(t, err, "CleanupOldExited")
	assert.Equal(t, int64(1), deleted, "deleted count")

	// Fresh one should still exist
	exists, _ := SessionExists("fresh-exited")
	assert.True(t, exists, "fresh-exited should still exist")
}

func TestMaxUpdatedAt(t *testing.T) {
	setupTestDB(t)

	// Empty table
	ts, err := MaxUpdatedAt()
	require.NoError(t, err, "MaxUpdatedAt empty")
	assert.True(t, ts.IsZero(), "expected zero time for empty table, got %v", ts)

	// Add a session
	s := &SessionRow{ID: "max-test", Status: "idle", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s))

	ts, err = MaxUpdatedAt()
	require.NoError(t, err, "MaxUpdatedAt")
	assert.False(t, ts.IsZero(), "MaxUpdatedAt should not be zero after insert")
}

func TestContextSnapshotRoundTrip(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "snap-001", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s), "SaveSession")

	// Default values: all zero before the statusbar hook fires.
	got, err := GetContextSnapshot("snap-001")
	require.NoError(t, err, "GetContextSnapshot empty")
	require.True(t, got.ContextPct == 0 && got.TokensInput == 0 && got.TokensOutput == 0 && got.ContextWindowSize == 0, "default snapshot non-zero: %+v", got)

	// Statusbar tick: write the full snapshot atomically.
	require.NoError(t, UpdateContextSnapshot("snap-001", 19.0, 180_000, 10_000, 1_000_000), "UpdateContextSnapshot")
	got, err = GetContextSnapshot("snap-001")
	require.NoError(t, err, "GetContextSnapshot populated")
	assert.Equal(t, 19.0, got.ContextPct, "ContextPct")
	assert.Equal(t, int64(180_000), got.TokensInput, "TokensInput")
	assert.Equal(t, int64(10_000), got.TokensOutput, "TokensOutput")
	assert.Equal(t, int64(1_000_000), got.ContextWindowSize, "ContextWindowSize")

	// Backwards-compat: UpdateContextPct still works alongside the
	// snapshot fields. Writing pct alone shouldn't zero the abs fields.
	require.NoError(t, UpdateContextPct("snap-001", 21.5), "UpdateContextPct")
	got, _ = GetContextSnapshot("snap-001")
	assert.Equal(t, 21.5, got.ContextPct, "after pct-only update: ContextPct")
	assert.Equal(t, int64(180_000), got.TokensInput, "after pct-only update: TokensInput (preserved)")
}

// TestUpdateSessionModelRoundTrip covers the per-agent model the
// statusline hook records (sessions.model) and the dashboard reads back
// via GetContextSnapshot.Model: a fresh row reads ”, a write round-trips,
// an empty-model write is a no-op (never blanks a good value), and a
// SaveSession hook tick — which does NOT list the model column — leaves
// it intact, the same out-of-band guarantee the context columns rely on.
func TestUpdateSessionModelRoundTrip(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "model-001", Status: "idle", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s), "SaveSession")

	// Default: no model before the statusbar hook fires.
	got, err := GetContextSnapshot("model-001")
	require.NoError(t, err, "GetContextSnapshot empty")
	assert.Equal(t, "", got.Model, "model defaults to empty")

	// Statusbar tick records the model.
	require.NoError(t, UpdateSessionModel("model-001", "Opus 4.8"), "UpdateSessionModel")
	got, err = GetContextSnapshot("model-001")
	require.NoError(t, err, "GetContextSnapshot populated")
	assert.Equal(t, "Opus 4.8", got.Model, "model round-trips")

	// An empty model is a no-op — a stray render without one must not
	// blank a good value.
	require.NoError(t, UpdateSessionModel("model-001", ""), "empty model is a no-op")
	got, _ = GetContextSnapshot("model-001")
	assert.Equal(t, "Opus 4.8", got.Model, "empty write preserves the model")

	// A state-tracking hook tick (SaveSession) must not clobber the model
	// — it isn't one of the columns SaveSession owns.
	s.Status = "working"
	require.NoError(t, SaveSession(s), "SaveSession hook tick")
	got, _ = GetContextSnapshot("model-001")
	assert.Equal(t, "Opus 4.8", got.Model, "model preserved across a SaveSession tick")

	// A new model (e.g. the user switched with /model) updates normally.
	require.NoError(t, UpdateSessionModel("model-001", "Sonnet 4.6"), "switch model")
	got, _ = GetContextSnapshot("model-001")
	assert.Equal(t, "Sonnet 4.6", got.Model, "model updates on switch")
}

// Codex reports one active model slug for both the dashboard label and the
// resume-safe model ID. A trigger that rejects any UPDATE exposing different
// values proves UpdateSessionModelSlug changes both columns in one statement;
// the former pair of independent setters would fail on its first UPDATE.
func TestUpdateSessionModelSlugIsAtomic(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "codex-model-001", Status: "idle", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s), "SaveSession")

	d, err := Open()
	require.NoError(t, err, "Open")
	_, err = d.Exec(`CREATE TRIGGER require_consistent_model_slug
		BEFORE UPDATE OF model, model_id ON sessions
		WHEN NEW.model <> NEW.model_id
		BEGIN
			SELECT RAISE(ABORT, 'model slug columns diverged');
		END`)
	require.NoError(t, err, "install consistency trigger")

	require.NoError(t, UpdateSessionModelSlug("codex-model-001", "gpt-5.5"),
		"one statement must advance both model columns")
	got, err := GetContextSnapshot("codex-model-001")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, "gpt-5.5", got.Model)
	assert.Equal(t, "gpt-5.5", got.ModelID)

	require.NoError(t, UpdateSessionModelSlug("codex-model-001", ""), "empty slug is a no-op")
	got, err = GetContextSnapshot("codex-model-001")
	require.NoError(t, err, "GetContextSnapshot after empty slug")
	assert.Equal(t, "gpt-5.5", got.Model)
	assert.Equal(t, "gpt-5.5", got.ModelID)
}

// TestUpdateContextSnapshotEmptyDoesNotClobber locks down the guard
// that fixes the dashboard context-meter flicker: Claude Code emits
// statusline renders with an empty context_window block, which the
// hook turns into an all-zero UpdateContextSnapshot call. That call
// must be skipped — never allowed to overwrite a good snapshot back to
// zero.
func TestUpdateContextSnapshotEmptyDoesNotClobber(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "clob-001", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s), "SaveSession")

	// A populated statusline render writes a good snapshot.
	require.NoError(t, UpdateContextSnapshot("clob-001", 15.0, 146_000, 2_000, 1_000_000), "good snapshot")

	// An empty render — all-zero — must be a no-op, not a clobber.
	require.NoError(t, UpdateContextSnapshot("clob-001", 0, 0, 0, 0), "empty snapshot")

	got, err := GetContextSnapshot("clob-001")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, 15.0, got.ContextPct, "ContextPct preserved through an empty write")
	assert.Equal(t, int64(146_000), got.TokensInput, "TokensInput preserved")
	assert.Equal(t, int64(2_000), got.TokensOutput, "TokensOutput preserved")
	assert.Equal(t, int64(1_000_000), got.ContextWindowSize, "ContextWindowSize preserved")

	// A populated render after the empty one still updates normally.
	require.NoError(t, UpdateContextSnapshot("clob-001", 22.0, 200_000, 5_000, 1_000_000), "next good snapshot")
	got, _ = GetContextSnapshot("clob-001")
	assert.Equal(t, 22.0, got.ContextPct, "ContextPct updates on the next populated render")
}

// TestSaveSessionPreservesOutOfBandColumns locks down the dashboard
// context-meter dropout fix. The context-window columns and the
// nudge bookkeeping are owned by the statusline hook
// (UpdateContextSnapshot) and the context-nudge path — NOT by SaveSession.
// A state-tracking hook (Stop -> idle, UserPromptSubmit, every
// PreToolUse tick) calls SaveSession to update status, and that write
// must leave the out-of-band columns alone. It used to wipe them:
// INSERT OR REPLACE re-created the whole row, resetting every unlisted
// column to its DEFAULT 0 on every hook tick.
func TestSaveSessionPreservesOutOfBandColumns(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "keep-001", ConvID: "conv-keep", Status: "working", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s), "initial SaveSession")

	// The statusline hook and the context-nudge path write out-of-band
	// columns SaveSession does not own.
	require.NoError(t, UpdateContextSnapshot("keep-001", 24.0, 241_000, 5_000, 1_000_000), "context snapshot")
	require.NoError(t, SetNudgedPct("keep-001", 50), "SetNudgedPct")

	// A state-tracking hook re-saves the row to flip status (e.g. the
	// Stop hook marking the agent idle). None of the out-of-band
	// columns may be disturbed.
	s.Status = "idle"
	s.StatusDetail = ""
	require.NoError(t, SaveSession(s), "state-update SaveSession")

	snap, err := GetContextSnapshot("keep-001")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, 24.0, snap.ContextPct, "context_pct survives a state-update SaveSession")
	assert.Equal(t, int64(241_000), snap.TokensInput, "tokens_input survives")
	assert.Equal(t, int64(5_000), snap.TokensOutput, "tokens_output survives")
	assert.Equal(t, int64(1_000_000), snap.ContextWindowSize, "context_window_size survives")
	nudged, err := GetNudgedPct("keep-001")
	require.NoError(t, err, "GetNudgedPct")
	assert.Equal(t, 50.0, nudged, "nudged_pct survives")

	// The status update itself still landed.
	reloaded, err := LoadSession("keep-001")
	require.NoError(t, err, "LoadSession")
	assert.Equal(t, "idle", reloaded.Status, "status update applied")
}

func TestContextPctRoundTrip(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "ctx-001", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s), "SaveSession")

	// Default value: 0 right after insert.
	pct, err := GetContextPct("ctx-001")
	require.NoError(t, err, "GetContextPct")
	require.Equal(t, 0.0, pct, "default context_pct")

	// Update context_pct via the statusbar path.
	require.NoError(t, UpdateContextPct("ctx-001", 47.0), "UpdateContextPct")
	pct, _ = GetContextPct("ctx-001")
	require.Equal(t, 47.0, pct, "context_pct after UpdateContextPct")

	// ResetCompact (called on PostCompact) zeroes context_pct back out.
	require.NoError(t, ResetCompact("ctx-001"), "ResetCompact")
	pct, _ = GetContextPct("ctx-001")
	require.Equal(t, 0.0, pct, "context_pct after ResetCompact")
}

// TestNudgedPct exercises the new sessions.nudged_pct column: it
// defaults to 0, SetNudgedPct stamps the highest-fired threshold,
// and ResetCompact wipes it alongside context_pct so post-compact
// sessions can be re-nudged from scratch.
func TestNudgedPct(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "nudge-001", CreatedAt: time.Now()}
	require.NoError(t, SaveSession(s), "SaveSession")

	// Default: 0 right after insert.
	got, err := GetNudgedPct("nudge-001")
	require.NoError(t, err, "GetNudgedPct")
	assert.Equal(t, float64(0), got, "default nudged_pct")

	// Stamp a threshold.
	require.NoError(t, SetNudgedPct("nudge-001", 50), "SetNudgedPct")
	got, _ = GetNudgedPct("nudge-001")
	assert.Equal(t, float64(50), got, "after Set(50)")

	// Stamp a higher one.
	require.NoError(t, SetNudgedPct("nudge-001", 70), "SetNudgedPct(70)")
	got, _ = GetNudgedPct("nudge-001")
	assert.Equal(t, float64(70), got, "after Set(70)")

	// ResetCompact also zeroes nudged_pct — post-compact sessions get
	// re-nudged from min_pct upward on the next climb.
	require.NoError(t, UpdateContextPct("nudge-001", 80), "UpdateContextPct")
	require.NoError(t, ResetCompact("nudge-001"), "ResetCompact")
	got, _ = GetNudgedPct("nudge-001")
	assert.Equal(t, float64(0), got, "after ResetCompact, nudged_pct")
	pct, _ := GetContextPct("nudge-001")
	assert.Equal(t, 0.0, pct, "after ResetCompact, context_pct")
}

func TestNotifyState(t *testing.T) {
	setupTestDB(t)

	// No record
	_, found, err := GetNotifyTime("sess-1")
	require.NoError(t, err, "GetNotifyTime")
	assert.False(t, found, "expected no record")

	// Set
	require.NoError(t, SetNotifyTime("sess-1"), "SetNotifyTime")

	// Get
	ts, found, err := GetNotifyTime("sess-1")
	require.NoError(t, err, "GetNotifyTime")
	assert.True(t, found, "expected record")
	assert.LessOrEqual(t, time.Since(ts), 5*time.Second, "notified_at too old: %v", ts)
}

func TestLegacyImport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ResetForTest()

	// Create legacy session files
	sessDir := dir + "/.tclaude/claude-sessions"
	require.NoError(t, os.MkdirAll(sessDir, 0755))
	require.NoError(t, os.WriteFile(sessDir+"/abc12345.json", []byte(`{
		"id": "abc12345",
		"tmuxSession": "abc12345",
		"pid": 999,
		"cwd": "/tmp/test",
		"convId": "conv-legacy",
		"status": "idle",
		"created": "2025-01-01T00:00:00Z",
		"updated": "2025-01-01T12:00:00Z"
	}`), 0644))
	// Create .auto marker
	require.NoError(t, os.WriteFile(sessDir+"/abc12345.auto", []byte("auto-registered"), 0644))
	// Create legacy debug.log
	require.NoError(t, os.WriteFile(sessDir+"/debug.log", []byte("old debug data\n"), 0644))

	// Create legacy notify state
	notifyDir := dir + "/.tclaude/notify-state"
	require.NoError(t, os.MkdirAll(notifyDir, 0755))
	require.NoError(t, os.WriteFile(notifyDir+"/abc12345", []byte(""), 0644))

	// Open triggers migration + import
	_, err := Open()
	require.NoError(t, err, "Open")

	// Verify session was imported
	s, err := LoadSession("abc12345")
	require.NoError(t, err, "LoadSession")
	assert.Equal(t, "conv-legacy", s.ConvID, "ConvID")
	assert.True(t, s.AutoRegistered, "expected AutoRegistered = true")

	// Verify notify state was imported
	_, found, err := GetNotifyTime("abc12345")
	require.NoError(t, err, "GetNotifyTime")
	assert.True(t, found, "expected notify state to be imported")

	// Verify old dirs renamed
	_, err = os.Stat(sessDir)
	assert.True(t, os.IsNotExist(err), "old sessions dir should be renamed")
	_, err = os.Stat(sessDir + ".migrated")
	assert.NoError(t, err, "expected .migrated sessions dir")

	// Verify debug.log moved to new location
	newDebugLog := dir + "/.tclaude/debug.log"
	if data, err := os.ReadFile(newDebugLog); err != nil {
		assert.Fail(t, "expected debug.log at new location")
	} else {
		assert.Equal(t, "old debug data\n", string(data), "debug.log content")
	}
	// Old location should be gone (it was moved before the dir rename)
	_, err = os.Stat(sessDir + ".migrated/debug.log")
	assert.True(t, os.IsNotExist(err), "debug.log should not remain in old dir")
}
