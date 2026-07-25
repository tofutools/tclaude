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
	request func(map[string]any),
	requestErr error,
) {
	t.Helper()
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() {
		DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq
	})
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		require.Equal(t, http.MethodPost, method)
		require.Equal(t, "/v1/whoami/seance", path)
		body, ok := in.(map[string]any)
		require.True(t, ok, "request body type = %T", in)
		if request != nil {
			request(body)
		}
		if requestErr != nil {
			return requestErr
		}
		*(out.(*seanceResolveResp)) = resp
		return nil
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
	}, func(body map[string]any) {
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
	assert.Contains(t, out, "cwd:         "+cwd, "resumes from the predecessor's launch dir")
	assert.Contains(t, out, "what was the auth bug", "carries the question")
	assert.Contains(t, stderr.String(), "chain is only 2 generation(s) deep")
}

// The actual-run path builds the right plan and hands the captured
// answer back to the caller's stdout — verified through the swappable
// seanceRun boundary so no real harness is spawned.
func TestSeance_Run_InvokesRunnerWithResumePlan(t *testing.T) {
	const dead = "99999999-1111-1111-1111-111111111111"
	cwd := t.TempDir()
	stubSeanceDaemon(t, seanceResolveResp{
		Predecessor: dead,
		Harness:     "claude",
		Cwd:         cwd,
		Exact:       true,
	}, nil, nil)

	var captured seancePlan
	prev := seanceRun
	seanceRun = func(p seancePlan) error {
		captured = p
		_, _ = p.Stdout.Write([]byte("It was a nil session token on resume.\n"))
		return nil
	}
	t.Cleanup(func() { seanceRun = prev })

	var stdout, stderr bytes.Buffer
	rc := runSeance(&seanceParams{
		Question: "what did you learn?",
		Target:   dead,
		Timeout:  "30s",
	}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "rc; stderr=%s", stderr.String())

	assert.Equal(t, cwd, captured.Cwd, "runs in the predecessor's launch dir")
	assert.Equal(t, 30*time.Second, captured.Timeout, "honours --timeout")
	assert.Contains(t, strings.Join(captured.Argv, " "), "--resume "+dead, "resume argv")
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
