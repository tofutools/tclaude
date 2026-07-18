package terminal

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// withPreferred sets the package-level preference for one test and
// restores it afterwards, so cases don't leak into each other.
func withPreferred(t *testing.T, name string) {
	t.Helper()
	prev := preferred
	t.Cleanup(func() { preferred = prev })
	SetPreferred(name)
}

func TestCanonicalTerminalID(t *testing.T) {
	cases := []struct{ in, want string }{
		// Canonical IDs map to themselves.
		{"kitty", IDKitty},
		{"ghostty", IDGhostty},
		{"gnome-terminal", IDGnomeTerminal},
		{"terminal-app", IDTerminalApp},
		// Friendly aliases.
		{"iterm", IDITerm2},
		{"iTerm2", IDITerm2},
		{"Terminal.app", IDTerminalApp},
		{"apple", IDTerminalApp},
		{"gnome", IDGnomeTerminal},
		{"XFCE4", IDXfce4Terminal},
		// Whitespace tolerated.
		{"  kitty  ", IDKitty},
		// Unrecognised → empty.
		{"", ""},
		{"ghosty", ""},
		{"notaterminal", ""},
	}
	for _, c := range cases {
		if got := CanonicalTerminalID(c.in); got != c.want {
			t.Errorf("CanonicalTerminalID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKnownTerminalIDs(t *testing.T) {
	ids := KnownTerminalIDs()
	if !reflect.DeepEqual(ids, terminalPriority) {
		t.Fatalf("KnownTerminalIDs() = %v, want %v", ids, terminalPriority)
	}
	// Must be a copy — a caller mutating it must not corrupt the
	// package's priority order.
	ids[0] = "tampered"
	if terminalPriority[0] == "tampered" {
		t.Fatal("KnownTerminalIDs() returned the live slice, not a copy")
	}
}

// TestOrderedCandidates_NoPreference: with nothing preferred, the
// candidate order is exactly the shared priority list.
func TestOrderedCandidates_NoPreference(t *testing.T) {
	withPreferred(t, "")
	if got := orderedCandidates(); !reflect.DeepEqual(got, terminalPriority) {
		t.Fatalf("orderedCandidates() = %v, want %v", got, terminalPriority)
	}
}

// TestOrderedCandidates_Preference: a preferred terminal is spliced to
// the front, exactly once, and every other terminal is still present
// in its original relative order.
func TestOrderedCandidates_Preference(t *testing.T) {
	withPreferred(t, "kitty")
	got := orderedCandidates()

	if got[0] != IDKitty {
		t.Fatalf("orderedCandidates()[0] = %q, want kitty", got[0])
	}
	if len(got) != len(terminalPriority) {
		t.Fatalf("orderedCandidates() length = %d, want %d (no dup, no drop)", len(got), len(terminalPriority))
	}
	if slices.Index(got[1:], IDKitty) != -1 {
		t.Fatal("kitty appears twice — preferred terminal was not de-duplicated")
	}
	for _, id := range terminalPriority {
		if !slices.Contains(got, id) {
			t.Fatalf("orderedCandidates() dropped %q", id)
		}
	}
}

// TestOrderedCandidates_AliasPreference: an alias resolves to its
// canonical ID before being spliced to the front.
func TestOrderedCandidates_AliasPreference(t *testing.T) {
	withPreferred(t, "iterm")
	if got := orderedCandidates(); got[0] != IDITerm2 {
		t.Fatalf("orderedCandidates()[0] = %q, want iterm2 (alias 'iterm')", got[0])
	}
}

// TestOrderedCandidates_UnknownPreference: an unrecognised preference
// is ignored — selection falls back to plain priority order.
func TestOrderedCandidates_UnknownPreference(t *testing.T) {
	withPreferred(t, "definitely-not-a-terminal")
	if got := orderedCandidates(); !reflect.DeepEqual(got, terminalPriority) {
		t.Fatalf("orderedCandidates() = %v, want the unmodified priority list", got)
	}
}

// TestOpenWithCommand_RefusesUnderGoTest pins the TCL-584 guard: a
// test binary must never spawn a real terminal window, so
// OpenWithCommand refuses — before resolving a launcher — whenever it
// runs under `go test`. Callers that need to observe the open path in
// tests swap their own seam instead.
func TestOpenWithCommand_RefusesUnderGoTest(t *testing.T) {
	err := OpenWithCommand("echo tclaude-tcl-584")
	if !errors.Is(err, ErrRefusedInTest) {
		t.Fatalf("OpenWithCommand() under go test = %v, want ErrRefusedInTest", err)
	}
}
