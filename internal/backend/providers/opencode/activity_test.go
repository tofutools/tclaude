//go:build linux || darwin

package opencode

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func assertNativeSessionActivity(t *testing.T, runtime ports.Runtime, path string) {
	t.Helper()
	for _, test := range []struct {
		name, body string
		want       ports.AgentActivityObservedState
	}{
		{"unavailable", `{}`, ports.AgentActivityUnknown},
		{"busy", `{"/session/status":{"ses_test":{"type":"busy"}},"/question":[],"/permission":[]}`, ports.AgentActivityActive},
		{"retry", `{"/session/status":{"ses_test":{"type":"retry"}},"/question":[],"/permission":[]}`, ports.AgentActivityActive},
		{"question", `{"/session/status":{"ses_test":{"type":"busy"}},"/question":[{"sessionID":"ses_test"}],"/permission":[]}`, ports.AgentActivityAwaitingInput},
		{"permission", `{"/session/status":{},"/question":[],"/permission":[{"sessionID":"ses_test"}]}`, ports.AgentActivityAwaitingInput},
		{"idle omitted by native server", `{"/session/status":{},"/question":[],"/permission":[]}`, ports.AgentActivityIdle},
		{"idle ignores other sessions", `{"/session/status":{"ses_test":{"type":"idle"}},"/question":[{"sessionID":"ses_child"}],"/permission":[]}`, ports.AgentActivityIdle},
		{"unknown status", `{"/session/status":{"ses_test":{"type":"new-state"}},"/question":[],"/permission":[]}`, ports.AgentActivityUnknown},
		{"missing attention", `{"/session/status":{},"/question":[]}`, ports.AgentActivityUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(test.body), 0o600))
			result, err := runtime.Observe(context.Background())
			require.NoError(t, err)
			require.Equal(t, test.want, result.AgentActivity)
			require.Equal(t, test.want == ports.AgentActivityUnknown, result.AgentActivityObservedAt.IsZero())
		})
	}
}
