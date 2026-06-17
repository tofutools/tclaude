package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
)

// hasHeader reports whether the rendered header set contains header.
func hasHeader(cols []table.Column, header string) bool {
	for _, c := range cols {
		if c.Header == header {
			return true
		}
	}
	return false
}

// rowCells builds one row's cells exactly as View does — from the visible
// orderedColumns. Used to assert header/cell lockstep.
func rowCells(m *watchModel, e SessionEntry) []string {
	var cells []string
	for _, d := range m.orderedColumns() {
		if d.visible {
			cells = append(cells, d.cell(m, e))
		}
	}
	return cells
}

// JOH-209: the watch view auto-surfaces a HARNESS column only when a
// non-Claude conv is present (parity with the regular `conv ls` renderer),
// and a CC-only list keeps the original columns.
func TestWatchColumns_HarnessAutoShow(t *testing.T) {
	claudeOnly := &watchModel{
		entries:        []SessionEntry{{SessionID: "11111111-1111-1111-1111-111111111111", Harness: "claude"}},
		activeSessions: make(map[string]*SessionState),
	}
	assert.False(t, hasHeader(claudeOnly.columns(), "HARNESS"), "CC-only list must not show the HARNESS column")

	mixed := &watchModel{
		entries: []SessionEntry{
			{SessionID: "11111111-1111-1111-1111-111111111111", Harness: "claude"},
			{SessionID: "22222222-2222-2222-2222-222222222222", Harness: "codex"},
		},
		activeSessions: make(map[string]*SessionState),
	}
	assert.True(t, hasHeader(mixed.columns(), "HARNESS"), "a mixed-harness list auto-shows the HARNESS column")

	// Empty harness (a pre-column CC conv) is treated as claude, not "other".
	emptyHarness := &watchModel{
		entries:        []SessionEntry{{SessionID: "11111111-1111-1111-1111-111111111111", Harness: ""}},
		activeSessions: make(map[string]*SessionState),
	}
	assert.False(t, hasHeader(emptyHarness.columns(), "HARNESS"), "empty harness counts as claude — no HARNESS column")
}

// An explicit override shadows the smart auto-default in both directions:
// force-show HARNESS on a CC-only list, and force-hide GROUPS even when convs
// are grouped.
func TestWatchColumns_OverrideShadowsAuto(t *testing.T) {
	m := &watchModel{
		entries:        []SessionEntry{{SessionID: "a", Harness: "claude"}},
		groupsByConv:   map[string][]string{"a": {"alpha"}},
		activeSessions: make(map[string]*SessionState),
		colOverrides:   map[string]bool{},
	}

	// Auto: no harness column (CC only), groups column shown (a is grouped).
	require.False(t, hasHeader(m.columns(), "HARNESS"))
	require.True(t, hasHeader(m.columns(), "GROUPS"))

	// Force-show harness, force-hide groups.
	m.colOverrides[colKeyHarness] = true
	m.colOverrides[colKeyGroups] = false
	assert.True(t, hasHeader(m.columns(), "HARNESS"), "explicit show overrides the CC-only auto-hide")
	assert.False(t, hasHeader(m.columns(), "GROUPS"), "explicit hide overrides the grouped auto-show")
}

// The headers and the per-row cells are always the same length, across every
// combination of mode (global/scoped), semantic mode, grouping, harness mix,
// and overrides. This guards the single-source-of-truth invariant.
func TestWatchColumns_HeaderCellLockstep(t *testing.T) {
	entry := SessionEntry{SessionID: "22222222-2222-2222-2222-222222222222", Harness: "codex", ProjectPath: "/p", FileSize: 10, Modified: "2026-01-01T00:00:00Z"}
	for _, global := range []bool{false, true} {
		for _, semantic := range []bool{false, true} {
			for _, override := range []map[string]bool{nil, {colKeySize: false, colKeyHarness: true, colKeyProject: false}} {
				m := &watchModel{
					global:         global,
					semanticMode:   semantic,
					entries:        []SessionEntry{entry},
					groupsByConv:   map[string][]string{"22222222-2222-2222-2222-222222222222": {"alpha"}},
					semanticScores: map[string]float32{"22222222-2222-2222-2222-222222222222": 0.5},
					activeSessions: make(map[string]*SessionState),
					colOverrides:   override,
				}
				cols := m.columns()
				cells := rowCells(m, entry)
				assert.Equal(t, len(cols), len(cells),
					"header/cell length must match (global=%v semantic=%v override=%v)", global, semantic, override)
			}
		}
	}
}

