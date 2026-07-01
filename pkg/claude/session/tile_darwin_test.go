//go:build darwin

package session

import (
	"strings"
	"testing"
)

// Finder returns "L, T, R, B"; parse into an origin+size Rect.
func TestParseMacDesktopBounds(t *testing.T) {
	r, ok := parseMacDesktopBounds("0, 0, 1920, 1080\n")
	if !ok {
		t.Fatalf("expected ok")
	}
	if r != (Rect{X: 0, Y: 0, W: 1920, H: 1080}) {
		t.Errorf("got %+v", r)
	}

	// A non-zero origin (e.g. a secondary display) keeps the offset and
	// derives width/height from the corners.
	r, ok = parseMacDesktopBounds("100, 50, 1300, 850")
	if !ok || r != (Rect{X: 100, Y: 50, W: 1200, H: 800}) {
		t.Errorf("offset desktop: ok=%v r=%+v", ok, r)
	}
}

func TestParseMacDesktopBounds_Rejects(t *testing.T) {
	for _, bad := range []string{"", "0,0,0", "a, b, c, d", "0, 0, 0, 0", "10, 10, 5, 5"} {
		if _, ok := parseMacDesktopBounds(bad); ok {
			t.Errorf("expected reject for %q", bad)
		}
	}
}

// The generated AppleScript embeds the tty and the {L,T,R,B} bounds and
// targets the right app; an unsupported terminal yields an empty script.
func TestBuildMacTileScript(t *testing.T) {
	r := Rect{X: 10, Y: 20, W: 300, H: 400} // → bounds {10,20,310,420}

	term := buildMacTileScript("Terminal", "/dev/ttys003", r)
	if !strings.Contains(term, `tell application "Terminal"`) {
		t.Errorf("Terminal script missing app tell: %s", term)
	}
	if !strings.Contains(term, `"/dev/ttys003"`) {
		t.Errorf("Terminal script missing tty: %s", term)
	}
	if !strings.Contains(term, "{10, 20, 310, 420}") {
		t.Errorf("Terminal script missing bounds: %s", term)
	}

	iterm := buildMacTileScript("iTerm2", "/dev/ttys004", r)
	if !strings.Contains(iterm, `tell application "iTerm2"`) || !strings.Contains(iterm, "sessions of t") {
		t.Errorf("iTerm script wrong shape: %s", iterm)
	}
	if !strings.Contains(iterm, "{10, 20, 310, 420}") {
		t.Errorf("iTerm script missing bounds: %s", iterm)
	}

	if s := buildMacTileScript("Alacritty", "/dev/ttys005", r); s != "" {
		t.Errorf("unsupported terminal should yield empty script, got %q", s)
	}
	if s := buildMacTileScript("", "/dev/ttys006", r); s != "" {
		t.Errorf("empty terminal should yield empty script, got %q", s)
	}
}
