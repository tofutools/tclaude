package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
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
		"/v1/federation/peer/bob/v1/cron":           `{"jobs":[{"id":3,"target_kind":"group","group_name":"crew","interval_seconds":3600,"body":"stand\u001b[2Jup\nError: forged","action_kind":"message","enabled":true}]}`,
		"/v1/federation/peer/bob/v1/cron/3/logs":    `{"runs":[{"id":1,"job_id":3,"fired_at":"2026-11-06T00:00:00Z","status":"o\u001bk"}]}`,
		"/v1/federation/peer/bob/v1/triggers":       `{"triggers":[{"id":5,"name":"rev\u001b]0;x\u0007iews","scope":"group","group":"crew","source":"pr.opened","actions":[]}]}`,
		"/v1/federation/peer/bob/v1/triggers/5":     `{"id":5,"name":"reviews","scope":"group","group":"crew","source":"pr.opened","actions":[{"type":"spawn"},{"type":"message"}]}`,
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
	assert.NotContains(t, out, "\nError: forged", "a peer's text stays on its row")
	out = run(func(o, e *bytes.Buffer) int { return runCronLogs(&cronLogsParams{ID: "3", Remote: "bob"}, o, e) })
	assert.Contains(t, out, "untrusted content")
	out = run(func(o, e *bytes.Buffer) int { return runCronSetEnabled("3", "bob", false, o, e) })
	assert.Contains(t, out, "Disabled cron job #3 on bob")
	out = run(func(o, e *bytes.Buffer) int { return runTriggersLs(o, e, "bob") })
	assert.Contains(t, out, "Remote trigger rules of bob")
	out = run(func(o, e *bytes.Buffer) int { return runTriggersShow(o, e, "5", "bob") })
	assert.Contains(t, out, "Trigger:   reviews", "an action without its body does not crash the print")
	assert.Equal(t, []string{
		http.MethodGet + " /v1/federation/peer/bob/v1/cron",
		http.MethodGet + " /v1/federation/peer/bob/v1/cron/3/logs",
		http.MethodPost + " /v1/federation/peer/bob/v1/cron/3/disable",
		http.MethodGet + " /v1/federation/peer/bob/v1/triggers",
		http.MethodGet + " /v1/federation/peer/bob/v1/triggers",
		http.MethodGet + " /v1/federation/peer/bob/v1/triggers/5",
	}, calls)
}

// A job scheduled on a linked node is a message job, sent through its peer
// view; its own verbs follow it there.
func TestRemoteCronJobVerbs(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	var calls []string
	var sent map[string]any
	DaemonRequestImpl = func(method, path string, body, out any, _ DaemonOpts) error {
		calls = append(calls, method+" "+path)
		if b, ok := body.(map[string]any); ok {
			sent = b
		}
		switch {
		case method == http.MethodPost && path == "/v1/federation/peer/bob/v1/cron":
			return json.Unmarshal([]byte(`{"id":9,"target_kind":"group","group_name":"crew","interval_seconds":3600,"body":"ping","action_kind":"message","enabled":true,"owner_peer":"inst_x"}`), out)
		case strings.HasSuffix(path, "/run-now"):
			return json.Unmarshal([]byte(`{"status":"ok"}`), out)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	rc := runCronAdd(&cronAddParams{Target: "group:crew", Interval: "1h", Body: "ping", Remote: "bob"}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Contains(t, stdout.String(), "Remote cron job of bob")
	assert.Contains(t, stdout.String(), "Scheduled job #9 every 1h0m0s")
	assert.Equal(t, "message", sent["action_kind"])
	assert.NotContains(t, sent, "spawn_profile")

	stdout.Reset()
	stderr.Reset()
	rc = runCronAdd(&cronAddParams{Target: "group:crew", Interval: "1h", Action: "spawn", Instruction: "x", SpawnProfile: "p", Remote: "bob"}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "messages only")
	stderr.Reset()
	rc = runCronAdd(&cronAddParams{Interval: "1h", Body: "ping", Remote: "bob"}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc, "no self target on a peer")

	stdout.Reset()
	require.Equal(t, rcOK, runCronRunNow(&cronIDOnlyParams{ID: "9", Remote: "bob"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "Fired cron job #9 on bob (status: ok)")
	require.Equal(t, rcOK, runCronEnable(&cronIDOnlyParams{ID: "9", Remote: "bob"}, true, &stdout, &stderr))
	require.Equal(t, rcOK, runCronRm(&cronRmParams{ID: "9", Remote: "bob"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "Deleted cron job #9 on bob")
	assert.Equal(t, []string{
		"POST /v1/federation/peer/bob/v1/cron",
		"POST /v1/federation/peer/bob/v1/cron/9/run-now",
		"POST /v1/federation/peer/bob/v1/cron/9/enable",
		"DELETE /v1/federation/peer/bob/v1/cron/9",
	}, calls)
}
