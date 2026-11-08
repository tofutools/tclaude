package drawingboard

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The CSS font stacks the editor draws with (DRAWING_BOARD_FONTS).
var svgFonts = map[int]string{
	1: "'Segoe Print','Comic Sans MS','Chalkboard SE',cursive",
	2: "'Helvetica Neue',Helvetica,Arial,sans-serif",
	3: "'Cascadia Code','SF Mono',Menlo,Consolas,monospace",
}

func num(v float64) string { return strconv.FormatFloat(round2(v), 'f', -1, 64) }

func attr(b *bytes.Buffer, name, value string) {
	b.WriteByte(' ')
	b.WriteString(name)
	b.WriteString(`="`)
	_ = xml.EscapeText(b, []byte(value))
	b.WriteByte('"')
}

func pathD(pts [][2]float64) string {
	var d strings.Builder
	for i, p := range pts {
		if i == 0 {
			d.WriteString("M")
		} else {
			d.WriteString(" L")
		}
		d.WriteString(num(p[0]) + "," + num(p[1]))
	}
	return d.String()
}

// smoothD is a freehand stroke's path (smoothPath in drawing-board-island.js).
func smoothD(pts [][2]float64) string {
	if len(pts) < 3 {
		return pathD(pts)
	}
	d := "M" + num(pts[0][0]) + "," + num(pts[0][1])
	for i := 1; i < len(pts)-1; i++ {
		x, y, nx, ny := pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1]
		d += " Q" + num(x) + "," + num(y) + " " + num((x+nx)/2) + "," + num((y+ny)/2)
	}
	last := pts[len(pts)-1]
	return d + " L" + num(last[0]) + "," + num(last[1])
}

func renderSVG(sel, drawn []Element, crop Box) []byte {
	all := byID(drawn)
	var b bytes.Buffer
	w, h := math.Ceil(crop.Width), math.Ceil(crop.Height)
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="%s %s %s %s" width="%s" height="%s">`,
		num(crop.X), num(crop.Y), num(crop.Width), num(crop.Height), num(w), num(h))
	fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" fill="#ffffff"/>`, num(crop.X), num(crop.Y), num(crop.Width), num(crop.Height))
	for _, e := range sel {
		svgElement(&b, e, onLine(e, all))
	}
	b.WriteString("</svg>\n")
	return b.Bytes()
}

func svgCommon(b *bytes.Buffer, s style, e Element) {
	attr(b, "stroke", s.stroke)
	attr(b, "stroke-width", num(s.width))
	svgCommonRest(b, s, e)
}

// svgCommonRest is the shared attributes but the stroke, which text sets
// itself.
func svgCommonRest(b *bytes.Buffer, s style, e Element) {
	if len(s.dash) == 2 {
		attr(b, "stroke-dasharray", num(s.dash[0])+" "+num(s.dash[1]))
	}
	attr(b, "stroke-linecap", "round")
	attr(b, "stroke-linejoin", "round")
	if s.opacity != 1 {
		attr(b, "opacity", num(s.opacity))
	}
	attr(b, "data-element", e.ID)
}

func svgFill(s style) string {
	if s.fill == "" {
		return "none"
	}
	return s.fill
}

func svgElement(b *bytes.Buffer, e Element, line bool) {
	s := styleOf(e)
	x, y, w, h := e.X, e.Y, e.Width, e.Height
	switch e.Type {
	case "rectangle":
		b.WriteString("<rect")
		svgCommon(b, s, e)
		attr(b, "x", num(x))
		attr(b, "y", num(y))
		attr(b, "width", num(w))
		attr(b, "height", num(h))
		attr(b, "rx", num(math.Max(0, math.Min(10, math.Min(w/4, h/4)))))
		attr(b, "fill", svgFill(s))
		b.WriteString("/>")
	case "ellipse":
		b.WriteString("<ellipse")
		svgCommon(b, s, e)
		attr(b, "cx", num(x+w/2))
		attr(b, "cy", num(y+h/2))
		attr(b, "rx", num(math.Abs(w/2)))
		attr(b, "ry", num(math.Abs(h/2)))
		attr(b, "fill", svgFill(s))
		b.WriteString("/>")
	case "diamond":
		b.WriteString("<polygon")
		svgCommon(b, s, e)
		attr(b, "points", fmt.Sprintf("%s,%s %s,%s %s,%s %s,%s", num(x+w/2), num(y), num(x+w), num(y+h/2), num(x+w/2), num(y+h), num(x), num(y+h/2)))
		attr(b, "fill", svgFill(s))
		b.WriteString("/>")
	case "line", "freedraw":
		pts := absPoints(e)
		if len(pts) == 0 {
			return
		}
		b.WriteString("<path")
		svgCommon(b, s, e)
		d := pathD(pts)
		if e.Type == "freedraw" {
			d = smoothD(pts)
		}
		attr(b, "d", d)
		attr(b, "fill", "none")
		b.WriteString("/>")
	case "arrow":
		pts := absPoints(e)
		if len(pts) == 0 {
			return
		}
		tip, left, right := arrowHead(pts, s.width)
		b.WriteString("<g")
		svgCommon(b, s, e)
		b.WriteString(">")
		b.WriteString(`<path`)
		attr(b, "d", pathD(pts))
		b.WriteString(` fill="none"/><path`)
		attr(b, "d", pathD([][2]float64{left, tip, right}))
		b.WriteString(` fill="none"/></g>`)
	case "text":
		size := fontSize(e)
		lx := textAnchorX(e)
		b.WriteString("<text")
		svgCommonRest(b, s, e)
		if line {
			// Text on a line masks the line behind it.
			b.WriteString(` stroke="` + paperColor + `" stroke-width="5" paint-order="stroke"`)
		} else {
			b.WriteString(` stroke="none"`)
		}
		attr(b, "fill", s.stroke)
		attr(b, "x", num(lx))
		attr(b, "y", num(y+size))
		attr(b, "font-size", num(size))
		switch e.TextAlign {
		case "center":
			attr(b, "text-anchor", "middle")
		case "right":
			attr(b, "text-anchor", "end")
		}
		attr(b, "font-family", svgFonts[fontFamily(e)])
		if bold(e) {
			attr(b, "font-weight", "bold")
		}
		if italic(e) {
			attr(b, "font-style", "italic")
		}
		b.WriteString(` xml:space="preserve">`)
		for i, l := range textLines(e) {
			if l == "" {
				l = " "
			}
			dy := 0.0
			if i > 0 {
				dy = size * 1.25
			}
			b.WriteString("<tspan")
			attr(b, "x", num(lx))
			attr(b, "dy", num(dy))
			b.WriteString(">")
			_ = xml.EscapeText(b, []byte(l))
			b.WriteString("</tspan>")
		}
		b.WriteString("</text>")
	}
}
