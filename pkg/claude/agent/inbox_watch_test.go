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
	}
}
