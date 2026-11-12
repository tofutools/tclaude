package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tbOptions is the options route's answer, as the dashboard and the CLI
// read it.
type tbOptions struct {
	Group   string `json:"group"`
	Options struct {
		Idle *struct {
			Enabled      *bool     `json:"enabled"`
			Grace        string    `json:"grace"`
			Tell         *[]string `json:"tell"`
			CheckColumns *[]string `json:"check_columns"`
		} `json:"idle"`
	} `json:"options"`
	Resolved struct {
		Idle struct {
			Active       bool       `json:"active"`
			GloballyOff  bool       `json:"globally_off"`
			Enabled      tbOptValue `json:"enabled"`
			Grace        tbOptValue `json:"grace"`
			Nudge        tbOptValue `json:"nudge"`
			Tell         tbOptValue `json:"tell"`
			CheckColumns tbOptValue `json:"check_columns"`
		} `json:"idle"`
	} `json:"resolved"`
	Globals struct {
		IdleCheck *bool  `json:"idle_check"`
		IdleGrace string `json:"idle_grace"`
	} `json:"globals"`
	Version   int64    `json:"version"`
	UpdatedBy string   `json:"updated_by"`
	Keys      []string `json:"keys"`
	Code      string   `json:"code"`
	Current   *struct {
		Version int64 `json:"version"`
	} `json:"current"`
}

type tbOptValue struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

const tbOptionsPath = "/v1/groups/crew/taskboard/options"

func (h *tbAssignHarness) options(conv, method string, body any, want int) tbOptions {
	h.t.Helper()
	rec := h.call(conv, method, tbOptionsPath, body)
	require.Equal(h.t, want, rec.Code, rec.Body.String())
	var out tbOptions
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out
}

func (h *tbAssignHarness) setOptions(conv string, set map[string]string) tbOptions {
	h.t.Helper()
	return h.options(conv, http.MethodPatch, map[string]any{"set": set}, http.StatusOK)
}

// saveTaskboardConfig writes the global config the task board flows run
// under, with tb as its taskboard section.
func saveTaskboardConfig(t *testing.T, tb *config.TaskboardConfig) {
	t.Helper()
	require.NoError(t, config.Save(&config.Config{
		Features:  &config.FeaturesConfig{Taskboard: new(true)},
		Agent:     &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfTaskboard}},
		Taskboard: tb,
	}))
}

