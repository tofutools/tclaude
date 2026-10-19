package federationcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestSharesListsReceivedAndGiven(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var paths []string
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		paths = append(paths, method+" "+path)
		body := `{"shares":[{"peer":"inst_forge","peer_name":"forge","gateway":"team-key","dialect":"anthropic","models":["claude-*"],"max_usd":20,"spent_usd":7.6}],"errors":{"inst_lab":"peer is unreachable"}}`
		return json.Unmarshal([]byte(body), out)
	}
	var out, errOut bytes.Buffer
	require.Zero(t, runShares(&sharesParams{}, &out, &errOut))
	require.Contains(t, out.String(), "forge")
	require.Contains(t, out.String(), "$7.60")
	require.Contains(t, out.String(), "$20.00")
	require.Contains(t, out.String(), "--model-proxy team-key@forge")
	require.Contains(t, errOut.String(), "could not ask inst_lab")
	out.Reset()
	require.Zero(t, runShares(&sharesParams{Given: true}, &out, &errOut))
	require.NotContains(t, out.String(), "--model-proxy")
	require.Equal(t, []string{"GET /v1/models/shares/received", "GET /v1/models/shares"}, paths)
}
