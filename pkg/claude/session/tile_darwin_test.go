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

// The batched set-bounds script walks the app's windows once, dispatching
// on tty with an if / else-if chain — one {L,T,R,B} bounds tuple per
// move. Unsupported apps and empty move sets yield an empty script.
func TestBuildMacBatchTileScript(t *testing.T) {
	moves := []macMove{
		{tty: "/dev/ttys003", r: Rect{X: 10, Y: 20, W: 300, H: 400}},  // → {10,20,310,420}
		{tty: "/dev/ttys007", r: Rect{X: 320, Y: 20, W: 300, H: 400}}, // → {320,20,620,420}
	}

	term := buildMacBatchTileScript("Terminal", moves)
	if !strings.Contains(term, `tell application "Terminal"`) {
		t.Errorf("Terminal script missing app tell: %s", term)
	}
	for _, want := range []string{
		`if tt is "/dev/ttys003" then`, "{10, 20, 310, 420}",
		`else if tt is "/dev/ttys007" then`, "{320, 20, 620, 420}",
	} {
		if !strings.Contains(term, want) {
			t.Errorf("Terminal script missing %q: %s", want, term)
		}
	}
	if strings.Contains(term, "\treturn\n") {
		t.Errorf("batch script must not return early (several moves may match): %s", term)
	}

	iterm := buildMacBatchTileScript("iTerm2", moves)
	if !strings.Contains(iterm, `tell application "iTerm2"`) || !strings.Contains(iterm, "sessions of t") {
		t.Errorf("iTerm script wrong shape: %s", iterm)
	}
	if !strings.Contains(iterm, "{10, 20, 310, 420}") || !strings.Contains(iterm, "{320, 20, 620, 420}") {
		t.Errorf("iTerm script missing bounds: %s", iterm)
	}

	if s := buildMacBatchTileScript("Alacritty", moves); s != "" {
		t.Errorf("unsupported terminal should yield empty script, got %q", s)
	}
	if s := buildMacBatchTileScript("Terminal", nil); s != "" {
		t.Errorf("no moves should yield empty script, got %q", s)
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

// The enumeration script guards on `is running` (so a closed app is never
// launched), targets the right window hierarchy per app, and builds its
// output from a TEXT `out` so & stays string concatenation. Unsupported
// apps yield an empty script.
func TestBuildMacEnumBoundsScript(t *testing.T) {
	term := buildMacEnumBoundsScript("Terminal")
	if !strings.HasPrefix(term, `set out to ""`) {
		t.Errorf("enum script must start from a TEXT out (& list-concat hazard): %s", term)
	}
	if !strings.Contains(term, `if application "Terminal" is running then`) ||
		!strings.Contains(term, `tell application "Terminal"`) ||
		!strings.Contains(term, "tabs of w") ||
		!strings.Contains(term, "tty of t") {
		t.Errorf("Terminal enum script wrong shape: %s", term)
	}
	iterm := buildMacEnumBoundsScript("iTerm2")
	if !strings.Contains(iterm, `if application "iTerm2" is running then`) ||
		!strings.Contains(iterm, "sessions of t") ||
		!strings.Contains(iterm, "tty of s") {
		t.Errorf("iTerm enum script wrong shape: %s", iterm)
	}
	if s := buildMacEnumBoundsScript("kitty"); s != "" {
		t.Errorf("unsupported terminal → empty enum script, got %q", s)
	}
}

// Regression: the per-line emit must build TEXT ("tty L T R B").
// AppleScript's & with an integer left operand builds a LIST instead,
// which osascript prints with extra separators — parseMacEnumBounds then
// rejects every line, every window fails its bounds read, and the tiling
// pass silently no-ops. Execute the real fragment through osascript with
// stubbed values and assert the round-trip through the parser.
func TestMacEnumEmitLine_YieldsParseableText(t *testing.T) {
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("osascript not available")
	}
	script := "set out to \"\"\n" +
		"set b to {100, 200, 900, 800}\n" +
		"set t to {tty:\"/dev/ttys003\"}\n" +
		macEnumEmitLine("t") + "\n" +
		"return out"
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		t.Fatalf("osascript failed: %v (output %q)", err, out)
	}
	m := parseMacEnumBounds(string(out))
	r, ok := m["/dev/ttys003"]
	if !ok {
		t.Fatalf("emit output %q did not parse — the fragment must yield text, not a list", strings.TrimSpace(string(out)))
	}
	if r != (Rect{X: 100, Y: 200, W: 800, H: 600}) {
		t.Errorf("parsed rect: %+v", r)
	}
}

// Enumeration output parses into tty → current-bounds Rects; malformed
// and non-positive-size lines are skipped.
func TestParseMacEnumBounds(t *testing.T) {
	out := "/dev/ttys003 100 200 900 800\n" +
		"/dev/ttys007 0 25 640 505\n" +
		"\n" +
		"garbage line\n" +
		"/dev/ttys009 10 10 5 5\n" + // negative size → skipped
		"/dev/ttys010 a b c d\n"
	m := parseMacEnumBounds(out)
	if len(m) != 2 {
		t.Fatalf("expected 2 windows, got %d: %+v", len(m), m)
	}
	if m["/dev/ttys003"] != (Rect{X: 100, Y: 200, W: 800, H: 600}) {
		t.Errorf("ttys003: %+v", m["/dev/ttys003"])
	}
	if m["/dev/ttys007"] != (Rect{X: 0, Y: 25, W: 640, H: 480}) {
		t.Errorf("ttys007: %+v", m["/dev/ttys007"])
	}
	if len(parseMacEnumBounds("")) != 0 {
		t.Errorf("empty output → empty map")
	}
}
