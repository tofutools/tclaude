package federationcmd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"testing"
)

func TestBoardItemCLIHumanRoutesAndNoRetry(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl = oldAvail; agent.DaemonRequestImpl = oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	for _, tc := range []struct{ action, method, tail string }{{"items", "GET", ""}, {"publish", "POST", ""}, {"versions", "GET", "/item/versions"}, {"pin", "PUT", "/item/pin"}, {"fetch", "POST", "/item/versions/version/fetch"}, {"contents", "GET", "/item/versions/version/contents"}, {"preview", "POST", "/item/versions/version/preview"}, {"import", "POST", "/item/versions/version/import"}} {
		t.Run(tc.action, func(t *testing.T) {
			called := false
			agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
				called = true
				require.Equal(t, tc.method, method)
				require.Equal(t, "/v1/federation/boards/board/items"+tc.tail, path)
				require.True(t, opts.NoRetry)
				return nil
			}
			var out, err bytes.Buffer
			require.Zero(t, runBoardCommand(&boardCommandParams{Action: tc.action, Board: "board", Item: "item", Version: "version", PreviewToken: "permit"}, &out, &err), err.String())
			require.True(t, called)
		})
	}
	var out, err bytes.Buffer
	agent.DaemonRequestImpl = func(string, string, any, any, agent.DaemonOpts) error {
		t.Fatal("import without preview token cannot reach daemon")
		return nil
	}
	require.NotZero(t, runBoardCommand(&boardCommandParams{Action: "import", Board: "board", Item: "item", Version: "version"}, &out, &err))
	require.Contains(t, err.String(), "preview-token")
}
