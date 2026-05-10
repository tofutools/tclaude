package agent

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	if len(mm.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(mm.entries))
	}
	if mm.loadErr != "" {
		t.Errorf("loadErr should be cleared, got %q", mm.loadErr)
	}
	if mm.cursor != 0 {
		t.Errorf("cursor was 5 (out of bounds for new list of 2); want reset to 0, got %d", mm.cursor)
	}
}

// Load error surfaces in loadErr without dropping existing entries.
func TestInboxWatch_LoadErrorPreservesEntries(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}}
	m.cursor = 0

	m2, _ := m.Update(inboxLoadedMsg{err: errors.New("daemon down")})
	mm := m2.(*inboxWatchModel)
	if mm.loadErr != "daemon down" {
		t.Errorf("loadErr = %q, want %q", mm.loadErr, "daemon down")
	}
	if len(mm.entries) != 1 {
		t.Errorf("entries should be preserved on load error, got %d", len(mm.entries))
	}
}

// Up/down navigation respects bounds.
func TestInboxWatch_NavigationBounds(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1}, {ID: 2}, {ID: 3}}

	// down advances
	m2, _ := m.Update(asKey("j"))
	mm := m2.(*inboxWatchModel)
	if mm.cursor != 1 {
		t.Errorf("after j, cursor = %d, want 1", mm.cursor)
	}

	// down past end stays at last
	mm.cursor = 2
	m3, _ := mm.Update(asKey("j"))
	mm = m3.(*inboxWatchModel)
	if mm.cursor != 2 {
		t.Errorf("j at end should stay at 2, got %d", mm.cursor)
	}

	// up at top stays at 0
	mm.cursor = 0
	m4, _ := mm.Update(asKey("k"))
	mm = m4.(*inboxWatchModel)
	if mm.cursor != 0 {
		t.Errorf("k at top should stay at 0, got %d", mm.cursor)
	}

	// G jumps to end
	m5, _ := mm.Update(asKey("G"))
	mm = m5.(*inboxWatchModel)
	if mm.cursor != 2 {
		t.Errorf("G should jump to last (2), got %d", mm.cursor)
	}

	// g jumps to top
	m6, _ := mm.Update(asKey("g"))
	mm = m6.(*inboxWatchModel)
	if mm.cursor != 0 {
		t.Errorf("g should jump to first (0), got %d", mm.cursor)
	}
}

// Loaded message body switches into read view; esc returns to list.
func TestInboxWatch_ReadViewToggle(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})

	// Simulate the message-loaded message arriving.
	m2, _ := m.Update(inboxMessageLoadedMsg{id: 42, body: "From: x · Subject: y\n\nhello"})
	mm := m2.(*inboxWatchModel)
	if mm.readingID != 42 {
		t.Errorf("readingID = %d, want 42", mm.readingID)
	}
	if mm.readingBody == "" {
		t.Error("readingBody should be populated")
	}

	// Esc returns to list.
	m3, _ := mm.Update(asKey("esc"))
	mm = m3.(*inboxWatchModel)
	if mm.readingID != 0 {
		t.Errorf("readingID should be 0 after esc, got %d", mm.readingID)
	}
	if mm.readingBody != "" {
		t.Errorf("readingBody should be cleared after esc, got %q", mm.readingBody)
	}
}

// Read-failure flips back to the list view with a status message.
func TestInboxWatch_ReadErrorFlipsBack(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.readingID = 42
	m.readingBody = "(loading...)"

	m2, _ := m.Update(inboxMessageLoadedMsg{id: 42, err: errors.New("not found")})
	mm := m2.(*inboxWatchModel)
	if mm.readingID != 0 {
		t.Errorf("readingID should be 0 on read error, got %d", mm.readingID)
	}
	if mm.statusMsg == "" {
		t.Error("statusMsg should describe the read failure")
	}
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
	if !mm.replyFocused {
		t.Fatal("r should set replyFocused = true")
	}

	// While in reply mode, list keys must NOT mutate cursor or close
	// the read view.
	for _, k := range []string{"j", "k", "q", "G", "g"} {
		m3, _ := mm.Update(asKey(k))
		mm = m3.(*inboxWatchModel)
		if mm.cursor != 0 {
			t.Errorf("key %q while replyFocused should not move cursor; got %d", k, mm.cursor)
		}
		if mm.readingID != 42 {
			t.Errorf("key %q while replyFocused should not exit read view; readingID=%d", k, mm.readingID)
		}
	}

	// Esc cancels reply mode and returns to read view (NOT to list).
	m4, _ := mm.Update(asKey("esc"))
	mm = m4.(*inboxWatchModel)
	if mm.replyFocused {
		t.Error("esc in reply mode should clear replyFocused")
	}
	if mm.readingID != 42 {
		t.Errorf("esc in reply mode should keep read view open; readingID=%d", mm.readingID)
	}
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
	if mm.replyFocused {
		t.Error("successful send should clear replyFocused")
	}
	if mm.replyTextarea.Value() != "" {
		t.Errorf("textarea should be cleared on success, got %q", mm.replyTextarea.Value())
	}
	if !contains(mm.statusMsg, "sent") {
		t.Errorf("statusMsg should announce success, got %q", mm.statusMsg)
	}
}

