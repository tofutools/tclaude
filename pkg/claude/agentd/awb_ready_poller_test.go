package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestValidateAWBReadyPolling(t *testing.T) {
	policy := config.AWBProxyConfig{URL: "https://awb.example", Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	base := config.AWBReadyPollingConfig{Group: "builders", Cwd: "/repo"}
	d, err := validateAWBReadyPolling(policy, "tcl", base)
	assert.NoError(t, err)
	assert.Equal(t, time.Minute, d)
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
}
