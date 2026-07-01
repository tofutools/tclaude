//go:build linux

package session

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// Linux window tiling — two backends, mirroring the focus path's split
// (see focus_linux.go):
//
//   - Native Linux: the resolved focus tool (xdotool on X11, kdotool on
//     KDE Plasma Wayland) reads each window's current geometry and moves
//     (and, in resize mode, resizes) it. Monitors come from
//     `xrandr --listmonitors`.
//   - WSL: two PowerShell passes — one enumerates the monitors + the
//     current rect of each "tclaude:<id>" window, the other SetWindowPos's
//     them. The layout is computed in Go between the two.
//
// Both gather the focused windows onto ONE monitor (the first window's)
// and, by default, keep each window's current size — only repositioning
// it. opts.Resize switches to the screen-filling grid. Best-effort
// throughout: a missing tool, unreadable geometry, or an untrackable
// window degrades gracefully rather than erroring.

// platformTileWindows dispatches to the WSL or native-Linux backend, the
// same fork TryFocusAttachedSessionWithID makes.
func platformTileWindows(specs []TileSpec, opts TileOptions) {
	if isWSLFn() {
		tileWSLWindows(specs, opts)
		return
	}
	tileNativeLinuxWindows(specs, opts)
}

// =============================================================================
// Native Linux (xdotool / kdotool)
// =============================================================================

// tileNativeLinuxWindows arranges the focused windows on a native X11 /
// KDE-Wayland session. It resolves each spec to a window id, reads its
// current geometry, and — when at least two windows exist — gathers them
// onto the first window's monitor and lays them out (keeping sizes by
// default; grid-resizing when opts.Resize). If window sizes can't be read
// (e.g. a focus tool without getwindowgeometry), it falls back to the
// screen-filling grid so tiling still happens.
func tileNativeLinuxWindows(specs []TileSpec, opts TileOptions) {
	tool := resolveLinuxFocusTool()
	if tool == "" {
		slog.Debug("tiling: no focus tool (xdotool / kdotool); leaving windows as-is", "module", "tile")
		return
	}
	var ids []string
	for _, spec := range specs {
		tty := linuxTmuxClientTTY(spec.TmuxSession)
		if tty == "" {
			slog.Debug("tiling: no attached client tty; skipping", "tmux", spec.TmuxSession, "module", "tile")
			continue
		}
		if id := linuxWindowIDForTTY(tool, tty); id != "" {
			ids = append(ids, id)
		} else {
			slog.Debug("tiling: no window found for tty; skipping", "tmux", spec.TmuxSession, "tty", tty, "module", "tile")
		}
	}
	if len(ids) < 2 {
		slog.Debug("tiling: fewer than two resolvable windows; leaving as-is", "resolved", len(ids), "module", "tile")
		return
	}

	// Read current geometry for each window (best-effort).
	sizes := make([]Size, len(ids))
	var refCX, refCY int
	haveRef, allGeom := false, true
	for i, id := range ids {
		g, ok := linuxWindowGeometry(tool, id)
		if !ok {
			allGeom = false
			continue
		}
		sizes[i] = Size{W: g.W, H: g.H}
		if !haveRef {
			refCX, refCY = g.center()
			haveRef = true
		}
	}

	// Reference monitor = the first window's monitor; fall back to the
	// whole screen when monitors or geometry are unavailable.
	var area Rect
	found := false
	if haveRef {
		area, found = pickMonitor(refCX, refCY, linuxMonitors())
	}
	if !found {
		if a, ok := linuxScreenArea(); ok {
			area, found = a, true
		}
	}
	if !found {
		slog.Debug("tiling: could not resolve a target area; leaving windows as-is", "module", "tile")
		return
	}

	// Layout. No-resize needs every window's size; if any was unreadable,
	// fall back to the resize grid (which doesn't need sizes) so tiling
	// still happens.
	resize := opts.Resize
	var rects []Rect
	switch {
	case resize:
		rects = tileRects(len(ids), opts.Layout, area, opts.Gap, opts.Margin)
	case allGeom:
		rects = arrangeRects(sizes, opts.Layout, area, opts.Gap, opts.Margin)
	default:
		slog.Debug("tiling: window sizes unavailable; falling back to resize grid", "module", "tile")
		rects = tileRects(len(ids), opts.Layout, area, opts.Gap, opts.Margin)
		resize = true
	}

	for i, id := range ids {
		r := clampTopLeft(rects[i], area)
		placeLinuxWindow(tool, id, r, resize)
	}
}