// A board's options: everything inherited until the board sets it, each
// value with its source; members read them, owners change them, a write on a
// stale version is refused, and what is invalid for the board is refused.
func TestTaskboardOptionsReadWriteAndInheritance(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)

	// Nothing set: the idle check is off, by default.
	v := h.options(h.worker, http.MethodGet, nil, http.StatusOK)
	assert.Equal(t, "crew", v.Group)
	assert.Zero(t, v.Version)
	assert.Nil(t, v.Options.Idle)
	assert.False(t, v.Resolved.Idle.Active)
	assert.Equal(t, tbOptValue{false, "default"}, v.Resolved.Idle.Enabled)
	assert.Equal(t, tbOptValue{"5m", "default"}, v.Resolved.Idle.Grace)
	assert.Equal(t, tbOptValue{[]any{"assigner"}, "default"}, v.Resolved.Idle.Tell)
	assert.Contains(t, v.Keys, "idle.grace")

	// The global config is inherited and named.
	saveTaskboardConfig(t, &config.TaskboardConfig{IdleGrace: "2m", IdleCheck: new(true)})
	v = h.options("", http.MethodGet, nil, http.StatusOK)
	assert.True(t, v.Resolved.Idle.Active)
	assert.Equal(t, tbOptValue{true, "global"}, v.Resolved.Idle.Enabled)
	assert.Equal(t, tbOptValue{"2m", "global"}, v.Resolved.Idle.Grace)
	require.NotNil(t, v.Globals.IdleCheck)
	assert.True(t, *v.Globals.IdleCheck)
	assert.Equal(t, "2m", v.Globals.IdleGrace)

	// A member reads but does not change them; an owner does.
	rec := h.call(h.worker, http.MethodPatch, tbOptionsPath, map[string]any{"set": map[string]string{"idle.enabled": "off"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	v = h.setOptions(h.lead, map[string]string{"idle.enabled": "off", "idle.grace": "10m"})
	assert.Equal(t, int64(1), v.Version)
	assert.Equal(t, "agent:"+tbAgentID(t, h.lead), v.UpdatedBy)
	assert.False(t, v.Resolved.Idle.Active, "the board's off wins over the global on")
	assert.Equal(t, tbOptValue{false, "board"}, v.Resolved.Idle.Enabled)
	assert.Equal(t, tbOptValue{"10m", "board"}, v.Resolved.Idle.Grace)

	// A setting already in force is not a change.
	v = h.setOptions("", map[string]string{"idle.grace": "10m"})
	assert.Equal(t, int64(1), v.Version)

	// Invalid values and keys are refused, and change nothing.
	for _, set := range []map[string]string{
		{"idle.grace": "0"}, {"idle.grace": "soon"}, {"idle.enabled": "maybe"},
		{"idle.tell": "everyone"}, {"idle.surprise": "on"}, {"idle.check_columns": "in-ci"},
	} {
		rec = h.call("", http.MethodPatch, tbOptionsPath, map[string]any{"set": set})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", set, rec.Body.String())
	}
	rec = h.call("", http.MethodPatch, tbOptionsPath, map[string]any{})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, int64(1), h.options("", http.MethodGet, nil, http.StatusOK).Version)

	// A whole document replaces the stored one at the version it was read;
	// a stale version is refused with the current options.
	doc := map[string]any{"idle": map[string]any{"enabled": true, "tell": []string{"operator", "owners"}}}
	stale := h.options("", http.MethodPut, map[string]any{"options": doc, "version": 0}, http.StatusConflict)
	assert.Equal(t, "stale", stale.Code)
	require.NotNil(t, stale.Current)
	assert.Equal(t, int64(1), stale.Current.Version)
	v = h.options("", http.MethodPut, map[string]any{"options": doc, "version": 1}, http.StatusOK)
	assert.Equal(t, int64(2), v.Version)
	assert.True(t, v.Resolved.Idle.Active)
	assert.Equal(t, tbOptValue{"2m", "global"}, v.Resolved.Idle.Grace, "the grace the PUT left out is inherited again")
	assert.Equal(t, tbOptValue{[]any{"owners", "operator"}, "board"}, v.Resolved.Idle.Tell)
	rec = h.call("", http.MethodPut, tbOptionsPath, map[string]any{"options": doc})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a PUT names its version: %s", rec.Body.String())
	rec = h.call("", http.MethodPut, tbOptionsPath, map[string]any{"options": map[string]any{"idle": map[string]any{"grase": "10m"}}, "version": 2})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a misspelled key is refused, not dropped: %s", rec.Body.String())

	// A column listed in check_columns drops out when the column is removed.
	h.columns("", http.MethodPost, "", map[string]any{"label": "In CI"}, http.StatusCreated)
	v = h.setOptions("", map[string]string{"idle.check_columns": "in-ci"})
	assert.Equal(t, tbOptValue{[]any{"in-ci"}, "board"}, v.Resolved.Idle.CheckColumns)
	h.columns("", http.MethodDelete, "/in-ci", nil, http.StatusOK)
	v = h.options("", http.MethodGet, nil, http.StatusOK)
	assert.Equal(t, tbOptValue{[]any{}, "board"}, v.Resolved.Idle.CheckColumns)
	require.NotNil(t, v.Options.Idle)
	require.NotNil(t, v.Options.Idle.CheckColumns)
	assert.Empty(t, *v.Options.Idle.CheckColumns)
	// The removal is written to the stored options too: a new column of the
	// same slug is not checked.
	h.columns("", http.MethodPost, "", map[string]any{"label": "In CI"}, http.StatusCreated)
	v = h.options("", http.MethodGet, nil, http.StatusOK)
	assert.Equal(t, tbOptValue{[]any{}, "board"}, v.Resolved.Idle.CheckColumns)

	// Reset all: inherited again.
	v = h.options("", http.MethodPatch, map[string]any{"reset": []string{"all"}}, http.StatusOK)
	assert.Nil(t, v.Options.Idle)
	assert.Equal(t, tbOptValue{true, "global"}, v.Resolved.Idle.Enabled)
	assert.Greater(t, v.Version, int64(2))
}

// newTaskboardIdleBoardFlow is the assign flow with the worker live and idle
// in a pane and the global idle check left unset: each board opts in.
func newTaskboardIdleBoardFlow(t *testing.T) *tbAssignHarness {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	h.f.HaveAliveSession(h.worker, "tb-worker-session", "tb-worker-pane", h.f.TestCwd("worker"))
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	return h
}

func (h *tbAssignHarness) assigned(title string) tbTask {
	h.t.Helper()
	task := h.add(title, "next")
	code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(h.t, http.StatusOK, code, body)
	return task
}

func tbOperatorSubjects(t *testing.T, contains string) int {
	t.Helper()
	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, 50)
	require.NoError(t, err)
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Subject, contains) {
			n++
		}
	}
	return n
}

