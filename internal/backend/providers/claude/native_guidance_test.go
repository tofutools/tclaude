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
