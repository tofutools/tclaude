package conv

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	assert.Equal(t, "bbbb0000-0000-0000-0000-000000000002", m.filtered[0].SessionID, "expected bbbb first (most recent)")

	// Sort by size ascending
	m.sort = table.SortState{Key: "size", Direction: table.SortAsc}
	m.resortAndFilter()

	assert.Equal(t, int64(100), m.filtered[0].FileSize, "expected size 100 first (asc)")
	assert.Equal(t, int64(300), m.filtered[2].FileSize, "expected size 300 last (asc)")

	// Sort by size descending
	m.sort = table.SortState{Key: "size", Direction: table.SortDesc}
	m.resortAndFilter()

	assert.Equal(t, int64(300), m.filtered[0].FileSize, "expected size 300 first (desc)")
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
		assert.Equal(t, tt.firstID, m.filtered[0].SessionID, "sort key=%s dir=%d", tt.key, tt.dir)
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

	require.Len(t, m.filtered, 2, "expected 2 filtered entries")
	// Should be sorted by size asc within the "fix" filter
	assert.Equal(t, int64(100), m.filtered[0].FileSize, "expected smallest first")
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

	require.Len(t, m.entries, 1, "expected 1 entry")
	assert.Equal(t, "new", m.entries[0].FirstPrompt, "expected updated prompt")
}

func TestUpsertEntry_AppendsNew(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "aaaa0000-0000-0000-0000-000000000001"},
		},
		activeSessions: make(map[string]*SessionState),
	}

	m.upsertEntry(SessionEntry{SessionID: "bbbb0000-0000-0000-0000-000000000002"})

	require.Len(t, m.entries, 2, "expected 2 entries")
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

	require.Len(t, m.entries, 2, "expected 2 entries")
	for _, e := range m.entries {
		assert.NotEqual(t, "bbbb0000-0000-0000-0000-000000000002", e.SessionID, "entry bbbb should have been removed")
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

	assert.Len(t, m.entries, 1, "removing non-existent entry should not change length")
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

	require.Len(t, m.filtered, 2, "expected 2 matches for 'login'")

	// Clear search
	m.searchInput.SetValue("")
	m.applySearchFilter()

	require.Len(t, m.filtered, 3, "expected all 3 entries with empty search")
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

	require.Len(t, m.filtered, 2, "expected 2 matches across project path and git branch")
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

	require.Len(t, m.filtered, 1, "expected case-insensitive match")
}

// --- Group filter tests ---

// groupFilter narrows the visible rows to convs whose membership list
// includes the named group. Composes with the text search and the
// archived toggle as three independent passes.
func TestApplyGroupFilter_FiltersByGroupName(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "a", FirstPrompt: "x"},
			{SessionID: "b", FirstPrompt: "y"},
			{SessionID: "c", FirstPrompt: "z"},
		},
		groupsByConv: map[string][]string{
			"a": {"alpha", "shared"},
			"b": {"beta"},
			"c": {"alpha"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}

	m.groupFilter = "alpha"
	m.applySearchFilter()

	require.Len(t, m.filtered, 2, "expected 2 matches for group=alpha")
	gotIDs := map[string]bool{m.filtered[0].SessionID: true, m.filtered[1].SessionID: true}
	assert.True(t, gotIDs["a"] && gotIDs["c"], "expected a and c in filter result, got %+v", gotIDs)
}

// Group names compare case-insensitively at the filter layer — DB
// stores them case-sensitively but the picker user shouldn't have to
// reproduce exact casing.
func TestApplyGroupFilter_CaseInsensitive(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "a"},
		},
		groupsByConv: map[string][]string{
			"a": {"Alpha-Team"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}

	m.groupFilter = "alpha-TEAM"
	m.applySearchFilter()

	require.Len(t, m.filtered, 1, "expected case-insensitive match")
}

// Group filter and search filter compose: an entry must pass BOTH to
// appear. Pins the bug class where one filter would shadow the other.
func TestApplyGroupFilter_ComposesWithSearch(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "a", FirstPrompt: "fix login"},
			{SessionID: "b", FirstPrompt: "fix login"},
			{SessionID: "c", FirstPrompt: "add feature"},
		},
		groupsByConv: map[string][]string{
			"a": {"alpha"},
			"b": {"beta"},
			"c": {"alpha"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}

	// search "login" matches a + b; group "alpha" matches a + c.
	// Intersection: just a.
	m.searchInput.SetValue("login")
	m.groupFilter = "alpha"
	m.applySearchFilter()

	require.Len(t, m.filtered, 1, "expected intersection (login AND alpha) to yield 1")
	assert.Equal(t, "a", m.filtered[0].SessionID)
}

