package federationcmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

func TestHubUpdateCLINeverRetriesMutationAndUsesHubRoutes(t *testing.T) {
	oldAvailable, oldRequest := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvailable, oldRequest })
	agent.DaemonAvailableImpl = func() bool { return true }
	id := strings.Repeat("b", 32)
	for _, tc := range []struct {
		p            hubUpdateParams
		method, tail string
	}{{hubUpdateParams{}, "GET", "update"}, {hubUpdateParams{Action: "check", NoWait: true}, "POST", "update"}, {hubUpdateParams{Action: "apply", Version: "v1.2.3", NoWait: true}, "POST", "update"}, {hubUpdateParams{Action: "rollback", NoWait: true}, "POST", "update"}, {hubUpdateParams{Job: id}, "GET", "update/jobs/" + id}} {
		called := false
		agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
			called = true
			require.Equal(t, tc.method, method)
			require.Equal(t, "/v1/federation/hub/"+tc.tail, path)
			require.True(t, opts.NoRetry)
			if method == "POST" {
				require.Equal(t, selfupdate.Request{Action: tc.p.Action, Version: tc.p.Version}, in)
				*(out.(*selfupdate.Job)) = selfupdate.Job{ID: id, State: "running"}
			}
			return nil
		}
		var stdout, stderr bytes.Buffer
		require.Zero(t, runHubUpdate(&tc.p, &stdout, &stderr), stderr.String())
		require.True(t, called)
	}
	called := false
	agent.DaemonRequestImpl = func(string, string, any, any, agent.DaemonOpts) error { called = true; return nil }
	var stdout, stderr bytes.Buffer
	require.NotZero(t, runHubUpdate(&hubUpdateParams{Action: "apply", Version: "https://evil/binary"}, &stdout, &stderr))
	require.False(t, called)
}