func TestInboxWatch_ReplyFailureKeepsDraft(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.readingID = 42
	m.replyFocused = true
	m.replyTextarea.SetValue("hello")

	m2, _ := m.Update(inboxReplySentMsg{id: 42, err: errors.New("daemon down")})
	mm := m2.(*inboxWatchModel)
	if !mm.replyFocused {
		t.Error("failed send should keep replyFocused so user can retry")
	}
	if mm.replyTextarea.Value() != "hello" {
		t.Errorf("draft should be preserved on failure, got %q", mm.replyTextarea.Value())
	}
	if !contains(mm.statusMsg, "failed") {
		t.Errorf("statusMsg should mention the failure, got %q", mm.statusMsg)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
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
		if mm.cursor != 0 {
			t.Errorf("key %q in read view should not move list cursor; got %d", k, mm.cursor)
		}
		if mm.readingID != 42 {
			t.Errorf("key %q in read view should not exit read mode; readingID = %d", k, mm.readingID)
		}
		// And `/` must NOT slip through to enable search mode while
		// the user is reading — that would surprise the user the next
		// time they esc back to the list.
		if mm.searchFocused {
			t.Errorf("key %q in read view should not enable search mode", k)
		}
	}
}

// `/` in the list view focuses the search input. Esc with non-empty
// value clears the value (still focused). Esc again exits search mode.
func TestInboxWatch_SearchEscapeLadder(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 1, Subject: "alpha"}}

	m2, _ := m.Update(asKey("/"))
	mm := m2.(*inboxWatchModel)
	if !mm.searchFocused {
		t.Fatal("/ should focus the search input")
	}
	mm.searchInput.SetValue("foo")

	// First esc: clears value, stays focused.
	m3, _ := mm.Update(asKey("esc"))
	mm = m3.(*inboxWatchModel)
	if !mm.searchFocused {
		t.Error("esc with non-empty filter should keep search focused")
	}
	if mm.searchInput.Value() != "" {
		t.Errorf("esc should clear the filter; got %q", mm.searchInput.Value())
	}

	// Second esc: exits search mode entirely.
	m4, _ := mm.Update(asKey("esc"))
	mm = m4.(*inboxWatchModel)
	if mm.searchFocused {
		t.Error("esc on empty filter should exit search mode")
	}
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
	if len(v) != 1 || v[0].ID != 1 {
		t.Errorf("HOTFIX should match #1 only, got %+v", v)
	}

	// From substring.
	m.searchInput.SetValue("alice")
	v = m.visibleEntries()
	if len(v) != 1 || v[0].ID != 2 {
		t.Errorf("alice should match #2 only, got %+v", v)
	}

	// Group substring matches multiple.
	m.searchInput.SetValue("team-a")
	v = m.visibleEntries()
	if len(v) != 2 {
		t.Errorf("team-a should match 2 entries, got %d", len(v))
	}

	// Empty filter passes everything through unchanged.
	m.searchInput.SetValue("")
	v = m.visibleEntries()
	if len(v) != 3 {
		t.Errorf("empty filter should pass all 3 entries, got %d", len(v))
	}

	// Whitespace-only filter is treated as empty.
	m.searchInput.SetValue("   ")
	v = m.visibleEntries()
	if len(v) != 3 {
		t.Errorf("whitespace-only filter should pass all 3 entries, got %d", len(v))
	}
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
	if m.cursor != 0 {
		t.Errorf("cursor 2 should snap to 0 when filter shrinks list to 2 visible entries past cursor; got %d", m.cursor)
	}

	// j moves to filtered index 1.
	m2, _ := m.Update(asKey("j"))
	mm := m2.(*inboxWatchModel)
	if mm.cursor != 1 {
		t.Errorf("j should move to 1 (still inside filtered len=2), got %d", mm.cursor)
	}

	// j again must NOT advance past filtered end (len=2 → max index 1).
	m3, _ := mm.Update(asKey("j"))
	mm = m3.(*inboxWatchModel)
	if mm.cursor != 1 {
		t.Errorf("j past filtered end should clamp at 1, got %d", mm.cursor)
	}

	// G also clamps to filtered end.
	mm.cursor = 0
	m4, _ := mm.Update(asKey("G"))
	mm = m4.(*inboxWatchModel)
	if mm.cursor != 1 {
		t.Errorf("G should jump to filtered end (1), got %d", mm.cursor)
	}
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
	if mm.readingID != 30 {
		t.Errorf("enter on filtered cursor=1 should read ID 30 (alphabet), got %d", mm.readingID)
	}
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
	if mm.searchInput.Value() != "alpha" {
		t.Errorf("filter should survive reload, got %q", mm.searchInput.Value())
	}
	if got := len(mm.visibleEntries()); got != 2 {
		t.Errorf("filtered visible count after reload = %d, want 2", got)
	}
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
	if mm.deleteConfirmID != 20 {
		t.Fatalf("deleteConfirmID = %d, want 20 (cursor row)", mm.deleteConfirmID)
	}

	// While modal open, j/k must NOT move cursor or commit deletion.
	for _, k := range []string{"j", "k", "down", "up", "enter"} {
		mm.deleteConfirmID = 20
		mm.cursor = 1
		m3, _ := mm.Update(asKey(k))
		mm = m3.(*inboxWatchModel)
		if mm.cursor != 1 {
			t.Errorf("key %q during delete-confirm should not move cursor; got %d", k, mm.cursor)
		}
		if mm.deleteConfirmID != 0 {
			t.Errorf("key %q during delete-confirm should cancel (clear deleteConfirmID); got %d", k, mm.deleteConfirmID)
		}
		// Entries must be untouched on cancel.
		if len(mm.entries) != 3 {
			t.Errorf("key %q should not remove entries; got %d", k, len(mm.entries))
		}
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
	if mm.deleteConfirmID != 0 {
		t.Errorf("y should clear deleteConfirmID; got %d", mm.deleteConfirmID)
	}
	if len(mm.entries) != 2 {
		t.Fatalf("y should optimistically remove the row; entries = %d", len(mm.entries))
	}
	for _, e := range mm.entries {
		if e.ID == 20 {
			t.Errorf("entry #20 should be removed; still present")
		}
	}
	if cmd == nil {
		t.Error("y should return a cmd to POST the delete")
	}
}

