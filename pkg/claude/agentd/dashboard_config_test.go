package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// assertErrorContains asserts at least one error mentions want, so a
// test never depends on the order Validate appends its findings.
func assertErrorContains(t *testing.T, errs []string, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e, want) {
			return
		}
	}
	assert.Failf(t, "no matching error", "no error contains %q; got %v", want, errs)
}

// serveDashboardConfig routes r through a fresh mux carrying only the
// /api/config route — the same dispatch a real browser request takes.
// setupTestDB points HOME at a temp dir, so config.ConfigPath() is
// isolated and these tests never touch the developer's real config.
func serveDashboardConfig(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", handleDashboardConfigAPI)
	mux.ServeHTTP(w, r)
}

// configResp is the GET / save / dry-run / error response shape.
type configResp struct {
	Raw         string   `json:"raw"`
	Path        string   `json:"path"`
	Warning     string   `json:"warning"`
	Malformed   bool     `json:"malformed"`
	UnknownKeys []string `json:"unknown_keys"`
	Errors      []string `json:"errors"`
	Error       string   `json:"error"`
	Code        string   `json:"code"`
}

// wrapBody builds the POST wire shape: the edited config plus the
// canonical baseline the drift guard checks. base "" opts out of the
// guard (the dashboard always sends a real baseline).
func wrapBody(configJSON, base string) string {
	return `{"config":` + configJSON + `,"base":` + strconv.Quote(base) + `}`
}

// getConfig issues a GET and decodes the response.
func getConfig(t *testing.T) (*httptest.ResponseRecorder, configResp) {
	t.Helper()
	w := httptest.NewRecorder()
	serveDashboardConfig(w, dashboardRequest(http.MethodGet, "/api/config", ""))
	return w, decodeConfigResp(w)
}

// postConfig issues a POST with the given wrapper body.
func postConfig(t *testing.T, path, body string) (*httptest.ResponseRecorder, configResp) {
	t.Helper()
	w := httptest.NewRecorder()
	serveDashboardConfig(w, dashboardRequest(http.MethodPost, path, body))
	return w, decodeConfigResp(w)
}

func decodeConfigResp(w *httptest.ResponseRecorder) configResp {
	var resp configResp
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return resp
}

func TestDashboardConfig_GetReturnsDefaults(t *testing.T) {
	setupTestDB(t) // HOME → temp dir, so no config file exists yet
	withDashboardAuth(t)

	w, resp := getConfig(t)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, resp.Raw, `"log_level": "info"`, "defaults must be surfaced")
	assert.NotEmpty(t, resp.Path, "config file path must be reported")
	assert.Empty(t, resp.UnknownKeys, "a fresh config has no unknown keys")
}

func TestDashboardConfig_PostWritesFile(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	body := wrapBody(`{"log_level":"debug","notifications":{"enabled":true},"agent":{"clone_cooldown":"2m"}}`, "")
	w, resp := postConfig(t, "/api/config", body)
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
	gw, getResp := getConfig(t)
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, getResp.Raw, resp.Raw, "post-save raw must match a fresh GET")
}

func TestDashboardConfig_DryRunDoesNotWrite(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := postConfig(t, "/api/config?dry_run=1", wrapBody(`{"log_level":"warn"}`, ""))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, resp.Raw, `"log_level": "warn"`, "dry-run still previews the change")

	_, statErr := os.Stat(config.ConfigPath())
	assert.True(t, os.IsNotExist(statErr), "dry-run must not create the config file")
}

func TestDashboardConfig_PostInvalidReturns400(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := postConfig(t, "/api/config", wrapBody(`{"log_level":"loud"}`, ""))
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.NotEmpty(t, resp.Errors, "validation errors must be listed")
	assertErrorContains(t, resp.Errors, "log_level")

	_, statErr := os.Stat(config.ConfigPath())
	assert.True(t, os.IsNotExist(statErr), "an invalid POST must not write the file")
}

