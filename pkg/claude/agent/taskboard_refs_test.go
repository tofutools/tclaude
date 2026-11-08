package agent

import (
	"bytes"
	"encoding/json"
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
		{"kind":"awb","url":"https://awb.example/#/issues/tcl-a1b2c3","label":"tcl-a1b2c3"}]}`), &task))
	assert.Equal(t, "#7 +1", task.refText())
	refs := task.allRefs()
	require.Len(t, refs, 2)
	assert.Equal(t, "github #7 https://github.com/o/r/pull/7 (open · CI 23/25 failing · approved)", refs[0].line())
	assert.Equal(t, "awb tcl-a1b2c3 https://awb.example/#/issues/tcl-a1b2c3", refs[1].line())

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
}
