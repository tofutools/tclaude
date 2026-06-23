package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The user-level default model endpoint reads/writes the "model" key
// in ~/.claude/settings.json — and newFlow's World pins HOME to a
// per-test tmpdir, so these tests exercise the real file path without
// ever touching the developer's own settings.

// seedSettings writes a settings.json (creating ~/.claude) with the
// given content and mode, returning its path.
func seedSettings(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := session.ClaudeSettingsPath()
	require.NotEmpty(t, path, "ClaudeSettingsPath")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	return path
}

// getUserModel fetches GET /v1/claude-settings/default-model and
// returns the reported model.
func getUserModel(t *testing.T, mux http.Handler) string {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/claude-settings/default-model", nil))
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "GET body=%s", rec.Body.String())
	var resp struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Model
}

// Scenario: the human edits the user-level default model from the
// dashboard. PUT must update ONLY the "model" key — sibling keys (the
// settings file also carries hooks, permissions, sandbox config) and
// the file's permission bits survive the rewrite. GET and the
// dashboard snapshot both report the new value.
func TestUserDefaultModel_PutPreservesSiblingsAndMode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The dashboard mux's auth pins Origin against the popup base URL;
		// the test handler stamps it from the same value.
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)
		path := seedSettings(t, `{
	  "model": "claude-fable-5[1m]",
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "tclaude hook"}]}]},
	  "permissions": {"deny": ["Read(.envrc)"]}
	}`, 0o600)

		// The seeded value reads back via GET (and the snapshot below).
		require.Equal(t, "claude-fable-5[1m]", getUserModel(t, f.Mux))

		// PUT a new alias.
		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPut, "/v1/claude-settings/default-model",
			map[string]any{"model": "sonnet[1m]"}))
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "PUT body=%s", rec.Body.String())
		assert.Equal(t, "sonnet[1m]", getUserModel(t, f.Mux))

		// Sibling keys survived, the model changed, the mode held.
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var tree map[string]any
		require.NoError(t, json.Unmarshal(data, &tree))
		assert.Equal(t, "sonnet[1m]", tree["model"], "model rewritten")
		assert.Contains(t, tree, "hooks", "sibling keys preserved")
		assert.Contains(t, tree, "permissions", "sibling keys preserved")
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a private settings file stays private")

		// The dashboard snapshot carries it for the Groups-tab chip and
		// the spawn modal's Default label. (/api/* lives on the dashboard
		// mux, not the /v1 Unix-socket mux.)
		dash := agentd.BuildDashboardHandlerForTest()
		sr := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
		srec := testharness.Serve(dash, sr)
		require.Equal(t, http.StatusOK, srec.Code, "/api/snapshot body=%s", srec.Body.String())
		var snap struct {
			UserDefaultModel string `json:"user_default_model"`
		}
		require.NoError(t, json.Unmarshal(srec.Body.Bytes(), &snap))
		assert.Equal(t, "sonnet[1m]", snap.UserDefaultModel, "snapshot user_default_model")

		// And the dashboard's cookie-authed twin — the exact route the 🧠
		// chip's inline editor PUTs — works end to end too.
		dr := testharness.JSONRequest(t, http.MethodPut, "/api/claude-settings/default-model",
			map[string]any{"model": "haiku"})
		drec := testharness.Serve(dash, dr)
		require.Equal(t, http.StatusOK, drec.Code, "dashboard PUT body=%s", drec.Body.String())
		assert.Equal(t, "haiku", getUserModel(t, f.Mux), "dashboard twin writes the same file")
	})
}

// Scenario: clearing the user default (PUT model:"") removes the
// "model" key outright — claude then falls back to its built-in
// default — without disturbing sibling keys. Clearing when no
// settings file exists at all is a clean no-op, not an error.
func TestUserDefaultModel_ClearRemovesKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		path := seedSettings(t, `{"model": "opus", "theme": "dark"}`, 0o644)

		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPut, "/v1/claude-settings/default-model",
			map[string]any{"model": ""}))
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "PUT body=%s", rec.Body.String())

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var tree map[string]any
		require.NoError(t, json.Unmarshal(data, &tree))
		assert.NotContains(t, tree, "model", "cleared key is removed, not set to \"\"")
		assert.Equal(t, "dark", tree["theme"], "sibling keys preserved")
		assert.Equal(t, "", getUserModel(t, f.Mux), "GET reports unset")

		// Clearing with no settings.json present: no-op, no file created.
		require.NoError(t, os.Remove(path))
		rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPut, "/v1/claude-settings/default-model",
			map[string]any{"model": ""})))
		require.Equal(t, http.StatusOK, rec.Code, "clear-on-missing body=%s", rec.Body.String())
		_, err = os.Stat(path)
		assert.True(t, os.IsNotExist(err), "clearing must not conjure a settings file")
	})
}

// Scenario: setting a model when no settings.json exists yet creates
// the file with just that key — the path a fresh machine hits.
func TestUserDefaultModel_PutCreatesFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		path := session.ClaudeSettingsPath()
		require.NotEmpty(t, path)

		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPut, "/v1/claude-settings/default-model",
			map[string]any{"model": "haiku"}))
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "PUT body=%s", rec.Body.String())

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var tree map[string]any
		require.NoError(t, json.Unmarshal(data, &tree))
		assert.Equal(t, "haiku", tree["model"])
	})
}

// Scenario: guard rails. An unknown alias is a 400 (same ValidateModel
// gate as every other model surface) and a corrupt settings.json must
// fail the PUT — never be silently replaced, it also carries hooks and
// permission config. In both cases the file is left byte-identical.
func TestUserDefaultModel_PutGuards(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		path := seedSettings(t, `{"model": "opus"`, 0o644) // corrupt: unterminated

		before, err := os.ReadFile(path)
		require.NoError(t, err)

		// Invalid alias → 400 before any file I/O.
		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPut, "/v1/claude-settings/default-model",
			map[string]any{"model": "gpt"}))
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "invalid model body=%s", rec.Body.String())

		// Valid alias against a corrupt file → 500, file untouched.
		r = agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPut, "/v1/claude-settings/default-model",
			map[string]any{"model": "sonnet"}))
		rec = testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "corrupt file body=%s", rec.Body.String())

		after, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "failed PUTs must not modify the file")
	})
}

// Scenario: an agent (not the human) tries to rewrite the human's
// settings.json. The settings.default-model slug is not
// default-granted, so the PUT is refused — GET, by contrast, is open
// (the value is a model name, not a secret; agents may want to know
// what a default spawn runs on).
func TestUserDefaultModel_AgentNeedsGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		// An agent peer needs only a conv-id — the permission check runs
		// before any conv lookup, so no session/enrollment setup required.
		const caller = "cccc-1111-2222-3333-4444"
		seedSettings(t, `{"model": "opus"}`, 0o644)

		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPut, "/v1/claude-settings/default-model",
			map[string]any{"model": "haiku"}), caller)
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent PUT without the slug must be refused; body=%s", rec.Body.String())
		assert.Equal(t, "opus", getUserModel(t, f.Mux), "file unchanged")

		rg := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodGet, "/v1/claude-settings/default-model", nil), caller)
		grec := testharness.Serve(f.Mux, rg)
		assert.Equal(t, http.StatusOK, grec.Code, "agent GET is open; body=%s", grec.Body.String())
	})
}
