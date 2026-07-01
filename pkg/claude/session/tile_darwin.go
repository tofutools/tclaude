//go:build darwin

package session

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
// Everything is BATCHED: one osascript per running app enumerates the tty
// + bounds of every window in a single pass (run in parallel with the
// NSScreen monitor query), and one osascript per app sets every target
// window's bounds. The obvious per-window shape — resolve the owning app
// via lsof, read its bounds, set its bounds, times N — launches 2N
// osascripts plus N lsof scans (~1-2s each on macOS) sequentially and
// made large sets take many seconds; the batched shape is a handful of
// process launches regardless of N. Enumerating both apps also answers
// "which app owns this tty" for free, so no lsof/ps process-tree walk is
// needed at all.
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

// macTileApps are the terminal apps the tiling pass can drive: the two
// that script a window's bounds by tty without the Accessibility
// permission.
var macTileApps = []string{"Terminal", "iTerm2"}

// macWin is one spec resolved to a scriptable window with its CURRENT
// on-screen bounds (read before we move it).
type macWin struct {
	tty string
	app string // "Terminal" | "iTerm2"
	cur Rect
}

// macMove pairs one window (keyed by the tty of its tab/session) with its
// target bounds, for the batched set-bounds script.
type macMove struct {
	tty string
	r   Rect
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
	// Resolve each spec's attached client tty — one fast tmux call each,
	// kept in spec order (the tiling order).
	ttys := make([]string, 0, len(specs))
	for _, spec := range specs {
		tty := getTmuxClientTTY(spec.TmuxSession)
		if tty == "" {
			slog.Debug("tiling: no attached client tty; skipping", "tmux", spec.TmuxSession, "module", "tile")
			continue
		}
		ttys = append(ttys, tty)
	}
	if len(ttys) < 2 {
		slog.Debug("tiling: fewer than two attached windows; leaving as-is",
			"resolved", len(ttys), "module", "tile")
		return
	}

	// Read the monitors and every candidate window in PARALLEL: the
	// NSScreen query plus one enumeration osascript per app.
	var (
		monitors []Rect
		mu       sync.Mutex
		byApp    = map[string]map[string]Rect{} // app → tty → current bounds
		wg       sync.WaitGroup
	)
	wg.Go(func() {
		monitors = macMonitors() // may be nil → fall back to desktop bounds below
	})
	for _, app := range macTileApps {
		wg.Go(func() {
			m := macEnumWindowBounds(app)
			if len(m) == 0 {
				return
			}
			mu.Lock()
			byApp[app] = m
			mu.Unlock()
		})
	}
	wg.Wait()

	// Match each tty to the app that owns a window for it, keeping spec
	// order. A tty owned by an unscriptable terminal appears in no
	// enumeration and drops out — never leaving a hole or shrinking the
	// rest — and a lone survivor is left alone rather than maximised.
	wins := make([]macWin, 0, len(ttys))
	for _, tty := range ttys {
		found := false
		for _, app := range macTileApps {
			if cur, ok := byApp[app][tty]; ok {
				wins = append(wins, macWin{tty: tty, app: app, cur: cur})
				found = true
				break
			}
		}
		if !found {
			slog.Debug("tiling: tty not in any scriptable terminal; skipping", "tty", tty, "module", "tile")
		}
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

	// Batch the set-bounds into ONE osascript per app, apps in parallel.
	moves := map[string][]macMove{}
	for i, wn := range wins {
		moves[wn.app] = append(moves[wn.app], macMove{tty: wn.tty, r: clampTopLeft(rects[i], area)})
	}
	var setWG sync.WaitGroup
	for app, ms := range moves {
		setWG.Go(func() {
			script := buildMacBatchTileScript(app, ms)
			if script == "" {
				return // no moves for this app; defensive
			}
			if err := exec.Command("osascript", "-e", script).Run(); err != nil {
				slog.Debug("tiling: osascript batch set-bounds failed", "app", app, "err", err, "module", "tile")
			}
		})
	}
	setWG.Wait()
}

// macEnumWindowBounds enumerates one app's windows in a single osascript,
// returning tty → current window bounds for every tab/session. Best
// effort: an app that isn't running is guarded inside the script (`is
// running` doesn't launch it) and yields no lines, and the guard answers
// false for an app that isn't installed at all — either way, no windows.
func macEnumWindowBounds(app string) map[string]Rect {
	script := buildMacEnumBoundsScript(app)
	if script == "" {
		return nil
	}
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		slog.Debug("tiling: window enumeration failed", "app", app, "err", err, "module", "tile")
		return nil
	}
	return parseMacEnumBounds(string(out))
}

// macEnumEmitLine is the fragment that appends one "tty L T R B" line to
// `out` for the tab/session variable `v` (window bounds already read into
// `b`). `out` is TEXT, so every & below is string concatenation — an
// integer LEFT operand would make & build a LIST, which osascript prints
// with extra separators and the parser rejects (the bug that once made
// every bounds read fail and tiling silently no-op; regression-tested by
// TestMacEnumEmitLine_YieldsParseableText). The try swallows a
// tab/session that vanishes mid-enumeration.
func macEnumEmitLine(v string) string {
	return `try
					set out to out & (tty of ` + v + `) & " " & ((item 1 of b) as integer) & " " & ((item 2 of b) as integer) & " " & ((item 3 of b) as integer) & " " & ((item 4 of b) as integer) & linefeed
				end try`
}

