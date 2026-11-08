package drawingboard

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gobolditalic"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/gomonobolditalic"
	"golang.org/x/image/font/gofont/gomonoitalic"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// The raster draws geometry as polygons: fills through a nonzero-winding
// rasterizer, strokes as the union of one quad per segment and a disc per
// vertex (round caps and joins, as the editor's stroke-linecap/linejoin).
// Every piece is wound the same way, so overlaps add up and never cancel.

type raster struct {
	ctx   context.Context
	dst   *image.RGBA
	crop  Box
	scale float64
	faces map[string]font.Face
	z     *vector.Rasterizer // reused across paints
}

func renderPNG(ctx context.Context, sel, drawn []Element, crop Box, scale float64) ([]byte, int, int, error) {
	w := max(1, int(math.Round(crop.Width*scale)))
	h := max(1, int(math.Round(crop.Height*scale)))
	r := &raster{ctx: ctx, dst: image.NewRGBA(image.Rect(0, 0, w, h)), crop: crop, scale: scale, faces: map[string]font.Face{}, z: &vector.Rasterizer{}}
	defer func() {
		for _, f := range r.faces {
			_ = f.Close()
		}
	}()
	draw.Draw(r.dst, r.dst.Bounds(), image.White, image.Point{}, draw.Src)
	all := byID(drawn)
	for _, e := range sel {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		if err := r.element(e, onLine(e, all)); err != nil {
			return nil, 0, 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, r.dst); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), w, h, nil
}

// px maps a board point to pixels.
func (r *raster) px(p [2]float64) [2]float64 {
	return [2]float64{(p[0] - r.crop.X) * r.scale, (p[1] - r.crop.Y) * r.scale}
}

func parseColor(c string, opacity float64) color.NRGBA {
	hex := c[1:]
	if len(hex) == 3 || len(hex) == 4 {
		long := make([]byte, 0, 8)
		for i := 0; i < len(hex); i++ {
			long = append(long, hex[i], hex[i])
		}
		hex = string(long)
	}
	v, _ := strconv.ParseUint(hex, 16, 64)
	out := color.NRGBA{A: 255}
	switch len(hex) {
	case 6:
		out.R, out.G, out.B = uint8(v>>16), uint8(v>>8), uint8(v)
	case 8:
		out.R, out.G, out.B, out.A = uint8(v>>24), uint8(v>>16), uint8(v>>8), uint8(v)
	}
	out.A = uint8(math.Round(float64(out.A) * opacity))
	return out
}

// shape is one polygon (closed) or polyline (open) in pixels.
type shape struct {
	pts    [][2]float64
	closed bool
}

func (r *raster) toPx(pts [][2]float64, closed bool) shape {
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		out[i] = r.px(p)
	}
	return shape{pts: out, closed: closed}
}

// rect is a pixel rectangle, for clipping.
type rect struct{ x0, y0, x1, y1 float64 }

// canvas is the image's pixel box grown by margin.
func (r *raster) canvas(margin float64) rect {
	b := r.dst.Bounds()
	return rect{-margin, -margin, float64(b.Dx()) + margin, float64(b.Dy()) + margin}
}

// clipPoly clips a polygon to c (Sutherland–Hodgman). What lies outside
// the canvas costs nothing to rasterise then, however far a board's
// coordinates reach.
func clipPoly(poly [][2]float64, c rect) [][2]float64 {
	edges := []struct {
		in    func(p [2]float64) bool
		cross func(a, b [2]float64) [2]float64
	}{
		{func(p [2]float64) bool { return p[0] >= c.x0 }, func(a, b [2]float64) [2]float64 { return lerpX(a, b, c.x0) }},
		{func(p [2]float64) bool { return p[0] <= c.x1 }, func(a, b [2]float64) [2]float64 { return lerpX(a, b, c.x1) }},
		{func(p [2]float64) bool { return p[1] >= c.y0 }, func(a, b [2]float64) [2]float64 { return lerpY(a, b, c.y0) }},
		{func(p [2]float64) bool { return p[1] <= c.y1 }, func(a, b [2]float64) [2]float64 { return lerpY(a, b, c.y1) }},
	}
	out := poly
	for _, e := range edges {
		if len(out) == 0 {
			return nil
		}
		in := out
		out = make([][2]float64, 0, len(in)+4)
		prev := in[len(in)-1]
		for _, cur := range in {
			switch {
			case e.in(cur):
				if !e.in(prev) {
					out = append(out, e.cross(prev, cur))
				}
				out = append(out, cur)
			case e.in(prev):
				out = append(out, e.cross(prev, cur))
			}
			prev = cur
		}
	}
	return out
}

