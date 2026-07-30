package agent

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubSeanceDaemon(
	t *testing.T,
	resp seanceResolveResp,
	request func(string, map[string]any, DaemonOpts),
	requestErr error,
) {
	t.Helper()
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() {
		DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq
	})
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		require.Equal(t, http.MethodPost, method)
		body, ok := in.(map[string]any)
		require.True(t, ok, "request body type = %T", in)
		if request != nil {
			request(path, body, opts)
		}
		if requestErr != nil {
			return requestErr
		}
		switch path {
		case "/v1/whoami/seance":
			*(out.(*seanceResolveResp)) = resp
			return nil
		case "/v1/whoami/seance/run":
			*(out.(*seanceRunResp)) = seanceRunResp{
				Answer:      "It was a nil session token on resume.\n",
				Predecessor: resp.Predecessor,
				Harness:     resp.Harness,
			}
			return nil
		default:
			return &DaemonError{Status: http.StatusNotFound, Code: "not_found", Msg: path}
		}
	}
}

// A first-generation agent (never reincarnated) has no one to consult.
func TestSeance_NoPredecessorIsAClearError(t *testing.T) {
	stubSeanceDaemon(t, seanceResolveResp{}, nil, &DaemonError{
		Status: http.StatusNotFound,
		Code:   "not_found",
		Msg:    "you have no predecessor to consult",
	})
	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{PrintCmd: true}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, rcNotFound, rc, "rc")
	assert.Contains(t, stderr.String(), "no predecessor", "explains the empty grave")
}

func TestSeance_MapsDaemonPlanningErrors(t *testing.T) {
	tests := []struct {
		name string
		code string
		want int
	}{
		{name: "permission", code: "permission", want: rcAuth},
		{name: "unsupported harness", code: "unsupported_harness", want: rcInvalidArg},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubSeanceDaemon(t, seanceResolveResp{}, nil, &DaemonError{
				Status: http.StatusConflict,
				Code:   tc.code,
				Msg:    "planning failed",
			})
			var stdout, stderr bytes.Buffer
			rc := runSeance(&seanceParams{PrintCmd: true}, strings.NewReader(""), &stdout, &stderr)
			assert.Equal(t, tc.want, rc)
			assert.Contains(t, stderr.String(), "planning failed")
		})
	}
}

// --print-cmd resolves everything and prints the resume command + cwd
// without running anything (the free, no-cost targeting check).
func TestSeance_PrintCmd_BuildsHeadlessResumeArgv(t *testing.T) {
	const dead = "ffffffff-1111-1111-1111-111111111111"
	cwd := t.TempDir()
	stubSeanceDaemon(t, seanceResolveResp{
		Predecessor: dead,
		Harness:     "claude",
		Cwd:         cwd,
		Hops:        2,
		Requested:   3,
		Sandbox:     "off",
		Approval:    "auto",
	}, func(path string, body map[string]any, _ DaemonOpts) {
		assert.Equal(t, "/v1/whoami/seance", path)
		assert.Equal(t, "agt_deadbeef", body["target"])
		assert.Equal(t, 3, body["back"])
	}, nil)

	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "what was the auth bug you were chasing?",
		Target:   "agt_deadbeef",
		Back:     3,
		PrintCmd: true,
	}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "--resume "+dead, "resumes the dead conv")
	assert.Contains(t, out, "-p", "headless print mode")
	assert.Contains(t, out, "--no-session-persistence", "does not mutate the dead conversation")
	assert.Contains(t, out, "--permission-mode auto", "shows the recorded approval posture")
	assert.Contains(t, out, `"enabled":false`, "shows the recorded sandbox posture")
	assert.Contains(t, out, "cwd:         "+cwd, "resumes from the predecessor's launch dir")
	assert.Contains(t, out, "what was the auth bug", "carries the question")
	assert.Contains(t, stderr.String(), "chain is only 2 generation(s) deep")
}

func TestSeance_PrintCmd_ClaudeIncludesTmuxHostControlDeny(t *testing.T) {
	const dead = "eeeeeeee-1111-1111-1111-111111111111"
	const socketPath = "/daemon-resolved/tmux-1000/tclaude"
	t.Setenv("TMUX_TMPDIR", "relative-client-value-must-not-be-used")
	stubSeanceDaemon(t, seanceResolveResp{
		Predecessor:              dead,
		Harness:                  "claude",
		Cwd:                      t.TempDir(),
		Sandbox:                  "inherit",
		ClaudeTmuxSocketDenyPath: socketPath,
	}, nil, nil)

	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "what changed?",
		PrintCmd: true,
	}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), socketPath,
		"preview must include the same exact tmux deny as execution")
	assert.Contains(t, stdout.String(), "denyRead")
}

// The actual-run path pins execution to the exact planned generation and asks
// agentd to cross the managed-sandbox boundary. No local harness subprocess is
// spawned by this CLI.
func TestSeance_Run_InvokesDaemonWithPinnedResumePlan(t *testing.T) {
	const dead = "99999999-1111-1111-1111-111111111111"
	t.Setenv("TMUX_TMPDIR", "relative-client-value-must-not-affect-run")
	cwd := t.TempDir()
	var runBody map[string]any
	var runOpts DaemonOpts
	stubSeanceDaemon(t, seanceResolveResp{
		Predecessor: dead,
		Harness:     "claude",
		Cwd:         cwd,
		Exact:       true,
	}, func(path string, body map[string]any, opts DaemonOpts) {
		if path == "/v1/whoami/seance/run" {
			runBody = body
			runOpts = opts
		}
	}, nil)

	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "what did you learn?",
		Target:   dead,
		Timeout:  "30s",
	}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())

	require.NotNil(t, runBody, "billable run request was sent")
	assert.Equal(t, dead, runBody["target"], "execution is pinned to the planned exact generation")
	assert.Equal(t, "what did you learn?", runBody["question"])
	assert.Equal(t, int64((30 * time.Second).Milliseconds()), runBody["timeout_ms"])
	assert.Equal(t, 30*time.Second+30*time.Second, runOpts.Timeout, "HTTP deadline outlives harness timeout")
	assert.Contains(t, stdout.String(), "nil session token", "answer reaches the successor")
}

// An unparseable --timeout is rejected before any summoning happens.
func TestSeance_RejectsBadTimeout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "hi",
		Target:   "whatever",
		Timeout:  "soon",
	}, strings.NewReader(""), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc, "rc")
	assert.Contains(t, stderr.String(), "invalid --timeout", "explains why")
}
