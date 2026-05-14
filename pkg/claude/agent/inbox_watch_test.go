package agent

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// asKey wraps a key string into a KeyMsg for Update. The bubbletea v2
// API distinguishes KeyPressMsg (what arrives in production) from raw
// KeyMsg; tests use KeyPressMsg with a synthetic Code for the bindings
// our handler keys off (handler matches msg.String() so the surface
// name is what matters).
func asKey(s string) tea.KeyMsg {
	return tea.KeyPressMsg{Text: s, Code: rune(s[0])}
}

// Loading entries populates the list. Cursor stays valid; loadErr clears.
func TestInboxWatch_LoadedReplacesEntries(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.loadErr = "stale"
	m.cursor = 5

	m2, _ := m.Update(inboxLoadedMsg{entries: []inboxEntry{
		{ID: 1, Subject: "first"},
		{ID: 2, Subject: "second"},
	}})
	mm := m2.(*inboxWatchModel)
	require.Len(t, mm.entries, 2)
	assert.Empty(t, mm.loadErr, "loadErr should be cleared")
	assert.Equal(t, 0, mm.cursor, "cursor was 5 (out of bounds for new list of 2); want reset to 0")
}

// Load error surfaces in loadErr without dropping existing entries.
func TestInboxWatch_LoadErrorPreservesEntries(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}}
	m.cursor = 0

	m2, _ := m.Update(inboxLoadedMsg{err: errors.New("daemon down")})
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, "daemon down", mm.loadErr)
	assert.Len(t, mm.entries, 1, "entries should be preserved on load error")
}

// Up/down navigation respects bounds.
func TestInboxWatch_NavigationBounds(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}, {ID: 2}, {ID: 3}}

	// down advances
	m2, _ := m.Update(asKey("j"))
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, 1, mm.cursor, "after j, cursor")

	// down past end stays at last
	mm.cursor = 2
	m3, _ := mm.Update(asKey("j"))
	mm = m3.(*inboxWatchModel)
	assert.Equal(t, 2, mm.cursor, "j at end should stay at 2")

	// up at top stays at 0
	mm.cursor = 0
	m4, _ := mm.Update(asKey("k"))
	mm = m4.(*inboxWatchModel)
	assert.Equal(t, 0, mm.cursor, "k at top should stay at 0")

	// G jumps to end
	m5, _ := mm.Update(asKey("G"))
	mm = m5.(*inboxWatchModel)
	assert.Equal(t, 2, mm.cursor, "G should jump to last (2)")

	// g jumps to top
	m6, _ := mm.Update(asKey("g"))
	mm = m6.(*inboxWatchModel)
	assert.Equal(t, 0, mm.cursor, "g should jump to first (0)")
}

// Loaded message body switches into read view; esc returns to list.
func TestInboxWatch_ReadViewToggle(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})

	// Simulate the message-loaded message arriving.
	m2, _ := m.Update(inboxMessageLoadedMsg{id: 42, body: "From: x · Subject: y\n\nhello"})
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, int64(42), mm.readingID)
	assert.NotEmpty(t, mm.readingBody, "readingBody should be populated")

	// Esc returns to list.
	m3, _ := mm.Update(asKey("esc"))
	mm = m3.(*inboxWatchModel)
	assert.Equal(t, int64(0), mm.readingID, "readingID should be 0 after esc")
	assert.Empty(t, mm.readingBody, "readingBody should be cleared after esc")
}

// Read-failure flips back to the list view with a status message.
func TestInboxWatch_ReadErrorFlipsBack(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.readingID = 42
	m.readingBody = "(loading...)"

	m2, _ := m.Update(inboxMessageLoadedMsg{id: 42, err: errors.New("not found")})
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, int64(0), mm.readingID, "readingID should be 0 on read error")
	assert.NotEmpty(t, mm.statusMsg, "statusMsg should describe the read failure")
}

// `r` in the read view opens the reply textarea. While the textarea
// is focused, list keys (j/k/g/G) and read-mode keys (q) are ignored.
func TestInboxWatch_ReplyOpensAndIsolatesKeys(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}, {ID: 2}}
	m.readingID = 42
	m.readingBody = "hello"

	// Press r to open reply.
	m2, _ := m.Update(asKey("r"))
	mm := m2.(*inboxWatchModel)
	require.True(t, mm.replyFocused, "r should set replyFocused = true")

	// While in reply mode, list keys must NOT mutate cursor or close
	// the read view.
	for _, k := range []string{"j", "k", "q", "G", "g"} {
		m3, _ := mm.Update(asKey(k))
		mm = m3.(*inboxWatchModel)
		assert.Equal(t, 0, mm.cursor, "key %q while replyFocused should not move cursor", k)
		assert.Equal(t, int64(42), mm.readingID, "key %q while replyFocused should not exit read view", k)
	}

	// Esc cancels reply mode and returns to read view (NOT to list).
	m4, _ := mm.Update(asKey("esc"))
	mm = m4.(*inboxWatchModel)
	assert.False(t, mm.replyFocused, "esc in reply mode should clear replyFocused")
	assert.Equal(t, int64(42), mm.readingID, "esc in reply mode should keep read view open")
}

