package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// readAskTimeoutFromDisk returns the raw "askUserQuestionTimeout" value from the
// live settings.json, or ("", false) when the file or key is absent.
func readAskTimeoutFromDisk(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(session.ClaudeSettingsPath())
	if os.IsNotExist(err) {
		return "", false
	}
	require.NoError(t, err)
	var tree map[string]any
	require.NoError(t, json.Unmarshal(data, &tree))
	raw, ok := tree[askTimeoutSettingsKey]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	require.True(t, ok, "askUserQuestionTimeout should be a string")
	return s, true
}

// Under --yes on a fresh config the step must RECOMMEND but never write — it is
// a global behaviour change, so a scripted setup only prints advice (the
// "don't modify by default" policy). This is the deliberate divergence from
// configureFullscreenTUI, which does write under --yes.
func TestConfigureAskTimeout_YesRecommendsButDoesNotWrite(t *testing.T) {
	tempHome(t)

	out := captureStdout(t, func() {
		configureAskUserQuestionTimeout(&Params{Yes: true})
	})
	assert.Contains(t, out, "highly recommended", "should print the recommendation")
	assert.Contains(t, out, "not setting it under --yes", "must not silently set it under --yes")

	_, ok := readAskTimeoutFromDisk(t)
	assert.False(t, ok, "askUserQuestionTimeout must NOT be written under --yes")
}

// An existing interval value is a deliberate choice — left as-is, no nag.
func TestConfigureAskTimeout_PresentIntervalLeftAsIs(t *testing.T) {
	tempHome(t)
	seedSettings(t, `{"askUserQuestionTimeout":"5m","model":"opus"}`)

	out := captureStdout(t, func() {
		configureAskUserQuestionTimeout(&Params{Yes: true})
	})
	assert.Contains(t, out, `already set to "5m"`, "an existing value is reported and kept")

	got, ok := readAskTimeoutFromDisk(t)
	require.True(t, ok)
	assert.Equal(t, "5m", got, "existing value must be untouched")
}

// A deliberate "never" is also respected (with a hint about auto-continue), not
// overwritten.
func TestConfigureAskTimeout_PresentNeverRespected(t *testing.T) {
	tempHome(t)
	seedSettings(t, `{"askUserQuestionTimeout":"never"}`)

	out := captureStdout(t, func() {
		configureAskUserQuestionTimeout(&Params{Yes: true})
	})
	assert.Contains(t, out, `set to "never"`, "the never choice is reported")

	got, ok := readAskTimeoutFromDisk(t)
	require.True(t, ok)
	assert.Equal(t, "never", got, "never must be untouched")
}

// The writer sets the key while preserving every other key and the file's
// (private 0600) permission mode — the same care as enableFullscreenTUI.
func TestWriteClaudeAskTimeout_PreservesOthersAndMode(t *testing.T) {
	tempHome(t)
	path := session.ClaudeSettingsPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"model":"sonnet","hooks":{"Stop":[]}}`), 0o600))

	require.NoError(t, writeClaudeAskTimeout(path, "5m"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var tree map[string]any
	require.NoError(t, json.Unmarshal(data, &tree))
	assert.Equal(t, "5m", tree[askTimeoutSettingsKey], "the key is written")
	assert.Equal(t, "sonnet", tree["model"], "other keys are preserved")
	require.Contains(t, tree, "hooks", "nested keys are preserved")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the private mode is preserved")
}
