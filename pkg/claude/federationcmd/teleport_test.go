package federationcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestTeleportControlCLIReadAndSwitch(t *testing.T) {
	old := agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonRequestImpl = old })
	var methods []string
	var bodies []any
	agent.DaemonRequestImpl = func(method, path string, in, out any, _ agent.DaemonOpts) error {
		require.Equal(t, "/v1/federation/teleport", path)
		methods = append(methods, method)
		bodies = append(bodies, in)
		return json.Unmarshal([]byte(`{"disabled":false}`), out)
	}
	var stdout, stderr bytes.Buffer
	for _, mode := range []string{"status", "off", "on"} {
		require.Zero(t, runTeleportControl(mode, &stdout, &stderr), stderr.String())
	}
	require.Equal(t, []string{"GET", "PUT", "PUT"}, methods)
	require.Nil(t, bodies[0])
	require.Equal(t, map[string]bool{"disabled": true}, bodies[1])
	require.Equal(t, map[string]bool{"disabled": false}, bodies[2])
	require.NotZero(t, runTeleportControl("bad", &stdout, &stderr))
	require.Len(t, methods, 3)
	require.NotNil(t, teleportControlCmd().Commands())
}