// linuxTmuxClientTTY returns the tty of a client attached to the tmux
// session, or "" when none is attached. Same query focusLinuxTmuxSession
// makes.
func linuxTmuxClientTTY(tmuxSession string) string {
	out, err := clcommon.TmuxCommand("list-clients", "-t", tmuxSession, "-F", "#{client_tty}").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return ""
	}
	return lines[0]
}

// linuxWindowIDForTTY resolves the window id owning a tty by walking the
// processes on that tty up the tree via the focus tool — the read half of
// focusLinuxWindowByTTY, returning the id instead of activating it.
func linuxWindowIDForTTY(tool, tty string) string {
	out, err := exec.Command("lsof", "-t", tty).Output()
	if err != nil {
		return ""
	}
	for pid := range strings.FieldsSeq(string(out)) {
		if id := findLinuxWindowForPID(tool, pid); id != "" {
			return id
		}
	}
	return ""
}

// linuxWindowGeometry reads a window's current absolute position + size
// via `<tool> getwindowgeometry --shell <id>`. Returns ok=false when the
// tool can't report geometry (e.g. a kdotool build without the command).
func linuxWindowGeometry(tool, id string) (Rect, bool) {
	out, err := exec.Command(tool, "getwindowgeometry", "--shell", id).Output()
	if err != nil {
		return Rect{}, false
	}
	return parseXdotoolGeometryShell(string(out))
}

// parseXdotoolGeometryShell parses `getwindowgeometry --shell` output —
// KEY=value lines including X, Y, WIDTH, HEIGHT — into a Rect. Pure so
// it's unit-testable.
func parseXdotoolGeometryShell(out string) (Rect, bool) {
	var x, y, w, h int
	var haveX, haveY, haveW, haveH bool
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		switch k {
		case "X":
			x, haveX = n, true
		case "Y":
			y, haveY = n, true
		case "WIDTH":
			w, haveW = n, true
		case "HEIGHT":
			h, haveH = n, true
		}
	}
	if !haveX || !haveY || !haveW || !haveH || w <= 0 || h <= 0 {
		return Rect{}, false
	}
	return Rect{X: x, Y: y, W: w, H: h}, true
}

// placeLinuxWindow moves a window (and resizes it when resize is true) via
// the focus tool. Sizing first, then moving: some window managers clamp a
// move against the window's current size, so setting the target size first
// makes the move land where asked. In no-resize mode only the move runs,
// so the window keeps its dimensions. Best-effort — a failing sub-command
// is logged at debug, never fatal.
func placeLinuxWindow(tool, id string, r Rect, resize bool) {
	if resize {
		if err := exec.Command(tool, "windowsize", id, strconv.Itoa(r.W), strconv.Itoa(r.H)).Run(); err != nil {
			slog.Debug("tiling: windowsize failed", "tool", tool, "id", id, "err", err, "module", "tile")
		}
	}
	if err := exec.Command(tool, "windowmove", id, strconv.Itoa(r.X), strconv.Itoa(r.Y)).Run(); err != nil {
		slog.Debug("tiling: windowmove failed", "tool", tool, "id", id, "err", err, "module", "tile")
	}
}

// linuxMonitors enumerates the connected monitors' geometry via
// `xrandr --listmonitors` (which reports each monitor's WxH+X+Y even under
// XWayland). Returns nil when xrandr is unavailable — the caller then
// falls back to the whole-screen area.
func linuxMonitors() []Rect {
	out, err := exec.Command("xrandr", "--listmonitors").Output()
	if err != nil {
		return nil
	}
	return parseXrandrMonitors(string(out))
}

