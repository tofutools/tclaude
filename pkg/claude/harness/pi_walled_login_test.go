package harness

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Inside tclaude's sandbox pi's auth.json is read-only, so an OAuth login
// cannot refresh there: an expired one refuses the launch with what to do, a
// live one is noted, and an API key or an unnamed provider passes.
func TestPiWalledLoginCheck(t *testing.T) {
	agentDir := piTestAgentDir(t)
	require.NoError(t, os.MkdirAll(agentDir, 0o700))
	now := time.UnixMilli(1_800_000_000_000)
	expired, live := strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10), strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10)
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(`{
		"openai-codex": {"type":"oauth","access":"a","refresh":"r","expires":`+expired+`},
		"xai": {"type":"oauth","access":"a","refresh":"r","expires":`+live+`},
		"anthropic": {"type":"api_key","key":"sk-test"}
	}`), 0o600))

	_, err := PiWalledLoginCheck("openai-codex/gpt-5.5", now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run pi once outside the sandbox")
	assert.Contains(t, err.Error(), PiWalledLoginTicket)

	notice, err := PiWalledLoginCheck("xai/grok-5", now)
	require.NoError(t, err)
	assert.Contains(t, notice, "cannot refresh inside tclaude's sandbox")

	for _, model := range []string{"anthropic/claude-sonnet-4-5", "mock/mock-1", ""} {
		notice, err := PiWalledLoginCheck(model, now)
		require.NoError(t, err, model)
		assert.Empty(t, notice, model)
	}

	// With no model, the agent directory's default provider is the login.
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"defaultProvider":"openai-codex"}`), 0o600))
	_, err = PiWalledLoginCheck("", now)
	require.Error(t, err)
}
