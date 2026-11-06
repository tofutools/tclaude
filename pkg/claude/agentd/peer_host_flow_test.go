package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// host.read shows a peer this node's load, memory and disk capacity, but a
// restricted peer never a host path: disks and their warnings go by kind, and
// messages and errors, which may name a path, are left out. Unrestricted
// trust reads the status as this node's operator does.
func TestPeerHostStatusWithoutPaths(t *testing.T) {
	h := newPeerProcessHarness(t)
	work := h.f.TestCwd("secret-work-dir")
	every := 100.0
	require.NoError(t, config.Save(&config.Config{Host: &config.HostConfig{WorkDirs: []string{work}, WarnDiskAvailablePercent: &every}}))
	agentd.RefreshHostMetricsForTest()

	code, body := h.call("GET", "/api/host/status", nil)
	require.Equal(t, 403, code, "no grant: %s", body)
	h.grant(t, agentd.PermHostRead, "")
	code, body = h.call("GET", "/api/host/status", nil)
	require.Equal(t, 200, code, body)
	require.NotContains(t, body, work, "no host path")
	require.NotContains(t, body, "secret-work-dir")
	var got struct {
		Status string `json:"status"`
		CPU    struct {
			LogicalCores int `json:"logical_cores"`
		} `json:"cpu"`
		Disks []struct {
			Kind       string `json:"kind"`
			TotalBytes uint64 `json:"total_bytes"`
		} `json:"disks"`
		Warnings []map[string]string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Equal(t, "current", got.Status)
	require.Positive(t, got.CPU.LogicalCores)
	kinds := map[string]bool{}
	for _, d := range got.Disks {
		kinds[d.Kind] = true
	}
	require.True(t, kinds["work"], "the work disk by kind: %s", body)
	lowDisk := false
	for _, w := range got.Warnings {
		require.NotContains(t, w, "message", "a warning's message may name a path")
		if w["code"] == "low_disk" {
			lowDisk = true
			require.Regexp(t, `^disk(:[a-z]+)?$`, w["resource"])
		}
	}
	require.True(t, lowDisk, "every disk is below a 100%% threshold: %s", body)

	h.trustUnrestricted(t)
	code, body = h.call("GET", "/api/host/status", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, work, "unrestricted trust reads it as this node's operator does")
}