func lerpX(a, b [2]float64, x float64) [2]float64 {
	t := (x - a[0]) / (b[0] - a[0])
	return [2]float64{x, a[1] + (b[1]-a[1])*t}
}

func lerpY(a, b [2]float64, y float64) [2]float64 {
	t := (y - a[1]) / (b[1] - a[1])
	return [2]float64{a[0] + (b[0]-a[0])*t, y}
}

// clipSegment clips the segment a→b to c (Liang–Barsky), as fractions of
// its length; ok is false when no part of it is inside.
func clipSegment(a, b [2]float64, c rect) (t0, t1 float64, ok bool) {
	t0, t1 = 0, 1
	dx, dy := b[0]-a[0], b[1]-a[1]
	for _, e := range [4][2]float64{{-dx, a[0] - c.x0}, {dx, c.x1 - a[0]}, {-dy, a[1] - c.y0}, {dy, c.y1 - a[1]}} {
		p, q := e[0], e[1]
		if p == 0 {
			if q < 0 {
				return 0, 0, false
			}
			continue
		}
		t := q / p
		if p < 0 {
			t0 = math.Max(t0, t)
		} else {
			t1 = math.Min(t1, t)
		}
		if t0 > t1 {
			return 0, 0, false
		}
	}
	return t0, t1, true
}

// paint fills polygons with c, each clipped to the canvas, rasterised over
// the pixel box they cover.
func (r *raster) paint(polys [][][2]float64, c color.NRGBA) {
	if c.A == 0 || len(polys) == 0 {
		return
	}
	bounds := r.dst.Bounds()
	clip := r.canvas(1)
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	kept := polys[:0:0]
	for _, poly := range polys {
		poly = clipPoly(poly, clip)
		if len(poly) < 3 {
			continue
		}
		kept = append(kept, poly)
		for _, p := range poly {
			minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
			minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
		}
	}
	if len(kept) == 0 {
		return
	}
	box := image.Rect(int(math.Floor(minX))-1, int(math.Floor(minY))-1, int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1).Intersect(bounds)
	if box.Empty() {
		return
	}
	z := r.z
	z.Reset(box.Dx(), box.Dy())
	ox, oy := float32(box.Min.X), float32(box.Min.Y)
	for _, poly := range kept {
		z.MoveTo(float32(poly[0][0])-ox, float32(poly[0][1])-oy)
		for _, p := range poly[1:] {
			z.LineTo(float32(p[0])-ox, float32(p[1])-oy)
		}
		z.ClosePath()
	}
	z.DrawOp = draw.Over
	z.Draw(r.dst, box, image.NewUniform(c), image.Point{})
}

// signedArea is positive for a clockwise polygon in screen coordinates.
func signedArea(poly [][2]float64) float64 {
	a := 0.0
	for i := range poly {
		p, q := poly[i], poly[(i+1)%len(poly)]
		a += p[0]*q[1] - q[0]*p[1]
	}
	return a / 2
}

func clockwise(poly [][2]float64) [][2]float64 {
	if signedArea(poly) < 0 {
		for i, j := 0, len(poly)-1; i < j; i, j = i+1, j-1 {
			poly[i], poly[j] = poly[j], poly[i]
		}
	}
	return poly
}

