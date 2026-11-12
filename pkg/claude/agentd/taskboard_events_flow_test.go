package agentd_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A board switches its messages off one event at a time, and names who
// gets each: an added recipient gets a note, nobody hears twice, and whoever
// caused the event gets no note.
func TestTaskboardEventMessages(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	count := func(conv, subject string) int { return tbCountSubjects(tbInboxSubjects(t, conv), subject) }

	// assigned off: the assignee gets no brief.
	h.setOptions(h.lead, map[string]string{"events.assigned.enabled": "off"})
	quiet := h.add("quiet assign", "next")
	code, body := h.post(h.lead, quiet.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	assert.Zero(t, count(h.worker, "assigned to you"))

	// assigned to the assignee and the owners: the operator assigns, the
	// owner (lead) gets a note; the operator, who caused it, does not.
	h.setOptions(h.lead, map[string]string{"events.assigned.enabled": "default", "events.assigned.to": "assignee,owners,operator"})
	noted := h.add("noted assign", "next")
	operatorBefore := tbOperatorSubjects(t, "assigned to")
	code, body = h.post("", noted.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, 1, count(h.worker, "assigned to you"))
	assert.Equal(t, 1, count(h.lead, "assigned to "), "the owner's note")
	assert.Equal(t, operatorBefore, tbOperatorSubjects(t, "assigned to"), "the operator caused it")

	// The lead assigns: the lead (owner) caused it and gets no note; the
	// operator does.
	third := h.add("lead's assign", "next")
	leadBefore := count(h.lead, "assigned to ")
	code, body = h.post(h.lead, third.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, leadBefore, count(h.lead, "assigned to "))
	assert.Equal(t, operatorBefore+1, tbOperatorSubjects(t, "assigned to"))

	// Reassigning away with unassigned off: the old assignee is not told.
	h.setOptions(h.lead, map[string]string{"events.unassigned.enabled": "off"})
	code, body = h.post(h.lead, third.ID, "assign", map[string]any{"to": h.peer})
	require.Equal(t, http.StatusOK, code, body)
	assert.Zero(t, count(h.worker, "reassigned by"))

	// Comments off: nobody hears.
	h.setOptions(h.lead, map[string]string{"events.comment.enabled": "off"})
	code, _ = h.comment("", noted.ID, map[string]any{"body": "first thought"})
	require.Equal(t, http.StatusCreated, code)
	assert.Zero(t, count(h.worker, "comment by"))

	// Comments to the owners only: the lead hears, the assignee does not.
	h.setOptions(h.lead, map[string]string{"events.comment.enabled": "on", "events.comment.to": "owners"})
	code, _ = h.comment(h.peer, noted.ID, map[string]any{"body": "second thought"})
	require.Equal(t, http.StatusCreated, code)
	assert.Zero(t, count(h.worker, "comment by"))
	assert.Equal(t, 1, count(h.lead, "comment by"))
	msgs := tbInboxBodies(t, h.lead, "comment by")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "second thought", "an added recipient gets the comment itself")

	// A failed report to the assigner, the owners and the operator: the
	// lead is both assigner and owner and hears once; the operator in
	// Messages.
	h.setOptions(h.lead, map[string]string{"events.report_failed.to": "assigner,owners,operator"})
	failed := h.add("will fail", "next")
	code, body = h.post(h.lead, failed.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	leadBefore = count(h.lead, "reported failed")
	operatorBefore = tbOperatorSubjects(t, "reported failed")
	code, body = h.post(h.worker, failed.ID, "report", map[string]any{"outcome": "failed", "note": "no bwrap"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, leadBefore+1, count(h.lead, "reported failed"))
	assert.Equal(t, operatorBefore+1, tbOperatorSubjects(t, "reported failed"))

	// report_done off: the assigner is not told.
	h.setOptions(h.lead, map[string]string{"events.report_done.enabled": "off"})
	done := h.add("will pass", "next")
	code, body = h.post(h.lead, done.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.worker, done.ID, "report", map[string]any{"outcome": "done", "note": "ok"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Zero(t, count(h.lead, "reported done"))

	// unblocked off: a task waiting on the operator's task is reopened
	// silently when the operator closes it.
	h.setOptions(h.lead, map[string]string{"events.unblocked.enabled": "off"})
	waiting := h.add("waits on the operator", "next")
	code, body = h.post(h.lead, waiting.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "a question", "to": "operator"})
	op := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)
	code, body = h.post(h.worker, waiting.ID, "status", map[string]any{"status": "blocked", "on": op.ID})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post("", op.ID, "status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Zero(t, count(h.worker, "unblocked:"))

	// Everything back to the defaults: the old messages return.
	h.options("", http.MethodPatch, map[string]any{"reset": []string{"events"}}, http.StatusOK)
	back := h.add("defaults again", "next")
	workerBefore := count(h.worker, "assigned to you")
	leadBefore = count(h.lead, "assigned to ")
	code, body = h.post("", back.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, workerBefore+1, count(h.worker, "assigned to you"))
	assert.Equal(t, leadBefore, count(h.lead, "assigned to "), "no owner notes by default")
}

// The CLI lists every message with its recipients and source.
func TestTaskboardEventMessagesCLI(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	bridgeAgentClientToMux(t, h.f.Mux) // as the operator
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}
	rc, out, errOut := run("options", "show", "crew")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "messages:")
	assert.Regexp(t, `events.comment\s+on\s+assignee,creator\s+default`, out)

	rc, out, errOut = run("options", "set", "crew", "events.comment.enabled=off", "events.report_failed.to=assigner,owners")
	require.Equal(t, 0, rc, errOut)
	assert.Regexp(t, `events.comment\s+off\s+assignee,creator\s+set on this board`, out)
	assert.Regexp(t, `events.report_failed\s+on\s+assigner,owners\s+set on this board`, out)

	rc, out, errOut = run("options", "reset", "crew", "events.comment")
	require.Equal(t, 0, rc, errOut)
	assert.Regexp(t, `events.comment\s+on\s+assignee,creator\s+default`, out)

	rc, _, errOut = run("options", "set", "crew", "events.nope.enabled=off")
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, errOut, "no such board option")
}

// tbInboxBodies are the bodies of conv's inbox messages whose subject holds
// contains.
func tbInboxBodies(t *testing.T, conv, contains string) []string {
	t.Helper()
	msgs, err := db.ListInboxForActor(conv, tbAgentID(t, conv), 50)
	require.NoError(t, err)
	var out []string
	for _, m := range msgs {
		if strings.Contains(m.Subject, contains) {
			out = append(out, m.Body)
		}
	}
	return out
}
