package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type tbContextTask struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Stage    string   `json:"stage"`
	Status   string   `json:"status"`
	Body     string   `json:"body"`
	Queued   bool     `json:"queued"`
	Unmet    []string `json:"unmet"`
	Assignee *struct {
		AgentID string `json:"agent_id"`
	} `json:"assignee"`
	AssignedBy string `json:"assigned_by"`
}

type tbContext struct {
	Groups []struct {
		Group  string `json:"group"`
		Counts struct {
			Live         int `json:"live"`
			NeedsYou     int `json:"needs_you"`
			InProgress   int `json:"in_progress"`
			Next         int `json:"next"`
			Ideas        int `json:"ideas"`
			Blocked      int `json:"blocked"`
			Queued       int `json:"queued"`
			DoneThisWeek int `json:"done_this_week"`
			Dropped      int `json:"dropped"`
		} `json:"counts"`
		Tasks  []tbContextTask `json:"tasks"`
		Closed []tbContextTask `json:"closed"`
	} `json:"groups"`
}

func (h *taskboardHarness) context(conv string) tbContext {
	h.t.Helper()
	rec := h.call(conv, http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(h.t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out tbContext
	testharness.DecodeJSON(h.t, rec, &out)
	return out
}

func contextGroupNames(c tbContext) []string {
	names := []string{}
	for _, g := range c.Groups {
		names = append(names, g.Group)
	}
	return names
}

// The dashboard's read of every board: per group the counts and the ordered
// rows (queue, then ideas, then the recently closed), bodies left out, and
// only the groups the caller reads.
func TestTaskboardContextCountsAndRows(t *testing.T) {
	h := newTaskboardFlow(t)
	const member = "tb-ctx-member-1111"
	h.agent(t, member)
	h.f.HaveGroup("crew")
	h.f.HaveMember("crew", member)
	agentID, err := db.AgentIDForConv(member)
	require.NoError(t, err)

	add := func(body map[string]any) tbTask {
		return h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", body), http.StatusCreated)
	}
	first := add(map[string]any{"title": "first", "stage": "next", "body": "the long brief"})
	add(map[string]any{"title": "waits on first", "stage": "next", "after": []string{first.ID}})
	held := add(map[string]any{"title": "held", "stage": "next"})
	taken := add(map[string]any{"title": "taken", "stage": "next"})
	add(map[string]any{"title": "an idea"})
	closed := add(map[string]any{"title": "closed", "stage": "next"})
	dropped := add(map[string]any{"title": "dropped"})
	h.task(h.call("", http.MethodPost, "/v1/taskboard/tasks/"+held.ID+"/status",
		map[string]any{"status": "blocked", "reason": "waiting on review"}), http.StatusOK)
	h.task(h.call("", http.MethodPost, "/v1/taskboard/tasks/"+closed.ID+"/status", map[string]any{"status": "done"}), http.StatusOK)
	h.task(h.call("", http.MethodPost, "/v1/taskboard/tasks/"+dropped.ID+"/status", map[string]any{"status": "dropped"}), http.StatusOK)
	// The assign routes are a later slice; the store's columns already exist.
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE group_tasks SET assignee_id = ?, assigned_by = 'human:operator', assigned_at = ? WHERE id = ?`,
		agentID, time.Now().Unix(), taken.ID)
	require.NoError(t, err)

	c := h.context(member)
	require.Equal(t, []string{"crew"}, contextGroupNames(c))
	g := c.Groups[0]
	assert.Equal(t, 5, g.Counts.Live)
	assert.Equal(t, 1, g.Counts.NeedsYou)
	assert.Equal(t, 1, g.Counts.Blocked)
	assert.Equal(t, 1, g.Counts.InProgress)
	assert.Equal(t, 2, g.Counts.Next, "first and waits; taken is in progress")
	assert.Equal(t, 1, g.Counts.Ideas)
	assert.Equal(t, 1, g.Counts.Queued)
	assert.Equal(t, 1, g.Counts.DoneThisWeek)
	assert.Equal(t, 1, g.Counts.Dropped)

	titles := []string{}
	for _, task := range g.Tasks {
		titles = append(titles, task.Title)
		assert.Empty(t, task.Body, "rows carry no body")
	}
	assert.Equal(t, []string{"first", "waits on first", "held", "taken", "an idea"}, titles)
	assert.True(t, g.Tasks[1].Queued)
	assert.Equal(t, []string{first.ID}, g.Tasks[1].Unmet)
	require.NotNil(t, g.Tasks[3].Assignee)
	assert.Equal(t, agentID, g.Tasks[3].Assignee.AgentID)
	assert.Equal(t, "human:operator", g.Tasks[3].AssignedBy)
	assert.Nil(t, g.Tasks[0].Assignee)
	require.Len(t, g.Closed, 2)
	assert.ElementsMatch(t, []string{closed.ID, dropped.ID}, []string{g.Closed[0].ID, g.Closed[1].ID})

	// The dashboard serves the same read to the operator on its own mux.
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), dashReq(t, http.MethodGet, "/v1/taskboard/context", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dash tbContext
	testharness.DecodeJSON(t, rec, &dash)
	assert.Equal(t, []string{"crew"}, contextGroupNames(dash))
}

// Visibility follows the board's read gate: membership, ownership and
// scoped grants each add a group; a deny removes one; a hidden group is
// absent, not listed empty.
func TestTaskboardContextVisibility(t *testing.T) {
	h := newTaskboardFlow(t)
	const (
		member   = "tb-ctx-member-2222"
		outsider = "tb-ctx-outsider-3333"
		owner    = "tb-ctx-owner-4444"
	)
	for _, c := range []string{member, outsider, owner} {
		h.agent(t, c)
	}
	crew := h.f.HaveGroup("crew")
	h.f.HaveGroup("website")
	h.f.HaveGroup("secret")
	h.f.HaveMember("crew", member)
	require.NoError(t, db.AddAgentGroupOwner(crew.ID, owner, "test"))
	h.task(h.call("", http.MethodPost, "/v1/groups/secret/taskboard/tasks", map[string]any{"title": "secret plan"}), http.StatusCreated)

	assert.ElementsMatch(t, []string{"crew", "website", "secret"}, contextGroupNames(h.context("")))
	assert.Equal(t, []string{"crew"}, contextGroupNames(h.context(member)))
	assert.Equal(t, []string{"crew"}, contextGroupNames(h.context(owner)))
	assert.Empty(t, contextGroupNames(h.context(outsider)))

	require.NoError(t, db.GrantAgentPermissionWithScope(outsider, agentd.PermTaskboardRead, `{"group":["website"]}`, "test"))
	assert.Equal(t, []string{"website"}, contextGroupNames(h.context(outsider)))
	rec := h.call(outsider, http.MethodGet, "/v1/taskboard/context", nil)
	assert.NotContains(t, rec.Body.String(), "secret plan")

	require.NoError(t, db.SetAgentPermissionOverride(member, agentd.PermTaskboardRead, db.PermEffectDeny, "test"))
	assert.Empty(t, contextGroupNames(h.context(member)), "a deny beats membership")

	// An unidentified caller gets the gate's refusal, not a list.
	rec = testharness.Serve(h.f.Mux, agentd.AsUnconfirmedPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/taskboard/context", nil)))
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret plan")
}

func TestTaskboardContextIsOffByDefault(t *testing.T) {
	f := newFlow(t)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/taskboard/context", nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "feature_disabled")
}
