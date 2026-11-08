package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A task's refs as the CLI prints them (tk_01ad0884): ls shows the first
// and how many more, show every one with a pull request's status, and an
// older node that only sends ref still reads as one ref.
func TestTaskboardRefsText(t *testing.T) {
	var task taskboardTask
	require.NoError(t, json.Unmarshal([]byte(`{"id":"tk_00000001","ref":{"kind":"github","url":"https://github.com/o/r/pull/7","label":"#7"},
		"refs":[{"kind":"github","url":"https://github.com/o/r/pull/7","label":"#7",
			"pr":{"number":7,"state":"open","checks":{"total":25,"passed":23,"failed":2,"pending":0,"state":"failing"},"reviews":{"decision":"approved","approved":1,"changes_requested":0}}},
		{"kind":"awb","url":"https://awb.example/#/issues/tcl-a1b2c3","label":"tcl-a1b2c3","tracker":{"title":"Show the context","status":"in_progress"}}]}`), &task))
	assert.Equal(t, "#7 +1", task.refText())
	refs := task.allRefs()
	require.Len(t, refs, 2)
	assert.Equal(t, "github #7 https://github.com/o/r/pull/7 (open · CI 23/25 failing · approved)", refs[0].line())
	assert.Equal(t, "awb tcl-a1b2c3 https://awb.example/#/issues/tcl-a1b2c3 (in_progress: Show the context)", refs[1].line())

	var old taskboardTask
	require.NoError(t, json.Unmarshal([]byte(`{"id":"tk_00000002","ref":{"kind":"linear","label":"LIN-1"}}`), &old))
	assert.Equal(t, "LIN-1", old.refText())
	assert.Len(t, old.allRefs(), 1)

	// A write that added refs to a node that kept none is refused loudly.
	var stderr bytes.Buffer
	assert.Equal(t, rcInvalidArg, refsKept(map[string]any{"add_refs": []string{"LIN-2"}}, old, &stderr))
	assert.Contains(t, stderr.String(), "predates tasks with several refs")
	assert.Equal(t, rcOK, refsKept(map[string]any{"add_refs": []string{"LIN-2"}}, task, &stderr))
	assert.Equal(t, rcOK, refsKept(map[string]any{"title": "x"}, old, &stderr))
	// A removal an older node ignored leaves its ref and sends no refs.
	assert.Equal(t, rcInvalidArg, refsKept(map[string]any{"rm_refs": []string{"#2"}}, old, &stderr))
	assert.Equal(t, rcInvalidArg, refsKept(map[string]any{"first_ref": "#2"}, old, &stderr), "so does a reorder")

	// Another node takes a ref linked on this node's awb server by its id.
	ref, label := taskboardRef{Kind: "awb", URL: "http://localhost:7777/#/issues/tcl-a1b2c3", Label: "tcl-a1b2c3", Local: true, ID: "tcl-a1b2c3"}.transfer()
	assert.Equal(t, [2]string{"awb:tcl-a1b2c3", ""}, [2]string{ref, label})
	ref, label = refs[0].transfer()
	assert.Equal(t, [2]string{"https://github.com/o/r/pull/7", "#7"}, [2]string{ref, label})
}

// --first-ref sends first_ref, after the other ref changes.
func TestTaskboardEditFirstRef(t *testing.T) {
	var got map[string]any
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		raw, _ := json.Marshal(in)
		got = nil
		require.NoError(t, json.Unmarshal(raw, &got))
		return json.Unmarshal([]byte(`{"id":"tk_00000001","title":"x","ref":{"kind":"linear","label":"LIN-2"},"refs":[{"kind":"linear","label":"LIN-2"},{"kind":"linear","label":"LIN-1"}]}`), out)
	}
	var stdout, stderr bytes.Buffer
	rc := runTaskboardEdit(&taskboardEditParams{Task: "tk_00000001", FirstRef: " LIN-2 ", AddRef: []string{"LIN-2"}}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, map[string]any{"first_ref": "LIN-2", "add_refs": []any{"LIN-2"}}, got)
	assert.Equal(t, rcInvalidArg, runTaskboardEdit(&taskboardEditParams{Task: "tk_00000001", FirstRef: "#2", ClearRef: true}, strings.NewReader(""), &stdout, &stderr))
}