// Empty groupFilter passes every entry through (the filter is opt-in).
func TestApplyGroupFilter_EmptyShowsAll(t *testing.T) {
	m := &watchModel{
		entries: []SessionEntry{
			{SessionID: "a"},
			{SessionID: "b"},
		},
		groupsByConv: map[string][]string{
			"a": {"alpha"},
		},
		activeSessions: make(map[string]*SessionState),
		searchInput:    newSearchInput(),
	}

	m.groupFilter = ""
	m.applySearchFilter()

	require.Len(t, m.filtered, 2, "empty group filter should pass all entries")
}

// matchesGroupFilter is the core predicate used by both
// applySearchFilter and rebuildSemanticFiltered. Verify it directly so
// regressions surface independent of the surrounding filter pipeline.
func TestMatchesGroupFilter(t *testing.T) {
	m := &watchModel{
		groupsByConv: map[string][]string{
			"a": {"alpha", "shared"},
			"b": {"beta"},
		},
	}

	cases := []struct {
		name   string
		filter string
		entry  SessionEntry
		want   bool
	}{
		{"empty filter passes anything", "", SessionEntry{SessionID: "a"}, true},
		{"empty filter passes empty membership", "", SessionEntry{SessionID: "x"}, true},
		{"matching primary group", "alpha", SessionEntry{SessionID: "a"}, true},
		{"matching secondary group", "shared", SessionEntry{SessionID: "a"}, true},
		{"non-matching group", "beta", SessionEntry{SessionID: "a"}, false},
		{"case-insensitive match", "ALPHA", SessionEntry{SessionID: "a"}, true},
		{"unknown conv id", "alpha", SessionEntry{SessionID: "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.groupFilter = tc.filter
			got := m.matchesGroupFilter(tc.entry)
			assert.Equal(t, tc.want, got, "matchesGroupFilter(filter=%q, entry=%s)", tc.filter, tc.entry.SessionID)
		})
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

	assert.Equal(t, 0, m.cursor, "cursor should have been reset to 0")
}

// --- shouldAcceptFSEvent tests ---

func TestShouldAcceptFSEvent_GlobalAcceptsAll(t *testing.T) {
	m := &watchModel{global: true}
	assert.True(t, m.shouldAcceptFSEvent("/any/path/file.jsonl"), "global mode should accept all events")
}

func TestShouldAcceptFSEvent_SingleProjectFilters(t *testing.T) {
	m := &watchModel{
		global:           false,
		claudeProjectDir: "/home/.claude/projects/-Users-foo-bar",
	}

	assert.True(t, m.shouldAcceptFSEvent("/home/.claude/projects/-Users-foo-bar/abc.jsonl"), "should accept event from own project dir")
	assert.False(t, m.shouldAcceptFSEvent("/home/.claude/projects/-Users-other/abc.jsonl"), "should reject event from different project dir")
}

// --- fsnotify debounce tests ---

func TestFsDebounceLoop_CreateSentImmediately(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := newTestWatcher(projectDir)
	require.NoError(t, err, "failed to create watcher")
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	filePath := filepath.Join(projectDir, convID+".jsonl")
	require.NoError(t, os.WriteFile(filePath, []byte(`{"type":"user"}`), 0644))

	select {
	case msg := <-outCh:
		assert.Equal(t, filePath, msg.FilePath)
		assert.False(t, msg.Removed, "expected Removed=false for create")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for create event")
	}
}

func TestFsDebounceLoop_DeleteSentImmediately(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "-Users-test-project")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	filePath := filepath.Join(projectDir, convID+".jsonl")
	require.NoError(t, os.WriteFile(filePath, []byte(`{"type":"user"}`), 0644))

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	w, err := newTestWatcher(projectDir)
	require.NoError(t, err, "failed to create watcher")
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	require.NoError(t, os.Remove(filePath))

	select {
	case msg := <-outCh:
		assert.Equal(t, filePath, msg.FilePath)
		assert.True(t, msg.Removed, "expected Removed=true for delete")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delete event")
	}
}

func TestFsDebounceLoop_WritesAreDebounced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		projectsDir := "/projects"
		filePath := filepath.Join(projectsDir, "-Users-test-project", "a1b2c3d4-e5f6-7890-abcd-ef1234567890.jsonl")
		events := make(chan fsnotify.Event, 2)
		errors := make(chan error)
		outCh := make(chan fsFileChangeMsg, 1)
		closeCh := make(chan struct{})
		go fsDebounceEvents(events, errors, nil, projectsDir, outCh, closeCh)

		events <- fsnotify.Event{Name: filePath, Op: fsnotify.Write}
		synctest.Wait()
		assertNoFSChange(t, outCh, "immediately after first write")

		// A later write resets the full debounce interval.
		time.Sleep(20 * time.Second)
		events <- fsnotify.Event{Name: filePath, Op: fsnotify.Write}
		synctest.Wait()
		time.Sleep(fsDebounceDelay - time.Nanosecond)
		synctest.Wait()
		assertNoFSChange(t, outCh, "one nanosecond before reset debounce deadline")

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case msg := <-outCh:
			assert.Equal(t, filePath, msg.FilePath)
			assert.False(t, msg.Removed)
		default:
			t.Fatal("debounced write was not emitted at the exact deadline")
		}
		close(closeCh)
		synctest.Wait()
	})
}

