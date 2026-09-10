package agentd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestAWBReadyPickupLogIncludesProcessAndIssue(t *testing.T) {
	var logs bytes.Buffer
	handler := slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)
	logAWBReadyPickup(logger, "builders", "tcl-a1")

	got := logs.String()
	assert.Contains(t, got, `"level":"INFO"`)
	assert.Contains(t, got, `"msg":"awb ready polling: picked up issue"`)
	assert.Contains(t, got, `"process":"builders"`)
	assert.Contains(t, got, `"issue":"tcl-a1"`)
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
