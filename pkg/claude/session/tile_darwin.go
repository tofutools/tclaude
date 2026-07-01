//go:build darwin

package session

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// macOS window tiling.
//
// The focused windows are Terminal.app / iTerm2 windows we identify by the
// attached client's tty (the same tty the focus path matches — see
// focus_darwin.go). Both apps expose a scriptable window `bounds` property
// ({left, top, right, bottom}, top-left origin), which we both READ (each
// window's current size, so the default no-resize layout can reposition
// without stretching) and SET.
//
// All focused windows are gathered onto ONE monitor — the monitor the
// first window is on — so a multi-monitor setup doesn't scatter them or
// straddle the gap. Monitors are enumerated via NSScreen; if that fails
// (no window-server access), we fall back to the whole-desktop bounds so
// tiling still degrades to something sane rather than erroring.
//
// Only Terminal.app and iTerm2 are driven — they script cleanly without
// the Accessibility permission a generic System-Events windowmover would
// need. Other terminals (Alacritty, kitty, Warp, …) have no bounds API we
// can drive by tty, so their windows are left in place (best-effort, logged).

// macWin is one spec resolved to a scriptable window with its CURRENT
// on-screen bounds (read before we move it).
type macWin struct {
	tty string
	app string // "Terminal" | "iTerm2"
	cur Rect
}

// platformTileWindows arranges the focused Terminal/iTerm windows. It
// resolves each spec to a scriptable window FIRST (reading its current
// bounds) and only tiles when at least two exist — so a windowless agent
// (e.g. focus.raise_only left it so) or an unscriptable terminal drops out
// and never leaves a hole or shrinks the rest, and a lone window is left
// alone rather than maximised. Everything is gathered onto the first
// window's monitor. By default windows keep their current size and are
// only repositioned; opts.Resize switches to the screen-filling grid.
// Best-effort throughout: a failed read/enumerate/set logs at debug.
func platformTileWindows(specs []TileSpec, opts TileOptions) {
	monitors := macMonitors() // may be nil → fall back to desktop bounds below
	wins := make([]macWin, 0, len(specs))
	for _, spec := range specs {
		tty := getTmuxClientTTY(spec.TmuxSession)
		if tty == "" {
			slog.Debug("tiling: no attached client tty; skipping", "tmux", spec.TmuxSession, "module", "tile")
			continue
		}
		termApp := terminalFromTTY(tty)
		if !macTileScriptable(termApp) {
			slog.Debug("tiling: terminal has no scriptable bounds; skipping",
				"tmux", spec.TmuxSession, "app", termApp, "module", "tile")
			continue
		}
		cur, ok := macReadWindowBounds(termApp, tty)
		if !ok {
			slog.Debug("tiling: could not read window bounds; skipping",
				"tmux", spec.TmuxSession, "app", termApp, "module", "tile")
			continue
		}
		wins = append(wins, macWin{tty: tty, app: termApp, cur: cur})
	}
	if len(wins) < 2 {
		slog.Debug("tiling: fewer than two scriptable windows; leaving as-is",
			"resolved", len(wins), "module", "tile")
		return
	}

	// Reference monitor = the monitor hosting the first window. When
	// NSScreen reported nothing, fall back to the whole-desktop bounds
	// (still no-resize, so no maximising — just not per-monitor).
	cx, cy := wins[0].cur.center()
	area, ok := pickMonitor(cx, cy, monitors)
	if !ok {
		area, ok = macScreenArea()
		if !ok {
			slog.Debug("tiling: no monitor info and no desktop bounds; leaving as-is", "module", "tile")
			return
		}
	}

	// Compute each window's target: resize → screen-filling grid cells;
	// default → keep current size, just reposition.
	var rects []Rect
	if opts.Resize {
		rects = tileRects(len(wins), opts.Layout, area, opts.Gap, opts.Margin)
	} else {
		sizes := make([]Size, len(wins))
		for i := range wins {
			sizes[i] = Size{W: wins[i].cur.W, H: wins[i].cur.H}
		}
		rects = arrangeRects(sizes, opts.Layout, area, opts.Gap, opts.Margin)
	}

	for i, wn := range wins {
		r := clampTopLeft(rects[i], area)
		script := buildMacTileScript(wn.app, wn.tty, r)
		if script == "" {
			continue // pre-filtered by macTileScriptable; defensive
		}
		if err := exec.Command("osascript", "-e", script).Run(); err != nil {
			slog.Debug("tiling: osascript set-bounds failed", "tty", wn.tty, "err", err, "module", "tile")
		}
	}
}