// parseXrandrMonitors parses `xrandr --listmonitors` output — one monitor
// per non-header line, geometry token like "1920/344x1080/193+0+0". Pure
// so it's unit-testable.
func parseXrandrMonitors(out string) []Rect {
	var mons []Rect
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Monitors:") {
			continue // header
		}
		// Find the geometry field: the one containing both 'x' and '+'.
		for field := range strings.FieldsSeq(line) {
			if strings.ContainsRune(field, 'x') && strings.ContainsRune(field, '+') {
				if r, ok := parseXrandrMonitorGeom(field); ok {
					mons = append(mons, r)
				}
				break
			}
		}
	}
	return mons
}

// parseXrandrMonitorGeom parses one xrandr monitor geometry token —
// "<W>/<mmW>x<H>/<mmH>+<X>+<Y>" — into a Rect. Pure/unit-testable.
func parseXrandrMonitorGeom(tok string) (Rect, bool) {
	left, right, ok := strings.Cut(tok, "x")
	if !ok {
		return Rect{}, false
	}
	// left = "W/mmW"; right = "H/mmH+X+Y".
	wStr, _, _ := strings.Cut(left, "/")
	hPart, xy, ok := strings.Cut(right, "+")
	if !ok {
		return Rect{}, false
	}
	hStr, _, _ := strings.Cut(hPart, "/")
	xStr, yStr, ok := strings.Cut(xy, "+")
	if !ok {
		return Rect{}, false
	}
	w, e1 := strconv.Atoi(strings.TrimSpace(wStr))
	h, e2 := strconv.Atoi(strings.TrimSpace(hStr))
	x, e3 := strconv.Atoi(strings.TrimSpace(xStr))
	y, e4 := strconv.Atoi(strings.TrimSpace(yStr))
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || w <= 0 || h <= 0 {
		return Rect{}, false
	}
	return Rect{X: x, Y: y, W: w, H: h}, true
}

// linuxScreenArea reads the whole X screen's pixel size (the fallback area
// when per-monitor info is unavailable). It tries `xdotool
// getdisplaygeometry` first (works under XWayland too) then `xrandr`'s
// "current WxH". Returns ok=false when neither is available.
func linuxScreenArea() (Rect, bool) {
	if out, err := exec.Command("xdotool", "getdisplaygeometry").Output(); err == nil {
		if r, ok := parseXdotoolGeometry(string(out)); ok {
			return r, true
		}
	}
	if out, err := exec.Command("xrandr").Output(); err == nil {
		if r, ok := parseXrandrCurrent(string(out)); ok {
			return r, true
		}
	}
	return Rect{}, false
}

// parseXdotoolGeometry parses `xdotool getdisplaygeometry` output ("W H")
// into a Rect anchored at the origin. Pure so it's unit-testable.
func parseXdotoolGeometry(out string) (Rect, bool) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 {
		return Rect{}, false
	}
	w, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return Rect{}, false
	}
	return Rect{X: 0, Y: 0, W: w, H: h}, true
}

// parseXrandrCurrent extracts the "current W x H" screen size from
// `xrandr` output (the "Screen 0: … current 1920 x 1080, …" header
// line). Pure so it's unit-testable.
func parseXrandrCurrent(out string) (Rect, bool) {
	_, rest, found := strings.Cut(out, "current ")
	if !found {
		return Rect{}, false
	}
	// rest starts like "1920 x 1080, maximum ..."
	if comma := strings.IndexByte(rest, ','); comma >= 0 {
		rest = rest[:comma]
	}
	parts := strings.Split(rest, "x")
	if len(parts) != 2 {
		return Rect{}, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return Rect{}, false
	}
	return Rect{X: 0, Y: 0, W: w, H: h}, true
}

// =============================================================================
// WSL tiling — PowerShell (enumerate → SetWindowPos)
// =============================================================================