// A successful reply send clears the textarea and exits reply mode.
// Failure leaves the textarea open with a status message so the user
// can retry without re-typing.
func TestInboxWatch_ReplySentSuccessClearsTextarea(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.readingID = 42
	m.replyFocused = true
	m.replyTextarea.SetValue("hello")

	m2, _ := m.Update(inboxReplySentMsg{id: 42, err: nil})
	mm := m2.(*inboxWatchModel)
	assert.False(t, mm.replyFocused, "successful send should clear replyFocused")
	assert.Empty(t, mm.replyTextarea.Value(), "textarea should be cleared on success")
	assert.Contains(t, mm.statusMsg, "sent", "statusMsg should announce success")
}

func TestInboxWatch_ReplyFailureKeepsDraft(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.readingID = 42
	m.replyFocused = true
	m.replyTextarea.SetValue("hello")

	m2, _ := m.Update(inboxReplySentMsg{id: 42, err: errors.New("daemon down")})
	mm := m2.(*inboxWatchModel)
	assert.True(t, mm.replyFocused, "failed send should keep replyFocused so user can retry")
	assert.Equal(t, "hello", mm.replyTextarea.Value(), "draft should be preserved on failure")
	assert.Contains(t, mm.statusMsg, "failed", "statusMsg should mention the failure")
}

// While in read view, list-mode keys (j, k, /) are ignored — only
// esc/q return to the list. Pins the bug class where a stray j press
// scrolls the underlying list while the user is reading.
func TestInboxWatch_ListKeysIgnoredInReadView(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}, {ID: 2}, {ID: 3}}
	m.readingID = 42
	m.cursor = 0

	for _, k := range []string{"j", "k", "g", "G", "/", "r"} {
		m2, _ := m.Update(asKey(k))
		mm := m2.(*inboxWatchModel)
		assert.Equal(t, 0, mm.cursor, "key %q in read view should not move list cursor", k)
		assert.Equal(t, int64(42), mm.readingID, "key %q in read view should not exit read mode", k)
		// And `/` must NOT slip through to enable search mode while
		// the user is reading — that would surprise the user the next
		// time they esc back to the list.
		assert.False(t, mm.searchFocused, "key %q in read view should not enable search mode", k)
	}
}

// `/` in the list view focuses the search input. Esc with non-empty
// value clears the value (still focused). Esc again exits search mode.
func TestInboxWatch_SearchEscapeLadder(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1, Subject: "alpha"}}

	m2, _ := m.Update(asKey("/"))
	mm := m2.(*inboxWatchModel)
	require.True(t, mm.searchFocused, "/ should focus the search input")
	mm.searchInput.SetValue("foo")

	// First esc: clears value, stays focused.
	m3, _ := mm.Update(asKey("esc"))
	mm = m3.(*inboxWatchModel)
	assert.True(t, mm.searchFocused, "esc with non-empty filter should keep search focused")
	assert.Empty(t, mm.searchInput.Value(), "esc should clear the filter")

	// Second esc: exits search mode entirely.
	m4, _ := mm.Update(asKey("esc"))
	mm = m4.(*inboxWatchModel)
	assert.False(t, mm.searchFocused, "esc on empty filter should exit search mode")
}

// visibleEntries filters by case-insensitive substring across multiple
// fields. Empty filter passes everything through.
func TestInboxWatch_FilterMatchesAcrossFields(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{
		{ID: 1, Subject: "Deploy hotfix", FromShort: "ops-1", Group: "team-a"},
		{ID: 2, Subject: "lunch", FromShort: "alice", Group: "team-b"},
		{ID: 3, Subject: "review pr", FromShort: "bob", Group: "team-a"},
	}

	// Subject substring (case-insensitive).
	m.searchInput.SetValue("HOTFIX")
	v := m.visibleEntries()
	if assert.Len(t, v, 1, "HOTFIX should match #1 only") {
		assert.Equal(t, int64(1), v[0].ID)
	}

	// From substring.
	m.searchInput.SetValue("alice")
	v = m.visibleEntries()
	if assert.Len(t, v, 1, "alice should match #2 only") {
		assert.Equal(t, int64(2), v[0].ID)
	}

	// Group substring matches multiple.
	m.searchInput.SetValue("team-a")
	v = m.visibleEntries()
	assert.Len(t, v, 2, "team-a should match 2 entries")

	// Empty filter passes everything through unchanged.
	m.searchInput.SetValue("")
	v = m.visibleEntries()
	assert.Len(t, v, 3, "empty filter should pass all 3 entries")

	// Whitespace-only filter is treated as empty.
	m.searchInput.SetValue("   ")
	v = m.visibleEntries()
	assert.Len(t, v, 3, "whitespace-only filter should pass all 3 entries")
}

