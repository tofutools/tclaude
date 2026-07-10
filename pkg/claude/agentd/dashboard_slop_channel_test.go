package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Functional tests for /api/slop/channel — the persistence backend of the
// Vegas radio's channel picker (the <select> in js/vegas.js). Same harness
// as the volume tests: setupTestDB isolates config.ConfigPath() under a
// temp HOME, withDashboardAuth satisfies the cookie gate.

// slopChannelResp is the GET / POST response shape.
type slopChannelResp struct {
	Channel   string   `json:"channel"`
	Channels  []string `json:"channels"`
	Persisted bool     `json:"persisted"`
	Error     string   `json:"error"`
	Code      string   `json:"code"`
}

func serveSlopChannel(t *testing.T, method, body string) (*httptest.ResponseRecorder, slopChannelResp) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/slop/channel", handleDashboardSlopChannelAPI)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, dashboardRequest(method, "/api/slop/channel", body))
	var resp slopChannelResp
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

func TestDashboardSlopChannel_GetDefaults(t *testing.T) {
	setupTestDB(t) // HOME → temp dir, no config file yet
	withDashboardAuth(t)

	w, resp := serveSlopChannel(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, config.DefaultSlopChannel, resp.Channel, "absent config defaults to the original lounge")
	assert.ElementsMatch(t, config.SlopChannels, resp.Channels, "GET ships the full catalog for the picker")
}

func TestDashboardSlopChannel_PostPersistsAndMerges(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Seed an unrelated setting + a volume to prove the channel write merges
	// into the existing config (and slop block) rather than replacing it.
	vol := 40
	require.NoError(t, config.Save(&config.Config{
		LogLevel: "debug",
		Slop:     &config.SlopConfig{MusicVolume: &vol},
	}))

	w, resp := serveSlopChannel(t, http.MethodPost, `{"channel":"groovesalad"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "groovesalad", resp.Channel)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel, "unrelated settings must survive a channel write")
	assert.Equal(t, "groovesalad", cfg.ResolvedSlopChannel())
	music, _ := cfg.ResolvedSlopVolumes()
	assert.Equal(t, 40, music, "the sibling volume in the slop block must survive")
}

func TestDashboardSlopChannel_PostRejectsUnknownChannel(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	for name, body := range map[string]string{
		"not in allowlist": `{"channel":"totally-not-a-channel"}`,
		"empty channel":    `{"channel":""}`,
		"not json":         `nope`,
	} {
		w, resp := serveSlopChannel(t, http.MethodPost, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s must 400; body=%s", name, w.Body.String())
		assert.NotEmpty(t, resp.Code, "%s must carry an error code", name)
	}
	_, statErr := os.Stat(config.ConfigPath())
	assert.True(t, os.IsNotExist(statErr), "rejected posts must not create the config file")
}

func TestDashboardSlopChannel_GetDegradesHandEditedUnknown(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// A hand-edited unknown channel parses fine (it's a string); Validate
	// flags it in the Config tab, but GET must still hand the browser a
	// streamable channel rather than a dead id.
	require.NoError(t, os.MkdirAll(config.DataDir(), 0o700))
	require.NoError(t, os.WriteFile(config.ConfigPath(),
		[]byte(`{"slop":{"channel":"bogus"}}`), 0o644))

	w, resp := serveSlopChannel(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, config.DefaultSlopChannel, resp.Channel, "an unknown saved channel degrades to the default")
}

func TestDashboardSlopChannel_PostRefusesCorruptConfig(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	require.NoError(t, os.MkdirAll(config.DataDir(), 0o700))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte("{not json"), 0o644))

	w, resp := serveSlopChannel(t, http.MethodPost, `{"channel":"lush"}`)
	assert.Equal(t, http.StatusConflict, w.Code, "a corrupt config must not be silently replaced")
	assert.Equal(t, "malformed_target", resp.Code)

	data, err := os.ReadFile(config.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, "{not json", string(data), "the corrupt file must be left untouched")
}
