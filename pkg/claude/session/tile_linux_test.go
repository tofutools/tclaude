//go:build linux

package session

import (
	"strings"
	"testing"
)

func TestParseXdotoolGeometry(t *testing.T) {
	r, ok := parseXdotoolGeometry("1920 1080\n")
	if !ok || r != (Rect{W: 1920, H: 1080}) {
		t.Errorf("ok=%v r=%+v", ok, r)
	}
	for _, bad := range []string{"", "1920", "1920 1080 1", "a b", "0 1080", "1920 0"} {
		if _, ok := parseXdotoolGeometry(bad); ok {
			t.Errorf("expected reject for %q", bad)
		}
	}
}

func TestParseXrandrCurrent(t *testing.T) {
	out := "Screen 0: minimum 320 x 200, current 2560 x 1440, maximum 16384 x 16384\n" +
		"DP-1 connected primary 2560x1440+0+0 ...\n"
	r, ok := parseXrandrCurrent(out)
	if !ok || r != (Rect{W: 2560, H: 1440}) {
		t.Errorf("ok=%v r=%+v", ok, r)
	}
	if _, ok := parseXrandrCurrent("no screen line here"); ok {
		t.Errorf("expected reject when 'current' absent")
	}
}

func TestParseWSLWorkArea(t *testing.T) {
	r, ok := parseWSLWorkArea("0 0 1920 1040\n")
	if !ok || r != (Rect{X: 0, Y: 0, W: 1920, H: 1040}) {
		t.Errorf("ok=%v r=%+v", ok, r)
	}
	// A secondary monitor at a non-zero origin keeps the offset.
	r, ok = parseWSLWorkArea("1920 0 2560 1400")
	if !ok || r != (Rect{X: 1920, Y: 0, W: 2560, H: 1400}) {
		t.Errorf("offset: ok=%v r=%+v", ok, r)
	}
	for _, bad := range []string{"", "0 0 1920", "a b c d", "0 0 0 1040", "0 0 1920 0"} {
		if _, ok := parseWSLWorkArea(bad); ok {
			t.Errorf("expected reject for %q", bad)
		}
	}
}

func TestSanitizeTileSessionID(t *testing.T) {
	// Realistic ids pass through untouched.
	for _, ok := range []string{"4d01388a", "agt_58f152", "a.b-c_d:e", "ABC123"} {
		if got := sanitizeTileSessionID(ok); got != ok {
			t.Errorf("sanitize(%q) = %q, want unchanged", ok, got)
		}
	}
	// Anything with an injection-capable char is rejected outright.
	for _, bad := range []string{"", "a'b", "a b", "a;b", "a$b", "a\"b", "a`b", "a\nb", "a}b"} {
		if got := sanitizeTileSessionID(bad); got != "" {
			t.Errorf("sanitize(%q) = %q, want \"\"", bad, got)
		}
	}
}

// buildWSLTileScript emits one $targets row per valid spec, embeds the
// rect coordinates, drops specs with an empty/unsafe id, and returns ""
// when nothing valid remains.
func TestBuildWSLTileScript(t *testing.T) {
	specs := []TileSpec{
		{SessionID: "sess1"},
		{SessionID: ""},        // dropped: empty
		{SessionID: "bad id'"}, // dropped: unsafe
		{SessionID: "sess2"},
	}
	rects := []Rect{
		{X: 0, Y: 0, W: 500, H: 500},
		{X: 500, Y: 0, W: 500, H: 500},
		{X: 0, Y: 500, W: 500, H: 500},
		{X: 500, Y: 500, W: 500, H: 500},
	}
	script := buildWSLTileScript(specs, rects)
	if !strings.Contains(script, "tclaude:sess1") || !strings.Contains(script, "tclaude:sess2") {
		t.Errorf("missing valid patterns: %s", script)
	}
	if strings.Contains(script, "bad id") {
		t.Errorf("unsafe id leaked into script: %s", script)
	}
	// sess2 uses rects[3] (index-aligned), so its geometry is 500,500.
	if !strings.Contains(script, "pat = 'tclaude:sess2'; x = 500; y = 500; w = 500; h = 500") {
		t.Errorf("sess2 row missing/misaligned: %s", script)
	}
	if !strings.Contains(script, "SetWindowPos") || !strings.Contains(script, "EnumWindows") {
		t.Errorf("script missing P/Invoke scaffolding: %s", script)
	}
	// Exactly two target rows (the two valid specs).
	if n := strings.Count(script, "pat = 'tclaude:"); n != 2 {
		t.Errorf("expected 2 target rows, got %d", n)
	}

	// No valid specs → empty script (caller skips the PowerShell launch).
	if s := buildWSLTileScript([]TileSpec{{SessionID: ""}}, []Rect{{}}); s != "" {
		t.Errorf("expected empty script for no valid specs, got %q", s)
	}
}