// Cursor stays in bounds against the FILTERED list. After the filter
// shrinks past the cursor position, clamp resets to 0; nav keys stop
// at the filtered end, not the underlying entries length.
func TestInboxWatch_NavigationRespectsFilter(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{
		{ID: 1, Subject: "alpha"},
		{ID: 2, Subject: "beta"},
		{ID: 3, Subject: "alphabet"},
	}
	m.cursor = 2

	m.searchInput.SetValue("alpha")
	m.clampCursor()
	assert.Equal(t, 0, m.cursor, "cursor 2 should snap to 0 when filter shrinks list to 2 visible entries past cursor")

	// j moves to filtered index 1.
	m2, _ := m.Update(asKey("j"))
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, 1, mm.cursor, "j should move to 1 (still inside filtered len=2)")

	// j again must NOT advance past filtered end (len=2 → max index 1).
	m3, _ := mm.Update(asKey("j"))
	mm = m3.(*inboxWatchModel)
	assert.Equal(t, 1, mm.cursor, "j past filtered end should clamp at 1")

	// G also clamps to filtered end.
	mm.cursor = 0
	m4, _ := mm.Update(asKey("G"))
	mm = m4.(*inboxWatchModel)
	assert.Equal(t, 1, mm.cursor, "G should jump to filtered end (1)")
}

// Enter on a filtered list reads the message at the FILTERED cursor
// position, not the underlying-entries position. Pins the bug where
// the wrong message would open after a filter narrowed the list.
func TestInboxWatch_EnterOnFilteredCursorReadsCorrectID(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{
		{ID: 10, Subject: "alpha"},
		{ID: 20, Subject: "beta"},
		{ID: 30, Subject: "alphabet"},
	}
	m.searchInput.SetValue("alpha")
	m.cursor = 1 // filtered index 1 = entries[2] = ID 30

	m2, _ := m.Update(asKey("enter"))
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, int64(30), mm.readingID, "enter on filtered cursor=1 should read ID 30 (alphabet)")
}

// Background reload (inboxLoadedMsg) preserves the active filter and
// clamps the cursor against the new filtered length.
func TestInboxWatch_FilterPersistsAcrossReload(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.searchInput.SetValue("alpha")
	m.cursor = 0

	m2, _ := m.Update(inboxLoadedMsg{entries: []inboxEntry{
		{ID: 1, Subject: "alpha-one"},
		{ID: 2, Subject: "beta"},
		{ID: 3, Subject: "alpha-two"},
	}})
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, "alpha", mm.searchInput.Value(), "filter should survive reload")
	assert.Len(t, mm.visibleEntries(), 2, "filtered visible count after reload")
}

// `delete` opens a y/n confirm modal pinned to the cursor's entry.
// While the modal is open, list-nav keys are ignored (every non-y key
// cancels). `y` triggers the delete command + optimistic removal.
func TestInboxWatch_DeleteOpensConfirmModal(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 10}, {ID: 20}, {ID: 30}}
	m.cursor = 1 // points at ID 20

	m2, _ := m.Update(asKey("delete"))
	mm := m2.(*inboxWatchModel)
	require.Equal(t, int64(20), mm.deleteConfirmID, "deleteConfirmID should be 20 (cursor row)")

	// While modal open, j/k must NOT move cursor or commit deletion.
	for _, k := range []string{"j", "k", "down", "up", "enter"} {
		mm.deleteConfirmID = 20
		mm.cursor = 1
		m3, _ := mm.Update(asKey(k))
		mm = m3.(*inboxWatchModel)
		assert.Equal(t, 1, mm.cursor, "key %q during delete-confirm should not move cursor", k)
		assert.Equal(t, int64(0), mm.deleteConfirmID, "key %q during delete-confirm should cancel (clear deleteConfirmID)", k)
		// Entries must be untouched on cancel.
		assert.Len(t, mm.entries, 3, "key %q should not remove entries", k)
	}
}