// macMonitors enumerates the connected screens' work-areas (visibleFrame,
// so the menu bar / Dock are excluded) via NSScreen, in the SAME
// top-left, main-screen-origin coordinate space Terminal/iTerm `bounds`
// use. Returns nil when the query fails (e.g. a process with no
// window-server connection) — the caller then falls back to the
// whole-desktop bounds.
func macMonitors() []Rect {
	out, err := exec.Command("osascript", "-e", macScreensScript).Output()
	if err != nil {
		slog.Debug("tiling: NSScreen enumeration failed; will fall back to desktop bounds",
			"err", err, "module", "tile")
		return nil
	}
	return parseMacMonitors(string(out))
}

// macScreensScript emits one "originX topY width height" line per screen.
// NSScreen frames use a bottom-left origin with y measured up from the
// MAIN screen's bottom; window `bounds` use a top-left origin with y down
// from the main screen's top. The conversion for a screen whose
// visibleFrame is (ox, oy, w, h) is topY = mainFullHeight - (oy + h);
// x is identical in both systems.
const macScreensScript = `use framework "AppKit"
use scripting additions
set mainH to item 2 of (item 2 of ((current application's NSScreen's mainScreen()'s frame()) as list))
set out to ""
repeat with s in (current application's NSScreen's screens())
	set vf to ((s's visibleFrame()) as list)
	set ox to (item 1 of (item 1 of vf))
	set oy to (item 2 of (item 1 of vf))
	set w to (item 1 of (item 2 of vf))
	set h to (item 2 of (item 2 of vf))
	set out to out & (ox as integer) & " " & ((mainH - (oy + h)) as integer) & " " & (w as integer) & " " & (h as integer) & linefeed
end repeat
return out`

// parseMacMonitors parses macScreensScript output — one "x y w h" line per
// screen — into work-area Rects. Malformed / non-positive lines are
// skipped. Pure so the parsing is unit-testable without a GUI.
func parseMacMonitors(out string) []Rect {
	var mons []Rect
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 4 {
			continue
		}
		n := make([]int, 4)
		ok := true
		for i, s := range f {
			v, err := strconv.Atoi(s)
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if !ok || n[2] <= 0 || n[3] <= 0 {
			continue
		}
		mons = append(mons, Rect{X: n[0], Y: n[1], W: n[2], H: n[3]})
	}
	return mons
}

// macReadWindowBounds reads the current on-screen bounds of the window
// owning `tty` in `termApp`, returning it as a Rect. Returns ok=false when
// the app can't be scripted or the tty isn't found.
func macReadWindowBounds(termApp, tty string) (Rect, bool) {
	script := buildMacReadBoundsScript(termApp, tty)
	if script == "" {
		return Rect{}, false
	}
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return Rect{}, false
	}
	// `bounds of w` yields the same "L, T, R, B" shape as the desktop
	// bounds, so the same parser (and its positive-size check) applies.
	return parseMacDesktopBounds(string(out))
}

