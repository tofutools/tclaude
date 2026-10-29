package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
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
	var patched []map[string]any
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		switch method {
		case http.MethodGet:
			return json.Unmarshal([]byte(boardFixture), out)
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

func TestBoardRmWritesTombstonesWithBumpedVersion(t *testing.T) {
	patched := fakeBoardDaemon(t)
	var out, errb bytes.Buffer
	rc := editBoardElements("plan", []string{"a"}, func(m map[string]any) { m["isDeleted"] = true }, &out, &errb)
	require.Equal(t, rcOK, rc, errb.String())
	require.Len(t, *patched, 1)
	e := (*patched)[0]
	assert.Equal(t, true, e["isDeleted"])
	assert.EqualValues(t, 3, e["version"])
	assert.NotContains(t, e, "server_version")

	rc = editBoardElements("plan", []string{"missing"}, func(map[string]any) {}, &out, &errb)
	assert.Equal(t, rcNotFound, rc)
}

func TestBoardSnapshotAge(t *testing.T) {
	assert.Contains(t, snapshotAge("4", "4"), "current board version")
	assert.Contains(t, snapshotAge("2", "5"), "3 version(s) newer")
	assert.Contains(t, snapshotAge("", "5"), "unknown")
}