func disc(c [2]float64, radius float64) [][2]float64 {
	n := max(8, min(48, int(radius*4)))
	out := make([][2]float64, n)
	for i := range n {
		a := 2 * math.Pi * float64(i) / float64(n)
		out[i] = [2]float64{c[0] + radius*math.Cos(a), c[1] + radius*math.Sin(a)}
	}
	return clockwise(out)
}

// segmentPolys outlines one straight piece of a stroke: a quad and a disc
// at each end (round caps and joins).
func segmentPolys(a, b [2]float64, hw float64) [][][2]float64 {
	out := [][][2]float64{disc(a, hw)}
	dx, dy := b[0]-a[0], b[1]-a[1]
	if l := math.Hypot(dx, dy); l > 0 {
		nx, ny := -dy/l*hw, dx/l*hw
		out = append(out, clockwise([][2]float64{{a[0] + nx, a[1] + ny}, {b[0] + nx, b[1] + ny}, {b[0] - nx, b[1] - ny}, {a[0] - nx, a[1] - ny}}),
			disc(b, hw))
	}
	return out
}

const (
	// minDashPeriod is the finest dash pattern drawn as dashes, in pixels;
	// finer reads as a solid line anyway.
	minDashPeriod = 3.0
	// maxDashes bounds the dashes one element draws on the canvas; a line
	// that would need more is drawn solid.
	maxDashes = 20000
	// paintBatch is how many polygons one rasterisation takes.
	paintBatch = 512
)

// stroke draws a polyline of the element's width, clipped to the canvas:
// only the parts on it are dashed and rasterised, while the dash pattern
// keeps its place along the whole line.
func (r *raster) stroke(sh shape, s style) {
	pts := sh.pts
	if sh.closed && len(pts) > 0 {
		pts = append(append([][2]float64{}, pts...), pts[0])
	}
	if len(pts) == 0 {
		return
	}
	width := math.Max(s.width*r.scale, 0.5)
	hw := width / 2
	clip := r.canvas(hw + 2)
	c := parseColor(s.stroke, s.opacity)
	var on, period float64
	if len(s.dash) == 2 {
		on, period = s.dash[0]*r.scale, (s.dash[0]+s.dash[1])*r.scale
		if period < minDashPeriod {
			period = 0
		} else {
			visible := 0.0
			for i := 1; i < len(pts); i++ {
				if t0, t1, ok := clipSegment(pts[i-1], pts[i], clip); ok {
					visible += (t1 - t0) * math.Hypot(pts[i][0]-pts[i-1][0], pts[i][1]-pts[i-1][1])
				}
			}
			if visible/period > maxDashes {
				period = 0
			}
		}
	}
	var batch [][][2]float64
	flush := func() {
		r.paint(batch, c)
		batch = batch[:0]
	}
	if len(pts) == 1 {
		if _, _, ok := clipSegment(pts[0], pts[0], clip); ok {
			batch = append(batch, disc(pts[0], hw))
		}
	}
	phase := 0.0 // where along the dash pattern the line is
	for i := 1; i < len(pts); i++ {
		if len(batch) >= paintBatch {
			if r.ctx.Err() != nil {
				return
			}
			flush()
		}
		a, b := pts[i-1], pts[i]
		l := math.Hypot(b[0]-a[0], b[1]-a[1])
		t0, t1, ok := clipSegment(a, b, clip)
		at := func(d float64) [2]float64 {
			if l == 0 {
				return a
			}
			return [2]float64{a[0] + (b[0]-a[0])*d/l, a[1] + (b[1]-a[1])*d/l}
		}
		if !ok {
			if period > 0 {
				phase = math.Mod(phase+l, period)
			}
			continue
		}
		if period == 0 {
			batch = append(batch, segmentPolys(at(t0*l), at(t1*l), hw)...)
			continue
		}
		pos, end := t0*l, t1*l
		phase = math.Mod(phase+pos, period)
		for pos < end {
			if phase < on {
				next := math.Min(end, pos+on-phase)
				batch = append(batch, segmentPolys(at(pos), at(next), hw)...)
				phase += next - pos
				pos = next
			} else {
				next := math.Min(end, pos+period-phase)
				phase += next - pos
				pos = next
			}
			if phase >= period {
				phase -= period
			}
			if len(batch) >= paintBatch {
				flush()
			}
		}
		phase = math.Mod(phase+(l-end), period)
	}
	flush()
}

