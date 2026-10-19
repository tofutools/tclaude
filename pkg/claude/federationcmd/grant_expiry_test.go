package federationcmd

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestParseGrantExpiry(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	at, err := parseGrantExpiry("", "", now)
	require.NoError(t, err)
	require.Nil(t, at, "neither flag grants permanently")
	at, err = parseGrantExpiry("7d", "", now)
	require.NoError(t, err)
	require.Equal(t, now.Add(7*24*time.Hour), *at)
	at, err = parseGrantExpiry("2w", "", now)
	require.NoError(t, err)
	require.Equal(t, now.Add(14*24*time.Hour), *at)
	at, err = parseGrantExpiry("90m", "", now)
	require.NoError(t, err)
	require.Equal(t, now.Add(90*time.Minute), *at)
	at, err = parseGrantExpiry("", "2026-10-24T18:00:00Z", now)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC), at.UTC())
	_, err = parseGrantExpiry("", "2026-10-24", now)
	require.NoError(t, err)
	for _, bad := range [][2]string{{"0s", ""}, {"-1d", ""}, {"soon", ""}, {"", "2026-10-18"}, {"", "tomorrow"}, {"1d", "2026-10-24"}} {
		_, err := parseGrantExpiry(bad[0], bad[1], now)
		require.Error(t, err, bad)
	}
}

func TestExtendSendsOnlyTheExpiry(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var calls []string
	var bodies []map[string]any
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		calls = append(calls, method+" "+path)
		bodies = append(bodies, in.(map[string]any))
		return nil
	}
	var out, errOut bytes.Buffer
	require.NotZero(t, runExtend(&extendParams{Peer: "bob", Slug: "models.proxy"}, &out, &errOut), "one of --for, --until or --never")
	require.NotZero(t, runExtend(&extendParams{Peer: "bob", Slug: "models.proxy", For: "1d", Never: true}, &out, &errOut))
	require.Zero(t, runExtend(&extendParams{Peer: "bob", Slug: "models.proxy", Scope: "http_proxy=team", For: "1d"}, &out, &errOut), errOut.String())
	require.Zero(t, runExtend(&extendParams{Peer: "bob", Slug: "models.proxy", Scope: "http_proxy=team", Never: true}, &out, &errOut), errOut.String())
	require.Equal(t, []string{"PATCH /v1/federation/grants", "PATCH /v1/federation/grants"}, calls)
	require.Equal(t, "http_proxy=team", bodies[0]["scope"])
	require.NotNil(t, bodies[0]["expires_at"])
	require.Nil(t, bodies[1]["expires_at"])
	require.Contains(t, out.String(), "no longer expires")
}
