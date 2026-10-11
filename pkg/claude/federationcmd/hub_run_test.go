package federationcmd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/noderun"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubRunCLIGatesAndNoRetries(t *testing.T) {
	oldAvailable, oldRequest := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvailable, oldRequest })
	agent.DaemonAvailableImpl = func() bool { return true }
	file := filepath.Join(testutil.CanonicalTempDir(t), "script")
	require.NoError(t, os.WriteFile(file, []byte("printf operator-script"), 0600))
	id := strings.Repeat("a", 32)
	for _, tc := range []struct {
		p            hubRunParams
		method, tail string
	}{{hubRunParams{}, "GET", "run"}, {hubRunParams{File: file, Timeout: time.Minute, NoWait: true}, "POST", "run"}, {hubRunParams{Job: id}, "GET", "run/jobs/" + id}, {hubRunParams{Job: id, Log: "stdout", Offset: 3}, "GET", "run/jobs/" + id + "/logs?stream=stdout&offset=3"}} {
		called := false
		agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
			called = true
			require.Equal(t, tc.method, method)
			require.Equal(t, "/v1/federation/hub/"+tc.tail, path)
			require.True(t, opts.NoRetry)
			if method == "POST" {
				require.Equal(t, "printf operator-script", in.(noderun.Request).Script)
				*(out.(*noderun.Job)) = noderun.Job{ID: id, State: "running", TimeoutSeconds: 60}
			}
			return nil
		}
		var stdout, stderr bytes.Buffer
		require.Zero(t, runHubScript(&tc.p, &stdout, &stderr), stderr.String())
		require.True(t, called)
	}
}
