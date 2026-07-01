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
// The focused windows are Terminal.app / iTerm2 windows we already
// identify by the attached client's tty (the same tty the focus path
// matches — see focus_darwin.go). Both apps expose a scriptable window
// `bounds` property ({left, top, right, bottom}, top-left origin), so we
// read the desktop size once, compute the grid in Go, and set each
// window's bounds with a small AppleScript.
//
// Only Terminal.app and iTerm2 are driven — they script cleanly without
// the Accessibility permission a generic System-Events windowmover would
// need. Other terminals (Alacritty, kitty, Warp, …) have no bounds API we
// can drive by tty, so their windows are left in place (best-effort, logged).

// macTileTarget is one spec resolved to a scriptable window.
type macTileTarget struct {
	tty string
	app string // "Terminal" | "iTerm2"
}

// platformTileWindows arranges the focused Terminal/iTerm windows into
// the configured layout. It resolves each spec to a scriptable window
// FIRST and sizes the grid to the windows that actually exist — so an
// agent with no attached client (e.g. focus.raise_only left it
// windowless, or the terminal isn't scriptable) drops out and doesn't
// leave a hole or shrink the rest. If fewer than two real windows remain,
// nothing is tiled (a lone window is left alone, not maximised).
// Best-effort throughout: a missing screen size or a failed set-bounds
// logs at debug rather than erroring.
func platformTileWindows(specs []TileSpec, opts TileOptions) {
	area, ok := macScreenArea()
	if !ok {
		slog.Debug("tiling: could not read desktop bounds; leaving windows as-is", "module", "tile")
		return
	}
	targets := make([]macTileTarget, 0, len(specs))
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
		targets = append(targets, macTileTarget{tty: tty, app: termApp})
	}
	if len(targets) < 2 {
		slog.Debug("tiling: fewer than two scriptable windows; leaving as-is",
			"resolved", len(targets), "module", "tile")
		return
	}
	rects := tileRects(len(targets), opts.Layout, area, opts.Gap, opts.Margin)
	for i, tg := range targets {
		script := buildMacTileScript(tg.app, tg.tty, rects[i])
		if script == "" {
			continue // pre-filtered by macTileScriptable; defensive
		}
		if err := exec.Command("osascript", "-e", script).Run(); err != nil {
			slog.Debug("tiling: osascript set-bounds failed", "tty", tg.tty, "err", err, "module", "tile")
		}
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
