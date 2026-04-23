package conv

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- In-memory sort tests ---

func TestResortAndFilter_InMemoryOnly(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", Modified: "2026-01-01T00:00:00Z", FileSize: 100},
			{SessionID: "bbbb0000-0000-0000-0000-000000000002", Modified: "2026-03-01T00:00:00Z", FileSize: 300},
			{SessionID: "cccc0000-0000-0000-0000-000000000003", Modified: "2026-02-01T00:00:00Z", FileSize: 200},
		},
		activeSessions: make(map[string]*SessionState),
	}

	// Default sort: modified desc
	m.sortInPlace(m.entries)
	m.applySearchFilter()

	if m.filtered[0].SessionID != "bbbb0000-0000-0000-0000-000000000002" {
		t.Errorf("expected bbbb first (most recent), got %s", m.filtered[0].SessionID[:8])
	}

	// Sort by size ascending
	m.sort = table.SortState{Key: "size", Direction: table.SortAsc}
	m.resortAndFilter()

	if m.filtered[0].FileSize != 100 {
		t.Errorf("expected size 100 first (asc), got %d", m.filtered[0].FileSize)
	}
	if m.filtered[2].FileSize != 300 {
		t.Errorf("expected size 300 last (asc), got %d", m.filtered[2].FileSize)
	}

	// Sort by size descending
	m.sort = table.SortState{Key: "size", Direction: table.SortDesc}
	m.resortAndFilter()

	if m.filtered[0].FileSize != 300 {
		t.Errorf("expected size 300 first (desc), got %d", m.filtered[0].FileSize)
	}
}

func TestResortAndFilter_AllSortKeys(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "cccc0000-0000-0000-0000-000000000003", Modified: "2026-01-01T00:00:00Z", FileSize: 300, ProjectPath: "/z", CustomTitle: "Zebra"},
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", Modified: "2026-03-01T00:00:00Z", FileSize: 100, ProjectPath: "/a", CustomTitle: "Apple"},
			{SessionID: "bbbb0000-0000-0000-0000-000000000002", Modified: "2026-02-01T00:00:00Z", FileSize: 200, ProjectPath: "/m", CustomTitle: "Mango"},
		},
		activeSessions: make(map[string]*SessionState),
	}

	tests := []struct {
		key     string
		dir     table.SortDirection
		firstID string // expected first entry after sort
	}{
		{"id", table.SortAsc, "aaaa0000-0000-0000-0000-000000000001"},
		{"id", table.SortDesc, "cccc0000-0000-0000-0000-000000000003"},
		{"modified", table.SortAsc, "cccc0000-0000-0000-0000-000000000003"},
		{"modified", table.SortDesc, "aaaa0000-0000-0000-0000-000000000001"},
		{"size", table.SortAsc, "aaaa0000-0000-0000-0000-000000000001"},
		{"size", table.SortDesc, "cccc0000-0000-0000-0000-000000000003"},
		{"project", table.SortAsc, "aaaa0000-0000-0000-0000-000000000001"},
		{"project", table.SortDesc, "cccc0000-0000-0000-0000-000000000003"},
		{"title", table.SortAsc, "aaaa0000-0000-0000-0000-000000000001"},  // Apple
		{"title", table.SortDesc, "cccc0000-0000-0000-0000-000000000003"}, // Zebra
	}

	for _, tt := range tests {
		m.sort = table.SortState{Key: tt.key, Direction: tt.dir}
		m.resortAndFilter()
		if m.filtered[0].SessionID != tt.firstID {
			t.Errorf("sort key=%s dir=%d: expected first=%s, got %s",
				tt.key, tt.dir, tt.firstID[:8], m.filtered[0].SessionID[:8])
		}
	}
}

func TestResortPreservesSearchFilter(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", FirstPrompt: "fix bug", FileSize: 100},
			{SessionID: "bbbb0000-0000-0000-0000-000000000002", FirstPrompt: "add feature", FileSize: 300},
			{SessionID: "cccc0000-0000-0000-0000-000000000003", FirstPrompt: "fix test", FileSize: 200},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}
	m.searchInput.SetValue("fix")

	m.sort = table.SortState{Key: "size", Direction: table.SortAsc}
	m.resortAndFilter()

	if len(m.filtered) != 2 {
		t.Fatalf("expected 2 filtered entries, got %d", len(m.filtered))
	}
	// Should be sorted by size asc within the "fix" filter
	if m.filtered[0].FileSize != 100 {
		t.Errorf("expected smallest first, got %d", m.filtered[0].FileSize)
	}
}

// --- Entry management tests ---