func (r *raster) fill(sh shape, s style) {
	if s.fill != "" {
		r.paint([][][2]float64{sh.pts}, parseColor(s.fill, s.opacity))
	}
}

func roundedRect(x, y, w, h, rx float64) [][2]float64 {
	if rx <= 0 {
		return [][2]float64{{x, y}, {x + w, y}, {x + w, y + h}, {x, y + h}}
	}
	var out [][2]float64
	corner := func(cx, cy, from float64) {
		for i := 0; i <= 8; i++ {
			a := from + math.Pi/2*float64(i)/8
			out = append(out, [2]float64{cx + rx*math.Cos(a), cy + rx*math.Sin(a)})
		}
	}
	corner(x+w-rx, y+rx, -math.Pi/2)
	corner(x+w-rx, y+h-rx, 0)
	corner(x+rx, y+h-rx, math.Pi/2)
	corner(x+rx, y+rx, math.Pi)
	return out
}

func ellipsePoints(cx, cy, rx, ry float64) [][2]float64 {
	out := make([][2]float64, 72)
	for i := range out {
		a := 2 * math.Pi * float64(i) / 72
		out[i] = [2]float64{cx + rx*math.Cos(a), cy + ry*math.Sin(a)}
	}
	return out
}

// smoothPoints flattens a freehand stroke's quadratic path (smoothD).
func smoothPoints(pts [][2]float64) [][2]float64 {
	if len(pts) < 3 {
		return pts
	}
	out := [][2]float64{pts[0]}
	cur := pts[0]
	for i := 1; i < len(pts)-1; i++ {
		ctl := pts[i]
		end := [2]float64{(pts[i][0] + pts[i+1][0]) / 2, (pts[i][1] + pts[i+1][1]) / 2}
		for s := 1; s <= 6; s++ {
			t := float64(s) / 6
			u := 1 - t
			out = append(out, [2]float64{u*u*cur[0] + 2*u*t*ctl[0] + t*t*end[0], u*u*cur[1] + 2*u*t*ctl[1] + t*t*end[1]})
		}
		cur = end
	}
	return append(out, pts[len(pts)-1])
}

func (r *raster) element(e Element, line bool) error {
	s := styleOf(e)
	x, y, w, h := e.X, e.Y, e.Width, e.Height
	switch e.Type {
	case "rectangle":
		sh := r.toPx(roundedRect(x, y, w, h, math.Max(0, math.Min(10, math.Min(w/4, h/4)))), true)
		r.fill(sh, s)
		r.stroke(sh, s)
	case "ellipse":
		sh := r.toPx(ellipsePoints(x+w/2, y+h/2, math.Abs(w/2), math.Abs(h/2)), true)
		r.fill(sh, s)
		r.stroke(sh, s)
	case "diamond":
		sh := r.toPx([][2]float64{{x + w/2, y}, {x + w, y + h/2}, {x + w/2, y + h}, {x, y + h/2}}, true)
		r.fill(sh, s)
		r.stroke(sh, s)
	case "line":
		if pts := absPoints(e); len(pts) > 0 {
			r.stroke(r.toPx(pts, false), s)
		}
	case "freedraw":
		if pts := absPoints(e); len(pts) > 0 {
			r.stroke(r.toPx(smoothPoints(pts), false), s)
		}
	case "arrow":
		pts := absPoints(e)
		if len(pts) == 0 {
			return nil
		}
		tip, left, right := arrowHead(pts, s.width)
		r.stroke(r.toPx(pts, false), s)
		r.stroke(r.toPx([][2]float64{left, tip, right}, false), s)
	case "text":
		return r.text(e, s, line)
	}
	return nil
}

