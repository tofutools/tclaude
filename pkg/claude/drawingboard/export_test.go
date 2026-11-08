package drawingboard

import (
	"bytes"
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
	img, err := Render(sample(t), Options{Format: FormatSVG})
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
	img, err := Render(sample(t), Options{})
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
	img, err := Render(wide, Options{Scale: 4})
	require.NoError(t, err)
	assert.LessOrEqual(t, img.Width, MaxSide)

	_, err = Render(wide, Options{Scale: 9})
	assert.Error(t, err)
	_, err = Render(wide, Options{Format: "gif"})
	assert.True(t, err != nil && strings.Contains(err.Error(), "format"))
}
