package session

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Falling back to a local write is only right when agentd was never reached.
// A daemon that answered has decided, or may already have applied the event.
func TestBrokerUnreachableFallsBackToDirect_OnlyWhenNeverReached(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
	read := &net.OpError{Op: "read", Net: "unix", Err: syscall.ECONNRESET}
	refused := &agentipc.BrokerStatusError{Status: http.StatusForbidden}

	t.Setenv(HookBrokerFallbackEnvVar, HookBrokerFallbackDirect)
	assert.True(t, BrokerUnreachableFallsBackToDirect(dial), "nobody listening")
	assert.False(t, BrokerUnreachableFallsBackToDirect(read), "the daemon had the request; it may have applied it")
	assert.False(t, BrokerUnreachableFallsBackToDirect(refused), "a refusal is a decision")
	assert.False(t, BrokerUnreachableFallsBackToDirect(
		&agentipc.BrokerStatusError{Status: http.StatusConflict}), "so is a stale-launch drop")
	assert.False(t, BrokerUnreachableFallsBackToDirect(nil))

	t.Setenv(HookBrokerFallbackEnvVar, "")
	assert.False(t, BrokerUnreachableFallsBackToDirect(dial),
		"a launch that cannot reach the database has nothing to fall back to")
}

// runBrokeredHook feeds one hook to the callback as a pane of a brokered
// launch whose agentd socket has nobody behind it.
func runBrokeredHook(t *testing.T, sessionID string, payload map[string]any) {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, _ = w.Write(data)
	require.NoError(t, w.Close())
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	t.Setenv("TCLAUDE_SESSION_ID", sessionID)
	t.Setenv(HookBrokerEnvVar, HookBrokerAgentd)
	t.Setenv(agentipc.SocketEnv, filepath.Join(t.TempDir(), "nobody-listens.sock"))
	require.NoError(t, runHookCallback(), "a hook never fails the harness's turn")
}

// A managed launch that can still reach the database keeps its status across
// a daemon restart: with agentd gone the event is applied locally. A launch
// without that permission drops it, as a walled launch always has.
func TestRunHookCallback_BrokeredLaunchSurvivesAnAbsentDaemon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fallback string
		want     string
	}{
		{"may write for itself", HookBrokerFallbackDirect, StatusExited},
		{"may not", "", StatusIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			db.ResetForTest()
			t.Cleanup(db.ResetForTest)
			require.NoError(t, SaveSessionState(&SessionState{
				ID: "spwn-fallback", ConvID: "conv-fallback", Status: StatusIdle,
			}))

			t.Setenv(HookBrokerFallbackEnvVar, tc.fallback)
			runBrokeredHook(t, "spwn-fallback", map[string]any{
				"session_id":      "conv-fallback",
				"hook_event_name": "SessionEnd",
				"reason":          "logout",
				"cwd":             dir,
			})

			state, err := LoadSessionState("spwn-fallback")
			require.NoError(t, err)
			assert.Equal(t, tc.want, state.Status)
		})
	}
}