// wslWin is a matched Windows Terminal window: its candidate index (spec
// order), OS window handle, and current on-screen rect.
type wslWin struct {
	idx  int
	hwnd int64
	cur  Rect
}

// tileWSLWindows arranges the focused Windows Terminal windows with two
// PowerShell passes: the first enumerates the monitors' work-areas and the
// current rect of each "tclaude:<id>" window; Go then picks the first
// window's monitor and computes the layout; the second SetWindowPos's each
// window. HWNDs are OS-wide handles, so they carry across the two passes.
func tileWSLWindows(specs []TileSpec, opts TileOptions) {
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("tiling: PowerShell not found; leaving windows as-is", "module", "tile")
		return
	}
	// Candidate patterns in spec order; specs with an empty/unsafe id drop.
	var ids []string
	for _, spec := range specs {
		if id := sanitizeTileSessionID(spec.SessionID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) < 2 {
		slog.Debug("tiling: fewer than two tileable WSL targets (missing/invalid session ids)", "module", "tile")
		return
	}

	enumOut, err := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", buildWSLEnumScript(ids)).Output()
	if err != nil {
		slog.Debug("tiling: WSL enumerate pass failed", "err", err, "module", "tile")
		return
	}
	mons, wins := parseWSLEnum(string(enumOut))
	if len(wins) < 2 {
		slog.Debug("tiling: fewer than two matching WSL windows on screen", "matched", len(wins), "module", "tile")
		return
	}

	cx, cy := wins[0].cur.center()
	area, ok := pickMonitor(cx, cy, mons)
	if !ok {
		slog.Debug("tiling: no monitor info from WSL; leaving windows as-is", "module", "tile")
		return
	}

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

	placements := make([]wslPlacement, len(wins))
	for i, w := range wins {
		placements[i] = wslPlacement{hwnd: w.hwnd, r: clampTopLeft(rects[i], area)}
	}
	if err := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", buildWSLApplyScript(placements)).Run(); err != nil {
		slog.Debug("tiling: WSL SetWindowPos pass failed", "err", err, "module", "tile")
	}
}

// wslPlacement pairs a window handle with its target rectangle.
type wslPlacement struct {
	hwnd int64
	r    Rect
}

// sanitizeTileSessionID returns s when it is a non-empty string of the
// safe title charset [A-Za-z0-9._:-], else "". It gates the value
// interpolated into the generated PowerShell so a session id can never
// inject script.
func sanitizeTileSessionID(s string) string {
	if s == "" {
		return ""
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == ':':
		default:
			return ""
		}
	}
	return s
}

// buildWSLEnumScript generates the PowerShell that prints one "MON x y w h"
// line per monitor work-area and one "WIN idx hwnd x y w h" line per
// matched "tclaude:<id>" window (first visible match per pattern, in
// candidate order). Pure so it's unit-testable.
func buildWSLEnumScript(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "'tclaude:" + id + "'" // id is sanitize-gated; safe in a single-quoted literal
	}
	return wslEnumPreamble +
		"$patterns = @(" + strings.Join(quoted, ", ") + ")\n" +
		wslEnumBody
}

// parseWSLEnum parses buildWSLEnumScript output into the monitor work-areas
// and the matched windows (sorted by candidate index). Pure/unit-testable.
func parseWSLEnum(out string) ([]Rect, []wslWin) {
	var mons []Rect
	var wins []wslWin
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		switch {
		case len(f) == 5 && f[0] == "MON":
			if r, ok := parseIntRect(f[1], f[2], f[3], f[4]); ok {
				mons = append(mons, r)
			}
		case len(f) == 7 && f[0] == "WIN":
			idx, e1 := strconv.Atoi(f[1])
			hwnd, e2 := strconv.ParseInt(f[2], 10, 64)
			r, ok := parseIntRect(f[3], f[4], f[5], f[6])
			if e1 == nil && e2 == nil && ok {
				wins = append(wins, wslWin{idx: idx, hwnd: hwnd, cur: r})
			}
		}
	}
	// Sort by candidate index so tiling order follows spec order.
	for i := 1; i < len(wins); i++ {
		for j := i; j > 0 && wins[j-1].idx > wins[j].idx; j-- {
			wins[j-1], wins[j] = wins[j], wins[j-1]
		}
	}
	return mons, wins
}

