package session

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// A 1000x1000 work-area with no gap/margin makes the arithmetic easy to
// eyeball across the layout modes.
var testArea = Rect{X: 0, Y: 0, W: 1000, H: 1000}

// coversArea asserts the returned rects stay inside the work-area (given
// the margin) and that there are exactly n of them.
func assertInside(t *testing.T, rects []Rect, n int, area Rect, margin int) {
	t.Helper()
	if len(rects) != n {
		t.Fatalf("expected %d rects, got %d", n, len(rects))
	}
	left, top := area.X+margin, area.Y+margin
	right, bottom := area.X+area.W-margin, area.Y+area.H-margin
	for i, r := range rects {
		if r.W < 1 || r.H < 1 {
			t.Errorf("rect[%d] has non-positive size: %+v", i, r)
		}
		if r.X < left || r.Y < top {
			t.Errorf("rect[%d] starts before the inset origin (%d,%d): %+v", i, left, top, r)
		}
		if r.X+r.W > right+1 || r.Y+r.H > bottom+1 { // +1 for integer-division slack
			t.Errorf("rect[%d] overflows the inset area (right=%d bottom=%d): %+v", i, right, bottom, r)
		}
	}
}

// Grid packs into ceil(sqrt(n)) columns; check the shape for a few n.
func TestTileRects_GridShape(t *testing.T) {
	cases := []struct {
		n, wantCols, wantRows int
	}{
		{1, 1, 1}, {2, 2, 1}, {3, 2, 2}, {4, 2, 2}, {5, 3, 2}, {9, 3, 3}, {10, 4, 3},
	}
	for _, tc := range cases {
		if got := gridCols(tc.n); got != tc.wantCols {
			t.Errorf("gridCols(%d) = %d, want %d", tc.n, got, tc.wantCols)
		}
		rects := tileRects(tc.n, config.TileLayoutGrid, testArea, 0, 0)
		assertInside(t, rects, tc.n, testArea, 0)
	}
}

// Two windows in a grid sit side by side, each half-width, full-height.
func TestTileRects_GridTwoSideBySide(t *testing.T) {
	rects := tileRects(2, config.TileLayoutGrid, testArea, 0, 0)
	if rects[0].X != 0 || rects[1].X != 500 {
		t.Errorf("expected left=0,right=500 columns, got %+v", rects)
	}
	if rects[0].W != 500 || rects[0].H != 1000 {
		t.Errorf("expected 500x1000 tiles, got %+v", rects[0])
	}
}

// Columns → n full-height side-by-side columns (one row).
func TestTileRects_Columns(t *testing.T) {
	rects := tileRects(4, config.TileLayoutColumns, testArea, 0, 0)
	assertInside(t, rects, 4, testArea, 0)
	for i, r := range rects {
		if r.H != 1000 {
			t.Errorf("column %d not full-height: %+v", i, r)
		}
		if r.W != 250 {
			t.Errorf("column %d expected width 250, got %+v", i, r)
		}
	}
}

// Rows → n full-width stacked rows (one column).
func TestTileRects_Rows(t *testing.T) {
	rects := tileRects(4, config.TileLayoutRows, testArea, 0, 0)
	assertInside(t, rects, 4, testArea, 0)
	for i, r := range rects {
		if r.W != 1000 {
			t.Errorf("row %d not full-width: %+v", i, r)
		}
		if r.H != 250 {
			t.Errorf("row %d expected height 250, got %+v", i, r)
		}
	}
}

// Gap eats into cell size and offsets later cells; margin insets the
// whole grid.
func TestTileRects_GapAndMargin(t *testing.T) {
	// 2 columns, gap=10, margin=20 in a 1000-wide area.
	// inner width = 1000 - 2*20 = 960; cellW = (960 - 10)/2 = 475.
	rects := tileRects(2, config.TileLayoutColumns, testArea, 10, 20)
	assertInside(t, rects, 2, testArea, 20)
	if rects[0].X != 20 {
		t.Errorf("first column should start at margin 20, got %+v", rects[0])
	}
	if rects[0].W != 475 {
		t.Errorf("expected cellW 475 with gap+margin, got %+v", rects[0])
	}
	if rects[1].X != 20+475+10 {
		t.Errorf("second column should be offset by cellW+gap, got %+v", rects[1])
	}
}

