package agentd_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an agent's `tclaude run --memory ...` asks agentd for a run
// cgroup. agentd moves the socket peer (never a pid the body names) and holds
// the cgroup until the caller's connection closes.
func TestRunCgroup_HoldsCgroupForConnectionLifetime(t *testing.T) {
	newFlow(t)
	type call struct {
		pid    int
		limits sandboxpolicy.ResourceLimits
	}
	calls := make(chan call, 1)
	released := make(chan struct{})
	t.Cleanup(agentd.SetPrepareRunCgroupForTest(
		func(pid int, limits sandboxpolicy.ResourceLimits) (string, sandboxpolicy.ResourceLimits, func(), error) {
			calls <- call{pid: pid, limits: limits}
			applied := limits
			pids := uint64(32)
			applied.PIDs = &pids
			return "/sys/fs/cgroup/agentd.service/tclaude-run", applied, func() { close(released) }, nil
		}))
	mux := agentd.BuildHandlerForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, agentd.AsAgentPeerWithPID(r, "run-cgroup-conv", 4242))
	}))
	t.Cleanup(srv.Close)

	body, err := json.Marshal(agent.RunCgroupRequest{Limits: sandboxpolicy.ResourceLimits{Memory: "1GiB"}})
	require.NoError(t, err)
	resp, err := http.Post(srv.URL+"/v1/run/cgroup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out agent.RunCgroupResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "/sys/fs/cgroup/agentd.service/tclaude-run", out.Cgroup)
	require.NotNil(t, out.Limits.PIDs, "the response reports the limits actually applied")

	got := <-calls
	require.Equal(t, 4242, got.pid, "only the socket peer is moved")
	require.Equal(t, "1GiB", got.limits.Memory)

	select {
	case <-released:
		t.Fatal("the cgroup was released while the caller was still connected")
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, resp.Body.Close())
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the connection did not release the cgroup")
	}
}

func TestRunCgroup_ReportsUnavailableDelegation(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPrepareRunCgroupForTest(
		func(int, sandboxpolicy.ResourceLimits) (string, sandboxpolicy.ResourceLimits, func(), error) {
			return "", sandboxpolicy.ResourceLimits{}, func() {}, errors.New("no delegated cgroup")
		}))
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/run/cgroup", agent.RunCgroupRequest{})
	rec := testharness.Serve(agentd.BuildHandlerForTest(), agentd.AsAgentPeerWithPID(r, "run-cgroup-conv", 4242))
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "no delegated cgroup")
}

func TestRunCgroup_RefusesUnconfirmedCaller(t *testing.T) {
	newFlow(t)
	called := false
	t.Cleanup(agentd.SetPrepareRunCgroupForTest(
		func(int, sandboxpolicy.ResourceLimits) (string, sandboxpolicy.ResourceLimits, func(), error) {
			called = true
			return "", sandboxpolicy.ResourceLimits{}, func() {}, nil
		}))
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/run/cgroup", agent.RunCgroupRequest{})
	rec := testharness.Serve(agentd.BuildHandlerForTest(), agentd.AsUnconfirmedPeer(r))
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.False(t, called)
}