// parseIntRect parses four decimal strings into a Rect (X, Y, W, H),
// requiring positive W/H.
func parseIntRect(xs, ys, ws, hs string) (Rect, bool) {
	x, e1 := strconv.Atoi(xs)
	y, e2 := strconv.Atoi(ys)
	w, e3 := strconv.Atoi(ws)
	h, e4 := strconv.Atoi(hs)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || w <= 0 || h <= 0 {
		return Rect{}, false
	}
	return Rect{X: x, Y: y, W: w, H: h}, true
}

// buildWSLApplyScript generates the PowerShell that SetWindowPos's each
// placement (move + resize, no z-order / activation change). Pure so it's
// unit-testable.
func buildWSLApplyScript(placements []wslPlacement) string {
	var b strings.Builder
	b.WriteString(wslApplyPreamble)
	b.WriteString("$flags = [uint32](0x0004 -bor 0x0010)\n")
	for _, p := range placements {
		fmt.Fprintf(&b, "[TclaudeApply]::SetWindowPos([IntPtr]%d, [IntPtr]::Zero, %d, %d, %d, %d, $flags) | Out-Null\n",
			p.hwnd, p.r.X, p.r.Y, p.r.W, p.r.H)
	}
	return b.String()
}

// wslEnumPreamble defines the P/Invoke helper and enumerates monitors; the
// per-pattern matching loop (wslEnumBody) follows the $patterns array the
// generator inserts between them.
const wslEnumPreamble = `Add-Type @"
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;
public class TclaudeEnum {
    public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);
    [DllImport("user32.dll")] public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr hWnd, StringBuilder lpString, int nMaxCount);
    [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr hWnd);
    [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
    [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr hWnd, out RECT r);
    public static List<Tuple<IntPtr,string,RECT>> Visible() {
        var res = new List<Tuple<IntPtr,string,RECT>>();
        EnumWindows((h,l) => {
            if (IsWindowVisible(h)) {
                var sb = new StringBuilder(512);
                GetWindowText(h, sb, 512);
                var t = sb.ToString();
                if (!string.IsNullOrEmpty(t)) { RECT r; GetWindowRect(h, out r); res.Add(Tuple.Create(h, t, r)); }
            }
            return true;
        }, IntPtr.Zero);
        return res;
    }
}
"@
Add-Type -AssemblyName System.Windows.Forms
foreach ($s in [System.Windows.Forms.Screen]::AllScreens) {
    $wa = $s.WorkingArea
    Write-Output "MON $($wa.X) $($wa.Y) $($wa.Width) $($wa.Height)"
}
`

// wslEnumBody matches each pattern to the first visible window whose title
// contains it and prints its handle + rect. It follows the $patterns array
// the generator inserts after wslEnumPreamble.
const wslEnumBody = `$wins = [TclaudeEnum]::Visible()
for ($i = 0; $i -lt $patterns.Count; $i++) {
    foreach ($w in $wins) {
        if ($w.Item2 -like "*$($patterns[$i])*") {
            $r = $w.Item3
            Write-Output "WIN $i $($w.Item1.ToInt64()) $($r.Left) $($r.Top) $($r.Right - $r.Left) $($r.Bottom - $r.Top)"
            break
        }
    }
}
`

// wslApplyPreamble defines the SetWindowPos P/Invoke used by the apply loop.
const wslApplyPreamble = `Add-Type @"
using System;
using System.Runtime.InteropServices;
public class TclaudeApply {
    [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr hWnd, IntPtr hWndInsertAfter, int X, int Y, int cx, int cy, uint uFlags);
}
"@
`
