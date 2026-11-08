package drawingboard

import (
	"bytes"
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
	dst   *image.RGBA
	crop  Box
	scale float64
	faces map[string]font.Face
}

func renderPNG(sel, drawn []Element, crop Box, scale float64) ([]byte, int, int, error) {
	w := max(1, int(math.Round(crop.Width*scale)))
	h := max(1, int(math.Round(crop.Height*scale)))
	r := &raster{dst: image.NewRGBA(image.Rect(0, 0, w, h)), crop: crop, scale: scale, faces: map[string]font.Face{}}
	draw.Draw(r.dst, r.dst.Bounds(), image.White, image.Point{}, draw.Src)
	all := byID(drawn)
	for _, e := range sel {
		if err := r.element(e, onLine(e, all)); err != nil {
			return nil, 0, 0, err
		}
	}
	for _, f := range r.faces {
		_ = f.Close()
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

// paint fills polygons with c, rasterised over the pixel box they cover.
func (r *raster) paint(polys [][][2]float64, c color.NRGBA) {
	if c.A == 0 || len(polys) == 0 {
		return
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, poly := range polys {
		for _, p := range poly {
			minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
			minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
		}
	}
	bounds := r.dst.Bounds()
	box := image.Rect(int(math.Floor(minX))-1, int(math.Floor(minY))-1, int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1).Intersect(bounds)
	if box.Empty() {
		return
	}
	z := vector.NewRasterizer(box.Dx(), box.Dy())
	ox, oy := float32(box.Min.X), float32(box.Min.Y)
	for _, poly := range polys {
		if len(poly) < 3 {
			continue
		}
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
	return out
}

// strokePolys outlines a polyline of the given pixel width.
func strokePolys(pts [][2]float64, width float64) [][][2]float64 {
	hw := width / 2
	var out [][][2]float64
	for i := range pts {
		out = append(out, clockwise(disc(pts[i], hw)))
		if i == 0 {
			continue
		}
		a, b := pts[i-1], pts[i]
		dx, dy := b[0]-a[0], b[1]-a[1]
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		nx, ny := -dy/l*hw, dx/l*hw
		out = append(out, clockwise([][2]float64{{a[0] + nx, a[1] + ny}, {b[0] + nx, b[1] + ny}, {b[0] - nx, b[1] - ny}, {a[0] - nx, a[1] - ny}}))
	}
	return out
}

// dashes splits a polyline into its dashes (on, off lengths in pixels).
func dashes(pts [][2]float64, on, off float64) [][][2]float64 {
	var out [][][2]float64
	var cur [][2]float64
	drawing, left := true, on
	cur = append(cur, pts[0])
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		l := math.Hypot(b[0]-a[0], b[1]-a[1])
		at := 0.0
		for l-at > left {
			at += left
			t := at / l
			p := [2]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t}
			if drawing {
				piece := make([][2]float64, 0, len(cur)+1)
				out = append(out, append(append(piece, cur...), p))
				cur = nil
				left = off
			} else {
				cur = [][2]float64{p}
				left = on
			}
			drawing = !drawing
		}
		left -= l - at
		if drawing {
			cur = append(cur, b)
		}
	}
	if drawing && len(cur) > 1 {
		out = append(out, cur)
	}
	return out
}

func (r *raster) stroke(sh shape, s style) {
	pts := sh.pts
	if sh.closed && len(pts) > 0 {
		pts = append(append([][2]float64{}, pts...), pts[0])
	}
	if len(pts) == 0 {
		return
	}
	width := math.Max(s.width*r.scale, 0.5)
	pieces := [][][2]float64{pts}
	if len(s.dash) == 2 {
		pieces = dashes(pts, s.dash[0]*r.scale, s.dash[1]*r.scale)
	}
	var polys [][][2]float64
	for _, p := range pieces {
		polys = append(polys, strokePolys(p, width)...)
	}
	r.paint(polys, parseColor(s.stroke, s.opacity))
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
	ax := textAnchorX(e)
	for i, l := range textLines(e) {
		if l == "" {
			continue
		}
		at := r.px([2]float64{ax, e.Y + size + float64(i)*size*1.25})
		adv := float64(font.MeasureString(face, l)) / 64
		switch e.TextAlign {
		case "center":
			at[0] -= adv / 2
		case "right":
			at[0] -= adv
		}
		put := func(src image.Image, dx, dy float64) {
			d := font.Drawer{Dst: r.dst, Src: src, Face: face, Dot: fixed.Point26_6{X: fixed.Int26_6((at[0] + dx) * 64), Y: fixed.Int26_6((at[1] + dy) * 64)}}
			d.DrawString(l)
		}
		if line {
			// The editor's 5-unit paper stroke behind text on a line.
			radius := 2.5 * r.scale
			for k := range 12 {
				a := 2 * math.Pi * float64(k) / 12
				put(halo, radius*math.Cos(a), radius*math.Sin(a))
			}
		}
		put(fg, 0, 0)
	}
	return nil
}
