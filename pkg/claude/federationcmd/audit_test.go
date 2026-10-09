package federationcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestFederationAuditCLIFlagsFilteringAndRendering(t *testing.T) {
	available, request := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = available, request })
	agent.DaemonAvailableImpl = func() bool { return true }
	calls := 0
	agent.DaemonRequestImpl = func(method, path string, in, out any, _ agent.DaemonOpts) error {
		calls++
		require.Equal(t, http.MethodGet, method)
		u, err := url.Parse(path)
		require.NoError(t, err)
		require.Equal(t, "/v1/federation/audit", u.Path)
		require.Equal(t, "laptop name", u.Query().Get("peer"))
		require.Equal(t, "10", u.Query().Get("limit"))
		since, err := time.Parse(time.RFC3339Nano, u.Query().Get("since"))
		require.NoError(t, err)
		require.WithinDuration(t, time.Now().Add(-24*time.Hour), since, time.Second)
		*out.(*[]db.FederationActivity) = []db.FederationActivity{{ID: "audit:1", At: time.Now(), Direction: "out", Peer: "peer-a", Kind: "mail", State: "accepted", Actor: "bad\x1b[31m\nlabel"}}
		return nil
	}
	var stdout, stderr bytes.Buffer
	p := &auditParams{Peer: "laptop name", Since: "24h", Limit: 10}
	require.Zero(t, runAudit(p, &stdout, &stderr), stderr.String())
	require.Contains(t, stdout.String(), "mail")
	require.NotContains(t, stdout.String(), "\x1b")
	require.NotContains(t, stdout.String(), "\nlabel")
	stdout.Reset()
	p.JSON = true
	require.Zero(t, runAudit(p, &stdout, &stderr), stderr.String())
	var rows []db.FederationActivity
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "audit:1", rows[0].ID)
	before := calls
	for _, since := range []string{"bad", "-1h", "0s"} {
		p.Since = since
		require.NotZero(t, runAudit(p, &stdout, &stderr))
	}
	require.Equal(t, before, calls, "invalid filters must not call the daemon")
	cmd := auditCmd()
	for _, name := range strings.Fields("peer since json limit") {
		require.NotNil(t, cmd.Flags().Lookup(name))
	}
}
