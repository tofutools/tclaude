package agent

import (
	"fmt"
	"io"
	"math"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Line routing for the CLI (tcl-n023fi, tcl-umx796): the paths the dashboard
// works out in board-model.js, so a line an agent writes or moves is stored as
// it will be drawn. testdata/board_routes.json holds the cases both must agree
// on. The dashboard draws attached ends from their bindings either way, so a
// path here that lags only shows to clients that do not route.

const (
	boardEndGap     = 4.0  // keeps an arrowhead readable against the outline
	boardElbowStub  = 24.0 // how far a path leaves a shape before it turns
	boardCurveSteps = 24
)

var boardRoutings = map[string]bool{"straight": true, "elbow": true, "curved": true}

// boardSnapSides names the snap points: the middle of each side, as
// Excalidraw fixedPoints.
var boardSnapSides = map[string][]float64{
	"top": {0.5, 0}, "right": {1, 0.5}, "bottom": {0.5, 1}, "left": {0, 0.5},
}

func boardSnapName(fp []float64) string {
	for name, point := range boardSnapSides {
		if len(fp) == 2 && point[0] == fp[0] && point[1] == fp[1] {
			return name
		}
	}
	return ""
}

// boardEnd is one end of a path. A free end has no direction and faces the
// other end.
type boardEnd struct {
	X, Y float64
	Dir  [2]float64
}

func (e boardEnd) free() bool { return e.Dir == [2]float64{} }

func boardRound(v float64) float64 { return math.Round(v*100) / 100 }

func snapDirection(fp []float64) [2]float64 {
	dx, dy := fp[0]-0.5, fp[1]-0.5
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

func snapEnd(el boardElementView, fp []float64) boardEnd {
	b := elementBox(el)
	dir := snapDirection(fp)
	return boardEnd{X: b.X + b.W*fp[0] + dir[0]*boardEndGap, Y: b.Y + b.H*fp[1] + dir[1]*boardEndGap, Dir: dir}
}

// facingSnap is the snap point on the side of el that faces (tx,ty).
func facingSnap(el boardElementView, tx, ty float64) []float64 {
	b := elementBox(el)
	dx := (tx - (b.X + b.W/2)) / math.Max(b.W, 1)
	dy := (ty - (b.Y + b.H/2)) / math.Max(b.H, 1)
	if math.Abs(dx) >= math.Abs(dy) {
		if dx < 0 {
			return boardSnapSides["left"]
		}
		return boardSnapSides["right"]
	}
	if dy < 0 {
		return boardSnapSides["top"]
	}
	return boardSnapSides["bottom"]
}

func facingDirection(from, to boardEnd) [2]float64 {
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
func elbowPath(start, end boardEnd) [][2]float64 {
	stub := math.Min(boardElbowStub, math.Hypot(end.X-start.X, end.Y-start.Y)/3)
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
func curvedPath(start, end boardEnd) [][2]float64 {
	reach := math.Max(30, math.Hypot(end.X-start.X, end.Y-start.Y)*0.4)
	c1 := [2]float64{start.X + start.Dir[0]*reach, start.Y + start.Dir[1]*reach}
	c2 := [2]float64{end.X + end.Dir[0]*reach, end.Y + end.Dir[1]*reach}
	path := make([][2]float64, 0, boardCurveSteps+1)
	for i := 0; i <= boardCurveSteps; i++ {
		t := float64(i) / boardCurveSteps
		u := 1 - t
		mix := func(s, p1, p2, e float64) float64 {
			return u*u*u*s + 3*u*u*t*p1 + 3*u*t*t*p2 + t*t*t*e
		}
		path = append(path, [2]float64{mix(start.X, c1[0], c2[0], end.X), mix(start.Y, c1[1], c2[1], end.Y)})
	}
	return path
}

// routePath is the path between two ends.
func routePath(routing string, start, end boardEnd) [][2]float64 {
	if routing != "elbow" && routing != "curved" {
		return [][2]float64{{start.X, start.Y}, {end.X, end.Y}}
	}
	if start.free() {
		start.Dir = facingDirection(start, end)
	}
	if end.free() {
		end.Dir = facingDirection(end, start)
	}
	if routing == "elbow" {
		return elbowPath(start, end)
	}
	return curvedPath(start, end)
}

// boardEdgePoint clips the ray from an element's centre toward (tx,ty) at its
// box, a small gap out.
func boardEdgePoint(el boardElementView, tx, ty float64) boardEnd {
	b := elementBox(el)
	cx, cy := b.X+b.W/2, b.Y+b.H/2
	dx, dy := tx-cx, ty-cy
	if dx == 0 && dy == 0 {
		return boardEnd{X: cx, Y: cy}
	}
	scale := 1 / math.Max(math.Abs(dx)/math.Max(b.W/2+boardEndGap, 1e-6), math.Abs(dy)/math.Max(b.H/2+boardEndGap, 1e-6))
	return boardEnd{X: cx + dx*scale, Y: cy + dy*scale}
}

// bindingTarget is the live element a binding names.
func (s *boardScene) bindingTarget(b *boardViewBinding) (boardElementView, bool) {
	if b == nil {
		return boardElementView{}, false
	}
	v, ok := s.view[b.ElementID]
	return v, ok
}

// routeLine works out where a line belongs now: from both bound ends, or from
// the bound end to its free tip, and between its own ends when it has no
// binding. Points of a line with no live binding are kept at its ends.
func (s *boardScene) routeLine(v boardElementView) [][2]float64 {
	ax, ay := v.X, v.Y
	bx, by := arrowEnd(v)
	if len(v.Points) > 0 && len(v.Points[0]) == 2 {
		ax, ay = v.X+v.Points[0][0], v.Y+v.Points[0][1]
	}
	from, okFrom := s.bindingTarget(v.StartBinding)
	to, okTo := s.bindingTarget(v.EndBinding)
	routed := v.Routing == "elbow" || v.Routing == "curved"
	anchored := (okFrom && v.StartBinding.FixedPoint != nil) || (okTo && v.EndBinding.FixedPoint != nil)
	if !routed && !anchored {
		// Plain lines keep meeting shapes the way `board add` always drew them.
		switch {
		case okFrom && okTo:
			sx, sy, ex, ey := arrowBetween(from, to)
			return [][2]float64{{sx, sy}, {ex, ey}}
		case okFrom:
			sx, sy, _, _ := arrowBetween(from, boardElementView{X: bx, Y: by})
			return [][2]float64{{sx, sy}, {bx, by}}
		case okTo:
			_, _, ex, ey := arrowBetween(boardElementView{X: ax, Y: ay}, to)
			return [][2]float64{{ax, ay}, {ex, ey}}
		}
	}
	aim := func(b *boardViewBinding, target boardElementView, ok bool, freeX, freeY float64) (float64, float64) {
		if !ok {
			return freeX, freeY
		}
		if b.FixedPoint != nil {
			e := snapEnd(target, b.FixedPoint)
			return e.X, e.Y
		}
		box := elementBox(target)
		return box.X + box.W/2, box.Y + box.H/2
	}
	endOf := func(b *boardViewBinding, target boardElementView, tx, ty float64) boardEnd {
		switch {
		case b.FixedPoint != nil:
			return snapEnd(target, b.FixedPoint)
		case routed:
			return snapEnd(target, facingSnap(target, tx, ty))
		}
		return boardEdgePoint(target, tx, ty)
	}
	start, end := boardEnd{X: ax, Y: ay}, boardEnd{X: bx, Y: by}
	if okFrom {
		tx, ty := aim(v.EndBinding, to, okTo, bx, by)
		start = endOf(v.StartBinding, from, tx, ty)
	}
	if okTo {
		tx, ty := aim(v.StartBinding, from, okFrom, ax, ay)
		end = endOf(v.EndBinding, to, tx, ty)
	}
	return routePath(v.Routing, start, end)
}

// handBent says a line's bends were placed by hand or by `board from-process`:
// it has no routing to work them out again from.
func handBent(v boardElementView) bool {
	return v.Routing == "" && len(v.Points) > 2
}

// setPath stores a path on a line: its origin, offsets and box.
func (s *boardScene) setPath(id string, path [][2]float64) {
	if len(path) == 1 { // ends that meet: a line still needs two points
		path = append(path, path[0])
	}
	v := s.view[id]
	ox, oy := boardRound(path[0][0]), boardRound(path[0][1])
	points := make([][]float64, 0, len(path))
	minX, minY, maxX, maxY := 0.0, 0.0, 0.0, 0.0
	for _, p := range path {
		px, py := boardRound(p[0]-ox), boardRound(p[1]-oy)
		points = append(points, []float64{px, py})
		minX, maxX = math.Min(minX, px), math.Max(maxX, px)
		minY, maxY = math.Min(minY, py), math.Max(maxY, py)
	}
	v.X, v.Y, v.Points = ox, oy, points
	v.Width, v.Height = maxX-minX, maxY-minY
	s.view[id] = v
	r := s.raw[id]
	r["x"], r["y"], r["points"] = ox, oy, points
	r["width"], r["height"] = v.Width, v.Height
}

// labelPoint is where attached text sits: the middle of a box, or halfway
// along a line's path.
func labelPoint(v boardElementView) (float64, float64) {
	if len(v.Points) == 0 {
		return v.X + v.Width/2, v.Y + v.Height/2
	}
	total := 0.0
	for i := 1; i < len(v.Points); i++ {
		total += math.Hypot(v.Points[i][0]-v.Points[i-1][0], v.Points[i][1]-v.Points[i-1][1])
	}
	left := total / 2
	for i := 1; i < len(v.Points); i++ {
		a, b := v.Points[i-1], v.Points[i]
		seg := math.Hypot(b[0]-a[0], b[1]-a[1])
		if left <= seg && seg > 0 {
			t := left / seg
			return v.X + a[0] + (b[0]-a[0])*t, v.Y + a[1] + (b[1]-a[1])*t
		}
		left -= seg
	}
	return v.X, v.Y
}

// placeAttachedText puts text back on its container, and says if it moved.
func (s *boardScene) placeAttachedText(id string) bool {
	v := s.view[id]
	container, ok := s.view[v.ContainerID]
	if v.ContainerID == "" || !ok || container.Type == "text" {
		return false
	}
	w, h := v.Width, v.Height
	if w == 0 || h == 0 {
		w, h = boardTextSize(v.Text)
	}
	cx, cy := labelPoint(container)
	x, y := boardRound(cx-w/2), boardRound(cy-h/2)
	if x == v.X && y == v.Y {
		return false
	}
	v.X, v.Y = x, y
	s.view[id] = v
	s.raw[id]["x"], s.raw[id]["y"] = x, y
	return true
}

// followLine re-routes a line, or shifts a stroke with the shape it starts on.
func (s *boardScene) followLine(id string) {
	v := s.view[id]
	if v.Type != "freedraw" {
		s.setPath(id, s.routeLine(v))
		return
	}
	from, ok := s.bindingTarget(v.StartBinding)
	if !ok || v.StartBinding.FixedPoint == nil || len(v.Points) == 0 {
		return
	}
	at := snapEnd(from, v.StartBinding.FixedPoint)
	v.X, v.Y = boardRound(at.X-v.Points[0][0]), boardRound(at.Y-v.Points[0][1])
	s.view[id] = v
	s.raw[id]["x"], s.raw[id]["y"] = v.X, v.Y
}

// ---- connect ----

type boardConnectParams struct {
	Routing  string `long:"routing" optional:"true" help:"How the line runs: straight, elbow or curved"`
	From     string `long:"from" optional:"true" help:"Attach the start to this element id"`
	FromSide string `long:"from-side" optional:"true" help:"Snap point for the start: top, right, bottom, left, or auto to let the line choose"`
	To       string `long:"to" optional:"true" help:"Attach the end to this element id"`
	ToSide   string `long:"to-side" optional:"true" help:"Snap point for the end: top, right, bottom, left, or auto to let the line choose"`
	FreeFrom bool   `long:"free-from" optional:"true" help:"Detach the start, leaving it where it is"`
	FreeTo   bool   `long:"free-to" optional:"true" help:"Detach the end, leaving it where it is"`
}

func boardConnectCmd() *cobra.Command {
	return boa.CmdT[boardConnectParams]{
		Use:   "connect <board> <line>",
		Short: "Change an arrow or line's routing and what its ends attach to",
		Long: "Changes one arrow or line: --routing picks how it runs, --from and --to attach its ends to elements, " +
			"--from-side and --to-side pick the snap point (the middle of a side), and --free-from and --free-to " +
			"detach an end where it is. An attached end follows its element, and text attached to the line stays on it.\n\n" +
			"Example: tclaude agent board connect plan e1 --routing elbow --from api --from-side right --to db --to-side left",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardConnectParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardConnect(p, args[0], args[1], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func checkBoardRouting(routing string) error {
	if routing != "" && !boardRoutings[routing] {
		return fmt.Errorf("--routing must be straight, elbow or curved, not %q", routing)
	}
	return nil
}

func checkBoardSide(flag, side string) error {
	if _, ok := boardSnapSides[side]; side != "" && side != "auto" && !ok {
		return fmt.Errorf("%s must be top, right, bottom, left or auto, not %q", flag, side)
	}
	return nil
}

// boardBindingFor is the binding JSON for an element and a side name.
func boardBindingFor(elemID, side string) (map[string]any, *boardViewBinding) {
	raw := map[string]any{"elementId": elemID}
	view := &boardViewBinding{ElementID: elemID}
	if fp, ok := boardSnapSides[side]; ok {
		raw["fixedPoint"], view.FixedPoint = fp, fp
	}
	return raw, view
}

func runBoardConnect(p *boardConnectParams, id, lineID string, stdout, stderr io.Writer) int {
	for _, err := range []error{checkBoardRouting(p.Routing), checkBoardSide("--from-side", p.FromSide), checkBoardSide("--to-side", p.ToSide)} {
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidArg
		}
	}
	if (p.FreeFrom && (p.From != "" || p.FromSide != "")) || (p.FreeTo && (p.To != "" || p.ToSide != "")) {
		fmt.Fprintln(stderr, "Error: an end is either attached or freed, not both")
		return rcInvalidArg
	}
	if p.Routing == "" && p.From == "" && p.FromSide == "" && p.To == "" && p.ToSide == "" && !p.FreeFrom && !p.FreeTo {
		fmt.Fprintln(stderr, "Error: nothing to change: give --routing, --from, --to, a side, --free-from or --free-to")
		return rcInvalidArg
	}
	doc, err := fetchBoard(id, -1)
	if err != nil {
		return boardErr(stderr, err)
	}
	s := loadBoardScene(doc)
	v, ok := s.view[lineID]
	if !ok {
		fmt.Fprintf(stderr, "Error: board %s has no live element %s\n", id, lineID)
		return rcNotFound
	}
	if v.Type != "arrow" && v.Type != "line" {
		fmt.Fprintf(stderr, "Error: %s is a %s; only arrows and lines connect\n", lineID, v.Type)
		return rcInvalidArg
	}
	// A freed end stays where it is drawn now, not where a stale path has it. A
	// line with hand-placed bends is drawn as stored, and only freeing keeps them.
	onlyFreeing := p.Routing == "" && p.From == "" && p.FromSide == "" && p.To == "" && p.ToSide == ""
	bent := handBent(v)
	if !bent {
		s.setPath(lineID, s.routeLine(v))
		v = s.view[lineID]
	}
	type end struct {
		key      string
		binding  **boardViewBinding
		elem     string
		side     string
		free     bool
		flagName string
	}
	for _, e := range []end{
		{"startBinding", &v.StartBinding, p.From, p.FromSide, p.FreeFrom, "--from"},
		{"endBinding", &v.EndBinding, p.To, p.ToSide, p.FreeTo, "--to"},
	} {
		switch {
		case e.free:
			*e.binding = nil
			delete(s.raw[lineID], e.key)
		case e.elem != "" || e.side != "":
			elem := e.elem
			if elem == "" {
				if *e.binding == nil {
					fmt.Fprintf(stderr, "Error: that end of %s is not attached; give %s too\n", lineID, e.flagName)
					return rcInvalidArg
				}
				elem = (*e.binding).ElementID
			}
			target, ok := s.view[elem]
			if !ok {
				fmt.Fprintf(stderr, "Error: board %s has no live element %s\n", id, elem)
				return rcNotFound
			}
			if target.Type == "arrow" || target.Type == "line" || target.Type == "freedraw" {
				fmt.Fprintf(stderr, "Error: lines attach to shapes and text, and %s is a %s\n", elem, target.Type)
				return rcInvalidArg
			}
			raw, view := boardBindingFor(elem, e.side)
			*e.binding = view
			s.raw[lineID][e.key] = raw
		}
	}
	if p.Routing != "" {
		v.Routing = p.Routing
		s.raw[lineID]["routing"] = p.Routing
	}
	s.view[lineID] = v
	if !bent || !onlyFreeing {
		s.setPath(lineID, s.routeLine(v))
	}
	changed := []string{lineID}
	for _, textID := range s.order {
		if s.view[textID].ContainerID == lineID && s.placeAttachedText(textID) {
			changed = append(changed, textID)
		}
	}
	return patchScene(id, s, changed, stdout, stderr)
}

// describeBoardEnd prints one end of a line for `board show`.
func describeBoardEnd(b *boardViewBinding) string {
	if b == nil {
		return "-"
	}
	if side := boardSnapName(b.FixedPoint); side != "" {
		return b.ElementID + ":" + side
	}
	return b.ElementID
}
