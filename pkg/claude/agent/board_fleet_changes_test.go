package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The operator's edits on a drawn map read back as staffing proposals; a
// redraw's own writes (and an untouched map) read as nothing.
func TestFleetChanges(t *testing.T) {
	var fleet fleetResp
	require.NoError(t, json.Unmarshal([]byte(`{"agents":[
	  {"agent_id":"agt_a","title":"asker","groups":["api"],"online":true},
	  {"agent_id":"agt_b","title":"builder","groups":["api"],"online":true},
	  {"agent_id":"agt_c","title":"idler","groups":["web"],"online":true},
	  {"agent_id":"agt_d","title":"gone","groups":["web"],"online":false}
	]}`), &fleet))

	// The drawn map, as the server would store it: written by an agent.
	elems, _ := fleetBoardElements(&fleet, &boardDoc{ID: "fleet"}, true)
	byID := map[string]map[string]any{}
	var ids []string
	for _, e := range elems {
		e["customData"] = map[string]any{"author": "agt_steward"}
		byID[e["id"].(string)] = e
		ids = append(ids, e["id"].(string))
	}
	docOf := func(extra ...map[string]any) *boardDoc {
		doc := &boardDoc{ID: "fleet"}
		for _, id := range ids {
			raw, err := json.Marshal(byID[id])
			require.NoError(t, err)
			doc.Elements = append(doc.Elements, raw)
		}
		for _, e := range extra {
			raw, err := json.Marshal(e)
			require.NoError(t, err)
			doc.Elements = append(doc.Elements, raw)
		}
		return doc
	}
	operator := map[string]any{"author": "operator"}

	changes, ok := fleetChanges(&fleet, docOf())
	require.True(t, ok)
	assert.Empty(t, changes, "an untouched map proposes nothing")

	_, ok = fleetChanges(&fleet, &boardDoc{ID: "other"})
	assert.False(t, ok, "a board without a map says so")

	webX := byID["fa-agt_c-web"]["x"].(float64)
	apiY := byID["fa-agt_a-api"]["y"].(float64)

	// Dragging asker's card into web.
	byID["fa-agt_a-api"]["x"] = webX
	byID["fa-agt_a-api"]["customData"] = operator
	// Deleting idler's card; the tombstone keeps only what the server keeps.
	byID["fa-agt_c-web"] = map[string]any{"id": "fa-agt_c-web", "type": "rectangle", "isDeleted": true, "customData": operator}
	// A redraw deleting gone's card is not the operator's doing.
	byID["fa-agt_d-web"] = map[string]any{"id": "fa-agt_d-web", "type": "rectangle", "isDeleted": true, "customData": map[string]any{"author": "agt_steward"}}

	arrow := map[string]any{"id": "ar1", "type": "arrow", "x": 10.0, "y": apiY + fleetCardH + fleetRowGap + 20, "width": webX, "height": 0.0,
		"points": [][]float64{{0, 0}, {webX, 0}}, "customData": operator}
	note := map[string]any{"id": "n1", "type": "text", "x": webX + 10, "y": apiY + 400, "width": 100.0, "height": 20.0, "text": " docs writer ", "customData": operator}
	stray := map[string]any{"id": "n2", "type": "text", "x": webX + 10, "y": -500.0, "width": 100.0, "height": 20.0, "text": "above the map", "customData": operator}
	theirs := map[string]any{"id": "n3", "type": "text", "x": webX + 10, "y": apiY + 500, "width": 100.0, "height": 20.0, "text": "steward's own", "customData": map[string]any{"author": "agt_steward"}}

	changes, ok = fleetChanges(&fleet, docOf(arrow, note, stray, theirs))
	require.True(t, ok)
	assert.Equal(t, []fleetChange{
		{Kind: "move", AgentID: "agt_a", Title: "asker", From: "api", To: "web", Via: "card"},
		{Kind: "move", AgentID: "agt_b", Title: "builder", From: "api", To: "web", Via: "arrow"},
		{Kind: "remove", AgentID: "agt_c", Title: "idler", From: "web", Via: "deleted card"},
		{Kind: "add", To: "web", Text: "docs writer", Via: "note"},
	}, changes)

	// An arrow bound to a card a redraw replaced does not fall to whichever
	// card now sits under its tail.
	bound := map[string]any{"id": "ar2", "type": "arrow", "x": 10.0, "y": apiY + 20, "points": [][]float64{{0, 0}, {webX, 0}},
		"startBinding": map[string]any{"elementId": "fa-agt_old-api"}, "customData": operator}
	byID["fa-agt_a-api"]["x"] = byID["fa-agt_b-api"]["x"]
	changes, _ = fleetChanges(&fleet, docOf(bound))
	assert.Equal(t, []fleetChange{{Kind: "remove", AgentID: "agt_c", Title: "idler", From: "web", Via: "deleted card"}}, changes)

	// Deleting a whole column removes each card in it.
	column := func(id string) map[string]any {
		return map[string]any{"id": id, "type": "rectangle", "isDeleted": true, "customData": operator}
	}
	byID["fg-web"] = column("fg-web")
	byID["fa-agt_d-web"] = column("fa-agt_d-web")
	changes, _ = fleetChanges(&fleet, docOf())
	assert.Equal(t, []fleetChange{
		{Kind: "remove", AgentID: "agt_c", Title: "idler", From: "web", Via: "deleted card"},
		{Kind: "remove", AgentID: "agt_d", Title: "gone", From: "web", Via: "deleted card"},
	}, changes)
}

// A card dropped where the agent already has one means leave this group,
// and the no-group column has no group to name.
func TestFleetChangesOwnColumnAndNoGroup(t *testing.T) {
	var fleet fleetResp
	require.NoError(t, json.Unmarshal([]byte(`{"agents":[
	  {"agent_id":"agt_a","title":"both","groups":["api","web"],"online":true},
	  {"agent_id":"agt_b","title":"loose","online":true}
	]}`), &fleet))
	elems, _ := fleetBoardElements(&fleet, &boardDoc{ID: "fleet"}, true)
	byID := map[string]map[string]any{}
	var doc boardDoc
	for _, e := range elems {
		byID[e["id"].(string)] = e
	}
	operator := map[string]any{"author": "operator"}
	byID["fa-agt_a-api"]["x"], byID["fa-agt_a-api"]["customData"] = byID["fa-agt_a-web"]["x"], operator
	byID["fa-agt_b-_no_group_"]["x"], byID["fa-agt_b-_no_group_"]["customData"] = byID["fa-agt_a-web"]["x"], operator
	for _, e := range byID {
		raw, err := json.Marshal(e)
		require.NoError(t, err)
		doc.Elements = append(doc.Elements, raw)
	}
	changes, ok := fleetChanges(&fleet, &doc)
	require.True(t, ok)
	assert.ElementsMatch(t, []fleetChange{
		{Kind: "remove", AgentID: "agt_a", Title: "both", From: "api", Via: "card"},
		{Kind: "move", AgentID: "agt_b", Title: "loose", To: "web", Via: "card"},
	}, changes)
}