func tbCountSubjects(subjects []string, contains string) int {
	n := 0
	for _, s := range subjects {
		if strings.Contains(s, contains) {
			n++
		}
	}
	return n
}

// The idle check is a board's own choice: off unless the board opts in, on
// with the board's grace and recipients, overruled only by the global
// kill switch; and a queued task never stalls.
func TestTaskboardIdleCheckPerBoard(t *testing.T) {
	h := newTaskboardIdleBoardFlow(t)
	now := time.Now()

	// Off by default: an idle hour is not a stall.
	off := h.assigned("left alone")
	agentd.SweepTaskboardIdleForTest(now.Add(time.Hour))
	assert.Zero(t, tbStalledEvents(t, off.ID))
	assert.Zero(t, tbCountSubjects(tbInboxSubjects(t, h.lead), "went idle"))

	// On, with a 10 minute grace: not at 5 minutes, at 15.
	h.setOptions(h.lead, map[string]string{"idle.enabled": "on", "idle.grace": "10m"})
	agentd.SweepTaskboardIdleForTest(now.Add(5 * time.Minute))
	assert.Zero(t, tbStalledEvents(t, off.ID))
	agentd.SweepTaskboardIdleForTest(now.Add(15 * time.Minute))
	assert.Equal(t, 1, tbStalledEvents(t, off.ID))
	lead := tbInboxSubjects(t, h.lead)
	assert.Equal(t, 1, tbCountSubjects(lead, "went idle without reporting"), "the assigner is told by default")
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.worker), "you went idle"), "and the assignee nudged")
	msgs, err := db.ListInboxForActor(h.lead, tbAgentID(t, h.lead), 50)
	require.NoError(t, err)
	var told string
	for _, m := range msgs {
		if strings.Contains(m.Subject, "went idle without reporting") {
			told = m.Body
		}
	}
	assert.Contains(t, told, "grace 10m")
	assert.Contains(t, told, "tclaude agent taskboard options set crew idle.enabled=off", "the message says how to turn it off")

	// Recipients: the operator and the owners, not the assigner as such, and
	// no nudge. The lead owns crew, so it still hears, once.
	h.setOptions(h.lead, map[string]string{"idle.tell": "owners,operator", "idle.nudge": "off"})
	second := h.assigned("second")
	leadBefore := tbCountSubjects(tbInboxSubjects(t, h.lead), "went idle")
	workerBefore := tbCountSubjects(tbInboxSubjects(t, h.worker), "you went idle")
	operatorBefore := tbOperatorSubjects(t, "went idle without reporting")
	agentd.SweepTaskboardIdleForTest(now.Add(30 * time.Minute))
	assert.Equal(t, 1, tbStalledEvents(t, second.ID))
	assert.Equal(t, leadBefore+1, tbCountSubjects(tbInboxSubjects(t, h.lead), "went idle"))
	assert.Equal(t, workerBefore, tbCountSubjects(tbInboxSubjects(t, h.worker), "you went idle"), "no nudge")
	assert.Equal(t, operatorBefore+1, tbOperatorSubjects(t, "went idle without reporting"))

	// Owners only: on a task the operator assigned, the owner hears and the
	// operator (the assigner) does not.
	h.setOptions(h.lead, map[string]string{"idle.tell": "owners"})
	byOperator := h.add("assigned by the operator", "next")
	code, body := h.post("", byOperator.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	leadBefore = tbCountSubjects(tbInboxSubjects(t, h.lead), "went idle")
	operatorBefore = tbOperatorSubjects(t, "went idle without reporting")
	agentd.SweepTaskboardIdleForTest(now.Add(40 * time.Minute))
	assert.Equal(t, 1, tbStalledEvents(t, byOperator.ID))
	assert.Equal(t, leadBefore+1, tbCountSubjects(tbInboxSubjects(t, h.lead), "went idle"))
	assert.Equal(t, operatorBefore, tbOperatorSubjects(t, "went idle without reporting"))

	// Telling no one still marks the task stalled.
	h.setOptions(h.lead, map[string]string{"idle.tell": "none"})
	third := h.assigned("third")
	leadBefore = tbCountSubjects(tbInboxSubjects(t, h.lead), "went idle")
	agentd.SweepTaskboardIdleForTest(now.Add(45 * time.Minute))
	assert.Equal(t, 1, tbStalledEvents(t, third.ID))
	assert.Equal(t, leadBefore, tbCountSubjects(tbInboxSubjects(t, h.lead), "went idle"))

	// A queued task (its dependency not done) never stalls. Once the
	// dependency is done the clock starts: the grace runs from then, not
	// from the assignment.
	dep := h.add("the dependency", "next")
	queued := h.assigned("queued work")
	rec := h.call("", http.MethodPatch, "/v1/taskboard/tasks/"+queued.ID, map[string]any{"add_deps": []string{dep.ID}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agentd.SweepTaskboardIdleForTest(now.Add(time.Hour))
	assert.Zero(t, tbStalledEvents(t, queued.ID))
	code, body = h.post("", dep.ID, "status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	closed := time.Now()
	agentd.SweepTaskboardIdleForTest(closed.Add(5 * time.Minute))
	assert.Zero(t, tbStalledEvents(t, queued.ID), "the wait on the dependency is not idling")
	agentd.SweepTaskboardIdleForTest(closed.Add(15 * time.Minute))
	assert.Equal(t, 1, tbStalledEvents(t, queued.ID))

	// The global kill switch: taskboard.idle_grace "0" overrules the board.
	saveTaskboardConfig(t, &config.TaskboardConfig{IdleGrace: "0"})
	killed := h.assigned("killed")
	agentd.SweepTaskboardIdleForTest(now.Add(2 * time.Hour))
	assert.Zero(t, tbStalledEvents(t, killed.ID))
	v := h.options("", http.MethodGet, nil, http.StatusOK)
	assert.True(t, v.Resolved.Idle.GloballyOff)
	assert.False(t, v.Resolved.Idle.Active)
}

// A task in a custom column waits on something outside its agent's turn and
// never stalls, unless the board lists the column in idle.check_columns.
func TestTaskboardIdleCheckColumns(t *testing.T) {
	h := newTaskboardIdleBoardFlow(t)
	now := time.Now()
	h.columns("", http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)
	h.columns("", http.MethodPost, "", map[string]any{"label": "In CI"}, http.StatusCreated)
	h.setOptions(h.lead, map[string]string{"idle.enabled": "on", "idle.grace": "1m"})
	inReview := h.assigned("in review")
	inCI := h.assigned("in CI")
	for id, col := range map[string]string{inReview.ID: "in-review", inCI.ID: "in-ci"} {
		code, body := h.post(h.worker, id, "stage", map[string]any{"stage": col})
		require.Equal(t, http.StatusOK, code, body)
	}
	agentd.SweepTaskboardIdleForTest(now.Add(10 * time.Minute))
	assert.Zero(t, tbStalledEvents(t, inReview.ID))
	assert.Zero(t, tbStalledEvents(t, inCI.ID))

	h.setOptions(h.lead, map[string]string{"idle.check_columns": "in-ci"})
	agentd.SweepTaskboardIdleForTest(now.Add(20 * time.Minute))
	assert.Zero(t, tbStalledEvents(t, inReview.ID))
	assert.Equal(t, 1, tbStalledEvents(t, inCI.ID))
}

// The board read path derives stalled from the same options: on a board
// that opts in with the global grace, the task shows as stalled and is
// counted; turning the check off clears it.
func TestTaskboardIdleCheckReadPath(t *testing.T) {
	h := newTaskboardIdleBoardFlow(t)
	saveTaskboardConfig(t, &config.TaskboardConfig{IdleGrace: "1ms"})
	task := h.assigned("watched")
	assert.False(t, h.stallView(task.ID).Stalled, "off by default")

	h.setOptions("", map[string]string{"idle.enabled": "on"})
	require.Eventually(t, func() bool { return h.stallView(task.ID).Stalled }, 5*time.Second, 5*time.Millisecond,
		"an idle assignee past the inherited 1ms grace stalls its task")
	assert.Equal(t, 1, h.contextCounts()["stalled"])

	h.setOptions("", map[string]string{"idle.enabled": "off"})
	assert.False(t, h.stallView(task.ID).Stalled)
	assert.Zero(t, h.contextCounts()["stalled"])
}

// The CLI shows, sets and resets a board's options.
func TestTaskboardOptionsCLI(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, &config.TaskboardConfig{IdleGrace: "3m"})
	bridgeAgentClientToMux(t, h.f.Mux) // as the operator
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}

	rc, out, errOut := run("options", "show", "crew")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "crew board options (all inherited)")
	assert.Contains(t, out, "idle check: off")
	assert.Regexp(t, `idle.grace\s+3m\s+global`, out)

	rc, out, errOut = run("options", "set", "crew", "idle.enabled=on", "idle.grace=10m", "idle.tell=assigner,operator")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "idle check: on")
	assert.Regexp(t, `idle.grace\s+10m\s+set on this board \(global: 3m\)`, out)
	assert.Regexp(t, `idle.tell\s+assigner,operator\s+set on this board`, out)
	assert.Contains(t, out, "version 1")

	rc, _, errOut = run("options", "set", "crew", "idle.grace")
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, errOut, "is not key=value")
	rc, _, errOut = run("options", "set", "crew", "idle.grace=1s")
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, errOut, "between")

	rc, out, errOut = run("options", "reset", "crew", "idle.grace")
	require.Equal(t, 0, rc, errOut)
	assert.Regexp(t, `idle.grace\s+3m\s+global`, out)

	rc, out, errOut = run("options", "show", "crew", "--json")
	require.Equal(t, 0, rc, errOut)
	var v tbOptions
	require.NoError(t, json.Unmarshal([]byte(out), &v), out)
	assert.True(t, v.Resolved.Idle.Active)
	assert.Equal(t, int64(2), v.Version)
}

