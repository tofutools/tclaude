package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tbContextLimits is the context's limits summary for the crew board, as the
// lanes' heads read it.
type tbContextLimits struct {
	Per   string `json:"per"`
	Lanes []struct {
		Lane    string         `json:"lane"`
		Max     int            `json:"max"`
		AtLimit string         `json:"at_limit"`
		Count   int            `json:"count"`
		Counts  map[string]int `json:"counts"`
		Queued  int            `json:"queued"`
	} `json:"lanes"`
}

func (h *tbAssignHarness) contextLimits() *tbContextLimits {
	h.t.Helper()
	rec := h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Groups []struct {
			Group   string `json:"group"`
			Options *struct {
				Limits *tbContextLimits `json:"limits"`
			} `json:"options"`
		} `json:"groups"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	for _, g := range out.Groups {
		if g.Group == "crew" {
			require.NotNil(h.t, g.Options)
			return g.Options.Limits
		}
	}
	h.t.Fatal("no crew board in the context")
	return nil
}

// The context carries each limited lane with how full it is and how many
// wait in line for it, so the lanes' heads read "1 / 1, full" and the
// queued count without reading the options.
func TestTaskboardContextLimits(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	assert.Nil(t, h.contextLimits(), "no limits: no summary")

	h.setOptions(h.lead, map[string]string{"limits.in_progress.max": "1"})
	l := h.contextLimits()
	require.NotNil(t, l)
	assert.Equal(t, "board", l.Per)
	require.Len(t, l.Lanes, 1)
	assert.Equal(t, "in_progress", l.Lanes[0].Lane)
	assert.Equal(t, 1, l.Lanes[0].Max)
	assert.Equal(t, "queue", l.Lanes[0].AtLimit)
	assert.Zero(t, l.Lanes[0].Count)

	first := h.assigned("first")
	second := h.add("second", "next")
	code, body := h.limitAssign(h.lead, second.ID, h.peer, nil)
	require.Equal(t, http.StatusOK, code, body)
	l = h.contextLimits()
	assert.Equal(t, 1, l.Lanes[0].Count, "first fills it")
	assert.Equal(t, 1, l.Lanes[0].Queued, "second waits")
	_ = first

	// Per assignee: each agent's own count, the fullest as the lane's. The
	// peer has a slot of its own now, so its task goes in at once.
	h.setOptions(h.lead, map[string]string{"limits.per": "assignee"})
	l = h.contextLimits()
	assert.Equal(t, "assignee", l.Per)
	assert.Equal(t, 1, l.Lanes[0].Count)
	assert.Len(t, l.Lanes[0].Counts, 2, "one each")
	assert.Zero(t, l.Lanes[0].Queued, "the line went in")
}
