package db

import (
	"os"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db == nil {
		t.Fatal("Open returned nil db")
	}

	// Verify schema version
	var ver int
	if err := db.QueryRow("SELECT version FROM schema_version").Scan(&ver); err != nil {
		t.Fatalf("schema_version query: %v", err)
	}
	if ver != currentVersion {
		t.Fatalf("expected version %d, got %d", currentVersion, ver)
	}
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
	if err := SaveSession(s); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// Load
	loaded, err := LoadSession("test-001")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.ConvID != "conv-abc" {
		t.Errorf("ConvID = %q, want %q", loaded.ConvID, "conv-abc")
	}
	if loaded.PID != 12345 {
		t.Errorf("PID = %d, want 12345", loaded.PID)
	}

	// FindByConvID
	found, err := FindSessionByConvID("conv-abc")
	if err != nil {
		t.Fatalf("FindSessionByConvID: %v", err)
	}
	if found == nil || found.ID != "test-001" {
		t.Errorf("FindSessionByConvID returned %v", found)
	}

	// FindByConvID (miss)
	notFound, err := FindSessionByConvID("nonexistent")
	if err != nil {
		t.Fatalf("FindSessionByConvID miss: %v", err)
	}
	if notFound != nil {
		t.Errorf("expected nil for nonexistent conv ID, got %v", notFound)
	}

	// Exists
	exists, err := SessionExists("test-001")
	if err != nil {
		t.Fatalf("SessionExists: %v", err)
	}
	if !exists {
		t.Error("expected session to exist")
	}

	// List
	all, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("ListSessions returned %d sessions, want 1", len(all))
	}

	// Delete
	if err := DeleteSession("test-001"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	exists, _ = SessionExists("test-001")
	if exists {
		t.Error("session should not exist after delete")
	}
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
	if err != nil {
		t.Fatalf("insert old session: %v", err)
	}

	// Create a fresh "exited" session
	fresh := &SessionRow{
		ID:        "fresh-exited",
		Status:    "exited",
		CreatedAt: time.Now(),
	}
	if err := SaveSession(fresh); err != nil {
		t.Fatalf("SaveSession fresh: %v", err)
	}

	// Cleanup with 24h threshold
	deleted, err := CleanupOldExited(24 * time.Hour)
	if err != nil {
		t.Fatalf("CleanupOldExited: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	// Fresh one should still exist
	exists, _ := SessionExists("fresh-exited")
	if !exists {
		t.Error("fresh-exited should still exist")
	}
}

func TestMaxUpdatedAt(t *testing.T) {
	setupTestDB(t)

	// Empty table
	ts, err := MaxUpdatedAt()
	if err != nil {
		t.Fatalf("MaxUpdatedAt empty: %v", err)
	}
	if !ts.IsZero() {
		t.Errorf("expected zero time for empty table, got %v", ts)
	}

	// Add a session
	s := &SessionRow{ID: "max-test", Status: "idle", CreatedAt: time.Now()}
	if err := SaveSession(s); err != nil {
		t.Fatal(err)
	}

	ts, err = MaxUpdatedAt()
	if err != nil {
		t.Fatalf("MaxUpdatedAt: %v", err)
	}
	if ts.IsZero() {
		t.Error("MaxUpdatedAt should not be zero after insert")
	}
}

func TestContextSnapshotRoundTrip(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "snap-001", CreatedAt: time.Now()}
	if err := SaveSession(s); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// Default values: all zero before the statusbar hook fires.
	got, err := GetContextSnapshot("snap-001")
	if err != nil {
		t.Fatalf("GetContextSnapshot empty: %v", err)
	}
	if got.ContextPct != 0 || got.TokensInput != 0 || got.TokensOutput != 0 || got.ContextWindowSize != 0 {
		t.Fatalf("default snapshot non-zero: %+v", got)
	}

	// Statusbar tick: write the full snapshot atomically.
	if err := UpdateContextSnapshot("snap-001", 19.0, 180_000, 10_000, 1_000_000); err != nil {
		t.Fatalf("UpdateContextSnapshot: %v", err)
	}
	got, err = GetContextSnapshot("snap-001")
	if err != nil {
		t.Fatalf("GetContextSnapshot populated: %v", err)
	}
	if got.ContextPct != 19.0 {
		t.Errorf("ContextPct = %v, want 19", got.ContextPct)
	}
	if got.TokensInput != 180_000 {
		t.Errorf("TokensInput = %v, want 180000", got.TokensInput)
	}
	if got.TokensOutput != 10_000 {
		t.Errorf("TokensOutput = %v, want 10000", got.TokensOutput)
	}
	if got.ContextWindowSize != 1_000_000 {
		t.Errorf("ContextWindowSize = %v, want 1000000", got.ContextWindowSize)
	}

	// Backwards-compat: UpdateContextPct still works alongside the
	// snapshot fields. Writing pct alone shouldn't zero the abs fields.
	if err := UpdateContextPct("snap-001", 21.5); err != nil {
		t.Fatalf("UpdateContextPct: %v", err)
	}
	got, _ = GetContextSnapshot("snap-001")
	if got.ContextPct != 21.5 {
		t.Errorf("after pct-only update: ContextPct = %v, want 21.5", got.ContextPct)
	}
	if got.TokensInput != 180_000 {
		t.Errorf("after pct-only update: TokensInput = %v, want 180000 (preserved)", got.TokensInput)
	}
}

func TestCompactStateRoundTrip(t *testing.T) {
	setupTestDB(t)

	s := &SessionRow{ID: "ctx-001", CreatedAt: time.Now()}
	if err := SaveSession(s); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// Default values: 0/0 right after insert.
	pct, pending, err := GetCompactState("ctx-001")
	if err != nil {
		t.Fatalf("GetCompactState: %v", err)
	}
	if pct != 0 || pending != 0 {
		t.Fatalf("default state = (%v, %v), want (0, 0)", pct, pending)
	}

	// Update context_pct via the statusbar path.
	if err := UpdateContextPct("ctx-001", 47.0); err != nil {
		t.Fatalf("UpdateContextPct: %v", err)
	}
	pct, pending, _ = GetCompactState("ctx-001")
	if pct != 47.0 || pending != 0 {
		t.Fatalf("post-pct state = (%v, %v), want (47, 0)", pct, pending)
	}

	// Claim compact: first call wins, second is a no-op.
	claimed, err := TryClaimCompact("ctx-001")
	if err != nil {
		t.Fatalf("TryClaimCompact: %v", err)
	}
	if !claimed {
		t.Fatal("first TryClaimCompact should win")
	}
	pct, pending, _ = GetCompactState("ctx-001")
	if pct != 47.0 || pending == 0 {
		t.Fatalf("post-claim state = (%v, %v), want pct=47 pending>0", pct, pending)
	}

	again, err := TryClaimCompact("ctx-001")
	if err != nil {
		t.Fatalf("TryClaimCompact (re): %v", err)
	}
	if again {
		t.Fatal("second TryClaimCompact must not win after a pending claim")
	}

	// ResetCompact wipes both fields back to zero.
	if err := ResetCompact("ctx-001"); err != nil {
		t.Fatalf("ResetCompact: %v", err)
	}
	pct, pending, _ = GetCompactState("ctx-001")
	if pct != 0 || pending != 0 {
		t.Fatalf("post-reset state = (%v, %v), want (0, 0)", pct, pending)
	}
}

func TestNotifyState(t *testing.T) {
	setupTestDB(t)

	// No record
	_, found, err := GetNotifyTime("sess-1")
	if err != nil {
		t.Fatalf("GetNotifyTime: %v", err)
	}
	if found {
		t.Error("expected no record")
	}

	// Set
	if err := SetNotifyTime("sess-1"); err != nil {
		t.Fatalf("SetNotifyTime: %v", err)
	}

	// Get
	ts, found, err := GetNotifyTime("sess-1")
	if err != nil {
		t.Fatalf("GetNotifyTime: %v", err)
	}
	if !found {
		t.Error("expected record")
	}
	if time.Since(ts) > 5*time.Second {
		t.Errorf("notified_at too old: %v", ts)
	}
}

func TestLegacyImport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ResetForTest()

	// Create legacy session files
	sessDir := dir + "/.tclaude/claude-sessions"
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessDir+"/abc12345.json", []byte(`{
		"id": "abc12345",
		"tmuxSession": "abc12345",
		"pid": 999,
		"cwd": "/tmp/test",
		"convId": "conv-legacy",
		"status": "idle",
		"created": "2025-01-01T00:00:00Z",
		"updated": "2025-01-01T12:00:00Z"
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Create .auto marker
	if err := os.WriteFile(sessDir+"/abc12345.auto", []byte("auto-registered"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create legacy debug.log
	if err := os.WriteFile(sessDir+"/debug.log", []byte("old debug data\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create legacy notify state
	notifyDir := dir + "/.tclaude/notify-state"
	if err := os.MkdirAll(notifyDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notifyDir+"/abc12345", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Open triggers migration + import
	_, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Verify session was imported
	s, err := LoadSession("abc12345")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if s.ConvID != "conv-legacy" {
		t.Errorf("ConvID = %q, want conv-legacy", s.ConvID)
	}
	if !s.AutoRegistered {
		t.Error("expected AutoRegistered = true")
	}

	// Verify notify state was imported
	_, found, err := GetNotifyTime("abc12345")
	if err != nil {
		t.Fatalf("GetNotifyTime: %v", err)
	}
	if !found {
		t.Error("expected notify state to be imported")
	}

	// Verify old dirs renamed
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Error("old sessions dir should be renamed")
	}
	if _, err := os.Stat(sessDir + ".migrated"); err != nil {
		t.Error("expected .migrated sessions dir")
	}

	// Verify debug.log moved to new location
	newDebugLog := dir + "/.tclaude/debug.log"
	if data, err := os.ReadFile(newDebugLog); err != nil {
		t.Error("expected debug.log at new location")
	} else if string(data) != "old debug data\n" {
		t.Errorf("debug.log content = %q, want %q", string(data), "old debug data\n")
	}
	// Old location should be gone (it was moved before the dir rename)
	if _, err := os.Stat(sessDir + ".migrated/debug.log"); !os.IsNotExist(err) {
		t.Error("debug.log should not remain in old dir")
	}
}
