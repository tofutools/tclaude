package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestAWBReadyInitialMessageLeavesClosureToOperator(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1")
	assert.Contains(t, message, "tclaude proxy awb show tcl-a1")
	assert.Contains(t, message, "record progress")
	assert.Contains(t, message, "Leave closing the issue to the operator")
	assert.NotContains(t, strings.ToLower(message), "close it")
}

func TestValidateAWBReadyPolling(t *testing.T) {
	policy := config.AWBProxyConfig{URL: "https://awb.example", Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	base := config.AWBReadyPollingConfig{Group: "builders", Cwd: "/repo"}
	d, err := validateAWBReadyPolling(policy, "tcl", base)
	assert.NoError(t, err)
	assert.Equal(t, time.Minute, d)
	base.Interval = "15s"
	d, err = validateAWBReadyPolling(policy, "tcl", base)
	assert.NoError(t, err)
	assert.Equal(t, 15*time.Second, d)
	base.Interval = ""
	badGroup := base
	badGroup.Group = ""
	_, err = validateAWBReadyPolling(policy, "tcl", badGroup)
	assert.ErrorContains(t, err, "group")
	_, err = validateAWBReadyPolling(policy, "TCL", base)
	assert.ErrorContains(t, err, "lowercase")
	bad := base
	bad.Cwd = "relative"
	_, err = validateAWBReadyPolling(policy, "tcl", bad)
	assert.ErrorContains(t, err, "absolute")
	bad = base
	bad.Interval = "tomorrow"
	_, err = validateAWBReadyPolling(policy, "tcl", bad)
	assert.ErrorContains(t, err, "invalid interval")
	_, err = validateAWBReadyPolling(policy, "other", base)
	assert.ErrorContains(t, err, "allowed_workspaces")
	policy.AllowWrite = false
	_, err = validateAWBReadyPolling(policy, "tcl", base)
	assert.ErrorContains(t, err, "allow_write")
	policy.AllowWrite = true
	policy.Username = ""
	_, err = validateAWBReadyPolling(policy, "tcl", base)
	assert.ErrorContains(t, err, "username")
	policy.Username = "worker"
	policy.URL = "file:///tmp/awb"
	_, err = validateAWBReadyPolling(policy, "tcl", base)
	assert.ErrorContains(t, err, "invalid url")
}
