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

// Functional tests for /api/ask-profile — the persistence backend of the
// Config tab's "Ask defaults" section (ask-profile.js) and a thin editor
// over config.json's "ask" block (JOH-253). Same harness as the cost /
// config tests: setupTestDB points HOME at a temp dir so
// config.ConfigPath() is isolated, withDashboardAuth satisfies the cookie
// gate.

// askProfileResp is the GET / POST response shape.
type askProfileResp struct {
	Model         string   `json:"model"`
	Effort        string   `json:"effort"`
	ModelSet      bool     `json:"model_set"`
	EffortSet     bool     `json:"effort_set"`
	DefaultModel  string   `json:"default_model"`
	DefaultEffort string   `json:"default_effort"`
	Models        []string `json:"models"`
	Efforts       []string `json:"efforts"`
	Error         string   `json:"error"`
	Code          string   `json:"code"`
}

func serveAskProfile(t *testing.T, method, body string) (*httptest.ResponseRecorder, askProfileResp) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ask-profile", handleDashboardAskProfileAPI)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, dashboardRequest(method, "/api/ask-profile", body))
	var resp askProfileResp
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

// GET with no config returns the fast-by-default profile and the harness
// catalog for the selectors, with both fields flagged unset.
func TestDashboardAskProfile_GetDefault(t *testing.T) {
	setupTestDB(t) // HOME → temp dir, no config file yet
	withDashboardAuth(t)

	w, resp := serveAskProfile(t, http.MethodGet, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, config.DefaultAskModel, resp.Model, "absent config → fast default model")
	assert.Equal(t, config.DefaultAskEffort, resp.Effort, "absent config → fast default effort")
	assert.False(t, resp.ModelSet, "nothing pinned")
	assert.False(t, resp.EffortSet, "nothing pinned")
	assert.Equal(t, config.DefaultAskModel, resp.DefaultModel)
	assert.Equal(t, config.DefaultAskEffort, resp.DefaultEffort)
	assert.Contains(t, resp.Models, "haiku", "catalog drives the model selector")
	assert.Contains(t, resp.Efforts, "low", "catalog drives the effort selector")
}

// POST persists a pinned profile, normalizes it, and merges into the
// existing config rather than replacing it.
func TestDashboardAskProfile_PostPersistsAndMerges(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Seed an unrelated setting to prove the write merges.
	require.NoError(t, config.Save(&config.Config{LogLevel: "debug"}))

	// Mixed case proves the value is normalized before storage.
	w, resp := serveAskProfile(t, http.MethodPost, `{"model":"Opus","effort":"HIGH"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "opus", resp.Model)
	assert.Equal(t, "high", resp.Effort)
	assert.True(t, resp.ModelSet)
	assert.True(t, resp.EffortSet)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel, "unrelated settings must survive an ask-profile write")
	require.NotNil(t, cfg.Ask)
	assert.Equal(t, "opus", cfg.Ask.Model)
	assert.Equal(t, "high", cfg.Ask.Effort)
}

// Posting blank fields clears the override and drops the now-empty ask
// block so config.json never accrues a redundant "ask": {}.
func TestDashboardAskProfile_PostBlankClears(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	_, _ = serveAskProfile(t, http.MethodPost, `{"model":"sonnet","effort":"medium"}`)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Ask)

	w, resp := serveAskProfile(t, http.MethodPost, `{"model":"","effort":""}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, config.DefaultAskModel, resp.Model, "cleared → resolves to fast default")
	assert.False(t, resp.ModelSet)

	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.Ask, "empty ask block is dropped, not persisted as {}")
}

// Pinning only one field keeps the other on its fast default.
func TestDashboardAskProfile_PostModelOnly(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := serveAskProfile(t, http.MethodPost, `{"model":"sonnet","effort":""}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "sonnet", resp.Model)
	assert.True(t, resp.ModelSet)
	assert.Equal(t, config.DefaultAskEffort, resp.Effort, "unpinned effort stays on the fast default")
	assert.False(t, resp.EffortSet)
}

// An invalid model / effort is rejected with a 400 and never persisted.
func TestDashboardAskProfile_PostInvalidRejected(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := serveAskProfile(t, http.MethodPost, `{"model":"not-a-real-model","effort":""}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_model", resp.Code)

	w, resp = serveAskProfile(t, http.MethodPost, `{"model":"","effort":"ludicrous"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_effort", resp.Code)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.Ask, "a rejected profile is never written")
}

// A corrupt config.json on disk is refused with a 409 rather than being
// silently replaced by defaults-plus-profile — the Config tab owns that
// recovery. The (valid) profile is validated first, so this exercises the
// Update's loadErr branch, and the corrupt file is left untouched.
func TestDashboardAskProfile_MalformedConfig409(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	require.NoError(t, os.MkdirAll(config.ConfigDir(), 0o755))
	corrupt := []byte("{ this is not valid json")
	require.NoError(t, os.WriteFile(config.ConfigPath(), corrupt, 0o644))

	w, resp := serveAskProfile(t, http.MethodPost, `{"model":"opus","effort":"high"}`)
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "malformed_target", resp.Code)

	// The corrupt file must be left exactly as-is — not overwritten.
	got, err := os.ReadFile(config.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, corrupt, got, "a 409 must not rewrite the corrupt config")
}

// The endpoint refuses anything but GET / POST.
func TestDashboardAskProfile_MethodNotAllowed(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	w, _ := serveAskProfile(t, http.MethodDelete, "")
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
