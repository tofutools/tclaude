package agent

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retireStub swaps in a fake daemon for runRetire: it captures the
// request path (so the test can inspect the query the CLI built) and
// replies with the supplied response body, marshalled through JSON so it
// lands in runRetire's anonymous response struct exactly like a real
// daemon reply. Restores both seams via t.Cleanup.
func retireStub(t *testing.T, resp map[string]any) *string {
	t.Helper()
	prevAvail := DaemonAvailableImpl
	prevReq := DaemonRequestImpl
	t.Cleanup(func() {
		DaemonAvailableImpl = prevAvail
		DaemonRequestImpl = prevReq
	})
	var capturedPath string
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, path string, _, out any, _ DaemonOpts) error {
		capturedPath = path
		raw, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	}
	return &capturedPath
}

// queryOf pulls the query values out of a captured "/v1/.../retire?..."
// path so the test can assert on individual params.
func queryOf(t *testing.T, path string) url.Values {
	t.Helper()
	u, err := url.Parse(path)
	require.NoError(t, err, "captured path should parse")
	return u.Query()
}

// TestRunRetire_DefaultsDoBoth: a bare `retire <sel>` shuts the session
// down AND cleans up the worktree — the dashboard modal's default both
// ways. The CLI spells both params out rather than leaning on the
// server defaults.
func TestRunRetire_DefaultsDoBoth(t *testing.T) {
	path := retireStub(t, map[string]any{
		"conv_id":  "abcdef0123456789",
		"outcome":  map[string]any{"retired": true},
		"shutdown": map[string]any{"action": "soft_stopped"},
		"worktree": map[string]any{
			"action": "scheduled",
			"detail": "worktree + branch will be removed after the agent exits",
		},
	})

	var stdout, stderr bytes.Buffer
	rc := runRetire(&retireParams{Selector: "some-agent"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	q := queryOf(t, *path)
	assert.Equal(t, "1", q.Get("shutdown"), "default retire shuts down")
	assert.Equal(t, "1", q.Get("delete_worktree"), "default retire deletes the worktree")

	out := stdout.String()
	assert.Contains(t, out, "sent /exit", "should report the shutdown")
	assert.Contains(t, out, "worktree + branch will be removed after the agent exits",
		"should surface the worktree outcome detail")
}

// TestRunRetire_NoDeleteWorktree: --no-delete-worktree sends
// delete_worktree=0 and (the server omits the worktree block, so) prints
// no worktree line. Shutdown still defaults on.
func TestRunRetire_NoDeleteWorktree(t *testing.T) {
	path := retireStub(t, map[string]any{
		"conv_id":  "abcdef0123456789",
		"outcome":  map[string]any{"retired": true},
		"shutdown": map[string]any{"action": "soft_stopped"},
		// no "worktree" key — server only includes it when requested
	})

	var stdout, stderr bytes.Buffer
	rc := runRetire(&retireParams{Selector: "some-agent", NoDeleteWorktree: true}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	q := queryOf(t, *path)
	assert.Equal(t, "1", q.Get("shutdown"), "shutdown still defaults on")
	assert.Equal(t, "0", q.Get("delete_worktree"), "--no-delete-worktree opts out")
	assert.NotContains(t, stdout.String(), "worktree:", "no worktree line when none requested")
}

// TestRunRetire_NoShutdown: --no-shutdown sends shutdown=0 but the
// worktree default still rides along (delete_worktree=1); the server is
// trusted to keep a live agent's worktree, so the two flags stay
// independent rather than the CLI coupling them.
func TestRunRetire_NoShutdown(t *testing.T) {
	path := retireStub(t, map[string]any{
		"conv_id": "abcdef0123456789",
		"outcome": map[string]any{"retired": true},
		"worktree": map[string]any{
			"action": "kept",
			"detail": "worktree kept — session still running (retire without shutdown)",
		},
	})

	var stdout, stderr bytes.Buffer
	rc := runRetire(&retireParams{Selector: "some-agent", NoShutdown: true}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	q := queryOf(t, *path)
	assert.Equal(t, "0", q.Get("shutdown"), "--no-shutdown opts out of shutdown")
	assert.Equal(t, "1", q.Get("delete_worktree"), "worktree default is independent of shutdown")
	assert.Contains(t, stdout.String(), "worktree kept", "should surface the kept-worktree note")
}

// TestRunRetire_WorktreeNoneStaysSilent: action "none" (agent had no
// worktree) must not print a noisy "worktree: no worktree" line, even
// though delete_worktree was requested by default.
func TestRunRetire_WorktreeNoneStaysSilent(t *testing.T) {
	retireStub(t, map[string]any{
		"conv_id":  "abcdef0123456789",
		"outcome":  map[string]any{"retired": true},
		"shutdown": map[string]any{"action": "skipped:already_offline"},
		"worktree": map[string]any{"action": "none", "detail": "no worktree"},
	})

	var stdout, stderr bytes.Buffer
	rc := runRetire(&retireParams{Selector: "some-agent"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.NotContains(t, stdout.String(), "worktree:", "action=none should stay silent")
}

// TestRunRetire_EmptySelectorRejected: a blank selector bails before any
// daemon I/O.
func TestRunRetire_EmptySelectorRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runRetire(&retireParams{Selector: "   "}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, strings.ToLower(stderr.String()), "selector is required")
}
