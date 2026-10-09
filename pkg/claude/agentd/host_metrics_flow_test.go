package agentd_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/hostmetrics"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestHostMetricsReadPermissionAndCache(t *testing.T) {
	f := newFlow(t)
	f.HaveConvWithTitle("host-reader", "host reader")
	r := accountQuery(t, f, "host-reader", "/v1/host/status")
	require.Equal(t, 403, r.Code)
	require.NoError(t, db.GrantAgentPermission("host-reader", agentd.PermHostRead, "test"))
	r = accountQuery(t, f, "host-reader", "/v1/host/status")
	require.Equal(t, 200, r.Code)
	require.Contains(t, r.Body.String(), `"status":"warming"`)
	root := f.TestCwd("work")
	future := filepath.Join(root, "future")
	f.HaveGroup("team")
	_, err := db.SetAgentGroupDefaultCwd("team", future)
	require.NoError(t, err)
	cfg := &config.Config{Host: &config.HostConfig{WorkDirs: []string{future}}}
	require.NoError(t, config.Save(cfg))
	f.HaveAliveSession("host-reader", "host-one", "tclaude-host-one", root)
	f.HaveConvWithTitle("host-idle", "host idle")
	f.HaveAliveSession("host-idle", "host-two", "tclaude-host-two", root)
	f.HaveConvWithTitle("host-exited", "host exited")
	f.HaveAliveSession("host-exited", "host-three", "tclaude-host-three", root)
	f.SetSessionStatus("host-exited", "exited")
	f.HaveMember("team", "host-reader")
	f.HaveMember("team", "host-idle")
	f.HaveMember("team", "host-exited")
	f.HaveConvWithTitle("host-unmanaged", "unmanaged wrapper session")
	f.HaveAliveSession("host-unmanaged", "host-four", "tclaude-host-four", root)
	agentd.RefreshHostMetricsForTest()
	r = accountQuery(t, f, "host-reader", "/v1/host/status")
	require.Equal(t, 200, r.Code, r.Body.String())
	var s struct {
		hostmetrics.Snapshot
		Status   string                `json:"status"`
		Warnings []hostmetrics.Warning `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &s))
	require.Equal(t, "current", s.Status)
	require.NotNil(t, s.Tclaude)
	require.Equal(t, 2, s.Tclaude.LiveAgents)
	require.Equal(t, 3, s.Tclaude.LiveSessions)
	count := 0
	for _, d := range s.Disks {
		if d.Path.Path == future {
			count++
			require.Equal(t, root, d.MeasuredPath)
			require.Empty(t, d.Error)
		}
	}
	require.Equal(t, 1, count, "group and extra directory are deduped")
	observed := s.ObservedAt
	f.SetSessionStatus("host-idle", "exited")
	r = accountQuery(t, f, "host-reader", "/v1/host/status")
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &s))
	require.Equal(t, observed, s.ObservedAt)
	require.Equal(t, 3, s.Tclaude.LiveSessions, "requests only read the cache")
	agentd.RefreshHostMetricsForTest()
	r = accountQuery(t, f, "host-reader", "/v1/host/status")
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &s))
	require.Equal(t, 2, s.Tclaude.LiveSessions)
	r = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/permissions/deny", map[string]any{"target": "host-reader", "slug": agentd.PermHostRead})))
	require.Equal(t, 200, r.Code)
	r = accountQuery(t, f, "host-reader", "/v1/host/status")
	require.Equal(t, 403, r.Code)
	r = accountQuery(t, f, "", "/v1/host/status")
	require.Equal(t, 200, r.Code)
	r = testharness.Serve(f.Mux, agentd.AsUnconfirmedPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/host/status", nil)))
	require.Equal(t, 403, r.Code)
}

func TestHostMetricsFreshRead(t *testing.T) {
	f := newFlow(t)
	f.HaveConvWithTitle("host-fresh-reader", "host reader")
	r := accountQuery(t, f, "host-fresh-reader", "/v1/host/status?fresh=1")
	require.Equal(t, 403, r.Code, "fresh does not bypass host.read")
	require.NoError(t, db.GrantAgentPermission("host-fresh-reader", agentd.PermHostRead, "test"))
	r = accountQuery(t, f, "host-fresh-reader", "/v1/host/status?fresh=1")
	require.Equal(t, 200, r.Code, r.Body.String())
	require.Contains(t, r.Body.String(), `"status":"current"`)
	r = accountQuery(t, f, "host-fresh-reader", "/v1/host/status?fresh=1")
	require.Equal(t, 429, r.Code)
}