// buildMacReadBoundsScript returns the AppleScript that reads the bounds
// of the window owning `tty` in `termApp` as "L, T, R, B". Mirrors
// buildMacTileScript's tty-matching structure. Pure/unit-testable.
func buildMacReadBoundsScript(termApp, tty string) string {
	ttyLit := strconv.Quote(tty)
	const emit = `set b to bounds of w
				return ((item 1 of b) as integer) & ", " & ((item 2 of b) as integer) & ", " & ((item 3 of b) as integer) & ", " & ((item 4 of b) as integer)`
	switch termApp {
	case "Terminal":
		return fmt.Sprintf(`tell application "Terminal"
	repeat with w in windows
		repeat with t in tabs of w
			if tty of t is %s then
				%s
			end if
		end repeat
	end repeat
end tell`, ttyLit, emit)
	case "iTerm2":
		return fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %s then
					%s
				end if
			end repeat
		end repeat
	end repeat
end tell`, ttyLit, emit)
	default:
		return ""
	}
}

// macTileScriptable reports whether buildMacTileScript can drive termApp
// — the two apps that script a window's bounds by tty without the
// Accessibility permission. Keeps the resolve-phase filter and the
// script switch in agreement.
func macTileScriptable(termApp string) bool {
	return termApp == "Terminal" || termApp == "iTerm2"
}

// macScreenArea reads the primary desktop's pixel bounds via Finder,
// returning a top-left-origin Rect. Finder reports the FULL screen (it
// does not exclude the menu bar / Dock); the config focus.tile.margin is
// the intended way to inset for those. Returns ok=false when the query
// fails (no GUI session, osascript missing).
func macScreenArea() (Rect, bool) {
	out, err := exec.Command("osascript", "-e",
		`tell application "Finder" to get bounds of window of desktop`).Output()
	if err != nil {
		return Rect{}, false
	}
	return parseMacDesktopBounds(string(out))
}

// parseMacDesktopBounds parses Finder's "L, T, R, B" desktop-bounds line
// into a Rect (origin at L,T; width R-L; height B-T). Pure so the parsing
// is unit-testable without a GUI. Returns ok=false on a malformed line or
// a non-positive size.
func parseMacDesktopBounds(out string) (Rect, bool) {
	parts := strings.Split(strings.TrimSpace(out), ",")
	if len(parts) != 4 {
		return Rect{}, false
	}
	n := make([]int, 4)
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return Rect{}, false
		}
		n[i] = v
	}
	left, top, right, bottom := n[0], n[1], n[2], n[3]
	w, h := right-left, bottom-top
	if w <= 0 || h <= 0 {
		return Rect{}, false
	}
	return Rect{X: left, Y: top, W: w, H: h}, true
}

// buildMacTileScript returns the AppleScript that sets the bounds of the
// window owning `tty` in `termApp` to `r`. It supports "Terminal" and
// "iTerm2" (the two apps that script by tty without Accessibility); any
// other app returns "" so the caller skips it. Pure so the generated
// script is unit-testable.
//
// AppleScript window `bounds` are {left, top, right, bottom} with a
// top-left origin — the same coordinate space Rect uses — so the mapping
// is {X, Y, X+W, Y+H}.
func buildMacTileScript(termApp, tty string, r Rect) string {
	left, top := r.X, r.Y
	right, bottom := r.X+r.W, r.Y+r.H
	// tty is a device path like /dev/ttys003 — no quotes to escape — but
	// wrap the comparison value defensively all the same.
	ttyLit := strconv.Quote(tty)
	bounds := fmt.Sprintf("{%d, %d, %d, %d}", left, top, right, bottom)

	switch termApp {
	case "Terminal":
		return fmt.Sprintf(`tell application "Terminal"
	repeat with w in windows
		repeat with t in tabs of w
			if tty of t is %s then
				set bounds of w to %s
				return
			end if
		end repeat
	end repeat
end tell`, ttyLit, bounds)
	case "iTerm2":
		return fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %s then
					set bounds of w to %s
					return
				end if
			end repeat
		end repeat
	end repeat
end tell`, ttyLit, bounds)
	default:
		return ""
	}
}
