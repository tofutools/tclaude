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

// Functional tests for /api/slop/volumes — the persistence backend of
// the slop-mode volume mixer (slop-volume.js). Same harness as the
// dashboard_config tests: setupTestDB points HOME at a temp dir so
// config.ConfigPath() is isolated, withDashboardAuth satisfies the
// cookie gate.

// slopVolumesResp is the GET / POST response shape.
type slopVolumesResp struct {
	MusicVolume   *int   `json:"music_volume"`
	EffectsVolume *int   `json:"effects_volume"`
	Error         string `json:"error"`
	Code          string `json:"code"`
}

func serveSlopVolumes(t *testing.T, method, body string) (*httptest.ResponseRecorder, slopVolumesResp) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/slop/volumes", handleDashboardSlopVolumesAPI)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, dashboardRequest(method, "/api/slop/volumes", body))
	var resp slopVolumesResp
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

func TestDashboardSlopVolumes_GetDefaults(t *testing.T) {
	setupTestDB(t) // HOME → temp dir, no config file yet
	withDashboardAuth(t)

	w, resp := serveSlopVolumes(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.NotNil(t, resp.MusicVolume)
	require.NotNil(t, resp.EffectsVolume)
	assert.Equal(t, 50, *resp.MusicVolume, "absent config defaults the music to half volume")
	assert.Equal(t, 100, *resp.EffectsVolume, "absent config defaults the effects to full volume")
}

func TestDashboardSlopVolumes_PostPersistsAndMerges(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Seed an unrelated setting to prove the volume write merges into
	// the existing config rather than replacing it.
	require.NoError(t, config.Save(&config.Config{LogLevel: "debug"}))

	w, resp := serveSlopVolumes(t, http.MethodPost, `{"music_volume":40,"effects_volume":70}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, 40, *resp.MusicVolume)
	assert.Equal(t, 70, *resp.EffectsVolume)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel, "unrelated settings must survive a volume write")
	music, effects := cfg.ResolvedSlopVolumes()
	assert.Equal(t, 40, music)
	assert.Equal(t, 70, effects)

	// A partial POST updates only the named volume.
	w, resp = serveSlopVolumes(t, http.MethodPost, `{"effects_volume":0}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, 40, *resp.MusicVolume, "absent key leaves the other volume as-is")
	assert.Equal(t, 0, *resp.EffectsVolume, "an explicit 0 is a valid (silent) volume")
}

func TestDashboardSlopVolumes_GetClampsHandEditedValues(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// A hand-edited out-of-range value parses fine (it's an int);
	// Validate flags it in the Config tab, but GET must still hand the
	// browser a usable 0–100 — otherwise the mixer shows "500%" and
	// every subsequent POST echoing it back is rejected.
	require.NoError(t, os.MkdirAll(config.DataDir(), 0o700))
	require.NoError(t, os.WriteFile(config.ConfigPath(),
		[]byte(`{"slop":{"music_volume":500,"effects_volume":-3}}`), 0o644))

	w, resp := serveSlopVolumes(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, 100, *resp.MusicVolume, "out-of-range high clamps to 100")
	assert.Equal(t, 0, *resp.EffectsVolume, "out-of-range low clamps to 0")
}

func TestDashboardSlopVolumes_PostRejectsBadInput(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	for name, body := range map[string]string{
		"out of range high": `{"music_volume":101}`,
		"out of range low":  `{"effects_volume":-1}`,
		"empty body":        `{}`,
		"not json":          `nope`,
	} {
		w, _ := serveSlopVolumes(t, http.MethodPost, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s must 400; body=%s", name, w.Body.String())
	}
	_, statErr := os.Stat(config.ConfigPath())
	assert.True(t, os.IsNotExist(statErr), "rejected posts must not create the config file")
}

func TestDashboardSlopVolumes_PostRefusesCorruptConfig(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	require.NoError(t, os.MkdirAll(config.DataDir(), 0o700))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte("{not json"), 0o644))

	w, resp := serveSlopVolumes(t, http.MethodPost, `{"music_volume":50}`)
	assert.Equal(t, http.StatusConflict, w.Code, "a corrupt config must not be silently replaced")
	assert.Equal(t, "malformed_target", resp.Code)

	data, err := os.ReadFile(config.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, "{not json", string(data), "the corrupt file must be left untouched")
}
