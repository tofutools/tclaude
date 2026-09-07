package claude

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestClaudeNativeGuidanceCodecRequiresExactSession(t *testing.T) {
	normalize := claudeNativeNormalizer("session-exact")
	raw := ports.RawNativeCallback{ReceivedAt: time.Now(), Body: []byte(`{"session_id":"session-exact","hook_event_name":"UserPromptSubmit","prompt":"go"}`)}
	event, nativeKind, err := normalize(raw)
	require.NoError(t, err)
	require.Equal(t, "user_prompt", event.Kind)
	require.Equal(t, "session-exact", event.NativeCorrelation)
	require.Equal(t, "UserPromptSubmit", nativeKind)
	response, err := encodeClaudeGuidance(nativeKind, "use the exact workspace")
	require.NoError(t, err)
	require.True(t, json.Valid(response))
	_, _, err = normalize(ports.RawNativeCallback{ReceivedAt: time.Now(), Body: []byte(`{"session_id":"successor","hook_event_name":"SessionStart"}`)})
	require.Error(t, err)
}

func TestClaudeActivityRequiresPrimaryNativeNotification(t *testing.T) {
	normalize := claudeNativeNormalizer("primary")
	for _, tc := range []struct {
		body, kind string
		invalid    bool
	}{
		{body: `{"session_id":"primary","hook_event_name":"Notification","notification_type":"idle_prompt"}`, kind: "activity_idle"},
		{body: `{"session_id":"primary","hook_event_name":"Notification","notification_type":"permission_prompt"}`, kind: "activity_awaiting_input"},
		{body: `{"session_id":"primary","hook_event_name":"Notification","notification_type":"elicitation_dialog"}`, kind: "activity_awaiting_input"},
		{body: `{"session_id":"primary","hook_event_name":"Stop"}`, kind: "activity_unknown"},
		{body: `{"session_id":"primary","hook_event_name":"PreToolUse"}`, kind: "activity_active"},
		{body: `{"session_id":"primary","hook_event_name":"Notification","notification_type":"idle_prompt","agent_id":"nested"}`, invalid: true},
		{body: `{"session_id":"other","hook_event_name":"Notification","notification_type":"idle_prompt"}`, invalid: true},
		{body: `{"session_id":"primary","hook_event_name":"Notification","notification_type":"auth_success"}`, invalid: true},
	} {
		event, _, err := normalize(ports.RawNativeCallback{Body: []byte(tc.body), ReceivedAt: time.Now()})
		if tc.invalid {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tc.kind, event.Kind)
	}
}