var fontData = map[string][]byte{
	"sans":             goregular.TTF,
	"sans-bold":        gobold.TTF,
	"sans-italic":      goitalic.TTF,
	"sans-bold-italic": gobolditalic.TTF,
	"mono":             gomono.TTF,
	"mono-bold":        gomonobold.TTF,
	"mono-italic":      gomonoitalic.TTF,
	"mono-bold-italic": gomonobolditalic.TTF,
}

var (
	parsedFonts   = map[string]*opentype.Font{}
	parsedFontsMu sync.Mutex
)

func parsedFont(key string) (*opentype.Font, error) {
	parsedFontsMu.Lock()
	defer parsedFontsMu.Unlock()
	if f, ok := parsedFonts[key]; ok {
		return f, nil
	}
	f, err := opentype.Parse(fontData[key])
	if err != nil {
		return nil, fmt.Errorf("font %s: %w", key, err)
	}
	parsedFonts[key] = f
	return f, nil
}

func (r *raster) face(e Element, px float64) (font.Face, error) {
	key := "sans"
	if fontFamily(e) == 3 {
		key = "mono"
	}
	if bold(e) {
		key += "-bold"
	}
	if italic(e) {
		key += "-italic"
	}
	cacheKey := key + "@" + strconv.FormatFloat(px, 'f', 2, 64)
	if f, ok := r.faces[cacheKey]; ok {
		return f, nil
	}
	parsed, err := parsedFont(key)
	if err != nil {
		return nil, err
	}
	f, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	r.faces[cacheKey] = f
	return f, nil
}

func (r *raster) text(e Element, s style, line bool) error {
	size := fontSize(e)
	px := size * r.scale
	if px < 1 {
		return nil
	}
	face, err := r.face(e, px)
	if err != nil {
		return err
	}
	fg := image.NewUniform(parseColor(s.stroke, s.opacity))
	halo := image.NewUniform(parseColor(paperColor, s.opacity))
	radius := 2.5 * r.scale // the editor's 5-unit paper stroke behind text on a line
	bounds := r.dst.Bounds()
	ax := textAnchorX(e)
	for i, l := range textLines(e) {
		if l == "" {
			continue
		}
		at := r.px([2]float64{ax, e.Y + size + float64(i)*size*1.25})
		// A line wholly above or below the image draws nothing.
		if at[1]+px/2+radius < 0 || at[1]-px-radius > float64(bounds.Dy()) {
			continue
		}
		adv := float64(font.MeasureString(face, l)) / 64
		switch e.TextAlign {
		case "center":
			at[0] -= adv / 2
		case "right":
			at[0] -= adv
		}
		// Only the glyphs that reach the image are drawn.
		x, from, to := at[0], -1, len(l)
		startX := x
		for k, ch := range l {
			a, _ := face.GlyphAdvance(ch)
			w := float64(a) / 64
			if from < 0 && x+w+px+radius >= 0 {
				from, startX = k, x
			}
			if x-px-radius > float64(bounds.Dx()) {
				to = k
				break
			}
			x += w
		}
		if from < 0 || from >= to {
			continue
		}
		visible := l[from:to]
		put := func(src image.Image, dx, dy float64) {
			d := font.Drawer{Dst: r.dst, Src: src, Face: face, Dot: fixed.Point26_6{X: fixed.Int26_6((startX + dx) * 64), Y: fixed.Int26_6((at[1] + dy) * 64)}}
			d.DrawString(visible)
		}
		if line {
			for k := range 8 {
				a := 2 * math.Pi * float64(k) / 8
				put(halo, radius*math.Cos(a), radius*math.Sin(a))
			}
		}
		put(fg, 0, 0)
	}
	return nil
}
