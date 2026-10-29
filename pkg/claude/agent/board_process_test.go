package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		case method == http.MethodGet && path == "/v1/boards/flow":
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
	arrow := els["p-choose-ship-done"]
	require.NotNil(t, arrow)
	assert.Equal(t, map[string]any{"elementId": "p-choose"}, arrow["startBinding"])
	assert.Equal(t, "ship", els["p-choose-ship-done-label"]["text"], "a choice's outcomes are labelled")
	assert.NotContains(t, els, "p-begin-next-implement-label", "a single way on is not")
	// Laid out along the flow: start, then implement, then the decision.
	assert.Less(t, els["p-begin"]["x"].(float64), els["p-implement"]["x"].(float64))
	assert.Less(t, els["p-implement"]["x"].(float64), els["p-choose"]["x"].(float64))
}

