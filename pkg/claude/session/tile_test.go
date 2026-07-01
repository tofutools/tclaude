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