// Cascade staggers windows diagonally; each is the same size and the last
// one still fits inside the area.
func TestTileRects_Cascade(t *testing.T) {
	rects := tileRects(4, config.TileLayoutCascade, testArea, 0, 0)
	assertInside(t, rects, 4, testArea, 0)
	// All windows the same size.
	for i := 1; i < len(rects); i++ {
		if rects[i].W != rects[0].W || rects[i].H != rects[0].H {
			t.Errorf("cascade windows differ in size: %+v vs %+v", rects[0], rects[i])
		}
	}
	// Strictly increasing offset (n>1 → non-zero step).
	for i := 1; i < len(rects); i++ {
		if rects[i].X <= rects[i-1].X || rects[i].Y <= rects[i-1].Y {
			t.Errorf("cascade step not increasing at %d: %+v then %+v", i, rects[i-1], rects[i])
		}
	}
}

// An unknown layout falls back to grid (same as config.TileLayout's
// resolver), so tileRects never returns nil for n>=1.
func TestTileRects_UnknownLayoutFallsBackToGrid(t *testing.T) {
	rects := tileRects(4, "spiral", testArea, 0, 0)
	grid := tileRects(4, config.TileLayoutGrid, testArea, 0, 0)
	if len(rects) != len(grid) {
		t.Fatalf("unknown layout should mirror grid, got %d vs %d", len(rects), len(grid))
	}
	for i := range rects {
		if rects[i] != grid[i] {
			t.Errorf("rect[%d] unknown-layout %+v != grid %+v", i, rects[i], grid[i])
		}
	}
}

// A margin larger than the area must not produce negative sizes — the
// per-cell floor keeps every rectangle at least 1px.
func TestTileRects_OversizedMarginClamps(t *testing.T) {
	rects := tileRects(4, config.TileLayoutGrid, Rect{W: 100, H: 100}, 0, 200)
	if len(rects) != 4 {
		t.Fatalf("expected 4 rects, got %d", len(rects))
	}
	for i, r := range rects {
		if r.W < 1 || r.H < 1 {
			t.Errorf("rect[%d] non-positive under oversized margin: %+v", i, r)
		}
	}
}

// tileRects on n==0 returns nil (defensive; callers guard n<2 anyway).
func TestTileRects_ZeroN(t *testing.T) {
	if got := tileRects(0, config.TileLayoutGrid, testArea, 0, 0); got != nil {
		t.Errorf("expected nil for n=0, got %+v", got)
	}
}

// TileAgentWindows is a no-op for < 2 specs — assert it does not panic and
// (indirectly) that platformTileWindows is never reached. We can't observe
// the OS effect, so this just pins the guard.
func TestTileAgentWindows_GuardsSmallSets(t *testing.T) {
	TileAgentWindows(nil, TileOptions{})
	TileAgentWindows([]TileSpec{{TmuxSession: "a"}}, TileOptions{})
	// No panic == pass.
}

// arrangeRects keeps every window's ORIGINAL size — it only moves them.
func TestArrangeRects_PreservesSizes(t *testing.T) {
	sizes := []Size{{300, 200}, {640, 480}, {800, 600}}
	for _, layout := range []string{config.TileLayoutGrid, config.TileLayoutColumns, config.TileLayoutRows, config.TileLayoutCascade} {
		rects := arrangeRects(sizes, layout, testArea, 10, 5)
		if len(rects) != len(sizes) {
			t.Fatalf("%s: got %d rects", layout, len(rects))
		}
		for i := range sizes {
			if rects[i].W != sizes[i].W || rects[i].H != sizes[i].H {
				t.Errorf("%s: rect[%d] resized: got %dx%d want %dx%d",
					layout, i, rects[i].W, rects[i].H, sizes[i].W, sizes[i].H)
			}
		}
	}
}

