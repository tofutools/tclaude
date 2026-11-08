package drawingboard

import "math"

// Line routing: the paths the dashboard works out in drawing-board-model.js
// (routePath and its helpers), shared by the CLI, which stores a line as it
// will be drawn, and the renderer, which draws it. The CLI's
// testdata/drawing_board_routes.json holds the cases both must agree on.

const (
	EndGap     = 4.0  // keeps an arrowhead readable against the outline
	ElbowStub  = 24.0 // how far a path leaves a shape before it turns
	CurveSteps = 24
)

// End is one end of a path. A free end has no direction and faces the other
// end.
type End struct {
	X, Y float64
	Dir  [2]float64
}

// Free says the end leaves in no particular direction.
func (e End) Free() bool { return e.Dir == [2]float64{} }

func facingDirection(from, to End) [2]float64 {
	dx, dy := to.X-from.X, to.Y-from.Y
	if math.Abs(dx) >= math.Abs(dy) {
		if dx < 0 {
			return [2]float64{-1, 0}
		}
		return [2]float64{1, 0}
	}
	if dy < 0 {
		return [2]float64{0, -1}
	}
	return [2]float64{0, 1}
}

// dropBends leaves out repeated points and points in the middle of a run.
func dropBends(path [][2]float64) [][2]float64 {
	out := make([][2]float64, 0, len(path))
	for _, p := range path {
		if n := len(out); n > 0 && out[n-1] == p {
			continue
		}
		for len(out) >= 2 {
			a, b := out[len(out)-2], out[len(out)-1]
			if (b[0]-a[0])*(p[1]-a[1]) != (b[1]-a[1])*(p[0]-a[0]) {
				break
			}
			out = out[:len(out)-1]
		}
		out = append(out, p)
	}
	return out
}

// elbowPath leaves each end along its side, then joins the two stubs with
// horizontal and vertical runs.
func elbowPath(start, end End) [][2]float64 {
	stub := math.Min(ElbowStub, math.Hypot(end.X-start.X, end.Y-start.Y)/3)
	a := [2]float64{start.X + start.Dir[0]*stub, start.Y + start.Dir[1]*stub}
	b := [2]float64{end.X + end.Dir[0]*stub, end.Y + end.Dir[1]*stub}
	along := 1 // the axis the path leaves on
	if start.Dir[0] != 0 {
		along = 0
	}
	across := 1 - along
	at := func(main, side float64) [2]float64 {
		if along == 0 {
			return [2]float64{main, side}
		}
		return [2]float64{side, main}
	}
	var bends [][2]float64
	if (end.Dir[0] != 0) == (start.Dir[0] != 0) {
		sd, ed := start.Dir[along], end.Dir[along]
		switch {
		case sd == ed:
			turn := math.Min(a[along], b[along])
			if sd > 0 {
				turn = math.Max(a[along], b[along])
			}
			bends = [][2]float64{at(turn, a[across]), at(turn, b[across])}
		case (b[along]-a[along])*sd >= 0:
			mid := (a[along] + b[along]) / 2
			bends = [][2]float64{at(mid, a[across]), at(mid, b[across])}
		default:
			mid := (a[across] + b[across]) / 2
			bends = [][2]float64{at(a[along], mid), at(b[along], mid)}
		}
	} else {
		corner := at(b[along], a[across])
		if (corner[along]-a[along])*start.Dir[along] < 0 || (corner[across]-b[across])*end.Dir[across] < 0 {
			corner = at(a[along], b[across])
		}
		bends = [][2]float64{corner}
	}
	path := [][2]float64{{start.X, start.Y}, a}
	path = append(path, bends...)
	return dropBends(append(path, b, [2]float64{end.X, end.Y}))
}

// curvedPath is a cubic leaving each end along its side, sampled into points.
func curvedPath(start, end End) [][2]float64 {
	reach := math.Max(30, math.Hypot(end.X-start.X, end.Y-start.Y)*0.4)
	c1 := [2]float64{start.X + start.Dir[0]*reach, start.Y + start.Dir[1]*reach}
	c2 := [2]float64{end.X + end.Dir[0]*reach, end.Y + end.Dir[1]*reach}
	path := make([][2]float64, 0, CurveSteps+1)
	for i := 0; i <= CurveSteps; i++ {
		t := float64(i) / CurveSteps
		u := 1 - t
		mix := func(s, p1, p2, e float64) float64 {
			return u*u*u*s + 3*u*u*t*p1 + 3*u*t*t*p2 + t*t*t*e
		}
		path = append(path, [2]float64{mix(start.X, c1[0], c2[0], end.X), mix(start.Y, c1[1], c2[1], end.Y)})
	}
	return path
}

// RoutePath is the path between two ends.
func RoutePath(routing string, start, end End) [][2]float64 {
	if routing != "elbow" && routing != "curved" {
		return [][2]float64{{start.X, start.Y}, {end.X, end.Y}}
	}
	if start.Free() {
		start.Dir = facingDirection(start, end)
	}
	if end.Free() {
		end.Dir = facingDirection(end, start)
	}
	if routing == "elbow" {
		return elbowPath(start, end)
	}
	return curvedPath(start, end)
}