// On a delete error, the model reloads (which restores the entry from
// the daemon if the row is still there). statusMsg announces failure.
func TestInboxWatch_DeleteSentErrorTriggersReload(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	m.entries = []inboxEntry{{ID: 10}, {ID: 30}} // already optimistically removed #20
	m.statusMsg = "deleting #20…"

	m2, cmd := m.Update(inboxDeleteSentMsg{id: 20, err: errors.New("boom")})
	mm := m2.(*inboxWatchModel)
	if cmd == nil {
		t.Error("delete error should trigger a reload cmd to restore state")
	}
	if !contains(mm.statusMsg, "failed") {
		t.Errorf("statusMsg should announce failure; got %q", mm.statusMsg)
	}
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
		if mm.deleteConfirmID != 0 {
			t.Errorf("key %q in read view should not open delete-confirm; got %d", k, mm.deleteConfirmID)
		}
		if mm.readingID != 42 {
			t.Errorf("key %q in read view should not exit read mode; got %d", k, mm.readingID)
		}
	}
}

// Empty list: pressing delete must NOT open the modal (nothing to
// confirm).
func TestInboxWatch_DeleteOnEmptyListNoOp(t *testing.T) {
	m := newInboxWatchModel(&inboxWatchParams{Limit: 50})
	// no entries
	m2, _ := m.Update(asKey("delete"))
	mm := m2.(*inboxWatchModel)
	if mm.deleteConfirmID != 0 {
		t.Errorf("delete on empty list should not open modal; got %d", mm.deleteConfirmID)
	}
}

// removeEntryByID drops the matching row, leaves order otherwise
// intact, and is a no-op when the ID isn't present.
func TestInboxWatch_RemoveEntryByID(t *testing.T) {
	in := []inboxEntry{{ID: 1}, {ID: 2}, {ID: 3}}
	out := removeEntryByID(in, 2)
	if len(out) != 2 || out[0].ID != 1 || out[1].ID != 3 {
		t.Errorf("removeEntryByID(2) = %+v, want [1, 3]", out)
	}
	out2 := removeEntryByID(in, 99)
	if len(out2) != 3 {
		t.Errorf("removeEntryByID for missing ID should return original; got %+v", out2)
	}
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
	if mm.searchFocused {
		t.Error("down arrow should unfocus search")
	}
	if mm.cursor != 1 {
		t.Errorf("down arrow should advance cursor to 1; got %d", mm.cursor)
	}
}