// Grid flow-pack lays windows left-to-right and wraps to a new row when
// the next one won't fit.
func TestArrangeRects_GridWraps(t *testing.T) {
	// area width 1000, margin 0, gap 0; three 400-wide windows → 2 per row.
	sizes := []Size{{400, 100}, {400, 120}, {400, 100}}
	rects := arrangeRects(sizes, config.TileLayoutGrid, Rect{W: 1000, H: 1000}, 0, 0)
	if rects[0].X != 0 || rects[1].X != 400 {
		t.Errorf("first two should share row 0: %+v %+v", rects[0], rects[1])
	}
	if rects[2].X != 0 {
		t.Errorf("third should wrap to x=0: %+v", rects[2])
	}
	// Row height is the tallest in row 0 (120), so row 1 starts at y=120.
	if rects[2].Y != 120 {
		t.Errorf("third should drop below the tallest row-0 window (120): %+v", rects[2])
	}
}

// Columns = one row; Rows = one column — both at current sizes.
func TestArrangeRects_ColumnsAndRows(t *testing.T) {
	sizes := []Size{{200, 100}, {300, 150}}
	cols := arrangeRects(sizes, config.TileLayoutColumns, Rect{W: 2000, H: 2000}, 10, 0)
	if cols[0].Y != cols[1].Y {
		t.Errorf("columns share a row: %+v %+v", cols[0], cols[1])
	}
	if cols[1].X != 200+10 {
		t.Errorf("second column offset by first width + gap: %+v", cols[1])
	}
	rows := arrangeRects(sizes, config.TileLayoutRows, Rect{W: 2000, H: 2000}, 10, 0)
	if rows[0].X != rows[1].X {
		t.Errorf("rows share a column: %+v %+v", rows[0], rows[1])
	}
	if rows[1].Y != 100+10 {
		t.Errorf("second row offset by first height + gap: %+v", rows[1])
	}
}

// pickMonitor returns the monitor containing the point; the nearest when
// none contains it; and ok=false only when there are no monitors.
func TestPickMonitor(t *testing.T) {
	left := Rect{X: 0, Y: 0, W: 1920, H: 1080}
	right := Rect{X: 1920, Y: 0, W: 2560, H: 1440}
	mons := []Rect{left, right}

	m, ok := pickMonitor(100, 100, mons)
	if !ok || m != left {
		t.Errorf("point on left monitor: ok=%v m=%+v", ok, m)
	}
	m, ok = pickMonitor(3000, 200, mons)
	if !ok || m != right {
		t.Errorf("point on right monitor: ok=%v m=%+v", ok, m)
	}
	// A point below both (in neither) picks the nearest by center.
	m, ok = pickMonitor(200, 5000, mons)
	if !ok || m != left {
		t.Errorf("point off both picks nearest (left): ok=%v m=%+v", ok, m)
	}
	if _, ok := pickMonitor(0, 0, nil); ok {
		t.Errorf("no monitors → ok=false")
	}
}

// clampTopLeft keeps a window's top-left inside the area without touching
// its size.
func TestClampTopLeft(t *testing.T) {
	area := Rect{X: 0, Y: 0, W: 1000, H: 800}
	// Off the right/bottom → pulled back inside; size unchanged.
	got := clampTopLeft(Rect{X: 5000, Y: 5000, W: 400, H: 300}, area)
	if got.X >= area.W || got.Y >= area.H {
		t.Errorf("top-left not clamped inside: %+v", got)
	}
	if got.W != 400 || got.H != 300 {
		t.Errorf("size changed by clamp: %+v", got)
	}
	// Already inside → unchanged.
	in := Rect{X: 100, Y: 100, W: 400, H: 300}
	if clampTopLeft(in, area) != in {
		t.Errorf("in-bounds rect should be unchanged")
	}
	// Negative origin monitor: clamp respects the origin.
	neg := Rect{X: -1920, Y: 0, W: 1920, H: 1080}
	g := clampTopLeft(Rect{X: -5000, Y: 0, W: 300, H: 200}, neg)
	if g.X < neg.X {
		t.Errorf("clamp ignored negative monitor origin: %+v", g)
	}
}
