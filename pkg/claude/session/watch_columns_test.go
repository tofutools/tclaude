package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
)

func sessionHasHeader(columns []table.Column, header string) bool {
	for _, column := range columns {
		if column.Header == header {
			return true
		}
	}
	return false
}

func TestSessionWatchColumns_HarnessAutoShow(t *testing.T) {
	claudeOnly := model{allSessions: []*SessionState{{Harness: "claude"}}}
	assert.False(t, sessionHasHeader(claudeOnly.columns(), "HARNESS"))

	mixed := model{allSessions: []*SessionState{{Harness: "claude"}, {Harness: "codex"}}}
	assert.True(t, sessionHasHeader(mixed.columns(), "HARNESS"))

	legacy := model{allSessions: []*SessionState{{Harness: ""}}}
	assert.False(t, sessionHasHeader(legacy.columns(), "HARNESS"), "empty legacy harness means claude")
}

func TestSessionWatchColumns_OverrideShadowsDefaults(t *testing.T) {
	m := model{
		allSessions:  []*SessionState{{Harness: "claude"}},
		colOverrides: map[string]bool{sessionColHarness: true, sessionColStatus: false},
	}
	assert.True(t, sessionHasHeader(m.columns(), "HARNESS"))
	assert.False(t, sessionHasHeader(m.columns(), "STATUS"))
}

// The shared table sorter maps number/F-keys over the currently visible
// sortable columns. HARNESS participates in that mapping, and hiding it must
// automatically shift the later keys without a sessions-specific key map.
func TestSessionWatchColumns_SortKeysFollowVisibleColumns(t *testing.T) {
	m := model{
		allSessions:  []*SessionState{{Harness: "claude"}, {Harness: "codex"}},
		colOverrides: map[string]bool{},
	}

	require.True(t, m.sort.HandleSortKey(m.columns(), "f2"))
	assert.Equal(t, "harness", m.sort.Key, "HARNESS is the second visible sortable column")

	m.sort = table.SortState{}
	m.colOverrides[sessionColHarness] = false
	require.True(t, m.sort.HandleSortKey(m.columns(), "2"))
	assert.Equal(t, "project", m.sort.Key, "hiding HARNESS shifts PROJECT onto 2/F2")

	m.sort = table.SortState{}
	m.colOverrides[sessionColProject] = false
	require.True(t, m.sort.HandleSortKey(m.columns(), "f2"))
	assert.Equal(t, "status", m.sort.Key, "hiding PROJECT shifts STATUS onto 2/F2")
}

func TestSortSessionsByKey_Harness(t *testing.T) {
	sessions := []*SessionState{
		{ID: "opencode", Harness: "opencode"},
		{ID: "legacy", Harness: ""},
		{ID: "claude", Harness: "claude"},
		{ID: "codex", Harness: "codex"},
	}

	SortSessionsByKey(sessions, "harness", table.SortAsc)
	assert.Equal(t, []string{"legacy", "claude", "codex", "opencode"}, sessionIDs(sessions))

	SortSessionsByKey(sessions, "harness", table.SortDesc)
	assert.Equal(t, []string{"opencode", "codex", "legacy", "claude"}, sessionIDs(sessions),
		"equal displayed harnesses retain their relative order")
}

func sessionIDs(sessions []*SessionState) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	return ids
}

func TestSessionWatchColumns_HeaderCellLockstep(t *testing.T) {
	state := &SessionState{
		ID: "session-1", Harness: "codex", Cwd: "/repo", Status: StatusIdle,
		Updated: time.Now(),
	}
	for _, overrides := range []map[string]bool{
		nil,
		{sessionColHarness: false, sessionColProject: false},
		{sessionColHarness: true, sessionColStatus: false, sessionColUpdated: false},
	} {
		m := model{allSessions: []*SessionState{state}, sessions: []*SessionState{state}, colOverrides: overrides}
		var cells []string
		for _, definition := range m.orderedColumns() {
			if definition.visible {
				cells = append(cells, definition.cell(&m, state))
			}
		}
		assert.Len(t, cells, len(m.columns()), "override=%v", overrides)
	}
}

func TestSessionWatchColumns_ToggleableSet(t *testing.T) {
	m := model{allSessions: []*SessionState{{Harness: "codex"}}}
	var keys []string
	for _, column := range m.toggleableColumns() {
		keys = append(keys, column.key)
	}
	assert.Equal(t, []string{sessionColHarness, sessionColProject, sessionColStatus, sessionColUpdated}, keys)
}

func TestSessionWatchColumns_TogglePersistsAndReloads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	m := model{colOverrides: map[string]bool{}}
	m.setColumnOverride(sessionColHarness, true)
	m.setColumnOverride(sessionColUpdated, false)

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.SessionWatch)
	assert.True(t, cfg.SessionWatch.Columns[sessionColHarness])
	assert.False(t, cfg.SessionWatch.Columns[sessionColUpdated])
	assert.Equal(t, m.colOverrides, loadSessionColumnOverrides())

	m.resetColumnOverrides()
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.SessionWatch.Columns)
	assert.Empty(t, loadSessionColumnOverrides())
}

func TestSessionWatchColumns_RenderSelector(t *testing.T) {
	m := model{
		allSessions:  []*SessionState{{Harness: "codex"}},
		colOverrides: map[string]bool{sessionColProject: false},
	}
	out := m.renderColumnSelector()
	assert.Contains(t, out, "[x] HARNESS")
	assert.Contains(t, out, "[ ] PROJECT")
	assert.Contains(t, out, "▸ ")
}