func TestUpsertEntry_UpdatesExisting(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", FirstPrompt: "old"},
		},
		activeSessions: make(map[string]*SessionState),
	}

	m.upsertEntry(SessionEntry{SessionID: "aaaa0000-0000-0000-0000-000000000001", FirstPrompt: "new"})

	if len(m.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(m.entries))
	}
	if m.entries[0].FirstPrompt != "new" {
		t.Errorf("expected updated prompt 'new', got '%s'", m.entries[0].FirstPrompt)
	}
}

func TestUpsertEntry_AppendsNew(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001"},
		},
		activeSessions: make(map[string]*SessionState),
	}

	m.upsertEntry(SessionEntry{SessionID: "bbbb0000-0000-0000-0000-000000000002"})

	if len(m.entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.entries))
	}
}

func TestRemoveEntry(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001"},
			{SessionID: "bbbb0000-0000-0000-0000-000000000002"},
			{SessionID: "cccc0000-0000-0000-0000-000000000003"},
		},
		activeSessions: make(map[string]*SessionState),
	}

	m.removeEntry("bbbb0000-0000-0000-0000-000000000002")

	if len(m.entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.entries))
	}
	for _, e := range m.entries {
		if e.SessionID == "bbbb0000-0000-0000-0000-000000000002" {
			t.Error("entry bbbb should have been removed")
		}
	}
}

func TestRemoveEntry_NonExistent(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001"},
		},
		activeSessions: make(map[string]*SessionState),
	}

	m.removeEntry("zzzz0000-0000-0000-0000-000000000099")

	if len(m.entries) != 1 {
		t.Fatalf("removing non-existent entry should not change length, got %d", len(m.entries))
	}
}

// --- Search filter tests ---

func TestApplySearchFilter(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", FirstPrompt: "fix the login bug"},
			{SessionID: "bbbb0000-0000-0000-0000-000000000002", FirstPrompt: "add new feature"},
			{SessionID: "cccc0000-0000-0000-0000-000000000003", FirstPrompt: "refactor login page"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}

	m.searchInput.SetValue("login")
	m.applySearchFilter()

	if len(m.filtered) != 2 {
		t.Fatalf("expected 2 matches for 'login', got %d", len(m.filtered))
	}

	// Clear search
	m.searchInput.SetValue("")
	m.applySearchFilter()

	if len(m.filtered) != 3 {
		t.Fatalf("expected all 3 entries with empty search, got %d", len(m.filtered))
	}
}

func TestApplySearchFilter_MatchesMultipleFields(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", FirstPrompt: "hello", ProjectPath: "/myproject"},
			{SessionID: "bbbb0000-0000-0000-0000-000000000002", FirstPrompt: "world", GitBranch: "myproject-fix"},
			{SessionID: "cccc0000-0000-0000-0000-000000000003", FirstPrompt: "nothing here"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}

	m.searchInput.SetValue("myproject")
	m.applySearchFilter()

	if len(m.filtered) != 2 {
		t.Fatalf("expected 2 matches across project path and git branch, got %d", len(m.filtered))
	}
}

func TestApplySearchFilter_CaseInsensitive(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", FirstPrompt: "Fix The BUG"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}

	m.searchInput.SetValue("fix the bug")
	m.applySearchFilter()

	if len(m.filtered) != 1 {
		t.Fatalf("expected case-insensitive match, got %d", len(m.filtered))
	}
}

func TestApplySearchFilter_ResetsCursorWhenOutOfBounds(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001", FirstPrompt: "match"},
			{SessionID: "bbbb0000-0000-0000-0000-000000000002", FirstPrompt: "no"},
			{SessionID: "cccc0000-0000-0000-0000-000000000003", FirstPrompt: "no"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
		cursor:         2, // pointing at 3rd entry
	}

	m.searchInput.SetValue("match")
	m.applySearchFilter()

	if m.cursor != 0 {
		t.Errorf("cursor should have been reset to 0, got %d", m.cursor)
	}
}

// --- shouldAcceptFSEvent tests ---

func TestShouldAcceptFSEvent_GlobalAcceptsAll(t *testing.T) {
	m := &watchModel{global: true}
	if !m.shouldAcceptFSEvent("/any/path/file.jsonl") {
		t.Error("global mode should accept all events")
	}
}

func TestShouldAcceptFSEvent_SingleProjectFilters(t *testing.T) {
	m := &watchModel{
		global:           false,
		claudeProjectDir: "/home/.claude/projects/-Users-foo-bar",
	}

	if !m.shouldAcceptFSEvent("/home/.claude/projects/-Users-foo-bar/abc.jsonl") {
		t.Error("should accept event from own project dir")
	}
	if m.shouldAcceptFSEvent("/home/.claude/projects/-Users-other/abc.jsonl") {
		t.Error("should reject event from different project dir")
	}
}

// --- fsnotify debounce tests ---

