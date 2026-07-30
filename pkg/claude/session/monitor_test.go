package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// monitorHook builds a PostToolUse payload the way Claude Code emits one
// for a `Monitor` call. The tool_response shape (taskId / timeoutMs /
// persistent) is undocumented upstream and was established empirically
// from a live transcript; these tests are what pins it.
func monitorHook(t *testing.T, toolName string, input, response any) HookCallbackInput {
	t.Helper()
	in, err := json.Marshal(input)
	require.NoError(t, err)
	var resp json.RawMessage
	if response != nil {
		resp, err = json.Marshal(response)
		require.NoError(t, err)
	}
	return HookCallbackInput{
		HookEventName: "PostToolUse",
		ToolName:      toolName,
		ToolInput:     in,
		ToolResponse:  resp,
	}
}

func TestMonitorLaunch_DecodesACommandWatch(t *testing.T) {
	now := time.Now()
	got, ok := monitorLaunch(monitorHook(t, "Monitor",
		map[string]any{
			"command":     "tail -f build.log | grep --line-buffered ERROR",
			"description": "errors in build.log",
			"timeout_ms":  300000,
			"persistent":  false,
		},
		map[string]any{"taskId": "b1888u33s", "timeoutMs": 300000, "persistent": false},
	), now)

	require.True(t, ok)
	assert.Equal(t, "b1888u33s", got.ID)
	assert.Equal(t, "tail -f build.log | grep --line-buffered ERROR", got.Command)
	assert.Equal(t, "errors in build.log", got.Label)
	assert.False(t, got.WS)
	assert.True(t, got.Deadline.Equal(now.Add(5*time.Minute)),
		"the harness enforces timeout_ms, so the ledger records it as an absolute bound")
}

func TestMonitorLaunch_PersistentWatchHasNoDeadline(t *testing.T) {
	got, ok := monitorLaunch(monitorHook(t, "Monitor",
		map[string]any{"command": "tail -f app.log", "description": "app errors", "persistent": true},
		map[string]any{"taskId": "b2", "timeoutMs": 300000, "persistent": true},
	), time.Now())

	require.True(t, ok)
	assert.True(t, got.Deadline.IsZero(),
		"a persistent watch runs until the session ends; timeoutMs is meaningless for it")
}

func TestMonitorLaunch_WebsocketWatchIsFlaggedAndLabelledByURL(t *testing.T) {
	got, ok := monitorLaunch(monitorHook(t, "Monitor",
		map[string]any{"ws": map[string]any{"url": "wss://events.example.com/stream"}},
		map[string]any{"taskId": "b3", "timeoutMs": 60000, "persistent": false},
	), time.Now())

	require.True(t, ok)
	assert.True(t, got.WS, "a ws watch has no descendant process for the reconcile to match")
	assert.Equal(t, "", got.Command)
	assert.Equal(t, "wss://events.example.com/stream", got.Label,
		"with no description the socket URL is the readable label")
}

func TestMonitorLaunch_DegradesRatherThanDropsOnAPoorPayload(t *testing.T) {
	now := time.Now()

	// No tool_response at all: the launch still happened, so it is still
	// evidence — it only loses the id and the deadline.
	got, ok := monitorLaunch(monitorHook(t, "Monitor",
		map[string]any{"command": "tail -f app.log"}, nil), now)
	require.True(t, ok)
	assert.Equal(t, "", got.ID)
	assert.True(t, got.Deadline.IsZero())

	// A response that is not an object at all.
	raw := monitorHook(t, "Monitor", map[string]any{"command": "tail -f app.log"}, nil)
	raw.ToolResponse = json.RawMessage(`"stopped"`)
	got, ok = monitorLaunch(raw, now)
	require.True(t, ok, "an unreadable response must not fail the hook or lose the launch")
	assert.Equal(t, "", got.ID)

	// A zero or absent timeout must fold to "no deadline", never to a
	// deadline in the past that would retire a live watch instantly.
	got, ok = monitorLaunch(monitorHook(t, "Monitor",
		map[string]any{"command": "tail -f app.log"},
		map[string]any{"taskId": "b4", "timeoutMs": 0, "persistent": false},
	), now)
	require.True(t, ok)
	assert.True(t, got.Deadline.IsZero())
}

func TestMonitorLaunch_IgnoresEverythingElse(t *testing.T) {
	now := time.Now()

	_, ok := monitorLaunch(monitorHook(t, "Bash",
		map[string]any{"command": "npm run dev", "run_in_background": true},
		map[string]any{"backgroundTaskId": "b5"}), now)
	assert.False(t, ok, "a background shell belongs to the other ledger")

	// A Monitor call carrying neither a command nor a socket describes no
	// watch this ledger could ever retire on evidence.
	_, ok = monitorLaunch(monitorHook(t, "Monitor",
		map[string]any{"description": "something"},
		map[string]any{"taskId": "b6"}), now)
	assert.False(t, ok)

	pre := monitorHook(t, "Monitor", map[string]any{"command": "tail -f app.log"}, nil)
	pre.HookEventName = "PreToolUse"
	_, ok = monitorLaunch(pre, now)
	assert.False(t, ok, "only the PostToolUse proves the watch actually started")

	bad := monitorHook(t, "Monitor", map[string]any{"command": "tail -f app.log"}, nil)
	bad.ToolInput = json.RawMessage(`{not json`)
	_, ok = monitorLaunch(bad, now)
	assert.False(t, ok)
}

func TestHarnessTracksMonitors(t *testing.T) {
	assert.True(t, harnessTracksMonitors("claude"))
	assert.False(t, harnessTracksMonitors("codex"), "Codex has no monitor concept")
	assert.True(t, harnessTracksMonitors(""), "an unset harness is the default, Claude Code")
	assert.False(t, harnessTracksMonitors("not-a-harness"),
		"an unresolvable harness folds to false rather than growing an unretirable count")
}