func TestFsDebounceLoop_IgnoresNonJsonl(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		projectsDir := "/projects"
		events := make(chan fsnotify.Event, 1)
		errors := make(chan error)
		outCh := make(chan fsFileChangeMsg, 1)
		closeCh := make(chan struct{})
		go fsDebounceEvents(events, errors, nil, projectsDir, outCh, closeCh)

		events <- fsnotify.Event{Name: filepath.Join(projectsDir, "-Users-test-project", "notes.txt"), Op: fsnotify.Create}
		synctest.Wait()
		assertNoFSChange(t, outCh, "after non-jsonl event was consumed")
		close(closeCh)
		synctest.Wait()
	})
}

func TestFsDebounceLoop_IgnoresWrongDepth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		projectsDir := "/projects"
		events := make(chan fsnotify.Event, 1)
		errors := make(chan error)
		outCh := make(chan fsFileChangeMsg, 1)
		closeCh := make(chan struct{})
		go fsDebounceEvents(events, errors, nil, projectsDir, outCh, closeCh)

		filePath := filepath.Join(projectsDir, "-Users-test-project", "subdir", "a1b2c3d4-e5f6-7890-abcd-ef1234567890.jsonl")
		events <- fsnotify.Event{Name: filePath, Op: fsnotify.Create}
		synctest.Wait()
		assertNoFSChange(t, outCh, "after wrong-depth event was consumed")
		close(closeCh)
		synctest.Wait()
	})
}

func TestFsDebounceLoop_ForwardsShortUUIDToValidationLayer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		projectsDir := "/projects"
		filePath := filepath.Join(projectsDir, "-Users-test-project", "short.jsonl")
		events := make(chan fsnotify.Event, 1)
		errors := make(chan error)
		outCh := make(chan fsFileChangeMsg, 1)
		closeCh := make(chan struct{})
		go fsDebounceEvents(events, errors, nil, projectsDir, outCh, closeCh)

		// UUID validation belongs to handleFSChange, so the debounce loop must
		// still forward a right-depth .jsonl create immediately.
		events <- fsnotify.Event{Name: filePath, Op: fsnotify.Create}
		synctest.Wait()
		select {
		case msg := <-outCh:
			assert.Equal(t, filePath, msg.FilePath)
		default:
			t.Fatal("right-depth .jsonl create was not forwarded")
		}
		close(closeCh)
		synctest.Wait()
	})
}

func TestFsDebounceLoop_AutoWatchesNewSubdir(t *testing.T) {
	projectsDir := t.TempDir()

	outCh := make(chan fsFileChangeMsg, 16)
	closeCh := make(chan struct{})
	defer close(closeCh)

	// Watch the projects dir itself (simulates startup with no project dirs)
	w, err := newTestWatcher(projectsDir)
	require.NoError(t, err)
	defer w.Close()

	go fsDebounceLoop(w, projectsDir, outCh, closeCh)

	// Create a new project subdir
	projectDir := filepath.Join(projectsDir, "-Users-new-project")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Watcher.Add is synchronous once the loop consumes the directory event.
	// Poll the externally observable watch list instead of guessing how long
	// the OS event will take to arrive.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Contains(c, w.WatchList(), projectDir, "watch list after project directory create")
	}, 5*time.Second, 10*time.Millisecond)

	// Now create a .jsonl file inside the new subdir
	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	filePath := filepath.Join(projectDir, convID+".jsonl")
	require.NoError(t, os.WriteFile(filePath, []byte(`{"type":"user"}`), 0644))

	select {
	case msg := <-outCh:
		assert.Equal(t, filePath, msg.FilePath)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out — new subdir was not auto-watched")
	}
}

func TestFsDebounceLoop_CloseChStopsLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := make(chan fsnotify.Event)
		errors := make(chan error)
		outCh := make(chan fsFileChangeMsg)
		closeCh := make(chan struct{})
		done := make(chan struct{})
		go func() {
			fsDebounceEvents(events, errors, nil, "/projects", outCh, closeCh)
			close(done)
		}()

		close(closeCh)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("fsDebounceLoop did not exit after closeCh was closed")
		}
	})
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

func assertNoFSChange(t *testing.T, outCh <-chan fsFileChangeMsg, context string) {
	t.Helper()
	select {
	case msg := <-outCh:
		t.Fatalf("unexpected filesystem change %q %s", msg.FilePath, context)
	default:
	}
}
