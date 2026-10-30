package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const reviewProcessYAML = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: review
name: Review flow
start: begin
nodes:
  begin:
    type: start
    next: implement
  implement:
    type: task
    name: Implement
    performer: { kind: agent, prompt: "Build it" }
    next: choose
  choose:
    type: decision
    performer: { kind: human, ask: "Ship it?" }
    next: { ship: done, rework: implement }
  done:
    type: end
    result: success
`

func TestBoardFromProcessDrawsTheFlowOnANewBoard(t *testing.T) {
	var calls []capturedReq
	shown, _ := json.Marshal(map[string]string{"source": reviewProcessYAML})
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		switch {
		case path == "/v1/process/templates/review":
			return 200, "", string(shown)
		case method == http.MethodGet && path == "/v1/boards/flow?since=0":
			return 404, "not_found", ""
		case method == http.MethodPost:
			return 201, "", `{"id":"flow","title":"Review flow"}`
		default:
			return 200, "", `{"version":1,"accepted":11}`
		}
	})
	var stdout, stderr bytes.Buffer
	rc := runBoardFromProcess(&boardFromProcessParams{}, "flow", "review", &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), "Drew process review on board flow: 4 step(s), 4 outcome arrow(s).")

	create := calls[2]
	assert.Equal(t, map[string]string{"id": "flow", "title": "Review flow"}, create.body, "a missing board is created, titled after the process")
	patch := calls[len(calls)-1]
	require.Equal(t, http.MethodPatch, patch.method)
	els := map[string]map[string]any{}
	for _, raw := range patch.body.(map[string]any)["elements"].([]json.RawMessage) {
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		els[m["id"].(string)] = m
	}
	assert.Equal(t, "diamond", els["p-choose"]["type"])
	assert.Equal(t, "#ffec99", els["p-choose"]["backgroundColor"], "a human step is yellow")
	assert.Equal(t, "#a5d8ff", els["p-implement"]["backgroundColor"], "an agent step is blue")
	assert.Equal(t, "ellipse", els["p-begin"]["type"])
	assert.Equal(t, "Implement", els["p-implement-label"]["text"])
	assert.Equal(t, "choose", els["p-choose-label"]["text"], "an unnamed step is labelled by its id")
	arrow := els["pa-choose-ship-done"]
	require.NotNil(t, arrow)
	assert.Equal(t, map[string]any{"elementId": "p-choose"}, arrow["startBinding"])
	assert.Equal(t, "ship", els["pa-choose-ship-done-label"]["text"], "a choice's outcomes are labelled")
	assert.NotContains(t, els, "pa-begin-next-implement-label", "a single way on is not")
	// Laid out along the flow: start, then implement, then the decision.
	assert.Less(t, els["p-begin"]["x"].(float64), els["p-implement"]["x"].(float64))
	assert.Less(t, els["p-implement"]["x"].(float64), els["p-choose"]["x"].(float64))
}

func TestBoardFromProcessRedrawReplacesTheSketch(t *testing.T) {
	doc := &boardDoc{ID: "flow", Elements: []json.RawMessage{
		// A step deleted on the board: the redraw must outrank the tombstone.
		json.RawMessage(`{"id":"p-implement","type":"rectangle","version":5,"isDeleted":true}`),
		// An outcome the process no longer has, and the operator's own note.
		json.RawMessage(`{"id":"pa-choose-rework-design","type":"arrow","version":2,"x":0,"y":0,"points":[[0,0],[1,0]]}`),
		json.RawMessage(`{"id":"note","type":"text","version":1,"x":0,"y":0,"width":50,"height":20,"text":"keep"}`),
	}}
	p, err := model.ParseAuthoring([]byte(reviewProcessYAML))
	require.NoError(t, err)
	els := map[string]map[string]any{}
	for _, e := range processBoardElements(p.Template, p.Edges, doc, "right", 60) {
		els[e["id"].(string)] = e
	}
	assert.Equal(t, int64(6), els["p-implement"]["version"])
	assert.Equal(t, false, els["p-implement"]["isDeleted"])
	assert.Equal(t, true, els["pa-choose-rework-design"]["isDeleted"], "a stale outcome is deleted")
	assert.Equal(t, int64(3), els["pa-choose-rework-design"]["version"])
	assert.NotContains(t, els, "note", "the operator's elements are left alone")
	assert.Greater(t, els["p-begin"]["y"].(float64), 20.0, "the sketch goes below existing content")
}

// One unprintable character would make the server refuse the whole sketch.
func TestBoardProcessLabelsAreFiltered(t *testing.T) {
	tmpl := &model.Template{Nodes: map[string]model.Node{
		"ship": {Name: "Sh\u200dip"},
		"bare": {Name: "\u200d"},
	}}
	elems := processBoardElements(tmpl, nil, &boardDoc{}, "right", boardDefaultGap)
	els := map[string]map[string]any{}
	for _, e := range elems {
		els[e["id"].(string)] = e
	}
	assert.Equal(t, "Ship", els["p-ship-label"]["text"])
	assert.Equal(t, "bare", els["p-bare-label"]["text"], "nothing printable left: the node id")
}

func TestProcessIDsNeverCollide(t *testing.T) {
	ids := newProcessIDs()
	a, b := ids.get("p-", "a.b"), ids.get("p-", "a_b")
	assert.Equal(t, "p-a_b", a)
	assert.NotEqual(t, a, b, "a.b and a_b get different ids")
	x := ids.get("p-", "x")
	assert.NotEqual(t, x+"-label", ids.get("p-", "x-label"), "a step named x-label does not take x's label")
	assert.Equal(t, a, ids.get("p-", "a.b"), "an id is stable once given")
}
