package agentd

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// resolveDashboardPort picks the loopback port the dashboard + popup
// bind to: flag > config > random (0). A non-positive value at a tier
// means "not set here" and falls through.
func TestResolveDashboardPort(t *testing.T) {
	cfgWith := func(port int) *config.Config {
		return &config.Config{Agent: &config.AgentConfig{DashboardPort: port}}
	}
	cases := []struct {
		name     string
		flag     int
		cfg      *config.Config
		wantPort int
		wantSrc  string
	}{
		{"flag wins over config", 8080, cfgWith(9090), 8080, "flag"},
		{"config when no flag", 0, cfgWith(9090), 9090, "config"},
		{"default when neither set", 0, cfgWith(0), 0, "default (random)"},
		{"default when cfg nil", 0, nil, 0, "default (random)"},
		{"default when agent block nil", 0, &config.Config{}, 0, "default (random)"},
		{"non-positive flag is unset", -1, cfgWith(9090), 9090, "config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, src := resolveDashboardPort(tc.flag, tc.cfg)
			assert.Equal(t, tc.wantPort, port)
			assert.Equal(t, tc.wantSrc, src)
		})
	}
}

// freeTCPPort grabs an OS-assigned free loopback port and releases it,
// returning the number. Inherently racy (another process could grab it
// before the caller rebinds) but fine for these single-process tests.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// A random port (0) always binds and yields a loopback URL.
func TestStartPopupServer_RandomPort(t *testing.T) {
	srv, url, err := startPopupServer(defaultDashboardBind, 0)
	require.NoError(t, err)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })
	assert.Regexp(t, `^http://127\.0\.0\.1:\d+$`, url)
}

// A fixed port that is free binds to exactly that port — the stable URL
// the feature exists to provide.
func TestStartPopupServer_FixedPort(t *testing.T) {
	port := freeTCPPort(t)
	srv, url, err := startPopupServer(defaultDashboardBind, port)
	require.NoError(t, err)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })
	assert.Equal(t, fmt.Sprintf("http://127.0.0.1:%d", port), url)
}

// A fixed port already in use is a HARD error, not a silent fallback to
// a random port — the operator must learn the configured port is taken,
// since falling back would break the bookmark / reverse-proxy it backs.
func TestStartPopupServer_PortInUseIsFatal(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })
	port := occupied.Addr().(*net.TCPAddr).Port

	srv, url, err := startPopupServer(defaultDashboardBind, port)
	require.Error(t, err, "binding an in-use port must fail, not fall back")
	assert.Nil(t, srv)
	assert.Empty(t, url)
}

// An out-of-range port likewise fails the bind rather than degrading.
func TestStartPopupServer_OutOfRangeIsFatal(t *testing.T) {
	srv, url, err := startPopupServer(defaultDashboardBind, 70000)
	require.Error(t, err)
	assert.Nil(t, srv)
	assert.Empty(t, url)
}
