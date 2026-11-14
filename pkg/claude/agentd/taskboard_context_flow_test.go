package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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
	now := time.Now().UnixNano()
	_, err = d.Exec(`UPDATE group_tasks SET assignee_id = ?, assigned_by = 'human:operator', assigned_at = ?, started_at = ? WHERE id = ?`,
		agentID, now, now, taken.ID)
	require.NoError(t, err)

	c := h.context(member)
	require.Equal(t, []string{"crew"}, contextGroupNames(c))
	g := c.Groups[0]
	assert.Equal(t, 5, g.Counts.Live)
	assert.Equal(t, 0, g.Counts.NeedsYou, "a blocked task does not need the operator")
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

// On by default; features.taskboard false answers 404.
func TestTaskboardContextOnByDefaultAndCanBeTurnedOff(t *testing.T) {
	f := newFlow(t)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/taskboard/context", nil)))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Taskboard: new(false)}}))
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/taskboard/context", nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "feature_disabled")
}

// The bounds: at most 50 live rows (the counts still cover them all), at
// most 8 closed rows, and a done task older than a week leaves the weekly
// count.
func TestTaskboardContextBounds(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	for i := 0; i < 52; i++ {
		h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "live", "stage": "next"}), http.StatusCreated)
	}
	var closed []string
	for i := 0; i < 10; i++ {
		task := h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "closed"}), http.StatusCreated)
		h.task(h.call("", http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/status", map[string]any{"status": "done"}), http.StatusOK)
		closed = append(closed, task.ID)
	}
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE group_tasks SET closed_at = ? WHERE id = ?`, time.Now().Add(-8*24*time.Hour).UnixNano(), closed[0])
	require.NoError(t, err)

	rec := h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Groups []struct {
			Counts struct {
				Live         int `json:"live"`
				Next         int `json:"next"`
				DoneThisWeek int `json:"done_this_week"`
			} `json:"counts"`
			Tasks     []tbContextTask `json:"tasks"`
			Closed    []tbContextTask `json:"closed"`
			Truncated bool            `json:"truncated"`
		} `json:"groups"`
	}
	testharness.DecodeJSON(t, rec, &out)
	require.Len(t, out.Groups, 1)
	g := out.Groups[0]
	assert.Equal(t, 52, g.Counts.Live)
	assert.Equal(t, 52, g.Counts.Next)
	assert.Len(t, g.Tasks, 50)
	assert.True(t, g.Truncated)
	assert.Len(t, g.Closed, 8)
	assert.Equal(t, 9, g.Counts.DoneThisWeek)
}

// `tclaude agent taskboard ls --summary` reads the same counts, for the
// boards the caller can read, as text and as JSON.
func TestTaskboardLsSummaryCLI(t *testing.T) {
	h := newTaskboardFlow(t)
	const member = "tb-ctx-cli-6666"
	h.agent(t, member)
	h.f.HaveGroup("crew")
	h.f.HaveGroup("secret")
	h.f.HaveMember("crew", member)
	h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "queued", "stage": "next"}), http.StatusCreated)
	h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "an idea"}), http.StatusCreated)
	h.task(h.call("", http.MethodPost, "/v1/groups/secret/taskboard/tasks", map[string]any{"title": "secret plan"}), http.StatusCreated)
	bridgeAgentClientToMuxAsAgent(t, h.f.Mux, member)

	stdout := new(bytes.Buffer)
	require.Equal(t, 0, agent.RunTaskboardSummary(false, stdout, new(bytes.Buffer)))
	assert.Equal(t, "crew: 2 · 1 next · 1 idea\n", stdout.String(), "only the readable board")

	stdout.Reset()
	require.Equal(t, 0, agent.RunTaskboardSummary(true, stdout, new(bytes.Buffer)))
	var out struct {
		Groups []agent.TaskboardSummaryGroup `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Len(t, out.Groups, 1)
	assert.Equal(t, "crew", out.Groups[0].Group)
	assert.Equal(t, 2, out.Groups[0].Counts.Live)
	assert.Equal(t, 1, out.Groups[0].Counts.Ideas)
}

// The dashboard's Task board section writes through the CLI's own routes on
// the dashboard mux, as the operator: capture, reorder across the Ideas band,
// block and close.
func TestTaskboardDashboardWrites(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	dash := agentd.BuildDashboardHandlerForTest()
	send := func(method, path string, body any) tbTask {
		t.Helper()
		rec := testharness.Serve(dash, dashReq(t, method, path, body))
		require.Less(t, rec.Code, 300, "%s %s: %s", method, path, rec.Body.String())
		var out tbTask
		testharness.DecodeJSON(t, rec, &out)
		return out
	}
	first := send(http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "first", "stage": "next"})
	assert.Equal(t, "human:operator", first.CreatedBy)
	idea := send(http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "an idea"})
	// Dropping the idea above the queued task promotes it in the same write.
	moved := send(http.MethodPost, "/v1/taskboard/tasks/"+idea.ID+"/move", map[string]any{"before": first.ID})
	assert.Equal(t, "next", moved.Stage)
	send(http.MethodPatch, "/v1/taskboard/tasks/"+first.ID, map[string]any{"title": "first, renamed"})
	blocked := send(http.MethodPost, "/v1/taskboard/tasks/"+first.ID+"/status", map[string]any{"status": "blocked", "reason": "waiting on CI"})
	assert.Equal(t, "waiting on CI", blocked.BlockReason)
	assert.Equal(t, []string{"an idea", "first, renamed"}, h.list("", "/v1/groups/crew/taskboard"))
	send(http.MethodPost, "/v1/taskboard/tasks/"+idea.ID+"/status", map[string]any{"status": "done"})
	assert.Equal(t, []string{"first, renamed"}, h.list("", "/v1/groups/crew/taskboard"))
}

// The section's Assign and Unassign go through V1.3's routes on the
// dashboard mux: an assigned idea is promoted, and the context names the
// assignee.
func TestTaskboardDashboardAssign(t *testing.T) {
	h := newTaskboardFlow(t)
	const member = "tb-dash-assign-7777"
	h.agent(t, member)
	h.f.HaveGroup("crew")
	h.f.HaveMember("crew", member)
	agentID, err := db.AgentIDForConv(member)
	require.NoError(t, err)
	dash := agentd.BuildDashboardHandlerForTest()
	send := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		rec := testharness.Serve(dash, dashReq(t, method, path, body))
		require.Less(t, rec.Code, 300, "%s %s: %s", method, path, rec.Body.String())
		return rec
	}
	var idea tbTask
	testharness.DecodeJSON(t, send(http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "an idea"}), &idea)
	send(http.MethodPost, "/v1/taskboard/tasks/"+idea.ID+"/assign", map[string]any{"to": agentID})

	c := h.context("")
	require.Len(t, c.Groups, 1)
	require.Len(t, c.Groups[0].Tasks, 1)
	task := c.Groups[0].Tasks[0]
	assert.Equal(t, "next", task.Stage, "assigning an idea promotes it")
	require.NotNil(t, task.Assignee)
	assert.Equal(t, agentID, task.Assignee.AgentID)
	assert.Equal(t, 1, c.Groups[0].Counts.InProgress)

	send(http.MethodPost, "/v1/taskboard/tasks/"+idea.ID+"/unassign", map[string]any{})
	assert.Nil(t, h.context("").Groups[0].Tasks[0].Assignee)
}
