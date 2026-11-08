package drawingboard

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Export renders a drawing board, or part of it, to an image without a
// browser: SVG markup that mirrors the dashboard editor's own drawing
// (DrawingBoardElement in drawing-board-island.js), or a PNG rasterised from
// the same geometry. It is fresh by construction — the elements as stored
// now — so an agent or a peer gets the board as it is, open dashboard or not.
//
// Unlike the editor, the raster draws in the Go fonts (Go Regular for the
// hand and sans faces, Go Mono for mono), so text is metrically close, not
// identical.

const (
	FormatPNG = "png"
	FormatSVG = "svg"

	// Pad is the margin around an export of whole elements, as the
	// dashboard's snapshot crops (svgToPng).
	Pad = 24.0
	// MaxSide caps a PNG's longest side in pixels; the scale shrinks to fit.
	MaxSide = 4096
	// DefaultScale is a PNG's pixels per drawing-board unit.
	DefaultScale = 2.0
	MaxScale     = 4.0

	paperColor = "#fbfaf7" // the editor's paper, under text on a line
)

// ErrNothingToExport is an export that selects nothing: an empty board, or a
// region with nothing in it.
var ErrNothingToExport = errors.New("nothing to export")

// Options choose what an export draws and how.
type Options struct {
	Format string // FormatPNG (default) or FormatSVG
	// Elements limits the export to these ids, with the text attached to
	// them and the lines bound between them. Empty means the whole board.
	Elements []string
	// Region limits the export to the elements that meet this box, cropped
	// to it. It applies when Elements is empty.
	Region *Box
	Scale  float64 // PNG pixels per unit; 0 is DefaultScale
}

// MissingElementsError names requested ids the board does not have.
type MissingElementsError struct{ IDs []string }

func (e *MissingElementsError) Error() string {
	return "no such element on the drawing board: " + strings.Join(e.IDs, ", ")
}

// Image is a rendered export.
type Image struct {
	Data        []byte
	ContentType string
	Width       int // pixels (PNG) or user units (SVG)
	Height      int
	Crop        Box      // the part of the board it shows
	Elements    []string // the ids drawn, bottom first
}

// Render draws the selected part of a board. elements are the stored
// elements in their stored order, tombstones included or not.
func Render(elements []Element, opts Options) (*Image, error) {
	format := strings.ToLower(strings.TrimSpace(opts.Format))
	if format == "" {
		format = FormatPNG
	}
	if format != FormatPNG && format != FormatSVG {
		return nil, fmt.Errorf("unknown format %q (want png or svg)", opts.Format)
	}
	scale := opts.Scale
	if scale == 0 {
		scale = DefaultScale
	}
	if scale < 0 || scale > MaxScale || math.IsNaN(scale) {
		return nil, fmt.Errorf("scale must be above 0 and at most %g", float64(MaxScale))
	}
	drawn := Drawn(elements)
	sel, crop, err := Select(drawn, opts.Elements, opts.Region)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(sel))
	for _, e := range sel {
		ids = append(ids, e.ID)
	}
	img := &Image{Crop: crop, Elements: ids}
	if format == FormatSVG {
		img.Data = renderSVG(sel, drawn, crop)
		img.ContentType = "image/svg+xml"
		img.Width, img.Height = int(math.Ceil(crop.Width)), int(math.Ceil(crop.Height))
		return img, nil
	}
	scale = math.Min(scale, MaxSide/math.Max(crop.Width, crop.Height))
	data, w, h, err := renderPNG(sel, drawn, crop, scale)
	if err != nil {
		return nil, err
	}
	img.Data, img.ContentType, img.Width, img.Height = data, "image/png", w, h
	return img, nil
}

