package drawingboard

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// Element is the part of a stored drawing-board element (the daemon's
// normalized Excalidraw subset) the renderer reads. Pointers keep absent
// fields absent, as the dashboard's `el.strokeWidth || 2` style defaults need.
type Element struct {
	ID              string      `json:"id"`
	Type            string      `json:"type"`
	X               float64     `json:"x"`
	Y               float64     `json:"y"`
	Width           float64     `json:"width"`
	Height          float64     `json:"height"`
	StrokeColor     string      `json:"strokeColor"`
	BackgroundColor string      `json:"backgroundColor"`
	StrokeWidth     float64     `json:"strokeWidth"`
	StrokeStyle     string      `json:"strokeStyle"`
	Opacity         *float64    `json:"opacity"`
	Points          [][]float64 `json:"points"`
	Text            string      `json:"text"`
	FontSize        float64     `json:"fontSize"`
	FontFamily      any         `json:"fontFamily"`
	FontWeight      any         `json:"fontWeight"`
	FontStyle       any         `json:"fontStyle"`
	TextAlign       string      `json:"textAlign"`
	ContainerID     string      `json:"containerId"`
	StartBinding    *Binding    `json:"startBinding"`
	EndBinding      *Binding    `json:"endBinding"`
	Routing         string      `json:"routing"`
	Z               *float64    `json:"z"`
	IsDeleted       bool        `json:"isDeleted"`
}

// Binding attaches one end of a line to an element; FixedPoint is the snap
// point as [x, y] fractions of the element's box.
type Binding struct {
	ElementID  string    `json:"elementId"`
	FixedPoint []float64 `json:"fixedPoint"`
}

// Box is an axis-aligned rectangle in drawing-board units.
type Box struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func (b Box) intersects(o Box) bool {
	return b.X <= o.X+o.Width && o.X <= b.X+b.Width && b.Y <= o.Y+o.Height && o.Y <= b.Y+b.Height
}

