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

// serveDashboardConfig routes r through a fresh mux carrying only the
// /api/config route — the same dispatch a real browser request takes.
// setupTestDB points HOME at a temp dir, so config.ConfigPath() is
// isolated and these tests never touch the developer's real config.
func serveDashboardConfig(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", handleDashboardConfigAPI)
	mux.ServeHTTP(w, r)
}

// configResp is the GET / save / dry-run response shape.
type configResp struct {
	Raw    string   `json:"raw"`
	Path   string   `json:"path"`
	Errors []string `json:"errors"`
}

func getConfigResp(t *testing.T, path, body string) (*httptest.ResponseRecorder, configResp) {
	t.Helper()
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}
	w := httptest.NewRecorder()
	serveDashboardConfig(w, dashboardRequest(method, path, body))
	var resp configResp
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

func TestDashboardConfig_GetReturnsDefaults(t *testing.T) {
	setupTestDB(t) // HOME → temp dir, so no config file exists yet
	withDashboardAuth(t)

	w, resp := getConfigResp(t, "/api/config", "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, resp.Raw, `"log_level": "info"`, "defaults must be surfaced")
	assert.NotEmpty(t, resp.Path, "config file path must be reported")
}

func TestDashboardConfig_PostWritesFile(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	body := `{"log_level":"debug","notifications":{"enabled":true},"agent":{"clone_cooldown":"2m"}}`
	w, resp := getConfigResp(t, "/api/config", body)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel)
	require.NotNil(t, cfg.Notifications)
	assert.True(t, cfg.Notifications.Enabled)
	require.NotNil(t, cfg.Agent)
	assert.Equal(t, "2m", cfg.Agent.CloneCooldown)

	// The response "raw" must equal what a fresh GET re-derives — that
	// is the diff baseline the editor re-syncs to after a save.
	gw, getResp := getConfigResp(t, "/api/config", "")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, getResp.Raw, resp.Raw, "post-save raw must match a fresh GET")
}

func TestDashboardConfig_DryRunDoesNotWrite(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := getConfigResp(t, "/api/config?dry_run=1", `{"log_level":"warn"}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, resp.Raw, `"log_level": "warn"`, "dry-run still previews the change")

	_, statErr := os.Stat(config.ConfigPath())
	assert.True(t, os.IsNotExist(statErr), "dry-run must not create the config file")
}

func TestDashboardConfig_PostInvalidReturns400(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := getConfigResp(t, "/api/config", `{"log_level":"loud"}`)
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.NotEmpty(t, resp.Errors, "validation errors must be listed")
	assert.Contains(t, resp.Errors[0], "log_level")

	_, statErr := os.Stat(config.ConfigPath())
	assert.True(t, os.IsNotExist(statErr), "an invalid POST must not write the file")
}

func TestDashboardConfig_MalformedJSONReturns400(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, _ := getConfigResp(t, "/api/config", `{not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// The GET baseline must round-trip through a dry-run unchanged —
// otherwise the editor would show a spurious diff for an untouched
// form. This guards the Validate→Normalize→Marshal canonicalisation.
func TestDashboardConfig_RoundTripStable(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gw, getResp := getConfigResp(t, "/api/config", "")
	require.Equal(t, http.StatusOK, gw.Code)
	require.NotEmpty(t, getResp.Raw)

	pw, preview := getConfigResp(t, "/api/config?dry_run=1", getResp.Raw)
	require.Equal(t, http.StatusOK, pw.Code, "body=%s", pw.Body.String())
	assert.Equal(t, getResp.Raw, preview.Raw,
		"the GET baseline must dry-run back to itself unchanged")
}

func TestDashboardConfig_MethodNotAllowed(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	serveDashboardConfig(w, dashboardRequest(http.MethodDelete, "/api/config", ""))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// A POST carrying a raw agent.sudo block must persist and validate.
func TestDashboardConfig_PersistsSudoBlock(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	body := `{"log_level":"info","agent":{"sudo":{"max_duration":"2h","default_duration":"30m"}}}`
	w, _ := getConfigResp(t, "/api/config", body)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Agent)
	require.NotNil(t, cfg.Agent.Sudo)
	assert.Equal(t, "2h", cfg.Agent.Sudo.MaxDuration)
	assert.Equal(t, "30m", cfg.Agent.Sudo.DefaultDuration)
}

func TestDashboardConfig_RejectsBadSudoDuration(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	body := `{"log_level":"info","agent":{"sudo":{"max_duration":"forever"}}}`
	w, resp := getConfigResp(t, "/api/config", body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.NotEmpty(t, resp.Errors)
	assert.Contains(t, resp.Errors[0], "max_duration")
}