// Select picks what an export draws from the drawn elements (bottom first)
// and the box it shows.
//   - ids: those elements, the text attached to any of them, and the lines
//     bound at both ends inside them; the box is theirs, padded.
//   - region: the elements that meet it; the box is the region.
//   - neither: everything, padded.
func Select(drawn []Element, ids []string, region *Box) ([]Element, Box, error) {
	var sel []Element
	switch {
	case len(ids) > 0:
		want := map[string]bool{}
		for _, id := range ids {
			if id = strings.TrimSpace(id); id != "" {
				want[id] = true
			}
		}
		have := map[string]bool{}
		for _, e := range drawn {
			have[e.ID] = true
		}
		var missing []string
		for id := range want {
			if !have[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, Box{}, &MissingElementsError{IDs: missing}
		}
		picked := map[string]bool{}
		for id := range want {
			picked[id] = true
		}
		for _, e := range drawn {
			bound := func(b *Binding) bool { return b != nil && want[b.ElementID] }
			if e.ContainerID != "" && want[e.ContainerID] {
				picked[e.ID] = true
			}
			if (e.Type == "arrow" || e.Type == "line") && bound(e.StartBinding) && bound(e.EndBinding) {
				picked[e.ID] = true
			}
		}
		// Text on a line that came along comes along too.
		for _, e := range drawn {
			if e.ContainerID != "" && picked[e.ContainerID] {
				picked[e.ID] = true
			}
		}
		for _, e := range drawn {
			if picked[e.ID] {
				sel = append(sel, e)
			}
		}
	case region != nil:
		if !(region.Width > 0 && region.Height > 0) {
			return nil, Box{}, errors.New("a region needs a positive width and height")
		}
		for _, e := range drawn {
			if Bounds(e).intersects(*region) {
				sel = append(sel, e)
			}
		}
		if len(sel) == 0 {
			return nil, Box{}, ErrNothingToExport
		}
		return sel, *region, nil
	default:
		sel = drawn
	}
	if len(sel) == 0 {
		return nil, Box{}, ErrNothingToExport
	}
	box := Bounds(sel[0])
	for _, e := range sel[1:] {
		box = union(box, Bounds(e))
	}
	return sel, Box{X: box.X - Pad, Y: box.Y - Pad, Width: math.Max(box.Width, 1) + 2*Pad, Height: math.Max(box.Height, 1) + 2*Pad}, nil
}

// onLine says a text element sits on a line (its container has points), where
// the editor masks the line behind it.
func onLine(e Element, drawn map[string]Element) bool {
	c, ok := drawn[e.ContainerID]
	return ok && len(c.Points) > 0
}

func byID(drawn []Element) map[string]Element {
	m := make(map[string]Element, len(drawn))
	for _, e := range drawn {
		m[e.ID] = e
	}
	return m
}

// style is what both renderers read from an element, with the editor's
// defaults.
type style struct {
	stroke, fill string // colours; fill "" is none
	width        float64
	opacity      float64
	dash         []float64
}

func styleOf(e Element) style {
	s := style{stroke: safeColor(e.StrokeColor, "#1e1e1e"), width: e.StrokeWidth, opacity: 1}
	if s.width == 0 {
		s.width = 2
	}
	if bg := safeColor(e.BackgroundColor, "transparent"); bg != "transparent" {
		s.fill = bg
	}
	if e.Opacity != nil {
		s.opacity = math.Max(0, math.Min(100, *e.Opacity)) / 100
	}
	switch e.StrokeStyle {
	case "dashed":
		s.dash = []float64{8, 6}
	case "dotted":
		s.dash = []float64{2, 5}
	}
	return s
}

// safeColor passes only plain colour values, as the editor does.
func safeColor(v, fallback string) string {
	if v == "transparent" {
		return v
	}
	if n := len(v); n >= 4 && n <= 9 && v[0] == '#' {
		for _, c := range v[1:] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return fallback
			}
		}
		return v
	}
	return fallback
}

// fontFamily is Excalidraw's number: 1 hand, 2 sans, 3 mono; anything else
// is the hand.
func fontFamily(e Element) int {
	if f, ok := e.FontFamily.(float64); ok && (f == 2 || f == 3) {
		return int(f)
	}
	return 1
}

func bold(e Element) bool   { return e.FontWeight == "bold" }
func italic(e Element) bool { return e.FontStyle == "italic" }

// textAnchorX is where a text line is anchored, by its alignment.
func textAnchorX(e Element) float64 {
	b := Bounds(e)
	switch e.TextAlign {
	case "center":
		return e.X + b.Width/2
	case "right":
		return e.X + b.Width
	}
	return e.X
}

// arrowHead is the two barbs' far ends of an arrow drawn along pts.
func arrowHead(pts [][2]float64, width float64) (tip, left, right [2]float64) {
	n := len(pts)
	tip = pts[n-1]
	from := pts[0]
	if n >= 2 {
		from = pts[n-2]
	}
	angle := math.Atan2(tip[1]-from[1], tip[0]-from[0])
	size := 10 + width*1.5
	left = [2]float64{tip[0] - size*math.Cos(angle-0.45), tip[1] - size*math.Sin(angle-0.45)}
	right = [2]float64{tip[0] - size*math.Cos(angle+0.45), tip[1] - size*math.Sin(angle+0.45)}
	return tip, left, right
}

// absPoints is a linear element's points in board units.
func absPoints(e Element) [][2]float64 {
	out := make([][2]float64, 0, len(e.Points))
	for _, p := range e.Points {
		if len(p) == 2 {
			out = append(out, [2]float64{e.X + p[0], e.Y + p[1]})
		}
	}
	return out
}
