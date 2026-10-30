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
	assert.NotContains(t, els, "fa-agt_d")
	assert.Equal(t, "#ffec99", els["fa-agt_a"]["backgroundColor"], "waiting on the operator is yellow")
	assert.Equal(t, "#b2f2bb", els["fa-agt_b"]["backgroundColor"], "working is green")
	assert.Equal(t, "builder\nworking\nPR r#7", els["fa-agt_b-label"]["text"])
	assert.Equal(t, "idler\nidle\ntcl-1", els["fa-agt_c-label"]["text"])
	assert.Equal(t, "api", els["fg-api-label"]["text"])
	assert.Less(t, els["fa-agt_a"]["x"].(float64), els["fa-agt_c"]["x"].(float64), "a column per group")
	assert.Less(t, els["fa-agt_a"]["y"].(float64), els["fa-agt_b"]["y"].(float64), "cards stack in their column")
	assert.Equal(t, true, els["fa-agt_gone"]["isDeleted"], "a card from the previous map is removed")
	assert.Equal(t, int64(4), els["fa-agt_gone"]["version"])
	assert.NotContains(t, els, "note", "the operator's elements are left alone")
	assert.Greater(t, els["fg-title"]["y"].(float64), 20.0, "the map goes below existing content")
	assert.ElementsMatch(t, []string{"fg-api", "fg-web"}, []string{elems[0]["id"].(string), elems[1]["id"].(string)}, "group boxes go under their cards")

	_, all := fleetBoardElements(&fleet, &boardDoc{ID: "fleet"}, true)
	assert.Equal(t, 4, all)
}
