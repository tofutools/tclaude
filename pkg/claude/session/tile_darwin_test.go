//go:build darwin

package session

import (
	"os/exec"
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

// NSScreen enumeration output parses one Rect per screen; malformed and
// non-positive lines are skipped.
func TestParseMacMonitors(t *testing.T) {
	// Two monitors: main 1920x1055 (menu bar excluded) and a right one at
	// x=1920. A blank line and a garbage line are dropped.
	out := "0 25 1920 1055\n1920 0 2560 1440\n\nbad line here\n0 0 0 100\n"
	mons := parseMacMonitors(out)
	if len(mons) != 2 {
		t.Fatalf("expected 2 monitors, got %d: %+v", len(mons), mons)
	}
	if mons[0] != (Rect{X: 0, Y: 25, W: 1920, H: 1055}) {
		t.Errorf("monitor 0: %+v", mons[0])
	}
	if mons[1] != (Rect{X: 1920, Y: 0, W: 2560, H: 1440}) {
		t.Errorf("monitor 1: %+v", mons[1])
	}
	if parseMacMonitors("") != nil {
		t.Errorf("empty output → nil")
	}
}

// The read-bounds script targets the right app/tty and returns the
// four bounds items; unsupported terminals yield an empty script.
func TestBuildMacReadBoundsScript(t *testing.T) {
	term := buildMacReadBoundsScript("Terminal", "/dev/ttys003")
	if !strings.Contains(term, `tell application "Terminal"`) ||
		!strings.Contains(term, `"/dev/ttys003"`) ||
		!strings.Contains(term, "bounds of w") {
		t.Errorf("Terminal read script wrong: %s", term)
	}
	iterm := buildMacReadBoundsScript("iTerm2", "/dev/ttys004")
	if !strings.Contains(iterm, "sessions of t") || !strings.Contains(iterm, "bounds of w") {
		t.Errorf("iTerm read script wrong: %s", iterm)
	}
	if s := buildMacReadBoundsScript("kitty", "/dev/ttys005"); s != "" {
		t.Errorf("unsupported terminal → empty read script, got %q", s)
	}
}

// Regression: the emit fragment must evaluate to TEXT ("L, T, R, B").
// AppleScript's & with an integer left operand builds a LIST instead,
// which osascript prints as "L, , , T, …" — parseMacDesktopBounds then
// rejects it, every window fails its bounds read, and the tiling pass
// silently no-ops. Execute the real fragment through osascript with a
// stubbed window record and assert the round-trip through the parser.
func TestMacReadBoundsEmit_YieldsParseableText(t *testing.T) {
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("osascript not available")
	}
	script := "set w to {bounds:{100, 200, 900, 800}}\n" + macReadBoundsEmit
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		t.Fatalf("osascript failed: %v (output %q)", err, out)
	}
	r, ok := parseMacDesktopBounds(string(out))
	if !ok {
		t.Fatalf("emit output %q did not parse — the fragment must yield text, not a list", strings.TrimSpace(string(out)))
	}
	if r != (Rect{X: 100, Y: 200, W: 800, H: 600}) {
		t.Errorf("parsed rect: %+v", r)
	}
}

// The window-bounds parser reuses the desktop parser: "L, T, R, B" →
// origin + size. Sanity-check the shared behavior for a window.
func TestParseWindowBounds_ReusesDesktopParser(t *testing.T) {
	r, ok := parseMacDesktopBounds("100, 200, 740, 680")
	if !ok || r != (Rect{X: 100, Y: 200, W: 640, H: 480}) {
		t.Errorf("window bounds parse: ok=%v r=%+v", ok, r)
	}
}
