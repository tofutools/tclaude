package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestAWBReadyPickupLogsSelectedDispatch(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "open"}
		if r.URL.Path == "/api/issues/tcl-a1" {
			issue.Status = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ready" {
			if err := json.NewEncoder(w).Encode([]awbIssue{issue}); err != nil {
				t.Errorf("encode ready response: %v", err)
			}
			return
		}
		if r.URL.Path != "/api/issues/tcl-a1" {
			t.Errorf("request path = %q, want /api/issues/tcl-a1", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(issue); err != nil {
			t.Errorf("encode issue response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config:    config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir()},
		session:   &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
	}
	require.NoError(t, worker.tick(context.Background()))

	got := logs.String()
	assert.Contains(t, got, `"level":"INFO"`)
	assert.Contains(t, got, `"msg":"awb ready polling: picked up issue"`)
	assert.Contains(t, got, `"process":"builders"`)
	assert.Contains(t, got, `"workspace":"tcl"`)
	assert.Contains(t, got, `"issue":"tcl-a1"`)
	assert.Contains(t, got, `"agent_id":"agt_`)
	assert.Equal(t, 1, strings.Count(got, `"msg":"awb ready polling: picked up issue"`))
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch, "the closed issue releases the dispatch after logging its pickup")
}

func TestAWBReadyPickupDoesNotLogResumedDispatch(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", "agt_reserved")
	require.NoError(t, err)
	require.True(t, selected)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/issues/tcl-a1" {
			t.Errorf("request path = %q, want /api/issues/tcl-a1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "closed"}); err != nil {
			t.Errorf("encode issue response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config:    config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir()},
		session:   &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
	}
	require.NoError(t, worker.tick(context.Background()))
	assert.NotContains(t, logs.String(), "awb ready polling: picked up issue")
}

func TestAWBReadyInitialMessageLeavesClosureToOperator(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1", false)
	assert.Contains(t, message, "tclaude proxy awb show tcl-a1")
	assert.Contains(t, message, "record progress")
	assert.Contains(t, message, "Leave closing the issue to the operator")
	assert.NotContains(t, strings.ToLower(message), "close it")
}

func TestAWBReadyInitialMessageExplainsPRMonitoring(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1", true)
	assert.Contains(t, message, "awb update --pull-request-url")
	assert.Contains(t, message, "daemon will close the issue")
}

func TestAWBReadyMonitorClosesMergedPRAfterAgentSettles(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", "agt_worker")
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)

	var closed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress", PullRequestURL: "https://github.com/acme/repo/pull/42"}
		if r.Method == http.MethodPost && r.URL.Path == "/api/issues/tcl-a1/close" {
			closed = true
			issue.Status = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(issue))
	}))
	t.Cleanup(server.Close)

	previousMerged, previousSettled := awbReadyPRMerged, awbReadyAgentSettled
	awbReadyPRMerged = func(_ context.Context, rawURL string) (bool, bool, error) {
		assert.Equal(t, "https://github.com/acme/repo/pull/42", rawURL)
		return true, true, nil
	}
	awbReadyAgentSettled = func(agentID string) (bool, error) {
		assert.Equal(t, "agt_worker", agentID)
		return true, nil
	}
	t.Cleanup(func() { awbReadyPRMerged, awbReadyAgentSettled = previousMerged, previousSettled })

	worker := testAWBReadyMonitorWorker(t, server.URL)
	require.NoError(t, worker.tick(context.Background()))
	assert.True(t, closed)
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch, "successful automatic closure releases the serial polling process")
}

func TestAWBReadyMonitorWaitsWhileAgentWorks(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", "agt_worker")
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)

	var closeRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			closeRequests++
		}
		_ = json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress", PullRequestURL: "https://github.com/acme/repo/pull/42"})
	}))
	t.Cleanup(server.Close)

	previousMerged, previousSettled := awbReadyPRMerged, awbReadyAgentSettled
	awbReadyPRMerged = func(context.Context, string) (bool, bool, error) { return true, true, nil }
	awbReadyAgentSettled = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { awbReadyPRMerged, awbReadyAgentSettled = previousMerged, previousSettled })

	require.NoError(t, testAWBReadyMonitorWorker(t, server.URL).tick(context.Background()))
	assert.Zero(t, closeRequests)
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.NotNil(t, dispatch)
}

func TestAWBReadyMonitorDisabledDoesNotInspectPR(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", "agt_worker")
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress", PullRequestURL: "https://github.com/acme/repo/pull/42"})
	}))
	t.Cleanup(server.Close)
	previousMerged := awbReadyPRMerged
	awbReadyPRMerged = func(context.Context, string) (bool, bool, error) {
		t.Fatal("monitor_pr=false must not inspect the pull request")
		return false, false, nil
	}
	t.Cleanup(func() { awbReadyPRMerged = previousMerged })

	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	require.NoError(t, worker.tick(context.Background()))
}

func TestAWBReadyAgentSettledForMissingAgent(t *testing.T) {
	setupTestDB(t)
	settled, err := liveAWBReadyAgentSettled("agt_missing")
	require.NoError(t, err)
	assert.True(t, settled, "an absent actor cannot still be working")
}

func testAWBReadyMonitorWorker(t *testing.T, serverURL string) awbReadyWorker {
	t.Helper()
	base, fault := validateAWBBaseURL(serverURL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: serverURL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	return awbReadyWorker{process: "builders", workspace: "tcl",
		config:  config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir(), MonitorPR: true},
		session: &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}}}
}

func TestAWBReadyQueryIncludesWorkspaceLabelsAndLimit(t *testing.T) {
	query := awbReadyQuery("tcl", []string{"backend", "urgent"})
	assert.Equal(t, "tcl", query.Get("workspace"))
	assert.Equal(t, "1", query.Get("limit"))
	assert.Equal(t, []string{"backend", "urgent"}, query["label"])
}

func TestValidateAWBReadyPolling(t *testing.T) {
	policy := config.AWBProxyConfig{URL: "https://awb.example", Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	base := config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: "/repo"}
	d, err := validateAWBReadyPolling(policy, "builders", base)
	assert.NoError(t, err)
	assert.Equal(t, time.Minute, d)
	base.Interval = "15s"
	d, err = validateAWBReadyPolling(policy, "builders", base)
	assert.NoError(t, err)
	assert.Equal(t, 15*time.Second, d)
	base.Interval = ""
	badGroup := base
	badGroup.Group = ""
	_, err = validateAWBReadyPolling(policy, "builders", badGroup)
	assert.ErrorContains(t, err, "group")
	_, err = validateAWBReadyPolling(policy, "Builders", base)
	assert.ErrorContains(t, err, "lowercase")
	bad := base
	bad.Cwd = "relative"
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "absolute")
	bad = base
	bad.Interval = "tomorrow"
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "invalid interval")
	bad = base
	bad.Workspace = "other"
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "allowed_workspaces")
	policy.AllowWrite = false
	_, err = validateAWBReadyPolling(policy, "builders", base)
	assert.ErrorContains(t, err, "allow_write")
	policy.AllowWrite = true
	policy.Username = ""
	_, err = validateAWBReadyPolling(policy, "builders", base)
	assert.ErrorContains(t, err, "username")
	policy.Username = "worker"
	bad = base
	bad.Labels = []string{" bad label "}
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "invalid label")
	policy.URL = "file:///tmp/awb"
	_, err = validateAWBReadyPolling(policy, "builders", base)
	assert.ErrorContains(t, err, "invalid url")
}
