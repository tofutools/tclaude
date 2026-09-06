package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestCodexNativeGuidanceCodecLearnsOnlyInitialExactThread(t *testing.T) {
	normalizer := newCodexNativeNormalizer("")
	raw := ports.RawNativeCallback{ReceivedAt: time.Now(), Body: []byte(`{"thread_id":"thread-exact","hook_event_name":"SessionStart"}`)}
	event, nativeKind, err := normalizer.Normalize(raw)
	require.NoError(t, err)
	require.True(t, normalizer.Matches("thread-exact"))
	require.Equal(t, "session_start", event.Kind)
	response, err := encodeCodexGuidance(nativeKind, "keep the current attempt")
	require.NoError(t, err)
	require.True(t, json.Valid(response))
	_, _, err = normalizer.Normalize(ports.RawNativeCallback{ReceivedAt: time.Now(), Body: []byte(`{"thread_id":"thread-successor","hook_event_name":"UserPromptSubmit"}`)})
	require.Error(t, err)
}