// `y` on the confirm modal optimistically removes the row and dispatches
// the delete cmd. The cmd execution is async and not asserted here —
// the optimistic removal is the user-visible change tested via state.
func TestInboxWatch_DeleteConfirmYRemovesOptimistically(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 10}, {ID: 20}, {ID: 30}}
	m.cursor = 1
	m.deleteConfirmID = 20

	m2, cmd := m.Update(asKey("y"))
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, int64(0), mm.deleteConfirmID, "y should clear deleteConfirmID")
	require.Len(t, mm.entries, 2, "y should optimistically remove the row")
	for _, e := range mm.entries {
		assert.NotEqual(t, int64(20), e.ID, "entry #20 should be removed; still present")
	}
	assert.NotNil(t, cmd, "y should return a cmd to POST the delete")
}

// On a delete error, the model reloads (which restores the entry from
// the daemon if the row is still there). statusMsg announces failure.
func TestInboxWatch_DeleteSentErrorTriggersReload(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 10}, {ID: 30}} // already optimistically removed #20
	m.statusMsg = "deleting #20…"

	m2, cmd := m.Update(inboxDeleteSentMsg{id: 20, err: errors.New("boom")})
	mm := m2.(*inboxWatchModel)
	assert.NotNil(t, cmd, "delete error should trigger a reload cmd to restore state")
	assert.Contains(t, mm.statusMsg, "failed", "statusMsg should announce failure")
}

// `delete` in the read view must NOT open the confirm modal — that
// would be surprising behaviour while the user is reading.
func TestInboxWatch_DeleteIgnoredInReadView(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}}
	m.readingID = 42

	for _, k := range []string{"delete", "backspace"} {
		m2, _ := m.Update(asKey(k))
		mm := m2.(*inboxWatchModel)
		assert.Equal(t, int64(0), mm.deleteConfirmID, "key %q in read view should not open delete-confirm", k)
		assert.Equal(t, int64(42), mm.readingID, "key %q in read view should not exit read mode", k)
	}
}

// Empty list: pressing delete must NOT open the modal (nothing to
// confirm).
func TestInboxWatch_DeleteOnEmptyListNoOp(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	// no entries
	m2, _ := m.Update(asKey("delete"))
	mm := m2.(*inboxWatchModel)
	assert.Equal(t, int64(0), mm.deleteConfirmID, "delete on empty list should not open modal")
}

// removeEntryByID drops the matching row, leaves order otherwise
// intact, and is a no-op when the ID isn't present.
func TestInboxWatch_RemoveEntryByID(t *testing.T) {
	in := []inboxEntry{{ID: 1}, {ID: 2}, {ID: 3}}
	out := removeEntryByID(in, 2)
	if assert.Len(t, out, 2, "removeEntryByID(2)") {
		assert.Equal(t, int64(1), out[0].ID)
		assert.Equal(t, int64(3), out[1].ID)
	}
	out2 := removeEntryByID(in, 99)
	assert.Len(t, out2, 3, "removeEntryByID for missing ID should return original")
}

// Operator mode (--target set) disables `r` (reply) in the read view
// and `del`/`backspace` in the list view, with a status message
// surfacing why.
func TestInboxWatch_OperatorViewIsReadOnly(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50, Target: "other-conv"})
	m.entries = []inboxEntry{{ID: 1}}
	m.readingID = 42

	// `r` in read view: must NOT open the reply textarea.
	m2, _ := m.Update(asKey("r"))
	mm := m2.(*inboxWatchModel)
	assert.False(t, mm.replyFocused, "operator-view r should not open reply textarea")
	assert.Contains(t, mm.statusMsg, "operator", "statusMsg should explain why reply was blocked")

	// `del` in list view: must NOT open the confirm modal.
	mm.readingID = 0
	mm.statusMsg = ""
	m3, _ := mm.Update(asKey("delete"))
	mm = m3.(*inboxWatchModel)
	assert.Equal(t, int64(0), mm.deleteConfirmID, "operator-view delete should not open confirm modal")
	assert.Contains(t, mm.statusMsg, "operator", "statusMsg should explain why delete was blocked")
}

// Up/down arrows from search-focused mode unfocus and move the cursor
// in a single keystroke (UX shortcut so the user can type, then jump
// to a result without an extra Enter or Esc).
func TestInboxWatch_ArrowFromSearchUnfocusesAndMoves(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}, {ID: 2}}
	m.searchFocused = true
	m.searchInput.Focus()

	m2, _ := m.Update(asKey("down"))
	mm := m2.(*inboxWatchModel)
	assert.False(t, mm.searchFocused, "down arrow should unfocus search")
	assert.Equal(t, 1, mm.cursor, "down arrow should advance cursor to 1")
}
