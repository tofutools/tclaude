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

func TestAWBReadyPickupLogIncludesProcessAndIssue(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "open"}
		if r.URL.Path == "/api/issues/tcl-a1" {
			issue.Status = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ready" {
			require.NoError(t, json.NewEncoder(w).Encode([]awbIssue{issue}))
			return
		}
		require.Equal(t, "/api/issues/tcl-a1", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(issue))
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
}

func TestAWBReadyInitialMessageLeavesClosureToOperator(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1")
	assert.Contains(t, message, "tclaude proxy awb show tcl-a1")
	assert.Contains(t, message, "record progress")
	assert.Contains(t, message, "Leave closing the issue to the operator")
	assert.NotContains(t, strings.ToLower(message), "close it")
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
