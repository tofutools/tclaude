package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFleetBoardElementsDrawAColumnPerGroup(t *testing.T) {
	var fleet fleetResp
	require.NoError(t, json.Unmarshal([]byte(`{
	  "generated_at": "2026-10-30T12:00:00Z",
	  "counts": {"agents": 4, "online": 3, "working": 1, "needs_you": 1, "idle": 1, "offline": 1},
	  "agents": [
	    {"agent_id": "agt_a", "title": "asker", "groups": ["api"], "online": true, "status": "awaiting_permission"},
	    {"agent_id": "agt_b", "title": "builder", "groups": ["api"], "online": true, "status": "working",
	     "prs": [{"url": "https://github.com/o/r/pull/7", "state": "open"}]},
	    {"agent_id": "agt_c", "title": "idler", "groups": ["web"], "online": true, "status": "idle", "task_label": "tcl-1"},
	    {"agent_id": "agt_d", "title": "sleeper", "groups": ["web"], "online": false}
	  ]
	}`), &fleet))
	doc := &boardDoc{ID: "fleet", Elements: []json.RawMessage{
		// The operator's own note, and a card from a previous map whose agent is gone.
		json.RawMessage(`{"id":"note","type":"text","version":1,"x":0,"y":0,"width":50,"height":20,"text":"keep"}`),
		json.RawMessage(`{"id":"fa-agt_gone","type":"rectangle","version":3,"x":0,"y":0,"width":10,"height":10}`),
	}}
	elems, cards := fleetBoardElements(&fleet, doc, false)
	els := map[string]map[string]any{}
	for _, e := range elems {
		els[e["id"].(string)] = e
	}
	assert.Equal(t, 3, cards, "offline agents only with --all")
	assert.NotContains(t, els, "fa-agt_d-web")
	assert.Equal(t, "#ffec99", els["fa-agt_a-api"]["backgroundColor"], "waiting on the operator is yellow")
	assert.Equal(t, "#b2f2bb", els["fa-agt_b-api"]["backgroundColor"], "working is green")
	assert.Equal(t, "builder\nworking\nPR r#7", els["fa-agt_b-api-label"]["text"])
	assert.Equal(t, "idler\nidle\ntcl-1", els["fa-agt_c-web-label"]["text"])
	assert.Equal(t, "api", els["fg-api-label"]["text"])
	assert.Less(t, els["fa-agt_a-api"]["x"].(float64), els["fa-agt_c-web"]["x"].(float64), "a column per group")
	assert.Less(t, els["fa-agt_a-api"]["y"].(float64), els["fa-agt_b-api"]["y"].(float64), "cards stack in their column")
	assert.Equal(t, true, els["fa-agt_gone"]["isDeleted"], "a card from the previous map is removed")
	assert.Equal(t, int64(4), els["fa-agt_gone"]["version"])
	assert.NotContains(t, els, "note", "the operator's elements are left alone")
	assert.Greater(t, els["fg-title"]["y"].(float64), 20.0, "the map goes below existing content")
	assert.ElementsMatch(t, []string{"fg-api", "fg-web"}, []string{elems[0]["id"].(string), elems[1]["id"].(string)}, "group boxes go under their cards")

	_, all := fleetBoardElements(&fleet, &boardDoc{ID: "fleet"}, true)
	assert.Equal(t, 4, all)

	// An agent in two groups has a card in each; a group named "title" does
	// not take the title's id; unprintable characters are dropped.
	fleet.Agents[0].Groups = []string{"api", "title"}
	fleet.Agents[0].Title = "👩\u200d💻 asker"
	elems, cards = fleetBoardElements(&fleet, &boardDoc{ID: "fleet"}, false)
	assert.Equal(t, 4, cards)
	seen := map[string]bool{}
	els = map[string]map[string]any{}
	for _, e := range elems {
		id := e["id"].(string)
		require.False(t, seen[id], "duplicate id %s", id)
		seen[id] = true
		els[id] = e
	}
	assert.Contains(t, els, "fa-agt_a-title")
	assert.Equal(t, "👩💻 asker\nneeds you: permission", els["fa-agt_a-api-label"]["text"])

	// A redraw stays where the previous map was, under the operator's marks.
	prev := &boardDoc{ID: "fleet", Elements: []json.RawMessage{
		json.RawMessage(`{"id":"fg-title","type":"text","version":2,"x":0,"y":500,"width":300,"height":30,"text":"Fleet"}`),
		json.RawMessage(`{"id":"circle","type":"ellipse","version":1,"x":0,"y":560,"width":300,"height":400}`),
	}}
	elems, _ = fleetBoardElements(&fleet, prev, false)
	for _, e := range elems {
		if e["id"] == "fg-title" {
			assert.Equal(t, 500.0, e["y"])
		}
	}
}