// buildMacEnumBoundsScript returns the AppleScript that emits one
// "tty L T R B" line per tab (Terminal) / session (iTerm2) of every
// window of `app`, e.g. "/dev/ttys003 100 200 900 800". The `is running`
// guard keeps the tell block from LAUNCHING an installed-but-closed app;
// it is evaluated without starting the app. Unsupported apps yield "".
// Pure/unit-testable.
func buildMacEnumBoundsScript(app string) string {
	switch app {
	case "Terminal":
		return fmt.Sprintf(`set out to ""
if application "Terminal" is running then
	tell application "Terminal"
		repeat with w in windows
			set b to bounds of w
			repeat with t in tabs of w
				%s
			end repeat
		end repeat
	end tell
end if
return out`, macEnumEmitLine("t"))
	case "iTerm2":
		return fmt.Sprintf(`set out to ""
if application "iTerm2" is running then
	tell application "iTerm2"
		repeat with w in windows
			set b to bounds of w
			repeat with t in tabs of w
				repeat with s in sessions of t
					%s
				end repeat
			end repeat
		end repeat
	end tell
end if
return out`, macEnumEmitLine("s"))
	default:
		return ""
	}
}

// parseMacEnumBounds parses buildMacEnumBoundsScript output — one
// "tty L T R B" line per tab/session — into tty → current-bounds Rect.
// Malformed and non-positive-size lines are skipped. Pure/unit-testable.
func parseMacEnumBounds(out string) map[string]Rect {
	m := map[string]Rect{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 5 {
			continue
		}
		n := make([]int, 4)
		ok := true
		for i, s := range f[1:] {
			v, err := strconv.Atoi(s)
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		w, h := n[2]-n[0], n[3]-n[1]
		if !ok || w <= 0 || h <= 0 {
			continue
		}
		m[f[0]] = Rect{X: n[0], Y: n[1], W: w, H: h}
	}
	return m
}

// buildMacBatchTileScript returns ONE AppleScript that walks `app`'s
// windows a single pass and sets the bounds of every window whose
// tab/session tty matches a move — one osascript launch per app instead
// of one per window. No early return: several moves may match tabs of
// several windows. Unsupported apps and empty move sets yield "".
// Pure/unit-testable.
//
// AppleScript window `bounds` are {left, top, right, bottom} with a
// top-left origin — the same coordinate space Rect uses — so the mapping
// is {X, Y, X+W, Y+H}.
func buildMacBatchTileScript(app string, moves []macMove) string {
	if len(moves) == 0 {
		return ""
	}
	// The tty → bounds dispatch, shared by both app shapes. tty is a
	// device path like /dev/ttys003 — no quotes to escape — but quote the
	// comparison value defensively all the same.
	var conds strings.Builder
	for i, m := range moves {
		kw := "if"
		if i > 0 {
			kw = "\t\t\t\telse if"
		}
		fmt.Fprintf(&conds, "%s tt is %s then\n\t\t\t\t\tset bounds of w to {%d, %d, %d, %d}\n",
			kw, strconv.Quote(m.tty), m.r.X, m.r.Y, m.r.X+m.r.W, m.r.Y+m.r.H)
	}
	conds.WriteString("\t\t\t\tend if")

	switch app {
	case "Terminal":
		return fmt.Sprintf(`tell application "Terminal"
	repeat with w in windows
		repeat with t in tabs of w
			try
				set tt to tty of t
				%s
			end try
		end repeat
	end repeat
end tell`, conds.String())
	case "iTerm2":
		return fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				try
					set tt to tty of s
					%s
				end try
			end repeat
		end repeat
	end repeat
end tell`, conds.String())
	default:
		return ""
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
// PRIMARY screen's bottom; window `bounds` use a top-left origin with y
// down from the primary screen's top. So the conversion for a screen whose
// visibleFrame is (ox, oy, w, h) is topY = primaryFullHeight - (oy + h);
// x is identical in both systems.
//
// The reference height MUST be the PRIMARY screen's — the menu-bar screen,
// the one whose AppKit frame origin is (0,0) — NOT NSScreen mainScreen(),
// which is the screen with keyboard focus and, for a background osascript,
// is whatever the user last touched (often a non-primary monitor). Using
// the focused screen's height would shift every monitor's Y by the height
// difference on a mixed-height multi-monitor setup, landing tiled windows
// off-screen. We find the primary by its (0,0) origin and fall back to
// screens()[0] if none reports it.
//
// rectOf() normalises whatever `frame()/visibleFrame() as list` yields —
// a flat {x,y,w,h} or a nested {{x,y},{w,h}}, depending on the OS /
// AppleScriptObjC version — to a flat 4-item list, so the parsing is
// robust to both. On any failure the whole script errors and macMonitors
// falls back to the whole-desktop bounds.
const macScreensScript = `use framework "AppKit"
use scripting additions
on rectOf(r)
	set f to (r as list)
	if (class of (item 1 of f)) is list then
		return {item 1 of (item 1 of f), item 2 of (item 1 of f), item 1 of (item 2 of f), item 2 of (item 2 of f)}
	else
		return {item 1 of f, item 2 of f, item 3 of f, item 4 of f}
	end if
end rectOf
set screenList to (current application's NSScreen's screens()) as list
set primaryH to 0
repeat with s in screenList
	set fr to rectOf((contents of s)'s frame())
	if ((item 1 of fr) = 0 and (item 2 of fr) = 0) then set primaryH to (item 4 of fr)
end repeat
if primaryH = 0 then set primaryH to (item 4 of rectOf((contents of (item 1 of screenList))'s frame()))
set out to ""
repeat with s in screenList
	set vf to rectOf((contents of s)'s visibleFrame())
	set ox to (item 1 of vf)
	set oy to (item 2 of vf)
	set w to (item 3 of vf)
	set h to (item 4 of vf)
	set out to out & (ox as integer) & " " & ((primaryH - (oy + h)) as integer) & " " & (w as integer) & " " & (h as integer) & linefeed
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
