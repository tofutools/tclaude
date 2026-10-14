package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Subprocess drill-in (E1) on the wire: the editor saves a ▣ node's inline
// body inside the node with namespaced body layout ("qa/unit") and reads it
// back without stale-layout warnings; "extract as template" creates the body
// as its own template through the ordinary create endpoint, and the ▣ node
// then saves as a reference to it.
func TestProcessEditorSubgraphSaveExtractAndReference(t *testing.T) {
	f, _ := processAuthoringFlow(t)

	body := map[string]any{
		"params": map[string]any{"target": map[string]any{"type": "string"}},
		"start":  "in",
		"nodes": map[string]any{
			"in":   map[string]any{"type": "start", "next": map[string]any{"pass": "unit"}},
			"unit": map[string]any{"type": "task", "name": "Unit", "performer": map[string]any{"kind": "program", "run": "make"}, "next": map[string]any{"pass": "ok", "fail": "bad"}},
			"ok":   map[string]any{"type": "end", "result": "success"},
			"bad":  map[string]any{"type": "end", "result": "failure"},
		},
	}
	sub := map[string]any{
		"type": "subprocess", "name": "QA suite",
		"subprocess": mergeMaps(body, map[string]any{
			"inputs":  map[string]any{"target": "{{ params.branch }}"},
			"outputs": map[string]any{"ok": "pass", "bad": "fail"},
		}),
	}
	draft := map[string]any{
		"template": map[string]any{
			"apiVersion": "tclaude.dev/v1alpha1", "kind": "ProcessTemplate", "name": "Release",
			"params": map[string]any{"branch": map[string]any{"type": "string"}},
			"nodes": map[string]any{
				"begin": map[string]any{"type": "start"},
				"qa":    sub,
				"ship":  map[string]any{"type": "end", "result": "success"},
				"stop":  map[string]any{"type": "end", "result": "failure"},
			},
		},
		"edges": []map[string]any{
			{"from": "", "outcome": "start", "to": "begin"},
			{"from": "begin", "outcome": "pass", "to": "qa"},
			{"from": "qa", "outcome": "pass", "to": "ship"},
			{"from": "qa", "outcome": "fail", "to": "stop"},
		},
		"layout": map[string]any{"nodes": map[string]any{
			"qa": map[string]any{"x": 200, "y": 100}, "qa/unit": map[string]any{"x": 40, "y": 60},
		}},
	}
	rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates", draft)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &created)

	read := func(id string) map[string]any {
		t.Helper()
		rec := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/"+id, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view map[string]any
		testharness.DecodeJSON(t, rec, &view)
		return view
	}
	view := read(created.ID)
	layout := view["layout"].(map[string]any)["nodes"].(map[string]any)
	assert.Contains(t, layout, "qa/unit", "namespaced body layout round-trips")
	for _, d := range asSlice(view["diagnostics"]) {
		assert.NotContains(t, d.(map[string]any)["code"], "stale_layout", "body layout is not stale: %v", d)
	}
	qa := view["template"].(map[string]any)["nodes"].(map[string]any)["qa"].(map[string]any)
	assert.Equal(t, "unit", qa["subprocess"].(map[string]any)["nodes"].(map[string]any)["in"].(map[string]any)["next"].(map[string]any)["pass"])

	// Extract: the body becomes its own template (the editor's extractableBody).
	extracted := map[string]any{
		"template": map[string]any{
			"apiVersion": "tclaude.dev/v1alpha1", "kind": "ProcessTemplate", "name": "QA suite",
			"params": body["params"],
			"nodes": map[string]any{
				"in": map[string]any{"type": "start"}, "unit": map[string]any{"type": "task", "name": "Unit", "performer": map[string]any{"kind": "program", "run": "make"}},
				"ok": map[string]any{"type": "end", "result": "success"}, "bad": map[string]any{"type": "end", "result": "failure"},
			},
		},
		"edges": []map[string]any{
			{"from": "", "outcome": "start", "to": "in"},
			{"from": "in", "outcome": "pass", "to": "unit"},
			{"from": "unit", "outcome": "pass", "to": "ok"},
			{"from": "unit", "outcome": "fail", "to": "bad"},
		},
		"layout": map[string]any{"nodes": map[string]any{"unit": map[string]any{"x": 40, "y": 60}}},
	}
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates", extracted)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var qaSuite struct {
		ID string `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &qaSuite)

	// The ▣ node now references it, keeping its ports.
	view = read(created.ID)
	nodes := view["template"].(map[string]any)["nodes"].(map[string]any)
	nodes["qa"] = map[string]any{"type": "subprocess", "name": "QA suite", "subprocess": map[string]any{
		"templateRef": qaSuite.ID,
		"inputs":      map[string]any{"target": "{{ params.branch }}"},
		"outputs":     map[string]any{"ok": "pass", "bad": "fail"},
	}}
	view["layout"] = map[string]any{"nodes": map[string]any{"qa": map[string]any{"x": 200, "y": 100}}}
	rec = processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+created.ID, view)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	ref := read(created.ID)["template"].(map[string]any)["nodes"].(map[string]any)["qa"].(map[string]any)["subprocess"].(map[string]any)
	assert.Equal(t, qaSuite.ID, ref["templateRef"])
	assert.NotContains(t, ref, "nodes")
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
