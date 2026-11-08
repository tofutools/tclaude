package drawingboard

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleBoard is two boxes joined by a labelled elbow arrow, an ellipse far
// to the right, a dashed line, a freehand stroke and free text.
const sampleBoard = `[
 {"id":"a","type":"rectangle","x":0,"y":0,"width":160,"height":80,"strokeColor":"#1e1e1e","backgroundColor":"#a5d8ff","strokeWidth":2},
 {"id":"a-label","type":"text","x":0,"y":0,"text":"Plan","fontSize":20,"fontFamily":2,"textAlign":"center","containerId":"a"},
 {"id":"b","type":"diamond","x":300,"y":150,"width":140,"height":100,"strokeColor":"#e03131","backgroundColor":"transparent"},
 {"id":"ab","type":"arrow","x":0,"y":0,"points":[[0,0],[10,10]],"routing":"elbow",
  "startBinding":{"elementId":"a","fixedPoint":[1,0.5]},"endBinding":{"elementId":"b","fixedPoint":[0.5,0]}},
 {"id":"ab-label","type":"text","x":0,"y":0,"text":"then","fontSize":16,"containerId":"ab"},
 {"id":"far","type":"ellipse","x":900,"y":0,"width":120,"height":60,"backgroundColor":"#b2f2bb","opacity":60},
 {"id":"dash","type":"line","x":0,"y":300,"points":[[0,0],[200,0],[200,60]],"strokeStyle":"dashed","strokeWidth":3},
 {"id":"pen","type":"freedraw","x":500,"y":320,"points":[[0,0],[20,10],[40,-5],[60,15],[80,0]],"strokeColor":"#2f9e44"},
 {"id":"note","type":"text","x":0,"y":420,"text":"Free <text> & more\nsecond line","fontSize":24,"fontWeight":"bold"},
 {"id":"gone","type":"rectangle","x":5000,"y":5000,"width":10,"height":10,"isDeleted":true}
]`

func sample(t *testing.T) []Element {
	t.Helper()
	var raws []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(sampleBoard), &raws))
	return DecodeElements(raws)
}

// keep writes an export under TCLAUDE_DRAWING_EXPORT_DIR, for a look by eye.
func keep(t *testing.T, name string, data []byte) {
	if dir := os.Getenv("TCLAUDE_DRAWING_EXPORT_DIR"); dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
}

func TestDrawnRoutesBoundLinesAndPlacesLabels(t *testing.T) {
	drawn := byID(Drawn(sample(t)))
	_, gone := drawn["gone"]
	assert.False(t, gone, "a tombstone is not drawn")

	ab := drawn["ab"]
	pts := absPoints(ab)
	require.GreaterOrEqual(t, len(pts), 3, "an elbow between a right side and a top side bends")
	assert.Equal(t, [2]float64{164, 40}, pts[0], "it leaves a's right snap point, a gap out")
	assert.Equal(t, [2]float64{370, 146}, pts[len(pts)-1], "it reaches b's top snap point, a gap out")

	label := drawn["a-label"]
	box := Bounds(label)
	assert.InDelta(t, 80, box.X+box.Width/2, 0.01, "a box's label is centred on it")
	assert.InDelta(t, 40, box.Y+box.Height/2, 0.01)
}

func TestSelectTakesLabelsAndLinesBetween(t *testing.T) {
	drawn := Drawn(sample(t))
	sel, box, err := Select(drawn, []string{"a", "b"}, nil)
	require.NoError(t, err)
	var ids []string
	for _, e := range sel {
		ids = append(ids, e.ID)
	}
	assert.Equal(t, []string{"a", "a-label", "b", "ab", "ab-label"}, ids, "the label, the arrow between and the arrow's label come along, in stacking order")
	assert.Equal(t, -Pad, box.X)
	assert.Equal(t, 440+Pad, box.X+box.Width)

	sel, _, err = Select(drawn, []string{"a"}, nil)
	require.NoError(t, err)
	assert.Len(t, sel, 2, "an arrow bound at one end only stays out")

	_, _, err = Select(drawn, []string{"a", "nope", "zzz"}, nil)
	var missing *MissingElementsError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, []string{"nope", "zzz"}, missing.IDs)

	sel, box, err = Select(drawn, nil, &Box{X: 850, Y: -50, Width: 300, Height: 200})
	require.NoError(t, err)
	require.Len(t, sel, 1)
	assert.Equal(t, "far", sel[0].ID)
	assert.Equal(t, Box{X: 850, Y: -50, Width: 300, Height: 200}, box, "a region crops to itself")

	_, _, err = Select(drawn, nil, &Box{X: -900, Y: -900, Width: 10, Height: 10})
	assert.ErrorIs(t, err, ErrNothingToExport)
	_, _, err = Select(nil, nil, nil)
	assert.ErrorIs(t, err, ErrNothingToExport)
}

