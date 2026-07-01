package session

// Auto-tiling of agent terminal windows after a bulk "focus" op.
//
// The dashboard's 🪟 windows… modal / command palette / group focus
// button raise (or open) the terminal windows of a set of running
// agents; the OS decides where each lands, which "leaves a bit to be
// desired" when several stack on top of each other. When the opt-in
// config focus.tile.enabled is set, agentd follows a bulk focus with a
// call to TileAgentWindows, which arranges just that focused set into a
// grid (or the configured layout).
//
// This file holds the platform-INDEPENDENT half: the layout arithmetic
// (tileRects) and the public entry point (TileAgentWindows), which reads
// the screen work-area and moves each window through a per-platform
// primitive (platformTileWindows, declared in tile_{darwin,linux,other}.go).
// The math is pure and unit-tested; only the enumerate-window + set-geometry
// step is OS-specific and best-effort (it no-ops on an unsupported desktop
// rather than erroring, exactly like the focus path it follows).

import (
	"log/slog"
	"math"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Rect is a screen-space rectangle in pixels: origin (X,Y) at the
// top-left, W wide and H tall. It is both the work-area input to
// tileRects and the per-window target rectangle it returns.
type Rect struct {
	X, Y, W, H int
}

// Size is a window's current pixel dimensions, read before arranging so
// the default (no-resize) layout can reposition windows without changing
// how big they are.
type Size struct {
	W, H int
}

// center returns the rect's center point.
func (r Rect) center() (int, int) { return r.X + r.W/2, r.Y + r.H/2 }

// pointInRect reports whether (px,py) lies inside r (half-open on the
// far edges, so adjacent monitors don't both claim a boundary pixel).
func pointInRect(px, py int, r Rect) bool {
	return px >= r.X && px < r.X+r.W && py >= r.Y && py < r.Y+r.H
}

// pickMonitor returns the work-area of the monitor that should host the
// tiled set: the monitor containing (px,py) — the first window's center —
// else the nearest monitor by center distance. ok is false only when
// monitors is empty, so the caller can fall back to a whole-desktop area.
func pickMonitor(px, py int, monitors []Rect) (Rect, bool) {
	if len(monitors) == 0 {
		return Rect{}, false
	}
	for _, m := range monitors {
		if pointInRect(px, py, m) {
			return m, true
		}
	}
	best, bestD := monitors[0], monitorDist2(px, py, monitors[0])
	for _, m := range monitors[1:] {
		if d := monitorDist2(px, py, m); d < bestD {
			best, bestD = m, d
		}
	}
	return best, true
}

// monitorDist2 is the squared distance from (px,py) to a monitor's center
// — used only to rank "nearest" when no monitor contains the point.
func monitorDist2(px, py int, m Rect) int {
	cx, cy := m.center()
	dx, dy := px-cx, py-cy
	return dx*dx + dy*dy
}

// clampTopLeft nudges r's top-left so the window stays reachable inside
// area (its size is untouched — a window wider/taller than the monitor
// may still overflow the far edge, but its title bar stays grabbable).
func clampTopLeft(r, area Rect) Rect {
	if r.X < area.X {
		r.X = area.X
	} else if maxX := area.X + area.W - 1; r.X > maxX {
		r.X = maxX
	}
	if r.Y < area.Y {
		r.Y = area.Y
	} else if maxY := area.Y + area.H - 1; r.Y > maxY {
		r.Y = maxY
	}
	return r
}

// TileSpec identifies one agent whose terminal window the tiling pass
// should place. It carries the same two handles the focus path keys on:
// TmuxSession (to find the attached client's tty on macOS/Linux) and
// SessionID (the "tclaude:<id>" window title WSL matches on). The ORDER
// of a []TileSpec is the tiling order — row-major across the grid.
type TileSpec struct {
	TmuxSession string
	SessionID   string
}

// TileOptions carries the resolved layout knobs (already read from
// config by the caller). Layout is one of config.TileLayout*; Resize
// stretches windows to fill their cells (false = keep current size, only
// reposition); Gap is the pixels left between adjacent tiles; Margin is
// the pixels of inset kept from the screen work-area edges.
type TileOptions struct {
	Layout string
	Resize bool
	Gap    int
	Margin int
}

// TileAgentWindows arranges the given agents' terminal windows into the
// configured layout. It is best-effort: a single-window set is left
// untouched (there is nothing to arrange), and an unsupported desktop
// no-ops with a debug log instead of erroring. The heavy lifting — read
// the screen work-area, find each window, set its geometry — happens in
// the per-platform platformTileWindows.
func TileAgentWindows(specs []TileSpec, opts TileOptions) {
	if len(specs) < 2 {
		// Tiling one (or zero) windows would just maximise it — not what
		// "arrange these side by side" means. Nothing to do.
		return
	}
	// Normalize defensively so a platform impl never sees a blank layout
	// or a negative gap/margin, even if a caller skipped config's
	// resolvers.
	if opts.Layout == "" {
		opts.Layout = config.TileLayoutGrid
	}
	if opts.Gap < 0 {
		opts.Gap = 0
	}
	if opts.Margin < 0 {
		opts.Margin = 0
	}
	slog.Debug("tiling agent windows", "count", len(specs), "layout", opts.Layout,
		"gap", opts.Gap, "margin", opts.Margin, "module", "tile")
	platformTileWindows(specs, opts)
}

// tileRects computes the target rectangle for each of n windows laid out
// in `area` (the screen work-area) using `layout`, leaving `gap` pixels
// between adjacent tiles and `margin` pixels of inset from every edge. It
// returns exactly n rectangles in row-major order (index i → the i-th
// TileSpec). n must be >= 1; the math is well-defined for n == 1 (a
// single tile filling the inset area) even though TileAgentWindows guards
// n < 2 before ever calling it.
func tileRects(n int, layout string, area Rect, gap, margin int) []Rect {
	if n <= 0 {
		return nil
	}
	if gap < 0 {
		gap = 0
	}
	if margin < 0 {
		margin = 0
	}

	// Inset the usable area by the margin on all sides. A margin larger
	// than the area clamps the box to zero size rather than going
	// negative — the per-cell floor below still yields a usable (if
	// tiny) rectangle.
	inner := Rect{
		X: area.X + margin,
		Y: area.Y + margin,
		W: max(0, area.W-2*margin),
		H: max(0, area.H-2*margin),
	}

	switch layout {
	case config.TileLayoutCascade:
		return cascadeRects(n, inner)
	case config.TileLayoutColumns:
		// n full-height columns side by side: one row, n columns.
		return gridRects(n, 1, n, inner, gap)
	case config.TileLayoutRows:
		// n full-width rows stacked: n rows, one column.
		return gridRects(n, n, 1, inner, gap)
	default: // config.TileLayoutGrid and any unexpected value
		cols := gridCols(n)
		rows := (n + cols - 1) / cols
		return gridRects(n, rows, cols, inner, gap)
	}
}

// gridCols picks the column count for a near-square grid of n windows:
// ceil(sqrt(n)). n=2 → 2 (side by side), n=4 → 2, n=5 → 3, n=9 → 3. rows
// is then ceil(n/cols); a partial last row simply leaves its trailing
// cells empty.
func gridCols(n int) int {
	return max(1, int(math.Ceil(math.Sqrt(float64(n)))))
}

// gridRects lays n windows into a uniform rows×cols grid inside `area`,
// row-major, with `gap` pixels between cells. Cells are equal-sized; when
// n < rows*cols the trailing cells stay empty (only n rectangles are
// returned). Each cell dimension is floored at 1px so a cramped screen
// never produces a zero/negative rectangle a window manager would reject.
func gridRects(n, rows, cols int, area Rect, gap int) []Rect {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	cellW := max(1, (area.W-gap*(cols-1))/cols)
	cellH := max(1, (area.H-gap*(rows-1))/rows)
	rects := make([]Rect, 0, n)
	for i := range n {
		r := i / cols
		c := i % cols
		rects = append(rects, Rect{
			X: area.X + c*(cellW+gap),
			Y: area.Y + r*(cellH+gap),
			W: cellW,
			H: cellH,
		})
	}
	return rects
}

// cascadeRects overlaps n windows with a fixed diagonal step — the
// macOS-style stagger. Each window is sized to a fraction of the
// work-area (cascadeSizePct) and offset so the last one still lands
// fully inside the area; with one window the step is zero. gap is
// intentionally unused here (cascade windows overlap by design).
func cascadeRects(n int, area Rect) []Rect {
	w := max(1, area.W*cascadeSizePct/100)
	h := max(1, area.H*cascadeSizePct/100)
	stepX, stepY := 0, 0
	if n > 1 {
		stepX = (area.W - w) / (n - 1)
		stepY = (area.H - h) / (n - 1)
	}
	rects := make([]Rect, 0, n)
	for i := range n {
		rects = append(rects, Rect{
			X: area.X + i*stepX,
			Y: area.Y + i*stepY,
			W: w,
			H: h,
		})
	}
	return rects
}

// cascadeSizePct is the fraction of the work-area each cascaded window
// occupies. 65% leaves room for the diagonal travel while keeping each
// window comfortably large.
const cascadeSizePct = 65

// noResizeCascadeStep is the diagonal offset (px) between windows in the
// no-resize cascade layout — big enough that each window's title bar
// stays visible above the one below it.
const noResizeCascadeStep = 34

// arrangeRects positions n windows at their CURRENT sizes (the default
// no-resize behaviour): it never changes a window's dimensions, only
// where its top-left lands, so an overlapping pile is spread out without
// being stretched to fill the screen. The layout controls the pattern:
//
//   - grid  — flow-pack left-to-right, wrapping to a new row when the
//     next window would run past the work-area's right edge.
//   - columns — a single left-to-right row (no wrap).
//   - rows    — a single top-to-bottom column.
//   - cascade — a diagonal stagger, windows overlapping by design.
//
// area is the target monitor's work-area; gap is the spacing left between
// windows; margin is the inset from the work-area edges. Returned rects
// carry each window's original size (sizes[i]) with a new position; a
// window larger than the work-area is placed at the inset origin and
// allowed to overflow (best-effort — we never shrink it).
func arrangeRects(sizes []Size, layout string, area Rect, gap, margin int) []Rect {
	n := len(sizes)
	if n == 0 {
		return nil
	}
	if gap < 0 {
		gap = 0
	}
	if margin < 0 {
		margin = 0
	}
	ox, oy := area.X+margin, area.Y+margin
	maxRight := area.X + area.W - margin
	rects := make([]Rect, n)

	switch layout {
	case config.TileLayoutCascade:
		for i := range n {
			rects[i] = Rect{X: ox + i*noResizeCascadeStep, Y: oy + i*noResizeCascadeStep, W: sizes[i].W, H: sizes[i].H}
		}
	case config.TileLayoutColumns:
		x := ox
		for i := range n {
			rects[i] = Rect{X: x, Y: oy, W: sizes[i].W, H: sizes[i].H}
			x += sizes[i].W + gap
		}
	case config.TileLayoutRows:
		y := oy
		for i := range n {
			rects[i] = Rect{X: ox, Y: y, W: sizes[i].W, H: sizes[i].H}
			y += sizes[i].H + gap
		}
	default: // grid — flow-pack with wrapping
		x, y, rowH := ox, oy, 0
		for i := range n {
			w, h := sizes[i].W, sizes[i].H
			if x > ox && x+w > maxRight { // won't fit on this row → wrap
				x = ox
				y += rowH + gap
				rowH = 0
			}
			rects[i] = Rect{X: x, Y: y, W: w, H: h}
			x += w + gap
			if h > rowH {
				rowH = h
			}
		}
	}
	return rects
}
