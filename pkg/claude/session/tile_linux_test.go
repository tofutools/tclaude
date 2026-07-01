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

// getwindowgeometry --shell KEY=value output → current window rect.
func TestParseXdotoolGeometryShell(t *testing.T) {
	out := "WINDOW=29360135\nX=100\nY=200\nWIDTH=800\nHEIGHT=600\nSCREEN=0\n"
	r, ok := parseXdotoolGeometryShell(out)
	if !ok || r != (Rect{X: 100, Y: 200, W: 800, H: 600}) {
		t.Errorf("ok=%v r=%+v", ok, r)
	}
	for _, bad := range []string{"", "X=1\nY=2\nWIDTH=3", "X=1\nY=2\nWIDTH=0\nHEIGHT=5"} {
		if _, ok := parseXdotoolGeometryShell(bad); ok {
			t.Errorf("expected reject for %q", bad)
		}
	}
}

// xrandr --listmonitors → per-monitor geometry.
func TestParseXrandrMonitors(t *testing.T) {
	out := "Monitors: 2\n" +
		" 0: +*eDP-1 1920/344x1080/193+0+0  eDP-1\n" +
		" 1: +HDMI-1 2560/598x1440/336+1920+0  HDMI-1\n"
	mons := parseXrandrMonitors(out)
	if len(mons) != 2 {
		t.Fatalf("expected 2 monitors, got %d: %+v", len(mons), mons)
	}
	if mons[0] != (Rect{X: 0, Y: 0, W: 1920, H: 1080}) {
		t.Errorf("monitor 0: %+v", mons[0])
	}
	if mons[1] != (Rect{X: 1920, Y: 0, W: 2560, H: 1440}) {
		t.Errorf("monitor 1: %+v", mons[1])
	}
}

func TestParseXrandrMonitorGeom(t *testing.T) {
	r, ok := parseXrandrMonitorGeom("1920/344x1080/193+100+50")
	if !ok || r != (Rect{X: 100, Y: 50, W: 1920, H: 1080}) {
		t.Errorf("ok=%v r=%+v", ok, r)
	}
	for _, bad := range []string{"", "1920x1080", "1920/344x1080/193", "a/bxc/d+0+0"} {
		if _, ok := parseXrandrMonitorGeom(bad); ok {
			t.Errorf("expected reject for %q", bad)
		}
	}
}

func TestSanitizeTileSessionID(t *testing.T) {
	for _, ok := range []string{"4d01388a", "agt_58f152", "a.b-c_d:e", "ABC123"} {
		if got := sanitizeTileSessionID(ok); got != ok {
			t.Errorf("sanitize(%q) = %q, want unchanged", ok, got)
		}
	}
	for _, bad := range []string{"", "a'b", "a b", "a;b", "a$b", "a\"b", "a`b", "a\nb", "a}b"} {
		if got := sanitizeTileSessionID(bad); got != "" {
			t.Errorf("sanitize(%q) = %q, want \"\"", bad, got)
		}
	}
}

// The enum script embeds the candidate patterns and the P/Invoke + monitor
// enumeration scaffolding.
func TestBuildWSLEnumScript(t *testing.T) {
	s := buildWSLEnumScript([]string{"sess1", "sess2"})
	if !strings.Contains(s, "'tclaude:sess1'") || !strings.Contains(s, "'tclaude:sess2'") {
		t.Errorf("missing patterns: %s", s)
	}
	if !strings.Contains(s, "AllScreens") || !strings.Contains(s, "GetWindowRect") || !strings.Contains(s, "EnumWindows") {
		t.Errorf("missing enumeration scaffolding: %s", s)
	}
	if !strings.Contains(s, `Write-Output "MON`) || !strings.Contains(s, `Write-Output "WIN`) {
		t.Errorf("missing MON/WIN output: %s", s)
	}
}

// parseWSLEnum reads MON + WIN lines, sorts windows by candidate index,
// and drops malformed lines.
func TestParseWSLEnum(t *testing.T) {
	out := "MON 0 0 1920 1040\n" +
		"MON 1920 0 2560 1400\n" +
		"WIN 1 111 1930 10 800 600\n" +
		"WIN 0 222 100 100 640 480\n" +
		"garbage line\n" +
		"WIN 2 0 0 0 0 0\n" // zero-size window dropped
	mons, wins := parseWSLEnum(out)
	if len(mons) != 2 || mons[1] != (Rect{X: 1920, Y: 0, W: 2560, H: 1400}) {
		t.Errorf("monitors: %+v", mons)
	}
	if len(wins) != 2 {
		t.Fatalf("expected 2 windows, got %d: %+v", len(wins), wins)
	}
	// Sorted by idx: idx 0 (hwnd 222) first, then idx 1 (hwnd 111).
	if wins[0].idx != 0 || wins[0].hwnd != 222 || wins[0].cur != (Rect{X: 100, Y: 100, W: 640, H: 480}) {
		t.Errorf("win[0]: %+v", wins[0])
	}
	if wins[1].idx != 1 || wins[1].hwnd != 111 {
		t.Errorf("win[1]: %+v", wins[1])
	}
}

// The apply script emits one SetWindowPos per placement with the handle +
// target rect.
func TestBuildWSLApplyScript(t *testing.T) {
	s := buildWSLApplyScript([]wslPlacement{
		{hwnd: 111, r: Rect{X: 0, Y: 0, W: 500, H: 500}},
		{hwnd: 222, r: Rect{X: 500, Y: 0, W: 500, H: 500}},
	})
	if !strings.Contains(s, "SetWindowPos") {
		t.Errorf("missing SetWindowPos: %s", s)
	}
	if !strings.Contains(s, "[IntPtr]111, [IntPtr]::Zero, 0, 0, 500, 500") {
		t.Errorf("first placement wrong: %s", s)
	}
	if !strings.Contains(s, "[IntPtr]222, [IntPtr]::Zero, 500, 0, 500, 500") {
		t.Errorf("second placement wrong: %s", s)
	}
	if n := strings.Count(s, "SetWindowPos([IntPtr]"); n != 2 {
		t.Errorf("expected 2 SetWindowPos calls, got %d", n)
	}
}
