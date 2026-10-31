package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const boardFixture = `{"id":"plan","title":"Plan","version":4,"updated_by":"operator","participants":["agt_1"],"elements":[
 {"id":"a","type":"rectangle","x":0,"y":0,"width":100,"height":50,"version":2,"versionNonce":9,"text":"API","server_version":1},
 {"id":"b","type":"rectangle","x":300,"y":0,"width":100,"height":50,"version":1,"versionNonce":3,"server_version":2},
 {"id":"e","type":"arrow","x":100,"y":25,"width":200,"height":0,"version":1,"versionNonce":3,"points":[[0,0],[200,0]],
  "startBinding":{"elementId":"a"},"endBinding":{"elementId":"b"},"customData":{"author":"agt_1"},"server_version":4}]}`

// fakeBoardDaemon serves the fixture on GET and records each PATCH body.
func fakeBoardDaemon(t *testing.T) *[]map[string]any {
	t.Helper()
	return fakeBoardDaemonWith(t, boardFixture)
}

func fakeBoardDaemonWith(t *testing.T, fixture string) *[]map[string]any {
	t.Helper()
	var patched []map[string]any
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		switch method {
		case http.MethodGet:
			return json.Unmarshal([]byte(fixture), out)
		case http.MethodPatch:
			assert.Equal(t, "/v1/boards/plan", path)
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			var body struct {
				Elements []map[string]any `json:"elements"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			patched = append(patched, body.Elements...)
			return json.Unmarshal([]byte(`{"version":5,"accepted":1}`), out)
		}
		t.Fatalf("unexpected %s %s", method, path)
		return nil
	}
	return &patched
}

func TestBoardOpenAndCloseUseTheCallersSessionRoute(t *testing.T) {
	var calls []string
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(method, path string, _, out any, _ DaemonOpts) error {
		calls = append(calls, method+" "+path)
		if method == http.MethodPost {
			return json.Unmarshal([]byte(`{"id":"plan","title":"Plan","version":4}`), out)
		}
		return nil
	}
	var out, errb bytes.Buffer
	require.Equal(t, rcOK, runBoardOpen("plan", &out, &errb), errb.String())
	assert.Contains(t, out.String(), `Board session open on plan "Plan", at version 4`)
	assert.Contains(t, out.String(), "open dashboard", "it says what still needs a dashboard")

	out.Reset()
	require.Equal(t, rcOK, runBoardClose("plan", &out, &errb), errb.String())
	assert.Contains(t, out.String(), "Board session on plan closed.")
	assert.Equal(t, []string{"POST /v1/boards/plan/session", "DELETE /v1/boards/plan/session"}, calls)
}

func TestBoardShowPrintsOneLinePerElement(t *testing.T) {
	fakeBoardDaemon(t)
	var out, errb bytes.Buffer
	require.Equal(t, rcOK, runBoardShow(&boardShowParams{}, "plan", &out, &errb), errb.String())
	s := out.String()
	assert.Contains(t, s, `Board plan "Plan": version 4`)
	assert.Contains(t, s, `rectangle a  x=0 y=0 w=100 h=50  "API"`)
	assert.Contains(t, s, `arrow     e  x=100 y=25 w=200 h=0  a -> b  by agt_1`)
}

func TestBoardAddArrowConnectsElementEdges(t *testing.T) {
	patched := fakeBoardDaemon(t)
	var out, errb bytes.Buffer
	rc := runBoardAdd(&boardAddParams{From: "a", To: "b", ID: "arr"}, "plan", "arrow", &out, &errb)
	require.Equal(t, rcOK, rc, errb.String())
	require.Len(t, *patched, 1)
	e := (*patched)[0]
	assert.Equal(t, "arr", e["id"])
	assert.EqualValues(t, 1, e["version"])
	// Centers (50,25) and (350,25), clipped to the boxes' edges.
	assert.EqualValues(t, 100, e["x"])
	assert.EqualValues(t, 25, e["y"])
	assert.Equal(t, []any{[]any{0.0, 0.0}, []any{200.0, 0.0}}, e["points"])
	assert.Equal(t, map[string]any{"elementId": "a"}, e["startBinding"])
	assert.Equal(t, map[string]any{"elementId": "b"}, e["endBinding"])
}

func TestBoardRmTakesAttachedTextAndFreesLines(t *testing.T) {
	patched := fakeBoardDaemonWith(t, `{"id":"plan","version":4,"elements":[
	 {"id":"a","type":"rectangle","x":0,"y":0,"width":100,"height":50,"version":2},
	 {"id":"a-note","type":"text","x":900,"y":0,"width":40,"height":20,"text":"t","containerId":"a","version":1},
	 {"id":"b","type":"rectangle","x":300,"y":0,"width":100,"height":50,"version":1},
	 {"id":"e","type":"arrow","x":104,"y":25,"width":192,"height":0,"version":1,"points":[[0,0],[192,0]],"server_version":4,
	  "startBinding":{"elementId":"a","fixedPoint":[1,0.5]},"endBinding":{"elementId":"b","fixedPoint":[0,0.5]}}]}`)
	var out, errb bytes.Buffer
	require.Equal(t, rcOK, runBoardRm("plan", []string{"a"}, &out, &errb), errb.String())
	byID := patchedByID(*patched)
	require.Len(t, byID, 3)
	assert.Equal(t, true, byID["a"]["isDeleted"])
	assert.EqualValues(t, 3, byID["a"]["version"])
	assert.NotContains(t, byID["a"], "server_version")
	assert.Equal(t, true, byID["a-note"]["isDeleted"], "attached text goes with its element")
	line := byID["e"]
	assert.NotContains(t, line, "isDeleted", "fixture lines carry no flag")
	assert.NotContains(t, line, "startBinding", "that end is free now")
	assert.Contains(t, line, "endBinding")
	assert.EqualValues(t, 104, line["x"], "the line stays where it was")

	before := len(*patched)
	assert.Equal(t, rcNotFound, runBoardRm("plan", []string{"missing"}, &out, &errb))
	assert.Len(t, *patched, before)
}

func TestBoardSnapshotAge(t *testing.T) {
	assert.Contains(t, snapshotAge("4", "4"), "current board version")
	assert.Contains(t, snapshotAge("2", "5"), "3 version(s) newer")
	assert.Contains(t, snapshotAge("", "5"), "unknown")
}

func TestBoardAddShapeLabelIsACentredTextElement(t *testing.T) {
	patched := fakeBoardDaemon(t)
	x, y := 100.0, 80.0
	var out, errb bytes.Buffer
	rc := runBoardAdd(&boardAddParams{X: &x, Y: &y, Text: "API", ID: "box"}, "plan", "rectangle", &out, &errb)
	require.Equal(t, rcOK, rc, errb.String())
	require.Len(t, *patched, 2)
	assert.NotContains(t, (*patched)[0], "text", "the shape itself carries no text")
	label := (*patched)[1]
	assert.Equal(t, "box-label", label["id"])
	assert.Equal(t, "text", label["type"])
	assert.Equal(t, "API", label["text"])
	// Centred on the 160x80 shape: center (180,120), label box 36x25.
	assert.EqualValues(t, 162, label["x"])
	assert.EqualValues(t, 107.5, label["y"])
}

func patchedByID(patched []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range patched {
		out[e["id"].(string)] = e
	}
	return out
}

func TestBoardAddRightOfWithLinkPlacesAndConnects(t *testing.T) {
	patched := fakeBoardDaemon(t)
	var out, errb bytes.Buffer
	rc := runBoardAdd(&boardAddParams{RightOf: "b", Link: true, ID: "c", Text: "DB"}, "plan", "rectangle", &out, &errb)
	require.Equal(t, rcOK, rc, errb.String())
	byID := patchedByID(*patched)
	require.Len(t, byID, 3, "the shape, its label and the link arrow")
	// b spans x 300..400, y 0..50: c goes 60 to its right, centred on it.
	assert.EqualValues(t, 460, byID["c"]["x"])
	assert.EqualValues(t, -15, byID["c"]["y"])
	var arrow map[string]any
	for id, e := range byID {
		if e["type"] == "arrow" {
			arrow = byID[id]
		}
	}
	require.NotNil(t, arrow)
	assert.Equal(t, map[string]any{"elementId": "b"}, arrow["startBinding"])
	assert.Equal(t, map[string]any{"elementId": "c"}, arrow["endBinding"])
	assert.EqualValues(t, 400, arrow["x"], "the arrow leaves b's right edge")
	assert.Contains(t, out.String(), "and arrow")

	rc = runBoardAdd(&boardAddParams{RightOf: "b", X: new(float64)}, "plan", "rectangle", &out, &errb)
	assert.Equal(t, rcInvalidArg, rc, "--x and --right-of are exclusive")
	rc = runBoardAdd(&boardAddParams{Link: true}, "plan", "rectangle", &out, &errb)
	assert.Equal(t, rcInvalidArg, rc, "--link needs a relative placement")
}

func TestBoardMoveCarriesBoundArrows(t *testing.T) {
	patched := fakeBoardDaemon(t)
	var out, errb bytes.Buffer
	y := 200.0
	require.Equal(t, rcOK, runBoardMove(&boardMoveParams{Y: &y}, "plan", "a", &out, &errb), errb.String())
	byID := patchedByID(*patched)
	require.Contains(t, byID, "a")
	require.Contains(t, byID, "e", "the arrow bound to a is re-run")
	assert.EqualValues(t, 200, byID["a"]["y"])
	assert.EqualValues(t, 3, byID["a"]["version"])
	pts := byID["e"]["points"].([]any)
	assert.Less(t, pts[1].([]any)[1].(float64), 0.0, "the arrow now climbs from a up to b")
}

func TestBoardLayoutStacksAFlowDownwards(t *testing.T) {
	patched := fakeBoardDaemon(t)
	var out, errb bytes.Buffer
	require.Equal(t, rcOK, runBoardLayout(&boardLayoutParams{Direction: "down"}, "plan", &out, &errb), errb.String())
	byID := patchedByID(*patched)
	// a points at b: b goes one step below a (50 tall + 1.5 × 60 gap).
	require.Contains(t, byID, "b")
	assert.EqualValues(t, 0, byID["b"]["x"])
	assert.EqualValues(t, 140, byID["b"]["y"])
	assert.NotContains(t, byID, "a", "a is already where the layout puts it")
	require.Contains(t, byID, "e")
	assert.Equal(t, rcInvalidArg, runBoardLayout(&boardLayoutParams{Direction: "sideways"}, "plan", &out, &errb))
}

func sceneOf(t *testing.T, elements string) *boardScene {
	t.Helper()
	var doc boardDoc
	require.NoError(t, json.Unmarshal([]byte(`{"id":"p","elements":[`+elements+`]}`), &doc))
	return loadBoardScene(&doc)
}

func TestBoardLabelsBelongToTheInnermostShape(t *testing.T) {
	s := sceneOf(t, `
	 {"id":"box","type":"rectangle","x":0,"y":0,"width":500,"height":300,"version":1},
	 {"id":"a","type":"rectangle","x":20,"y":20,"width":100,"height":50,"version":1},
	 {"id":"t","type":"text","x":40,"y":35,"width":40,"height":20,"text":"A","version":1},
	 {"id":"arr","type":"arrow","x":120,"y":45,"width":100,"height":0,"points":[[0,0],[100,0]],"startBinding":{"elementId":"a"},"version":1}`)
	for i := 0; i < 20; i++ {
		assert.Equal(t, "a", s.labelOwners()["t"], "the label belongs to a, not the group box")
	}
	changed := s.moveBy(map[string][2]float64{"box": {10, 0}})
	assert.Equal(t, []string{"box"}, changed, "moving the group box leaves the inner shape's label alone")
	changed = s.moveBy(map[string][2]float64{"a": {0, 100}})
	assert.ElementsMatch(t, []string{"a", "t", "arr"}, changed, "the half-bound arrow follows a")
	tipX, tipY := arrowEnd(s.view["arr"])
	assert.InDelta(t, 220, tipX, 0.01, "its free tip stays put")
	assert.InDelta(t, 45, tipY, 0.01)
}

func TestBoardLayoutBreaksACycleOnTheCycle(t *testing.T) {
	// Board order c, x, y with x->y, y->x, y->c: c is downstream, not on the cycle.
	s := sceneOf(t, `
	 {"id":"c","type":"rectangle","x":0,"y":0,"width":100,"height":50,"version":1},
	 {"id":"x","type":"rectangle","x":0,"y":100,"width":100,"height":50,"version":1},
	 {"id":"y","type":"rectangle","x":0,"y":200,"width":100,"height":50,"version":1},
	 {"id":"e1","type":"arrow","x":0,"y":0,"points":[[0,0],[1,1]],"startBinding":{"elementId":"x"},"endBinding":{"elementId":"y"},"version":1},
	 {"id":"e2","type":"arrow","x":0,"y":0,"points":[[0,0],[1,1]],"startBinding":{"elementId":"y"},"endBinding":{"elementId":"x"},"version":1},
	 {"id":"e3","type":"arrow","x":0,"y":0,"points":[[0,0],[1,1]],"startBinding":{"elementId":"y"},"endBinding":{"elementId":"c"},"version":1}`)
	pos := layoutPositions(s, "right", 60)
	assert.Less(t, pos["x"][0], pos["y"][0])
	assert.Less(t, pos["y"][0], pos["c"][0], "c comes after y, which points at it")
}

func TestBoardViewOverlap(t *testing.T) {
	view := boardBox{X: 0, Y: 0, W: 100, H: 100}
	assert.True(t, view.overlaps(elementBox(boardElementView{X: 90, Y: 90, Width: 50, Height: 50})), "partly in view")
	assert.False(t, view.overlaps(elementBox(boardElementView{X: 150, Y: 0, Width: 10, Height: 10})))
	// An arrow is placed by its points, not its (often zero) box.
	arrow := boardElementView{X: 200, Y: 50, Points: [][]float64{{0, 0}, {-150, 0}}}
	assert.Equal(t, boardBox{X: 50, Y: 50, W: 150, H: 0}, elementBox(arrow))
	assert.True(t, view.overlaps(elementBox(arrow)))
}

// The board draws text elements only, so a shape's text goes on its label.
func TestBoardTextWritesTheLabelTheBoardDraws(t *testing.T) {
	patched := fakeBoardDaemonWith(t, `{"id":"plan","version":4,"elements":[
	 {"id":"a","type":"rectangle","x":0,"y":0,"width":100,"height":50,"version":2},
	 {"id":"a-label","type":"text","x":10,"y":10,"width":40,"height":20,"text":"old","version":5},
	 {"id":"b","type":"rectangle","x":300,"y":0,"width":100,"height":50,"version":1},
	 {"id":"inner","type":"text","x":320,"y":10,"width":40,"height":20,"text":"old","version":1},
	 {"id":"c","type":"rectangle","x":600,"y":0,"width":100,"height":50,"version":1}]}`)
	var out, errb bytes.Buffer
	text := &boardTextParams{Text: "new"}

	require.Equal(t, rcOK, runBoardText(text, "plan", "a", &out, &errb), errb.String())
	byID := patchedByID(*patched)
	assert.NotContains(t, byID, "a", "the shape itself draws no text")
	assert.Equal(t, "new", byID["a-label"]["text"])
	assert.EqualValues(t, 6, byID["a-label"]["version"])

	require.Equal(t, rcOK, runBoardText(text, "plan", "b", &out, &errb), errb.String())
	assert.Equal(t, "new", patchedByID(*patched)["inner"]["text"], "text inside a shape is its label")

	require.Equal(t, rcOK, runBoardText(text, "plan", "a-label", &out, &errb), errb.String())

	before := len(*patched)
	assert.Equal(t, rcNotFound, runBoardText(text, "plan", "nope", &out, &errb))
	assert.Len(t, *patched, before, "a failed text change writes nothing")

	// With no text yet it attaches some, centred on the element.
	require.Equal(t, rcOK, runBoardText(text, "plan", "c", &out, &errb), errb.String())
	added := patchedByID(*patched)["c-label"]
	assert.Equal(t, "c", added["containerId"])
	assert.Equal(t, "new", added["text"])
	assert.EqualValues(t, 1, added["version"])
	assert.InDelta(t, 650, added["x"].(float64)+added["width"].(float64)/2, 0.01)
}

func TestBoardTextReachesLinesAndOutranksAnOldTombstone(t *testing.T) {
	patched := fakeBoardDaemonWith(t, `{"id":"plan","version":4,"elements":[
	 {"id":"l","type":"line","x":0,"y":0,"width":100,"height":100,"version":1,"points":[[0,0],[100,0],[100,100]]},
	 {"id":"l-label","type":"text","version":7,"isDeleted":true}]}`)
	var out, errb bytes.Buffer
	require.Equal(t, rcOK, runBoardText(&boardTextParams{Text: "hop"}, "plan", "l", &out, &errb), errb.String())
	label := patchedByID(*patched)["l-label"]
	assert.EqualValues(t, 8, label["version"], "above the tombstone, or the merge drops it")
	assert.Equal(t, false, label["isDeleted"])
	assert.InDelta(t, 100, label["x"].(float64)+label["width"].(float64)/2, 0.01, "halfway along the path")
	assert.InDelta(t, 0, label["y"].(float64)+label["height"].(float64)/2, 0.01)
}

// The dashboard's board-model.js is held to the same cases in board.test.mjs.
func TestBoardRoutesAgreeWithTheDashboard(t *testing.T) {
	data, err := os.ReadFile("testdata/board_routes.json")
	require.NoError(t, err)
	type end struct {
		X   float64    `json:"x"`
		Y   float64    `json:"y"`
		Dir [2]float64 `json:"dir"`
	}
	var cases []struct {
		Routing string       `json:"routing"`
		Start   end          `json:"start"`
		End     end          `json:"end"`
		Path    [][2]float64 `json:"path"`
	}
	require.NoError(t, json.Unmarshal(data, &cases))
	require.GreaterOrEqual(t, len(cases), 30)
	for _, c := range cases {
		got := routePath(c.Routing, boardEnd(c.Start), boardEnd(c.End))
		for i := range got {
			got[i] = [2]float64{boardRound(got[i][0]), boardRound(got[i][1])}
		}
		assert.Equal(t, c.Path, got, "%s %+v -> %+v", c.Routing, c.Start, c.End)
	}
}

const boardWiredFixture = `{"id":"plan","version":4,"elements":[
 {"id":"a","type":"rectangle","x":0,"y":0,"width":100,"height":60,"version":2},
 {"id":"b","type":"rectangle","x":300,"y":200,"width":100,"height":60,"version":1},
 {"id":"e","type":"arrow","x":104,"y":30,"width":192,"height":200,"version":3,"points":[[0,0],[96,0],[96,200],[192,200]],
  "routing":"elbow","startBinding":{"elementId":"a","fixedPoint":[1,0.5]},"endBinding":{"elementId":"b","fixedPoint":[0,0.5]}},
 {"id":"e-label","type":"text","x":180,"y":118,"width":40,"height":25,"text":"go","containerId":"e","version":1},
 {"id":"s","type":"freedraw","x":50,"y":-4,"width":9,"height":5,"version":1,"points":[[0,0],[5,5],[9,0]],
  "startBinding":{"elementId":"a","fixedPoint":[0.5,0]}}]}`

func TestBoardAddLineAttachesAtSnapPointsWithRouting(t *testing.T) {
	patched := fakeBoardDaemonWith(t, boardWiredFixture)
	var out, errb bytes.Buffer
	rc := runBoardAdd(&boardAddParams{From: "a", To: "b", FromSide: "bottom", ToSide: "top", Routing: "elbow", ID: "n", Text: "next"}, "plan", "line", &out, &errb)
	require.Equal(t, rcOK, rc, errb.String())
	byID := patchedByID(*patched)
	line := byID["n"]
	assert.Equal(t, "elbow", line["routing"])
	assert.Equal(t, map[string]any{"elementId": "a", "fixedPoint": []any{0.5, 1.0}}, line["startBinding"])
	assert.Equal(t, map[string]any{"elementId": "b", "fixedPoint": []any{0.5, 0.0}}, line["endBinding"])
	assert.EqualValues(t, 50, line["x"])
	assert.EqualValues(t, 64, line["y"])
	assert.Equal(t, []any{[]any{0.0, 0.0}, []any{0.0, 66.0}, []any{300.0, 66.0}, []any{300.0, 132.0}}, line["points"])
	assert.Equal(t, "n", byID["n-label"]["containerId"])

	for name, bad := range map[string]*boardAddParams{
		"routing":  {From: "a", To: "b", Routing: "zigzag"},
		"side":     {From: "a", To: "b", FromSide: "middle"},
		"loneside": {FromSide: "top"},
	} {
		assert.Equal(t, rcInvalidArg, runBoardAdd(bad, "plan", "arrow", &out, &errb), name)
	}
	assert.Equal(t, rcInvalidArg, runBoardAdd(&boardAddParams{Routing: "elbow"}, "plan", "rectangle", &out, &errb))
}

func TestBoardMoveKeepsSnapRoutingStrokesAndText(t *testing.T) {
	patched := fakeBoardDaemonWith(t, boardWiredFixture)
	y := 300.0
	var out, errb bytes.Buffer
	require.Equal(t, rcOK, runBoardMove(&boardMoveParams{Y: &y}, "plan", "a", &out, &errb), errb.String())
	byID := patchedByID(*patched)
	assert.EqualValues(t, 330, byID["e"]["y"], "the line leaves the same snap point")
	assert.Equal(t, "elbow", byID["e"]["routing"])
	assert.Greater(t, len(byID["e"]["points"].([]any)), 2, "it keeps its elbow")
	assert.EqualValues(t, 296, byID["s"]["y"], "a stroke keeps to the snap point it started on")
	assert.Contains(t, byID, "e-label", "text attached to the line stays on it")
}

func TestBoardConnectChangesRoutingAndEnds(t *testing.T) {
	patched := fakeBoardDaemonWith(t, boardWiredFixture)
	var out, errb bytes.Buffer
	rc := runBoardConnect(&boardConnectParams{Routing: "curved"}, "plan", "e", &out, &errb)
	require.Equal(t, rcOK, rc, errb.String())
	line := patchedByID(*patched)["e"]
	assert.Equal(t, "curved", line["routing"])
	assert.Len(t, line["points"], boardCurveSteps+1)
	assert.EqualValues(t, 4, line["version"])

	require.Equal(t, rcOK, runBoardConnect(&boardConnectParams{FreeTo: true, FromSide: "top"}, "plan", "e", &out, &errb), errb.String())
	line = patchedByID(*patched)["e"]
	assert.NotContains(t, line, "endBinding")
	assert.Equal(t, map[string]any{"elementId": "a", "fixedPoint": []any{0.5, 0.0}}, line["startBinding"])

	for name, p := range map[string]*boardConnectParams{
		"nothing": {},
		"both":    {FreeTo: true, To: "b"},
		"routing": {Routing: "zigzag"},
		"side":    {ToSide: "up"},
	} {
		assert.Equal(t, rcInvalidArg, runBoardConnect(p, "plan", "e", &out, &errb), name)
	}
	assert.Equal(t, rcInvalidArg, runBoardConnect(&boardConnectParams{Routing: "elbow"}, "plan", "a", &out, &errb), "a shape is no line")
	assert.Equal(t, rcInvalidArg, runBoardConnect(&boardConnectParams{To: "s"}, "plan", "e", &out, &errb), "lines do not attach to lines")
	assert.Equal(t, rcNotFound, runBoardConnect(&boardConnectParams{To: "nope"}, "plan", "e", &out, &errb))
}

func TestBoardShowNamesAttachmentsAndRouting(t *testing.T) {
	var doc boardDoc
	require.NoError(t, json.Unmarshal([]byte(boardWiredFixture), &doc))
	var lines []string
	for _, raw := range doc.Elements {
		var e boardElementView
		require.NoError(t, json.Unmarshal(raw, &e))
		lines = append(lines, formatBoardElement(e))
	}
	assert.Contains(t, lines[2], "a:right -> b:left  elbow")
	assert.Contains(t, lines[3], "on e")
}

func TestBoardPatchRejectsAFileWithoutElements(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `{"elements":null}`} {
		_, err := parseBoardElements([]byte(data))
		assert.ErrorContains(t, err, "no \"elements\"", data)
	}
	list, err := parseBoardElements([]byte(`[{"id":"a"}]`))
	require.NoError(t, err)
	assert.Len(t, list, 1)
}