func TestRenderSVGIsWellFormedAndEscapesText(t *testing.T) {
	img, err := Render(context.Background(), sample(t), Options{Format: FormatSVG})
	require.NoError(t, err)
	keep(t, "sample.svg", img.Data)
	assert.Equal(t, "image/svg+xml", img.ContentType)
	dec := xml.NewDecoder(bytes.NewReader(img.Data))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "the SVG must parse as XML")
	}
	svg := string(img.Data)
	assert.Contains(t, svg, "Free &lt;text&gt; &amp; more")
	assert.Contains(t, svg, `stroke-dasharray="8 6"`)
	assert.Contains(t, svg, `paint-order="stroke"`, "the arrow's label masks the arrow")
	assert.NotContains(t, svg, `data-element="gone"`)
	assert.NotContains(t, svg, "<script")
}

func TestRenderPNGDrawsTheBoard(t *testing.T) {
	img, err := Render(context.Background(), sample(t), Options{})
	require.NoError(t, err)
	keep(t, "sample.png", img.Data)
	assert.Equal(t, "image/png", img.ContentType)
	decoded, err := png.Decode(bytes.NewReader(img.Data))
	require.NoError(t, err)
	assert.Equal(t, img.Width, decoded.Bounds().Dx())
	assert.Equal(t, int((1020+2*Pad)*DefaultScale), img.Width)

	// The box's fill, well inside it, is its background colour.
	at := func(x, y float64) (uint32, uint32, uint32) {
		r, g, b, _ := decoded.At(int((x-img.Crop.X)*DefaultScale), int((y-img.Crop.Y)*DefaultScale)).RGBA()
		return r >> 8, g >> 8, b >> 8
	}
	r, g, b := at(20, 20)
	assert.Equal(t, []uint32{0xa5, 0xd8, 0xff}, []uint32{r, g, b})
	r, g, b = at(700, 60)
	assert.Equal(t, []uint32{0xff, 0xff, 0xff}, []uint32{r, g, b}, "empty paper is white")
	r, _, _ = at(300+70, 150) // b's top corner, on its red outline
	assert.Greater(t, r, uint32(0xc0))
}

func TestRenderPNGCapsTheSize(t *testing.T) {
	wide := []Element{{ID: "w", Type: "rectangle", X: 0, Y: 0, Width: 100000, Height: 10}}
	img, err := Render(context.Background(), wide, Options{Scale: 4})
	require.NoError(t, err)
	assert.LessOrEqual(t, img.Width, MaxSide)

	_, err = Render(context.Background(), wide, Options{Scale: 9})
	assert.Error(t, err)
	_, err = Render(context.Background(), wide, Options{Format: "gif"})
	assert.True(t, err != nil && strings.Contains(err.Error(), "format"))
}

// costly is boards the store accepts that are cheap to draw only when the
// renderer draws what reaches the image: dotted lines a million units long,
// a zigzag of 5000 points across them, and a long text in a big font on a
// line. Each must render in bounded time and memory, as a whole board and
// as a small region.
func TestRenderPNGStaysCheapOnCostlyBoards(t *testing.T) {
	var zigzag [][]float64
	for i := range 5000 {
		y := 1e6
		if i%2 == 0 {
			y = -1e6
		}
		zigzag = append(zigzag, []float64{float64(i), y})
	}
	long := strings.Repeat("costly text ", 330)
	boards := map[string][]Element{
		"dotted": {{ID: "d", Type: "line", Points: [][]float64{{-1e6, 0}, {1e6, 0}}, StrokeStyle: "dotted"}},
		"zigzag": {{ID: "z", Type: "line", Points: zigzag, StrokeStyle: "dashed"}},
		"text": {
			{ID: "l", Type: "arrow", Points: [][]float64{{0, 0}, {400, 0}}},
			{ID: "t", Type: "text", Text: long + "\n" + long, FontSize: 200, ContainerID: "l", Width: 10, Height: 10},
		},
	}
	for name, els := range boards {
		t.Run(name, func(t *testing.T) {
			img, err := Render(context.Background(), els, Options{Scale: MaxScale})
			require.NoError(t, err)
			assert.LessOrEqual(t, max(img.Width, img.Height), MaxSide)
			_, err = Render(context.Background(), els, Options{Scale: MaxScale, Region: &Box{X: 0, Y: -20, Width: 50, Height: 40}})
			require.NoError(t, err)
		})
	}
}

func TestRenderPNGStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Render(ctx, sample(t), Options{})
	assert.ErrorIs(t, err, context.Canceled)
}

