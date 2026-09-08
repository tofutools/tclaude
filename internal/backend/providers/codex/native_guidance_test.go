package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	normalizer.Set("thread-successor")
	_, _, err = normalizer.Normalize(ports.RawNativeCallback{ReceivedAt: time.Now(), Body: []byte(`{"thread_id":"thread-successor","hook_event_name":"SessionStart"}`)})
	require.NoError(t, err, "fork release rebinds callbacks before the forked terminal starts")
}

func TestCodexForkGuidanceUsesOnlyPublishedForkIdentity(t *testing.T) {
	receipt := filepath.Join(t.TempDir(), "result")
	normalizer := newCodexNativeNormalizer("source")
	normalizer.SetForkReceipt(receipt)
	callback := func(id string) error {
		raw, _ := json.Marshal(nativeHookInput{ThreadID: id, HookEventName: "SessionStart"})
		_, _, err := normalizer.Normalize(ports.RawNativeCallback{Body: raw})
		return err
	}
	require.Error(t, callback("source"))
	require.Error(t, callback("forked"), "callbacks cannot choose the fork identity")
	require.NoError(t, os.WriteFile(receipt, []byte(`"forked"`), 0600))
	require.NoError(t, callback("forked"))
	require.True(t, normalizer.Matches("forked"))
	require.Error(t, callback("source"))
	require.NoError(t, os.WriteFile(receipt, []byte(`"replacement"`), 0600))
	require.Error(t, callback("replacement"), "an established execution keeps its exact native identity")
	require.True(t, normalizer.Matches("forked"))
}