func union(a, b Box) Box {
	x0, y0 := math.Min(a.X, b.X), math.Min(a.Y, b.Y)
	x1, y1 := math.Max(a.X+a.Width, b.X+b.Width), math.Max(a.Y+a.Height, b.Y+b.Height)
	return Box{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

// DecodeElements reads stored element bodies, in the board's stored order.
// A body that does not decode is skipped.
func DecodeElements(bodies []json.RawMessage) []Element {
	out := make([]Element, 0, len(bodies))
	for _, raw := range bodies {
		var e Element
		if json.Unmarshal(raw, &e) == nil && e.ID != "" {
			out = append(out, e)
		}
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// textLines is a text element's lines, as the editor splits them.
func textLines(e Element) []string { return strings.Split(e.Text, "\n") }

func fontSize(e Element) float64 {
	if e.FontSize > 0 {
		return e.FontSize
	}
	return 20
}

// Bounds is an element's box (elementBounds in drawing-board-model.js). Text
// without a stored size is estimated as the editor estimates it.
func Bounds(e Element) Box {
	if len(e.Points) > 0 {
		minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
		for _, p := range e.Points {
			if len(p) != 2 {
				continue
			}
			minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
			minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
		}
		if !math.IsInf(minX, 1) {
			return Box{X: e.X + minX, Y: e.Y + minY, Width: maxX - minX, Height: maxY - minY}
		}
	}
	if e.Type == "text" {
		lines := textLines(e)
		size := fontSize(e)
		w := e.Width
		if w == 0 {
			longest := 1
			for _, l := range lines {
				longest = max(longest, len([]rune(l)))
			}
			w = float64(longest) * size * 0.58
		}
		h := e.Height
		if h == 0 {
			h = float64(len(lines)) * size * 1.25
		}
		return Box{X: e.X, Y: e.Y, Width: w, Height: h}
	}
	return Box{X: e.X, Y: e.Y, Width: e.Width, Height: e.Height}
}

// stacked is the live elements bottom first (stacked/liveElements): stored
// order, or by `z` (attached text standing with its container) once any
// element carries one.
func stacked(all []Element) []Element {
	byID := make(map[string]Element, len(all))
	anyZ := false
	for _, e := range all {
		byID[e.ID] = e
		anyZ = anyZ || e.Z != nil
	}
	out := make([]Element, 0, len(all))
	out = append(out, all...)
	if anyZ {
		key := func(e Element) float64 {
			if e.Z != nil {
				return *e.Z
			}
			if c, ok := byID[e.ContainerID]; ok && c.Z != nil {
				return *c.Z
			}
			return 0
		}
		sort.SliceStable(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	}
	live := out[:0]
	for _, e := range out {
		if !e.IsDeleted {
			live = append(live, e)
		}
	}
	return live
}

// Drawn is the live elements as the dashboard draws them (drawnElements):
// bottom first, bound line ends on their shapes, attached text on its
// container.
func Drawn(all []Element) []Element {
	live := map[string]Element{}
	for _, e := range all {
		if !e.IsDeleted {
			live[e.ID] = e
		}
	}
	order := stacked(all)
	drawn := make(map[string]Element, len(order))
	for _, e := range order {
		drawn[e.ID] = routeConnector(e, live)
	}
	for _, e := range order {
		if e.ContainerID == "" {
			continue
		}
		if c, ok := drawn[e.ContainerID]; ok && c.Type != "text" {
			drawn[e.ID] = textBoxOn(drawn[e.ID], c)
		}
	}
	out := make([]Element, 0, len(order))
	for _, e := range order {
		out = append(out, drawn[e.ID])
	}
	return out
}

func liveTarget(live map[string]Element, b *Binding) (Element, bool) {
	if b == nil {
		return Element{}, false
	}
	e, ok := live[b.ElementID]
	return e, ok
}

func sideOf(fp []float64) [2]float64 {
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

func snapAt(e Element, fp []float64) End {
	b := Bounds(e)
	dir := sideOf(fp)
	return End{X: b.X + b.Width*fp[0] + dir[0]*EndGap, Y: b.Y + b.Height*fp[1] + dir[1]*EndGap, Dir: dir}
}

func facingSnap(e Element, tx, ty float64) []float64 {
	b := Bounds(e)
	dx := (tx - (b.X + b.Width/2)) / math.Max(b.Width, 1)
	dy := (ty - (b.Y + b.Height/2)) / math.Max(b.Height, 1)
	if math.Abs(dx) >= math.Abs(dy) {
		if dx < 0 {
			return []float64{0, 0.5}
		}
		return []float64{1, 0.5}
	}
	if dy < 0 {
		return []float64{0.5, 0}
	}
	return []float64{0.5, 1}
}

func center(e Element) (float64, float64) {
	b := Bounds(e)
	return b.X + b.Width/2, b.Y + b.Height/2
}

func edgePoint(e Element, tx, ty float64) End {
	b := Bounds(e)
	cx, cy := b.X+b.Width/2, b.Y+b.Height/2
	dx, dy := tx-cx, ty-cy
	if dx == 0 && dy == 0 {
		return End{X: cx, Y: cy}
	}
	hw, hh := b.Width/2+EndGap, b.Height/2+EndGap
	scale := 1 / math.Max(math.Abs(dx)/math.Max(hw, 1e-6), math.Abs(dy)/math.Max(hh, 1e-6))
	return End{X: cx + dx*scale, Y: cy + dy*scale}
}

func validFixed(b *Binding) bool { return b != nil && len(b.FixedPoint) == 2 }

// routeConnector is where a line is drawn now (routeConnector in
// drawing-board-model.js): a free line, and a bound one with hand-placed
// bends and no routing, keep their stored path.
func routeConnector(e Element, live map[string]Element) Element {
	from, okFrom := liveTarget(live, e.StartBinding)
	to, okTo := liveTarget(live, e.EndBinding)
	if e.Type == "freedraw" {
		if !okFrom || !validFixed(e.StartBinding) || len(e.Points) == 0 || len(e.Points[0]) != 2 {
			return e
		}
		at := snapAt(from, e.StartBinding.FixedPoint)
		e.X, e.Y = round2(at.X-e.Points[0][0]), round2(at.Y-e.Points[0][1])
		return e
	}
	if (e.Type != "arrow" && e.Type != "line") || len(e.Points) == 0 {
		return e
	}
	for _, p := range e.Points {
		if len(p) != 2 {
			return e
		}
	}
	routed := e.Routing == "elbow" || e.Routing == "curved"
	unrouted := routed && len(e.Points) == 2
	if !unrouted && ((!okFrom && !okTo) || (e.Routing == "" && len(e.Points) > 2)) {
		return e
	}
	first, last := e.Points[0], e.Points[len(e.Points)-1]
	a := End{X: e.X + first[0], Y: e.Y + first[1]}
	b := End{X: e.X + last[0], Y: e.Y + last[1]}
	aim := func(bind *Binding, target Element, ok bool, free End) (float64, float64) {
		if !ok {
			return free.X, free.Y
		}
		if validFixed(bind) {
			s := snapAt(target, bind.FixedPoint)
			return s.X, s.Y
		}
		return center(target)
	}
	endOf := func(bind *Binding, target Element, tx, ty float64) End {
		switch {
		case validFixed(bind):
			return snapAt(target, bind.FixedPoint)
		case routed:
			return snapAt(target, facingSnap(target, tx, ty))
		}
		return edgePoint(target, tx, ty)
	}
	start, end := a, b
	if okFrom {
		tx, ty := aim(e.EndBinding, to, okTo, b)
		start = endOf(e.StartBinding, from, tx, ty)
	}
	if okTo {
		tx, ty := aim(e.StartBinding, from, okFrom, a)
		end = endOf(e.EndBinding, to, tx, ty)
	}
	path := RoutePath(e.Routing, start, end)
	if len(path) < 2 {
		path = append(path, path[0])
	}
	ox, oy := path[0][0], path[0][1]
	pts := make([][]float64, 0, len(path))
	for _, p := range path {
		pts = append(pts, []float64{round2(p[0] - ox), round2(p[1] - oy)})
	}
	e.X, e.Y, e.Points = round2(ox), round2(oy), pts
	bb := Bounds(Element{Points: pts})
	e.Width, e.Height = bb.Width, bb.Height
	return e
}

// labelPoint is where attached text sits: the middle of a box, or halfway
// along a line's path.
func labelPoint(e Element) (float64, float64) {
	if len(e.Points) == 0 {
		return center(e)
	}
	var lengths []float64
	total := 0.0
	for i := 1; i < len(e.Points); i++ {
		a, b := e.Points[i-1], e.Points[i]
		l := math.Hypot(b[0]-a[0], b[1]-a[1])
		lengths = append(lengths, l)
		total += l
	}
	left := total / 2
	for i, l := range lengths {
		if left <= l && l > 0 {
			a, b := e.Points[i], e.Points[i+1]
			t := left / l
			return e.X + a[0] + (b[0]-a[0])*t, e.Y + a[1] + (b[1]-a[1])*t
		}
		left -= l
	}
	return e.X + e.Points[0][0], e.Y + e.Points[0][1]
}

func textBoxOn(text, container Element) Element {
	ax, ay := labelPoint(container)
	b := Bounds(text)
	text.X, text.Y = round2(ax-b.Width/2), round2(ay-b.Height/2)
	return text
}