// A linked node reads a board's options under taskboard.read and changes
// them under taskboard.write, as a local caller does.
func TestPeerTaskboardOptions(t *testing.T) {
	fh := newFedHarness(t)
	saveTaskboardConfig(t, nil)
	fh.f.HaveGroup("crew")
	h := &peerProcessHarness{fedHarness: fh}
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(view, testharness.JSONRequest(t, method, tbOptionsPath, body))
		return rec.Code, rec.Body.String()
	}
	patch := map[string]any{"set": map[string]string{"idle.enabled": "on"}}

	code, body := call(http.MethodGet, nil)
	assert.NotEqual(t, http.StatusOK, code, body)

	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	code, body = call(http.MethodGet, nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"resolved"`)
	code, body = call(http.MethodPatch, patch)
	assert.Equal(t, http.StatusForbidden, code, "read does not write: %s", body)

	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	code, body = call(http.MethodPatch, patch)
	require.Equal(t, http.StatusOK, code, body)
	var v tbOptions
	require.NoError(t, json.Unmarshal([]byte(body), &v))
	assert.True(t, v.Resolved.Idle.Active)
	assert.Contains(t, v.UpdatedBy, "peer:", "the change is recorded as the peer's")
	code, body = call(http.MethodPut, map[string]any{"options": map[string]any{}, "version": v.Version})
	require.Equal(t, http.StatusOK, code, body)

	crew, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	row, err := db.GetGroupTaskboardOptions(crew.ID)
	require.NoError(t, err)
	assert.Equal(t, "{}", row.Options)
	assert.Equal(t, v.Version+1, row.Version)
}
