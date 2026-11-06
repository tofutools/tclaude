package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --remote sends the cron and trigger verbs through the peer view, prints
// the untrusted-content banner and strips control characters from the
// peer's answer.
func TestRemoteScheduleVerbsGoThroughThePeerView(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	var calls []string
	answers := map[string]string{
		"/v1/federation/peer/bob/v1/cron":           `{"jobs":[{"id":3,"target_kind":"group","group_name":"crew","interval_seconds":3600,"body":"stand\u001b[2Jup","action_kind":"message","enabled":true}]}`,
		"/v1/federation/peer/bob/v1/cron/3/logs":    `{"runs":[{"id":1,"job_id":3,"fired_at":"2026-11-06T00:00:00Z","status":"o\u001bk"}]}`,
		"/v1/federation/peer/bob/v1/triggers":       `{"triggers":[{"id":5,"name":"rev\u001b]0;x\u0007iews","scope":"group","group":"crew","source":"pr.opened","actions":[]}]}`,
		"/v1/federation/peer/bob/v1/triggers/5":     `{"id":5,"name":"reviews","scope":"group","group":"crew","source":"pr.opened","actions":[]}`,
		"/v1/federation/peer/bob/v1/cron/3/disable": ``,
	}
	DaemonRequestImpl = func(method, path string, _, out any, _ DaemonOpts) error {
		calls = append(calls, method+" "+path)
		answer, ok := answers[path]
		require.True(t, ok, "unexpected path %s", path)
		if out != nil && answer != "" {
			return json.Unmarshal([]byte(answer), out)
		}
		return nil
	}
	run := func(f func(stdout, stderr *bytes.Buffer) int) string {
		var stdout, stderr bytes.Buffer
		require.Equal(t, rcOK, f(&stdout, &stderr), stderr.String())
		assert.NotContains(t, stdout.String(), "\x1b", "control characters stripped")
		assert.NotContains(t, stdout.String(), "\a")
		return stdout.String()
	}

	out := run(func(o, e *bytes.Buffer) int { return runCronLs(&cronLsParams{Remote: "bob"}, o, e) })
	assert.Contains(t, out, "Remote cron jobs of bob, as that node reports it (untrusted content)")
	assert.Contains(t, out, "group:crew")
	out = run(func(o, e *bytes.Buffer) int { return runCronLogs(&cronLogsParams{ID: "3", Remote: "bob"}, o, e) })
	assert.Contains(t, out, "untrusted content")
	out = run(func(o, e *bytes.Buffer) int { return runCronSetEnabled("3", "bob", false, o, e) })
	assert.Contains(t, out, "Disabled cron job #3 on bob")
	out = run(func(o, e *bytes.Buffer) int { return runTriggersLs(o, e, "bob") })
	assert.Contains(t, out, "Remote trigger rules of bob")
	out = run(func(o, e *bytes.Buffer) int { return runTriggersShow(o, e, "5", "bob") })
	assert.Contains(t, out, "Trigger:   reviews")
	assert.Equal(t, []string{
		http.MethodGet + " /v1/federation/peer/bob/v1/cron",
		http.MethodGet + " /v1/federation/peer/bob/v1/cron/3/logs",
		http.MethodPost + " /v1/federation/peer/bob/v1/cron/3/disable",
		http.MethodGet + " /v1/federation/peer/bob/v1/triggers",
		http.MethodGet + " /v1/federation/peer/bob/v1/triggers",
		http.MethodGet + " /v1/federation/peer/bob/v1/triggers/5",
	}, calls)
}