// toggleableColumns lists only the user-toggleable columns, and PROJECT is
// only offered in global mode (it is meaningless scoped).
func TestWatchColumns_ToggleableSet(t *testing.T) {
	scoped := &watchModel{entries: []SessionEntry{{SessionID: "a"}}, activeSessions: make(map[string]*SessionState)}
	var scopedKeys []string
	for _, c := range scoped.toggleableColumns() {
		scopedKeys = append(scopedKeys, c.key)
	}
	assert.ElementsMatch(t, []string{colKeyHarness, colKeySize, colKeyModified, colKeyGroups}, scopedKeys,
		"scoped mode offers harness/size/modified/groups (no project)")

	global := &watchModel{global: true, entries: []SessionEntry{{SessionID: "a"}}, activeSessions: make(map[string]*SessionState)}
	var globalKeys []string
	for _, c := range global.toggleableColumns() {
		globalKeys = append(globalKeys, c.key)
	}
	assert.ElementsMatch(t, []string{colKeyHarness, colKeyProject, colKeySize, colKeyModified, colKeyGroups}, globalKeys,
		"global mode also offers project")
}

// Toggling persists to ~/.tclaude/config.json and survives a reload; reset
// clears the persisted overrides.
func TestWatchColumns_TogglePersistsAndReloads(t *testing.T) {
	setupHarnessTestHome(t) // temp HOME so config writes are sandboxed

	m := &watchModel{
		entries:        []SessionEntry{{SessionID: "a", Harness: "claude"}},
		activeSessions: make(map[string]*SessionState),
		colOverrides:   map[string]bool{},
	}

	m.setColumnOverride(colKeyHarness, true)
	m.setColumnOverride(colKeySize, false)

	// Persisted to disk under conv_watch.columns.
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.ConvWatch, "conv_watch block written")
	assert.Equal(t, true, cfg.ConvWatch.Columns[colKeyHarness])
	assert.Equal(t, false, cfg.ConvWatch.Columns[colKeySize])

	// A fresh model loads the same overrides.
	loaded := loadColumnOverrides()
	assert.Equal(t, true, loaded[colKeyHarness])
	assert.Equal(t, false, loaded[colKeySize])

	// Reset clears everything, back to auto-defaults.
	m.resetColumnOverrides()
	assert.Empty(t, m.colOverrides, "in-memory overrides cleared")
	cfg2, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg2.ConvWatch.Columns, "persisted overrides cleared")
	assert.Empty(t, loadColumnOverrides(), "a fresh load sees no overrides")
}

// The overlay renders each toggleable column with a checkbox reflecting its
// effective visibility, and a cursor marker on the active row.
func TestWatchColumns_RenderSelector(t *testing.T) {
	m := &watchModel{
		global:         true,
		entries:        []SessionEntry{{SessionID: "a", Harness: "codex"}},
		groupsByConv:   map[string][]string{"a": {"alpha"}},
		activeSessions: make(map[string]*SessionState),
		colOverrides:   map[string]bool{colKeySize: false},
		columnCursor:   0,
	}
	out := m.renderColumnSelector()
	assert.Contains(t, out, "PROJECT", "global mode lists the project column")
	assert.Contains(t, out, "[ ] SIZE", "SIZE overridden to hidden → empty checkbox")
	assert.Contains(t, out, "[x] HARNESS", "codex present → harness auto-on, checked")
	assert.Contains(t, out, "▸ ", "cursor marker present on the active row")
}