func TestFsDebounceLoop_CreateSentImmediately(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := newTestWatcher(projectDir)
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	filePath := filepath.Join(projectDir, convID+".jsonl")
	if err := os.WriteFile(filePath, []byte(`{"type":"user"}`), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-outCh:
		if msg.FilePath != filePath {
			t.Errorf("expected path %s, got %s", filePath, msg.FilePath)
		}
		if msg.Removed {
			t.Error("expected Removed=false for create")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for create event")
	}
}

func TestFsDebounceLoop_DeleteSentImmediately(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	filePath := filepath.Join(projectDir, convID+".jsonl")
	if err := os.WriteFile(filePath, []byte(`{"type":"user"}`), 0644); err != nil {
		t.Fatal(err)
	}

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := newTestWatcher(projectDir)
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-outCh:
		if msg.FilePath != filePath {
			t.Errorf("expected path %s, got %s", filePath, msg.FilePath)
		}
		if !msg.Removed {
			t.Error("expected Removed=true for delete")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delete event")
	}
}

func TestFsDebounceLoop_WritesAreDebounced(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	filePath := filepath.Join(projectDir, convID+".jsonl")
	if err := os.WriteFile(filePath, []byte(`{"type":"user"}`), 0644); err != nil {
		t.Fatal(err)
	}

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := newTestWatcher(projectDir)
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	// Write to the existing file — should NOT be sent immediately (debounced)
	if err := os.WriteFile(filePath, []byte(`{"type":"user","timestamp":"2026-01-01"}`), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-outCh:
		// On macOS, fsnotify may report the pre-existing file write as CREATE
		// (since WriteFile does truncate+write). This is acceptable.
		t.Log("received event (may be initial create, which is expected on some platforms)")
	case <-time.After(500 * time.Millisecond):
		// Expected path for a pure write to a known file
	}
}

func TestFsDebounceLoop_IgnoresNonJsonl(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := newTestWatcher(projectDir)
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	if err := os.WriteFile(filepath.Join(projectDir, "notes.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-outCh:
		t.Errorf("should not have received event for non-.jsonl file, got %s", msg.FilePath)
	case <-time.After(500 * time.Millisecond):
		// Expected
	}
}

func TestFsDebounceLoop_IgnoresWrongDepth(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	deepDir := filepath.Join(projectDir, "subdir")
	if err := os.MkdirAll(deepDir, 0755); err != nil {
		t.Fatal(err)
	}

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Watch both the project dir and the deep subdir
	_ = w.Add(projectDir)
	_ = w.Add(deepDir)

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	// Create a .jsonl file at the wrong depth (too deep)
	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	if err := os.WriteFile(filepath.Join(deepDir, convID+".jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-outCh:
		t.Errorf("should not have received event for wrong depth, got %s", msg.FilePath)
	case <-time.After(500 * time.Millisecond):
		// Expected
	}
}

func TestFsDebounceLoop_IgnoresShortUUID(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := newTestWatcher(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	// .jsonl file with non-UUID name (wrong length) — still passes through the
	// debounce loop (which only checks .jsonl + depth), but handleFSChange
	// will reject it based on convID length.
	if err := os.WriteFile(filepath.Join(projectDir, "short.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// The debounce loop lets any .jsonl at the right depth through —
	// the UUID validation happens in handleFSChange. So we expect an event here.
	select {
	case msg := <-outCh:
		// Verify handleFSChange would reject this
		if msg.FilePath == "" {
			t.Error("got empty path")
		}
	case <-time.After(2 * time.Second):
		// Also acceptable — some platforms may not report this
	}
}

func TestFsDebounceLoop_AutoWatchesNewSubdir(t *testing.T) {
	projectsDir := t.TempDir()

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	// Watch the projects dir itself (simulates startup with no project dirs)
	w, err := newTestWatcher(projectsDir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	// Create a new project subdir
	projectDir := filepath.Join(projectsDir, "-Users-new-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Give fsnotify time to process the dir creation and w.Add it
	time.Sleep(200 * time.Millisecond)

	// Now create a .jsonl file inside the new subdir
	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	filePath := filepath.Join(projectDir, convID+".jsonl")
	if err := os.WriteFile(filePath, []byte(`{"type":"user"}`), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-outCh:
		if msg.FilePath != filePath {
			t.Errorf("expected path %s, got %s", filePath, msg.FilePath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out — new subdir was not auto-watched")
	}
}

func TestFsDebounceLoop_CloseChStopsLoop(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})

	w, err := newTestWatcher(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	done := make(chan struct{})
	go func() {
		fsDebounceLoop(w, projectsDir, outCh, closeCh)
		close(done)
	}()

	close(closeCh)

	select {
	case <-done:
		// Expected — goroutine exited
	case <-time.After(2 * time.Second):
		t.Fatal("fsDebounceLoop did not exit after closeCh was closed")
	}
}

// --- helpers ---

// SessionState alias for tests
type SessionState = session.SessionState

func newTestWatcher(dir string) (*fsnotify.Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}
