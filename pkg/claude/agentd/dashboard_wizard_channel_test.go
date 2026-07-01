package agentd

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// The wizard radio reuses the slop /api/slop/channel backend (one shared
// radio); these tests cover the wizard-specific additions: the fantasy
// channel set and the `persisted` flag the browser uses to tell a fresh
// listener (who should hear the active theme's default station) apart from a
// deliberate pick.

// TestWizardChannels_InAllowlist pins the fantasy channels into the shared
// SomaFM allowlist (the SSRF gate + picker source of truth) and confirms the
// wizard default is itself streamable. The Go↔JS set match is covered by
// TestSlopNowPlaying_ChannelMatchesVegasJS; this guards the Go side alone.
func TestWizardChannels_InAllowlist(t *testing.T) {
	for _, id := range []string{"thistle", "folkfwd", "dronezone", "darkzone", "doomed", "deepspaceone"} {
		assert.True(t, config.IsKnownSlopChannel(id), "wizard channel %q must be allowlisted", id)
	}
	assert.True(t, config.IsKnownSlopChannel(config.DefaultWizardChannel),
		"the wizard default channel must be streamable")
	assert.Equal(t, "thistle", config.DefaultWizardChannel, "the wizard default is the Celtic Tavern")
}

// TestDashboardSlopChannel_PersistedFlag: GET reports persisted=false when no
// channel was ever chosen (so the browser applies the theme default) and
// persisted=true after a deliberate pick (so it's honored across both themes).
func TestDashboardSlopChannel_PersistedFlag(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Fresh config — no channel set.
	w, resp := serveSlopChannel(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.False(t, resp.Persisted, "a fresh config has no explicit channel")
	assert.Equal(t, config.DefaultSlopChannel, resp.Channel, "…but still resolves to a streamable default")

	// After an explicit pick.
	w, _ = serveSlopChannel(t, http.MethodPost, `{"channel":"thistle"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	w, resp = serveSlopChannel(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.True(t, resp.Persisted, "a saved channel reports persisted=true")
	assert.Equal(t, "thistle", resp.Channel)
}

// TestConfig_HasExplicitSlopChannel covers the nil-safe helper directly,
// including the hand-edited-unknown degrade (an unknown saved id is NOT a
// valid explicit choice — the browser should fall back to the theme default).
func TestConfig_HasExplicitSlopChannel(t *testing.T) {
	assert.False(t, (*config.Config)(nil).HasExplicitSlopChannel(), "nil config is not explicit")
	assert.False(t, (&config.Config{}).HasExplicitSlopChannel(), "no slop block is not explicit")
	assert.False(t, (&config.Config{Slop: &config.SlopConfig{}}).HasExplicitSlopChannel(),
		"a slop block with no channel is not explicit")

	known := "groovesalad"
	assert.True(t, (&config.Config{Slop: &config.SlopConfig{Channel: &known}}).HasExplicitSlopChannel(),
		"a known saved channel is explicit")

	bogus := "not-a-channel"
	assert.False(t, (&config.Config{Slop: &config.SlopConfig{Channel: &bogus}}).HasExplicitSlopChannel(),
		"an unknown saved channel is not a valid explicit choice")
}
