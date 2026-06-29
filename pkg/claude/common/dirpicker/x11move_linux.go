//go:build linux

package dirpicker

import (
	"context"
	"io"
	"log"
	"log/slog"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

func init() {
	// xgb logs connection/authority chatter to stderr by default (e.g. "Could
	// not get authority info"). Positioning is best-effort and its failures are
	// deliberately swallowed, so silence the library to keep agentd's stderr
	// clean.
	xgb.Logger = log.New(io.Discard, "", 0)
}

// Window positioning for the native Linux pickers.
//
// On a Wayland session the picker (zenity/kdialog) opens as a native Wayland
// surface, and Wayland deliberately denies any client — or any external tool —
// the ability to set a top-level's global position; the compositor places it,
// which on WSLg lands at an effectively random spot every launch. The only way
// to regain control is to run the dialog as an X11 (XWayland) window, which the
// caller does by forcing GDK_BACKEND=x11 / QT_QPA_PLATFORM=xcb when an X server
// is reachable. This file then nudges that X11 window next to the pointer,
// clamped to fit the monitor it lands on.
//
// Everything here is strictly best-effort: any failure (no X server, the window
// never appears, a request errors) is a silent no-op that leaves the dialog
// wherever the WM put it — exactly the pre-existing behaviour.

const (
	// positionPollInterval is how often we re-scan for the picker window
	// after launching it; the window maps asynchronously once the toolkit
	// finishes initialising.
	positionPollInterval = 75 * time.Millisecond
	// positionEdgeMargin keeps the dialog a few pixels off the monitor edge
	// so it never butts flush against (or spills past) a screen border.
	positionEdgeMargin = 8
)

// rect is a screen-space rectangle in root-window coordinates.
type rect struct{ x, y, w, h int }

// x11Available reports whether an X server can be reached right now. The caller
// uses it to decide whether forcing the X11 backend is safe: if we cannot talk
// to X we must not override GDK_BACKEND, or the picker would fail to start on a
// pure-Wayland / headless box.
func x11Available() bool {
	conn, err := xgb.NewConn()
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// positionPickerNearPointer waits for the X11 window owned by pid to map, then
// moves it next to the pointer, clamped onto the monitor under the pointer. It
// returns as soon as it has moved the window, the context ends, or it gives up.
func positionPickerNearPointer(ctx context.Context, pid uint32) {
	conn, err := xgb.NewConn()
	if err != nil {
		return
	}
	defer conn.Close()

	root := xproto.Setup(conn).DefaultScreen(conn).Root
	pidAtom := internAtom(conn, "_NET_WM_PID")
	if pidAtom == 0 {
		return // no way to identify the window by owner pid
	}

	ticker := time.NewTicker(positionPollInterval)
	defer ticker.Stop()
	for {
		if win, ok := findWindowByPID(conn, root, pidAtom, pid); ok {
			moveWindowNearPointer(conn, root, win)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// moveWindowNearPointer reads the pointer and the picker's geometry and issues
// the configure request that relocates it.
func moveWindowNearPointer(conn *xgb.Conn, root, win xproto.Window) {
	ptr, err := xproto.QueryPointer(conn, root).Reply()
	if err != nil || ptr == nil {
		return
	}
	top := topLevelWindow(conn, root, win)
	geom, err := xproto.GetGeometry(conn, xproto.Drawable(top)).Reply()
	if err != nil || geom == nil || geom.Width <= 1 || geom.Height <= 1 {
		return // not mapped / sized yet — a later poll will catch it
	}

	mon := monitorContaining(conn, root, int(ptr.RootX), int(ptr.RootY))
	x, y := clampNearPointer(int(ptr.RootX), int(ptr.RootY),
		int(geom.Width), int(geom.Height), mon, positionEdgeMargin)

	mask := uint16(xproto.ConfigWindowX | xproto.ConfigWindowY)
	xproto.ConfigureWindow(conn, top, mask, []uint32{uint32(int32(x)), uint32(int32(y))})
	conn.Sync()
	slog.Debug("dirpicker: positioned picker near pointer",
		"pointer", []int{int(ptr.RootX), int(ptr.RootY)}, "to", []int{x, y},
		"size", []int{int(geom.Width), int(geom.Height)}, "monitor", mon)
}

// clampNearPointer centres a w×h window on the pointer, then clamps it so the
// whole window stays within mon (less a margin). It is pure so the placement
// maths can be unit-tested without an X server.
func clampNearPointer(px, py, w, h int, mon rect, margin int) (int, int) {
	return clampAxis(px-w/2, mon.x, mon.w, w, margin),
		clampAxis(py-h/2, mon.y, mon.h, h, margin)
}

// clampAxis confines v (a window edge) to [start+margin, start+size-win-margin].
// When the window is larger than the monitor on this axis it aligns to the
// monitor's leading edge so the top-left stays visible.
func clampAxis(v, start, size, win, margin int) int {
	lo := start + margin
	hi := start + size - win - margin
	if hi < lo {
		return start
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// monitorContaining returns the RandR monitor rectangle that holds (px,py),
// falling back to the first monitor and finally the whole screen when RandR is
// unavailable or reports nothing.
func monitorContaining(conn *xgb.Conn, root xproto.Window, px, py int) rect {
	screen := xproto.Setup(conn).DefaultScreen(conn)
	full := rect{0, 0, int(screen.WidthInPixels), int(screen.HeightInPixels)}

	if randr.Init(conn) != nil {
		return full
	}
	mons, err := randr.GetMonitors(conn, root, true).Reply()
	if err != nil || mons == nil || len(mons.Monitors) == 0 {
		return full
	}
	first := rect{}
	for i, m := range mons.Monitors {
		r := rect{int(m.X), int(m.Y), int(m.Width), int(m.Height)}
		if i == 0 {
			first = r
		}
		if px >= r.x && px < r.x+r.w && py >= r.y && py < r.y+r.h {
			return r
		}
	}
	return first
}

// findWindowByPID walks the window tree under root looking for the window whose
// _NET_WM_PID property equals pid.
func findWindowByPID(conn *xgb.Conn, root xproto.Window, pidAtom xproto.Atom, pid uint32) (xproto.Window, bool) {
	return searchByPID(conn, root, pidAtom, pid)
}

func searchByPID(conn *xgb.Conn, win xproto.Window, pidAtom xproto.Atom, pid uint32) (xproto.Window, bool) {
	// A toolkit can own several windows under one pid — GTK keeps a 1x1 helper
	// alongside the visible dialog — so require the match to be a real, mapped,
	// sized window, otherwise we would "move" the invisible helper.
	if windowPID(conn, win, pidAtom) == pid && isPositionableWindow(conn, win) {
		return win, true
	}
	tree, err := xproto.QueryTree(conn, win).Reply()
	if err != nil || tree == nil {
		return 0, false
	}
	for _, child := range tree.Children {
		if w, ok := searchByPID(conn, child, pidAtom, pid); ok {
			return w, true
		}
	}
	return 0, false
}

// isPositionableWindow reports whether win is a viewable window with a real
// (>1x1) size — i.e. the visible dialog rather than an off-screen helper.
func isPositionableWindow(conn *xgb.Conn, win xproto.Window) bool {
	attrs, err := xproto.GetWindowAttributes(conn, win).Reply()
	if err != nil || attrs == nil || attrs.MapState != xproto.MapStateViewable {
		return false
	}
	geom, err := xproto.GetGeometry(conn, xproto.Drawable(win)).Reply()
	return err == nil && geom != nil && geom.Width > 1 && geom.Height > 1
}

// windowPID reads a window's _NET_WM_PID, or 0 when the property is absent.
func windowPID(conn *xgb.Conn, win xproto.Window, pidAtom xproto.Atom) uint32 {
	reply, err := xproto.GetProperty(conn, false, win, pidAtom, xproto.AtomCardinal, 0, 1).Reply()
	if err != nil || reply == nil || reply.Format != 32 || len(reply.Value) < 4 {
		return 0
	}
	return xgb.Get32(reply.Value)
}

// topLevelWindow climbs from win up to the top-level window managed by the WM
// (the child of root), which is the window a move request must target so the
// frame and its contents travel together.
func topLevelWindow(conn *xgb.Conn, root, win xproto.Window) xproto.Window {
	for win != root && win != 0 {
		tree, err := xproto.QueryTree(conn, win).Reply()
		if err != nil || tree == nil || tree.Parent == root || tree.Parent == 0 {
			return win
		}
		win = tree.Parent
	}
	return win
}

// internAtom resolves an existing atom by name, returning 0 when it does not
// exist on the server yet.
func internAtom(conn *xgb.Conn, name string) xproto.Atom {
	reply, err := xproto.InternAtom(conn, true, uint16(len(name)), name).Reply()
	if err != nil || reply == nil {
		return 0
	}
	return reply.Atom
}
