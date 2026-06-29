//go:build linux

package dirpicker

import "testing"

// clampNearPointer is the placement maths the X11 positioner relies on; these
// pin its centring + edge-clamping without needing a live X server.
func TestClampNearPointer(t *testing.T) {
	const margin = 8
	primary := rect{0, 0, 2560, 1440}
	secondary := rect{2560, 0, 1920, 1080} // a monitor to the right of primary

	cases := []struct {
		name         string
		px, py, w, h int
		mon          rect
		wantX, wantY int
	}{
		{
			name: "centres on pointer when it fits",
			px:   1280, py: 720, w: 800, h: 600, mon: primary,
			wantX: 880, wantY: 420, // 1280-400, 720-300
		},
		{
			name: "clamps off the right/top edges",
			px:   2550, py: 10, w: 800, h: 600, mon: primary,
			wantX: 2560 - 800 - margin, wantY: margin,
		},
		{
			name: "clamps off the left/bottom edges",
			px:   5, py: 1435, w: 800, h: 600, mon: primary,
			wantX: margin, wantY: 1440 - 600 - margin,
		},
		{
			name: "window larger than monitor aligns to origin",
			px:   1280, py: 720, w: 3000, h: 600, mon: primary,
			wantX: 0, wantY: 420,
		},
		{
			name: "respects a non-zero monitor offset",
			px:   3520, py: 540, w: 800, h: 600, mon: secondary,
			wantX: 3120, wantY: 240,
		},
		{
			name: "clamps within the offset monitor",
			px:   4470, py: 540, w: 800, h: 600, mon: secondary,
			wantX: 2560 + 1920 - 800 - margin, wantY: 240,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y := clampNearPointer(tc.px, tc.py, tc.w, tc.h, tc.mon, margin)
			if x != tc.wantX || y != tc.wantY {
				t.Fatalf("clampNearPointer = (%d,%d), want (%d,%d)", x, y, tc.wantX, tc.wantY)
			}
		})
	}
}
