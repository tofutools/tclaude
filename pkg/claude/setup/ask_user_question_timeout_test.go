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

// An interactive run (not --yes) that ACCEPTS the prompt writes the recommended
// interval — the one path that actually mutates settings.json.
func TestConfigureAskTimeout_InteractiveAcceptWrites(t *testing.T) {
	tempHome(t)

	var out string
	withStdin(t, "y\n", func() {
		out = captureStdout(t, func() {
			configureAskUserQuestionTimeout(&Params{Yes: false})
		})
	})
	assert.Contains(t, out, `set to "5m"`, "the accepted write is confirmed")

	got, ok := readAskTimeoutFromDisk(t)
	require.True(t, ok, "accepting the prompt must write the key")
	assert.Equal(t, "5m", got, "the recommended interval is written")
}

// Declining the interactive prompt leaves settings.json untouched — no key is
// written and the step reports it was skipped.
func TestConfigureAskTimeout_InteractiveDeclineSkips(t *testing.T) {
	tempHome(t)

	var out string
	withStdin(t, "n\n", func() {
		out = captureStdout(t, func() {
			configureAskUserQuestionTimeout(&Params{Yes: false})
		})
	})
	assert.Contains(t, out, "Skipped", "declining is reported")

	_, ok := readAskTimeoutFromDisk(t)
	assert.False(t, ok, "declining must not write the key")
}

// A corrupt settings.json is never rewritten: it also carries hooks,
// permissions and sandbox config, so the configure step warns and leaves the
// file byte-for-byte as it found it.
func TestConfigureAskTimeout_CorruptSettingsLeftUntouched(t *testing.T) {
	tempHome(t)
	const corrupt = `{ this is not valid json `
	seedSettings(t, corrupt)

	out := captureStdout(t, func() {
		configureAskUserQuestionTimeout(&Params{Yes: true})
	})
	assert.Contains(t, out, "Could not read", "a corrupt file is reported, not clobbered")

	data, err := os.ReadFile(session.ClaudeSettingsPath())
	require.NoError(t, err)
	assert.Equal(t, corrupt, string(data), "the corrupt file must be left untouched")
}

// checkAskUserQuestionTimeout is the read-only `--check` reporter. It classifies
// each on-disk state through the shared detectAskTimeoutStatus and prints a
// distinct line per state; it never writes.
func TestCheckAskUserQuestionTimeout_ReportsStates(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		tempHome(t)
		out := captureStdout(t, checkAskUserQuestionTimeout)
		assert.Contains(t, out, "not set", "an absent key is flagged")
	})

	t.Run("interval", func(t *testing.T) {
		tempHome(t)
		seedSettings(t, `{"askUserQuestionTimeout":"5m"}`)
		out := captureStdout(t, checkAskUserQuestionTimeout)
		assert.Contains(t, out, `set to "5m"`, "an interval is reported")
		assert.Contains(t, out, "auto-continue", "with the agentic hint")
	})

	t.Run("never", func(t *testing.T) {
		tempHome(t)
		seedSettings(t, `{"askUserQuestionTimeout":"never"}`)
		out := captureStdout(t, checkAskUserQuestionTimeout)
		assert.Contains(t, out, `is "never"`, "a deliberate never is reported")
	})

	t.Run("corrupt", func(t *testing.T) {
		tempHome(t)
		seedSettings(t, `{ not json `)
		out := captureStdout(t, checkAskUserQuestionTimeout)
		assert.Contains(t, out, "Could not read settings.json", "a corrupt file is reported")
	})
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
