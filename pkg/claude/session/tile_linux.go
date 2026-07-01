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
//     KDE Plasma Wayland) both `windowmove` and `windowsize` a window we
//     find by the attached client's tty. The screen size comes from
//     `xdotool getdisplaygeometry` (which reports the X screen even under
//     XWayland) with an `xrandr` fallback.
//   - WSL: a single PowerShell script reads the primary monitor's work
//     area and SetWindowPos's each Windows Terminal window we find by the
//     "tclaude:<id>" title the focus path already keys on. One script for
//     the whole set, because PowerShell start-up is the dominant cost.
//
// Best-effort throughout: a missing tool, an unreadable screen size, or
// an untrackable window each skip with a debug log rather than erroring.

// platformTileWindows dispatches to the WSL or native-Linux backend, the
// same fork TryFocusAttachedSessionWithID makes.
func platformTileWindows(specs []TileSpec, opts TileOptions) {
	if isWSLFn() {
		tileWSLWindows(specs, opts)
		return
	}
	tileNativeLinuxWindows(specs, opts)
}

// tileNativeLinuxWindows arranges the focused windows on a native X11 /
// KDE-Wayland session via xdotool/kdotool.
func tileNativeLinuxWindows(specs []TileSpec, opts TileOptions) {
	tool := resolveLinuxFocusTool()
	if tool == "" {
		slog.Debug("tiling: no focus tool (xdotool / kdotool); leaving windows as-is", "module", "tile")
		return
	}
	area, ok := linuxScreenArea()
	if !ok {
		slog.Debug("tiling: could not read screen geometry; leaving windows as-is", "module", "tile")
		return
	}
	rects := tileRects(len(specs), opts.Layout, area, opts.Gap, opts.Margin)
	for i, spec := range specs {
		tty := linuxTmuxClientTTY(spec.TmuxSession)
		if tty == "" {
			slog.Debug("tiling: no attached client tty; skipping", "tmux", spec.TmuxSession, "module", "tile")
			continue
		}
		windowID := linuxWindowIDForTTY(tool, tty)
		if windowID == "" {
			slog.Debug("tiling: no window found for tty; skipping", "tmux", spec.TmuxSession, "tty", tty, "module", "tile")
			continue
		}
		placeLinuxWindow(tool, windowID, rects[i])
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

// placeLinuxWindow moves + resizes a window via the focus tool. Sizing
// first, then moving: some window managers clamp a move against the
// window's current (possibly maximised) size, so setting the target size
// first makes the subsequent move land where asked. Best-effort — a
// failing sub-command is logged at debug, never fatal.
func placeLinuxWindow(tool, windowID string, r Rect) {
	if err := exec.Command(tool, "windowsize", windowID, strconv.Itoa(r.W), strconv.Itoa(r.H)).Run(); err != nil {
		slog.Debug("tiling: windowsize failed", "tool", tool, "id", windowID, "err", err, "module", "tile")
	}
	if err := exec.Command(tool, "windowmove", windowID, strconv.Itoa(r.X), strconv.Itoa(r.Y)).Run(); err != nil {
		slog.Debug("tiling: windowmove failed", "tool", tool, "id", windowID, "err", err, "module", "tile")
	}
}

// linuxScreenArea reads the primary screen's pixel size. It tries
// `xdotool getdisplaygeometry` first — it returns the X screen geometry
// even on a Wayland session (via XWayland), so it works for both display
// servers — then falls back to parsing `xrandr`'s "current WxH". Returns
// ok=false when neither is available (headless / no X).
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
// WSL tiling — PowerShell SetWindowPos over the "tclaude:<id>" windows
// =============================================================================

// tileWSLWindows positions the focused Windows Terminal windows with a
// single PowerShell script: it reads the primary monitor's work area,
// then SetWindowPos's each window whose title carries the agent's
// "tclaude:<id>" pattern (the same pattern focusWindowByTitlePattern
// matches). One PowerShell invocation for the whole set — start-up cost
// dominates, so batching matters.
func tileWSLWindows(specs []TileSpec, opts TileOptions) {
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("tiling: PowerShell not found; leaving windows as-is", "module", "tile")
		return
	}
	area, ok := wslScreenArea(psPath)
	if !ok {
		slog.Debug("tiling: could not read Windows work area; leaving windows as-is", "module", "tile")
		return
	}
	rects := tileRects(len(specs), opts.Layout, area, opts.Gap, opts.Margin)
	script := buildWSLTileScript(specs, rects)
	if script == "" {
		slog.Debug("tiling: no tileable WSL targets (missing/invalid session ids)", "module", "tile")
		return
	}
	if err := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script).Run(); err != nil {
		slog.Debug("tiling: WSL SetWindowPos script failed", "err", err, "module", "tile")
	}
}