// A malformed body must use the same {"errors":[...]} 400 contract as
// a validation failure — not a plain-text http.Error.
func TestDashboardConfig_MalformedJSONReturns400(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := postConfig(t, "/api/config", `{not json`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.NotEmpty(t, resp.Errors, "malformed body must report via the errors array")
	assertErrorContains(t, resp.Errors, "valid JSON")
}

func TestDashboardConfig_MissingConfigReturns400(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, resp := postConfig(t, "/api/config", `{"base":""}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.NotEmpty(t, resp.Errors)
	assertErrorContains(t, resp.Errors, "config")
}

// The GET baseline must round-trip through a dry-run unchanged —
// otherwise the editor would show a spurious diff for an untouched
// form. This guards the Validate→Normalize→Marshal canonicalisation.
func TestDashboardConfig_RoundTripStable(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	gw, getResp := getConfig(t)
	require.Equal(t, http.StatusOK, gw.Code)
	require.NotEmpty(t, getResp.Raw)

	pw, preview := postConfig(t, "/api/config?dry_run=1", wrapBody(getResp.Raw, getResp.Raw))
	require.Equal(t, http.StatusOK, pw.Code, "body=%s", pw.Body.String())
	assert.Equal(t, getResp.Raw, preview.Raw,
		"the GET baseline must dry-run back to itself unchanged")
}

// When the file changed since the editor loaded it, a save carrying
// the stale baseline must be refused with 409 — not blindly overwrite
// the newer config.
func TestDashboardConfig_StaleBaselineReturns409(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Write an initial config, then capture its canonical baseline.
	w, _ := postConfig(t, "/api/config", wrapBody(`{"log_level":"info"}`, ""))
	require.Equal(t, http.StatusOK, w.Code)
	gw, baseline := getConfig(t)
	require.Equal(t, http.StatusOK, gw.Code)

	// A second writer changes the file out from under the editor.
	w2, _ := postConfig(t, "/api/config", wrapBody(`{"log_level":"error"}`, ""))
	require.Equal(t, http.StatusOK, w2.Code)

	// A save carrying the now-stale baseline is refused.
	w3, resp := postConfig(t, "/api/config", wrapBody(`{"log_level":"debug"}`, baseline.Raw))
	require.Equal(t, http.StatusConflict, w3.Code, "body=%s", w3.Body.String())
	assert.Equal(t, "config_drift", resp.Code)

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "error", cfg.LogLevel, "the stale save must not have overwritten the file")
}

// A matching baseline must pass the drift guard and write.
func TestDashboardConfig_FreshBaselineSaves(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w, _ := postConfig(t, "/api/config", wrapBody(`{"log_level":"info"}`, ""))
	require.Equal(t, http.StatusOK, w.Code)
	_, baseline := getConfig(t)

	w2, _ := postConfig(t, "/api/config", wrapBody(`{"log_level":"warn"}`, baseline.Raw))
	require.Equal(t, http.StatusOK, w2.Code, "body=%s", w2.Body.String())

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "warn", cfg.LogLevel)
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

	body := wrapBody(`{"log_level":"info","agent":{"sudo":{"max_duration":"2h","default_duration":"30m"}}}`, "")
	w, _ := postConfig(t, "/api/config", body)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Agent)
	require.NotNil(t, cfg.Agent.Sudo)
	assert.Equal(t, "2h", cfg.Agent.Sudo.MaxDuration)
	assert.Equal(t, "30m", cfg.Agent.Sudo.DefaultDuration)
}

// A corrupt config file on disk must block a save: the editor shows
// defaults, so an unacknowledged save would silently discard whatever
// the unparseable file held. A dry-run is still allowed (no write);
// the real write needs replace_malformed=1.
func TestDashboardConfig_MalformedFileGuardsSave(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	require.NoError(t, os.MkdirAll(config.ConfigDir(), 0o755))
	const corrupt = `{ broken json `
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(corrupt), 0o644))

	gw, getResp := getConfig(t)
	require.Equal(t, http.StatusOK, gw.Code)
	assert.True(t, getResp.Malformed, "a corrupt file must be flagged malformed")
	assert.NotEmpty(t, getResp.Warning)

	body := wrapBody(`{"log_level":"debug"}`, getResp.Raw)

	// A dry-run only previews — allowed even against a corrupt target.
	dw, _ := postConfig(t, "/api/config?dry_run=1", body)
	require.Equal(t, http.StatusOK, dw.Code, "dry-run must be allowed; body=%s", dw.Body.String())

	// A real save without acknowledgement is refused; the file stays put.
	w, resp := postConfig(t, "/api/config", body)
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "malformed_target", resp.Code)
	onDisk, _ := os.ReadFile(config.ConfigPath())
	assert.Equal(t, corrupt, string(onDisk), "the corrupt file must be left intact")

	// With the explicit acknowledgement the save goes through.
	w2, _ := postConfig(t, "/api/config?replace_malformed=1", body)
	require.Equal(t, http.StatusOK, w2.Code, "body=%s", w2.Body.String())
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestDashboardConfig_RejectsBadSudoDuration(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	body := wrapBody(`{"log_level":"info","agent":{"sudo":{"max_duration":"forever"}}}`, "")
	w, resp := postConfig(t, "/api/config", body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.NotEmpty(t, resp.Errors)
	assertErrorContains(t, resp.Errors, "max_duration")
}

// Keys tclaude's schema does not model must be reported as unknown so
// the human is warned a save drops them — at every nesting depth, with
// arbitrary map keys (agent.sudo.overrides.<id>) correctly exempted.
func TestDashboardConfig_GetReportsUnknownKeys(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	require.NoError(t, os.MkdirAll(config.ConfigDir(), 0o755))
	// Top-level unknowns (human_notify, zzz_future); a nested unknown
	// (agent.future_flag); and a sudo override whose map key is
	// arbitrary-by-design (alice — must NOT be flagged) but which
	// carries an unknown field of its own (bogus — must be flagged).
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{
		"log_level":"info",
		"human_notify":{"channel":"telegram"},
		"zzz_future":1,
		"agent":{"future_flag":true,"sudo":{"overrides":{"alice":{"max_duration":"1h","bogus":2}}}}
	}`), 0o644))

	w, resp := getConfig(t)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, []string{
		"agent.future_flag",
		"agent.sudo.overrides.alice.bogus",
		"human_notify",
		"zzz_future",
	}, resp.UnknownKeys, "unknown keys at every depth listed (sorted); arbitrary map keys exempt")
}
