package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plugins-tab tests: every request goes through a real mux carrying
// the production /api/plugins routes — the same dispatch a browser
// takes — and check/run commands execute through the real `sh -c`
// path (with hermetic commands like `true` / `touch`). setupTestDB
// points HOME at a temp dir, so plugins.json and the catalog never
// touch the developer's real ~/.tclaude.

// setupPluginsTest isolates HOME, swaps in dashboard auth, and clears
// the (package-global) status cache so one test's check results can't
// leak into the next.
func setupPluginsTest(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	withDashboardAuth(t)
	pluginStatusCache.Lock()
	pluginStatusCache.byPlugin = map[string][]pluginStepResult{}
	pluginStatusCache.Unlock()
}

func servePlugins(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	registerDashboardPluginRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, dashboardRequest(method, path, body))
	return w
}

func decodePluginsList(t *testing.T, w *httptest.ResponseRecorder) pluginsListResponse {
	t.Helper()
	var resp pluginsListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func decodePluginView(t *testing.T, w *httptest.ResponseRecorder) dashboardPlugin {
	t.Helper()
	var v dashboardPlugin
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	return v
}

// catalogEntry finds a catalog plugin by name — assertions must not
// couple to catalog ordering.
func catalogEntry(t *testing.T, name string) Plugin {
	t.Helper()
	for _, p := range pluginCatalog() {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no catalog entry named %s", name)
	return Plugin{}
}

// installedPlugin finds an installed plugin by name in a list response.
func installedPlugin(t *testing.T, list pluginsListResponse, name string) dashboardPlugin {
	t.Helper()
	for _, p := range list.Plugins {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no installed plugin named %s", name)
	return dashboardPlugin{}
}

func TestPlugins_CRUDAndCatalog(t *testing.T) {
	setupPluginsTest(t)

	// Fresh registry: no plugins, but the built-in catalog is offered.
	w := servePlugins(t, http.MethodGet, "/api/plugins", "")
	require.Equal(t, http.StatusOK, w.Code)
	list := decodePluginsList(t, w)
	assert.Empty(t, list.Plugins)
	require.NotEmpty(t, list.Catalog)
	// The catalog's flagship entry models the two-process setup: a
	// long-running canvas container + the claude-mcp registration.
	require.Len(t, catalogEntry(t, "excalidraw-mcp").Steps, 2)

	// Create.
	w = servePlugins(t, http.MethodPost, "/api/plugins",
		`{"name":"demo","descr":"a demo","steps":[{"name":"probe","check":"true"}]}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	// Create fires its first status check in the background; drain it
	// so the assertions below are deterministic.
	bgWG.Wait()

	// Duplicate name refused.
	w = servePlugins(t, http.MethodPost, "/api/plugins",
		`{"name":"demo","steps":[{"name":"x","run":"true"}]}`)
	assert.Equal(t, http.StatusConflict, w.Code)

	// The background check ran `true` → the plugin reads "ok".
	w = servePlugins(t, http.MethodGet, "/api/plugins", "")
	list = decodePluginsList(t, w)
	require.Len(t, list.Plugins, 1)
	assert.Equal(t, "ok", list.Plugins[0].Status)
	assert.Equal(t, 0, list.Warn)

	// Rename via PUT, body carries the new name.
	w = servePlugins(t, http.MethodPut, "/api/plugins/demo",
		`{"name":"demo2","steps":[{"name":"probe","check":"true"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bgWG.Wait()
	w = servePlugins(t, http.MethodGet, "/api/plugins", "")
	list = decodePluginsList(t, w)
	require.Len(t, list.Plugins, 1)
	assert.Equal(t, "demo2", list.Plugins[0].Name)

	// Renaming onto another existing plugin's name is refused.
	w = servePlugins(t, http.MethodPost, "/api/plugins",
		`{"name":"other","steps":[{"name":"x","run":"true"}]}`)
	require.Equal(t, http.StatusCreated, w.Code)
	bgWG.Wait()
	w = servePlugins(t, http.MethodPut, "/api/plugins/other",
		`{"name":"demo2","steps":[{"name":"x","run":"true"}]}`)
	assert.Equal(t, http.StatusConflict, w.Code)

	// Updating a plugin that doesn't exist is a 404.
	w = servePlugins(t, http.MethodPut, "/api/plugins/ghost",
		`{"name":"ghost","steps":[{"name":"x","run":"true"}]}`)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Delete; a second delete of the same name is a 404.
	w = servePlugins(t, http.MethodDelete, "/api/plugins/demo2", "")
	require.Equal(t, http.StatusOK, w.Code)
	w = servePlugins(t, http.MethodDelete, "/api/plugins/demo2", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = servePlugins(t, http.MethodGet, "/api/plugins", "")
	list = decodePluginsList(t, w)
	require.Len(t, list.Plugins, 1)
	assert.Equal(t, "other", list.Plugins[0].Name)
}

func TestPlugins_CheckStatuses(t *testing.T) {
	setupPluginsTest(t)
	require.NoError(t, savePlugins([]Plugin{{
		Name: "mixed",
		Steps: []PluginStep{
			{Name: "passing", Check: "true"},
			{Name: "failing", Check: "false", Run: "true"},
			{Name: "run-only", Run: "true"},
		},
	}}))

	// Before any check runs, the snapshot reads "unknown" and raises
	// no warning — never-checked must not light the badge.
	plugins, warn, err := collectPluginsSnapshot()
	require.NoError(t, err)
	require.Len(t, plugins, 1)
	assert.Equal(t, "unknown", plugins[0].Status)
	assert.Equal(t, 0, warn)

	// Synchronous re-check: one failing step → plugin is "warn", and
	// the per-step verdicts land on the right steps.
	w := servePlugins(t, http.MethodPost, "/api/plugins/mixed/check", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	v := decodePluginView(t, w)
	assert.Equal(t, "warn", v.Status)
	require.Len(t, v.Steps, 3)
	assert.True(t, v.Steps[0].Checked)
	assert.True(t, v.Steps[0].OK)
	assert.True(t, v.Steps[1].Checked)
	assert.False(t, v.Steps[1].OK)
	assert.False(t, v.Steps[2].Checked, "a run-only step has no check verdict")

	// The "installed but not active" warning now drives the badge
	// count the snapshot carries.
	_, warn, err = collectPluginsSnapshot()
	require.NoError(t, err)
	assert.Equal(t, 1, warn)

	// Fix the definition so every check passes; check-all flips the
	// plugin to "ok" and clears the warning.
	require.NoError(t, savePlugins([]Plugin{{
		Name: "mixed",
		Steps: []PluginStep{
			{Name: "passing", Check: "true"},
			{Name: "fixed", Check: "true"},
		},
	}}))
	w = servePlugins(t, http.MethodPost, "/api/plugins/check", "")
	require.Equal(t, http.StatusOK, w.Code)
	list := decodePluginsList(t, w)
	require.Len(t, list.Plugins, 1)
	assert.Equal(t, "ok", list.Plugins[0].Status)
	assert.Equal(t, 0, list.Warn)
}

func TestPlugins_RunStep(t *testing.T) {
	setupPluginsTest(t)
	marker := filepath.Join(t.TempDir(), "started")
	require.NoError(t, savePlugins([]Plugin{{
		Name: "svc",
		Steps: []PluginStep{
			{Name: "service up", Check: "test -f '" + marker + "'", Run: "touch '" + marker + "'"},
			{Name: "check-only", Check: "true"},
		},
	}}))

	// Not "running" yet.
	w := servePlugins(t, http.MethodPost, "/api/plugins/svc/check", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "warn", decodePluginView(t, w).Status)

	// Run step 0 — the daemon executes the run command, then re-checks
	// the whole plugin so the response carries the post-run statuses.
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/steps/0/run", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res pluginRunResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.True(t, res.Ran)
	assert.True(t, res.OK)
	assert.Equal(t, "ok", res.Plugin.Status)
	_, err := os.Stat(marker)
	assert.NoError(t, err, "the run command actually executed")

	// A failing run command reports OK=false but still re-checks.
	require.NoError(t, savePlugins([]Plugin{{
		Name:  "svc",
		Steps: []PluginStep{{Name: "boom", Check: "true", Run: "false"}},
	}}))
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/steps/0/run", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.False(t, res.OK)
	assert.Equal(t, "ok", res.Plugin.Status, "the check still passes even though the run failed")

	// Guard rails: bad index / missing run command / unknown plugin.
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/steps/7/run", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	require.NoError(t, savePlugins([]Plugin{{
		Name:  "svc",
		Steps: []PluginStep{{Name: "probe-only", Check: "true"}},
	}}))
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/steps/0/run", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = servePlugins(t, http.MethodPost, "/api/plugins/ghost/steps/0/run", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestPlugins_ActivateDeactivate covers the whole-plugin toggle: a
// two-step plugin with marker-file state and an order log. Activate
// runs the steps forward (skipping satisfied ones), deactivate runs
// the stop commands in reverse and persists Disabled — which flips the
// status semantics to "off is expected".
func TestPlugins_ActivateDeactivate(t *testing.T) {
	setupPluginsTest(t)
	dir := t.TempDir()
	m1, m2 := filepath.Join(dir, "m1"), filepath.Join(dir, "m2")
	logFile := filepath.Join(dir, "log")
	readLog := func() []string {
		data, err := os.ReadFile(logFile)
		if err != nil {
			return nil
		}
		return strings.Fields(strings.TrimSpace(string(data)))
	}
	require.NoError(t, savePlugins([]Plugin{{
		Name: "svc",
		Steps: []PluginStep{
			{
				Name:  "first",
				Check: "test -f '" + m1 + "'",
				Run:   "touch '" + m1 + "' && echo run-first >> '" + logFile + "'",
				Stop:  "rm -f '" + m1 + "' && echo stop-first >> '" + logFile + "'",
			},
			{
				Name:  "second",
				Check: "test -f '" + m2 + "'",
				Run:   "touch '" + m2 + "' && echo run-second >> '" + logFile + "'",
				Stop:  "rm -f '" + m2 + "' && echo stop-second >> '" + logFile + "'",
			},
		},
	}}))

	// Deactivating an already-down plugin executes nothing (every check
	// already fails) but still records the intent: status reads "off",
	// not "warn", and the badge stays dark.
	w := servePlugins(t, http.MethodPost, "/api/plugins/svc/deactivate", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res pluginRunResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.False(t, res.Ran, "nothing was active, so no stop command ran")
	assert.True(t, res.OK)
	assert.True(t, res.Plugin.Disabled)
	assert.Equal(t, "off", res.Plugin.Status)
	_, warn, err := collectPluginsSnapshot()
	require.NoError(t, err)
	assert.Equal(t, 0, warn, "a deactivated, down plugin must not light the badge")

	// Activate: both steps run, in order, and the flag clears.
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/activate", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	res = pluginRunResponse{} // disabled is omitempty — reset between decodes
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.True(t, res.Ran)
	assert.True(t, res.OK)
	assert.False(t, res.Plugin.Disabled)
	assert.Equal(t, "ok", res.Plugin.Status)
	assert.Equal(t, []string{"run-first", "run-second"}, readLog())

	// Re-activating a healthy plugin is a no-op: every check passes, so
	// no run command executes again.
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/activate", "")
	require.Equal(t, http.StatusOK, w.Code)
	res = pluginRunResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.False(t, res.Ran, "already satisfied — nothing should re-run")
	assert.Equal(t, []string{"run-first", "run-second"}, readLog())

	// Deactivate: stop commands run in REVERSE order, markers vanish,
	// the flag persists in plugins.json.
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/deactivate", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	res = pluginRunResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.True(t, res.Ran)
	assert.True(t, res.OK)
	assert.Equal(t, "off", res.Plugin.Status)
	assert.Equal(t, []string{"run-first", "run-second", "stop-second", "stop-first"}, readLog())
	_, err = os.Stat(m1)
	assert.True(t, os.IsNotExist(err))
	stored, err := loadPlugins()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.True(t, stored[0].Disabled, "the deactivation intent is persisted")

	// A disabled plugin whose stoppable step is somehow active again
	// (here: the marker reappears behind tclaude's back) warns —
	// "deactivated but still running" is the inverse of the tab's
	// usual "installed but not started" signal.
	require.NoError(t, os.WriteFile(m1, nil, 0o644))
	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/check", "")
	require.Equal(t, http.StatusOK, w.Code)
	v := decodePluginView(t, w)
	assert.True(t, v.Disabled)
	assert.Equal(t, "warn", v.Status)
	_, warn, err = collectPluginsSnapshot()
	require.NoError(t, err)
	assert.Equal(t, 1, warn)

	// Unknown plugin → 404 on both verbs.
	w = servePlugins(t, http.MethodPost, "/api/plugins/ghost/activate", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = servePlugins(t, http.MethodPost, "/api/plugins/ghost/deactivate", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestPlugins_StopStep covers the per-step stop button: it executes
// the step's stop command and re-checks, and a step without one is a
// 400 — mirroring the run-step guard rails.
func TestPlugins_StopStep(t *testing.T) {
	setupPluginsTest(t)
	marker := filepath.Join(t.TempDir(), "started")
	require.NoError(t, savePlugins([]Plugin{{
		Name: "svc",
		Steps: []PluginStep{
			{Name: "service up", Check: "test -f '" + marker + "'", Run: "touch '" + marker + "'", Stop: "rm -f '" + marker + "'"},
			{Name: "no stop", Check: "true"},
		},
	}}))
	require.NoError(t, os.WriteFile(marker, nil, 0o644))

	w := servePlugins(t, http.MethodPost, "/api/plugins/svc/steps/0/stop", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res pluginRunResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.True(t, res.Ran)
	assert.True(t, res.OK)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "the stop command actually executed")
	// Stopping a single step does NOT flip the plugin-level Disabled
	// intent — the plugin is still enabled, so the dead check warns.
	assert.False(t, res.Plugin.Disabled)
	assert.Equal(t, "warn", res.Plugin.Status)

	w = servePlugins(t, http.MethodPost, "/api/plugins/svc/steps/1/stop", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, "a step without a stop command is refused")
}

// TestPlugins_UpdatePreservesDisabled guards the edit path: the modal
// PUT body never carries the Disabled flag, so the handler must keep
// the stored intent instead of silently re-enabling the plugin.
func TestPlugins_UpdatePreservesDisabled(t *testing.T) {
	setupPluginsTest(t)
	require.NoError(t, savePlugins([]Plugin{{
		Name:     "svc",
		Disabled: true,
		Steps:    []PluginStep{{Name: "probe", Check: "true"}},
	}}))
	w := servePlugins(t, http.MethodPut, "/api/plugins/svc",
		`{"name":"svc","steps":[{"name":"probe","check":"true","stop":"true"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bgWG.Wait()
	stored, err := loadPlugins()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.True(t, stored[0].Disabled, "an edit must not re-enable a deactivated plugin")
	assert.Equal(t, "true", stored[0].Steps[0].Stop, "the stop command round-trips through PUT")
}

func TestPlugins_Validation(t *testing.T) {
	setupPluginsTest(t)
	for name, body := range map[string]string{
		"empty name":            `{"name":"","steps":[{"name":"x","run":"true"}]}`,
		"no steps":              `{"name":"p","steps":[]}`,
		"step without commands": `{"name":"p","steps":[{"name":"x"}]}`,
		"step without name":     `{"name":"p","steps":[{"run":"true"}]}`,
		"slash in name":         `{"name":"a/b","steps":[{"name":"x","run":"true"}]}`,
		"malformed JSON":        `{"name":`,
	} {
		w := servePlugins(t, http.MethodPost, "/api/plugins", body)
		assert.Equalf(t, http.StatusBadRequest, w.Code, "%s: %s", name, w.Body.String())
	}
}

// TestPlugins_OutputCappedWhileStreaming guards the bounded-tail
// capture: a noisy command must come back tail-truncated without the
// daemon ever buffering the full stream (tailBuffer caps in Write).
func TestPlugins_OutputCappedWhileStreaming(t *testing.T) {
	setupPluginsTest(t)
	out, ok := runPluginShell("head -c 9000 /dev/zero | tr '\\0' x; echo END", pluginCheckTimeout)
	assert.True(t, ok)
	assert.LessOrEqual(t, len(out), pluginOutputMax+len("…"))
	assert.True(t, strings.HasPrefix(out, "…"), "truncated output is marked")
	assert.True(t, strings.HasSuffix(out, "END"), "the tail — where errors land — is what survives")

	// The writer itself: small writes never truncate, overflow keeps
	// the tail and flags it.
	tb := &tailBuffer{max: 8}
	_, _ = tb.Write([]byte("abc"))
	assert.Equal(t, "abc", tb.String())
	_, _ = tb.Write([]byte("defghijk"))
	assert.Equal(t, "…defghijk", tb.String())
	assert.True(t, tb.truncated)
}

// TestPlugins_ShellSuppressesTclaudeHooks guards that everything a
// plugin step runs sees TCLAUDE_IGNORE_HOOKS=1. Steps routinely invoke
// `claude` itself (the catalog's `claude mcp get` probes run every
// checker sweep); without the suppression those one-shot runs execute
// the user's installed tclaude hooks and each probe fired a spurious
// "Exited" desktop notification — once per minute.
func TestPlugins_ShellSuppressesTclaudeHooks(t *testing.T) {
	setupPluginsTest(t)
	out, ok := runPluginShell(`printf %s "$TCLAUDE_IGNORE_HOOKS"`, pluginCheckTimeout)
	assert.True(t, ok)
	assert.Equal(t, "1", out, "plugin step subprocesses must run with TCLAUDE_IGNORE_HOOKS=1")
}

// TestPlugins_BrokenRegistrySurfaces guards that an unreadable
// plugins.json is reported as an error — not rendered as "no plugins
// installed": 500 on the sync endpoints, error value on the snapshot
// path (which the dashboard shows as a banner via plugins_error).
func TestPlugins_BrokenRegistrySurfaces(t *testing.T) {
	setupPluginsTest(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(pluginsPath()), 0o755))
	require.NoError(t, os.WriteFile(pluginsPath(), []byte("{not json"), 0o644))

	w := servePlugins(t, http.MethodGet, "/api/plugins", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	w = servePlugins(t, http.MethodPost, "/api/plugins/check", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	_, _, err := collectPluginsSnapshot()
	require.Error(t, err)
}

// TestPlugins_StaleCacheRejected guards the race where a sweep working
// from an old plugins.json publishes results after the definition
// changed: pluginView must only trust a cached slot produced for the
// step's CURRENT check command.
func TestPlugins_StaleCacheRejected(t *testing.T) {
	setupPluginsTest(t)
	p := Plugin{Name: "svc", Steps: []PluginStep{{Name: "probe", Check: "true"}}}
	require.NoError(t, savePlugins([]Plugin{p}))
	checkPlugin(p)
	v := pluginView(p)
	require.True(t, v.Steps[0].Checked)

	// The definition changes (same name, same index, different check) —
	// the cached verdict is for the OLD command and must be ignored.
	edited := Plugin{Name: "svc", Steps: []PluginStep{{Name: "probe", Check: "false"}}}
	require.NoError(t, savePlugins([]Plugin{edited}))
	v = pluginView(edited)
	assert.False(t, v.Steps[0].Checked, "stale verdict for a different command must not surface")
	assert.Equal(t, "unknown", v.Status)

	// A fresh check of the edited definition repopulates honestly.
	checkPlugin(edited)
	v = pluginView(edited)
	assert.True(t, v.Steps[0].Checked)
	assert.False(t, v.Steps[0].OK)
	assert.Equal(t, "warn", v.Status)
}

func TestPlugins_AuthRequired(t *testing.T) {
	setupPluginsTest(t)
	mux := http.NewServeMux()
	registerDashboardPluginRoutes(mux)
	// No dashboard cookie → refused before any handler logic runs.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/plugins", nil))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestPlugins_InstallFromCatalog mirrors what the dashboard's
// "+ install" button does: POST the catalog definition verbatim. The
// copy then behaves like any hand-written plugin (editable, checkable).
func TestPlugins_InstallFromCatalog(t *testing.T) {
	setupPluginsTest(t)
	// The catalog's real commands probe docker + claude — stub the
	// exec so the create's background check stays hermetic.
	prevRun := runPluginShell
	runPluginShell = func(string, time.Duration) (string, bool) { return "stubbed", false }
	t.Cleanup(func() { runPluginShell = prevRun })

	def, err := json.Marshal(catalogEntry(t, "excalidraw-mcp"))
	require.NoError(t, err)
	w := servePlugins(t, http.MethodPost, "/api/plugins", string(def))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	bgWG.Wait()
	w = servePlugins(t, http.MethodGet, "/api/plugins", "")
	list := decodePluginsList(t, w)
	require.Len(t, list.Plugins, 1)
	require.Len(t, installedPlugin(t, list, "excalidraw-mcp").Steps, 2)
}