// wslScreenArea reads the primary monitor's WORK area (excludes the
// taskbar) via System.Windows.Forms, returning "X Y W H". Returns
// ok=false when PowerShell errors or the output is unparseable.
func wslScreenArea(psPath string) (Rect, bool) {
	const script = `Add-Type -AssemblyName System.Windows.Forms
$wa = [System.Windows.Forms.Screen]::PrimaryScreen.WorkingArea
Write-Output "$($wa.X) $($wa.Y) $($wa.Width) $($wa.Height)"`
	out, err := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return Rect{}, false
	}
	return parseWSLWorkArea(string(out))
}

// parseWSLWorkArea parses "X Y W H" (System.Windows.Forms WorkingArea)
// into a Rect. Pure so it's unit-testable.
func parseWSLWorkArea(out string) (Rect, bool) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 4 {
		return Rect{}, false
	}
	n := make([]int, 4)
	for i, s := range f {
		v, err := strconv.Atoi(s)
		if err != nil {
			return Rect{}, false
		}
		n[i] = v
	}
	if n[2] <= 0 || n[3] <= 0 {
		return Rect{}, false
	}
	return Rect{X: n[0], Y: n[1], W: n[2], H: n[3]}, true
}

// buildWSLTileScript generates the PowerShell that finds each agent's
// Windows Terminal window by its "tclaude:<id>" title and SetWindowPos's
// it to the matching rect. Specs with an empty or unsafe session id (see
// sanitizeTileSessionID) are dropped; if none remain the result is "" so
// the caller skips the PowerShell launch entirely. Pure so the generated
// script is unit-testable.
func buildWSLTileScript(specs []TileSpec, rects []Rect) string {
	var rows []string
	for i, spec := range specs {
		if i >= len(rects) {
			break
		}
		id := sanitizeTileSessionID(spec.SessionID)
		if id == "" {
			continue
		}
		r := rects[i]
		// id is charset-restricted to [A-Za-z0-9._:-] by sanitize, so it
		// cannot break out of the single-quoted PowerShell literal.
		rows = append(rows, fmt.Sprintf(
			"  @{ pat = 'tclaude:%s'; x = %d; y = %d; w = %d; h = %d }",
			id, r.X, r.Y, r.W, r.H))
	}
	if len(rows) == 0 {
		return ""
	}
	return wslTilePreamble + "$targets = @(\n" + strings.Join(rows, ",\n") + "\n)\n" + wslTileApply
}

// sanitizeTileSessionID returns s when it is a non-empty string of the
// safe title charset [A-Za-z0-9._:-], else "". It gates the value
// interpolated into the generated PowerShell so a session id can never
// inject script — the same defensive posture the send-keys sinks take,
// applied here to a script-generation sink.
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

// wslTilePreamble defines the P/Invoke helper used by the apply loop.
const wslTilePreamble = `Add-Type @"
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;
public class TclaudeTiler {
    public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);
    [DllImport("user32.dll")] public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr hWnd, StringBuilder lpString, int nMaxCount);
    [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr hWnd);
    [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr hWnd, IntPtr hWndInsertAfter, int X, int Y, int cx, int cy, uint uFlags);
    public static List<Tuple<IntPtr,string>> Visible() {
        var r = new List<Tuple<IntPtr,string>>();
        EnumWindows((h,l) => {
            if (IsWindowVisible(h)) {
                var sb = new StringBuilder(512);
                GetWindowText(h, sb, 512);
                var t = sb.ToString();
                if (!string.IsNullOrEmpty(t)) r.Add(Tuple.Create(h,t));
            }
            return true;
        }, IntPtr.Zero);
        return r;
    }
}
"@
`

// wslTileApply enumerates visible windows once and positions the first
// window matching each target's pattern. SWP_NOZORDER|SWP_NOACTIVATE:
// move + resize only, without disturbing the z-order the focus pass just
// established or stealing keyboard focus.
const wslTileApply = `$flags = [uint32](0x0004 -bor 0x0010)
$wins = [TclaudeTiler]::Visible()
foreach ($t in $targets) {
  foreach ($w in $wins) {
    if ($w.Item2 -like "*$($t.pat)*") {
      [TclaudeTiler]::SetWindowPos($w.Item1, [IntPtr]::Zero, [int]$t.x, [int]$t.y, [int]$t.w, [int]$t.h, $flags) | Out-Null
      break
    }
  }
}
`