// A translucent element composes once, as the editor's opacity does: where
// its strokes, its fill and its arrowhead overlap it is no darker than
// elsewhere (tk_403ecc01).
func TestRenderPNGAppliesOpacityPerElement(t *testing.T) {
	half := 50.0
	els := []Element{
		// A zigzag that crosses itself, thick, so its segments overlap.
		{ID: "z", Type: "line", Points: [][]float64{{0, 0}, {100, 100}, {100, 0}, {0, 100}}, StrokeWidth: 20, StrokeColor: "#000000", Opacity: &half},
		// A box whose stroke lies over its fill.
		{ID: "b", Type: "rectangle", X: 200, Y: 0, Width: 100, Height: 100, StrokeWidth: 10, StrokeColor: "#000000", BackgroundColor: "#000000", Opacity: &half},
		// An arrow, whose head is drawn over its shaft's end.
		{ID: "a", Type: "arrow", X: 400, Y: 50, Points: [][]float64{{0, 0}, {100, 0}}, StrokeWidth: 6, StrokeColor: "#000000", Opacity: &half},
	}
	img, err := Render(context.Background(), els, Options{Scale: 1})
	require.NoError(t, err)
	keep(t, "opacity.png", img.Data)
	decoded, err := png.Decode(bytes.NewReader(img.Data))
	require.NoError(t, err)
	grey := func(x, y float64) uint32 {
		r, _, _, _ := decoded.At(int(x-img.Crop.X), int(y-img.Crop.Y)).RGBA()
		return r >> 8
	}
	assert.InDelta(t, 0x80, grey(50, 50), 2, "where the zigzag crosses itself")
	assert.InDelta(t, 0x80, grey(25, 25), 2, "along one stroke")
	assert.InDelta(t, 0x80, grey(200, 50), 2, "where the box's stroke lies over its fill")
	assert.InDelta(t, 0x80, grey(250, 50), 2, "the fill alone")
	assert.InDelta(t, 0x80, grey(450, 50), 2, "the arrow's shaft")
	assert.InDelta(t, 0x80, grey(500, 50), 2, "the arrow's tip, under its head")
	assert.Equal(t, uint32(0xff), grey(150, 50), "the paper between them")
}

// A translucent element's layer covers all it draws: text at half opacity
// is its opaque self, half way to the paper, everywhere, even where the
// image cuts it off.
func TestRenderPNGTranslucentLayerCoversTheElement(t *testing.T) {
	half := 50.0
	text := Element{ID: "t", Type: "text", X: 0, Y: 0, Text: "Wide WAVY glyphs, gjpqy\nsecond line", FontSize: 40, FontStyle: "italic", StrokeColor: "#000000"}
	for name, region := range map[string]*Box{"whole": nil, "cut": {X: 30, Y: 10, Width: 120, Height: 40}} {
		t.Run(name, func(t *testing.T) {
			opaque, err := Render(context.Background(), []Element{text}, Options{Scale: 1, Region: region})
			require.NoError(t, err)
			faded := text
			faded.Opacity = &half
			translucent, err := Render(context.Background(), []Element{faded}, Options{Scale: 1, Region: region})
			require.NoError(t, err)
			a, err := png.Decode(bytes.NewReader(opaque.Data))
			require.NoError(t, err)
			b, err := png.Decode(bytes.NewReader(translucent.Data))
			require.NoError(t, err)
			require.Equal(t, a.Bounds(), b.Bounds())
			inked := 0
			for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
				for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
					full, _, _, _ := a.At(x, y).RGBA()
					got, _, _, _ := b.At(x, y).RGBA()
					want := 255 - (255-float64(full>>8))/2
					if full>>8 < 200 {
						inked++
					}
					require.InDelta(t, want, float64(got>>8), 2, "pixel %d,%d", x, y)
				}
			}
			assert.Positive(t, inked, "the text is drawn")
		})
	}
}

// A dashed line keeps its pattern where it leaves the image and comes back.
func TestDashesKeepTheirPhaseOffCanvas(t *testing.T) {
	els := []Element{{ID: "d", Type: "line", X: 0, Y: 0, Points: [][]float64{{0, 0}, {2000, 0}}, StrokeStyle: "dashed", StrokeWidth: 2}}
	whole, err := Render(context.Background(), els, Options{Scale: 1})
	require.NoError(t, err)
	part, err := Render(context.Background(), els, Options{Scale: 1, Region: &Box{X: 1000, Y: -10, Width: 100, Height: 20}})
	require.NoError(t, err)
	a, err := png.Decode(bytes.NewReader(whole.Data))
	require.NoError(t, err)
	b, err := png.Decode(bytes.NewReader(part.Data))
	require.NoError(t, err)
	for x := 0; x < 100; x++ {
		ra, _, _, _ := a.At(int(1000-whole.Crop.X)+x, int(-whole.Crop.Y)).RGBA()
		rb, _, _, _ := b.At(x, 10).RGBA()
		assert.InDelta(t, ra>>8, rb>>8, 40, "pixel %d along the line", x)
	}
}
