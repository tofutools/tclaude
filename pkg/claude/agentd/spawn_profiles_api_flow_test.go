package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These exercise the /v1/spawn-profiles CRUD surface (JOH-210) and, centrally,
// its harness-aware save-time validation — the structural fix for CodeRabbit
// #343: a profile's model/effort/sandbox are validated against the profile's
// OWN harness, not the Claude-only validator.

func profileReq(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, body))
	return testharness.Serve(f.Mux, r)
}

// wireProfile mirrors the fields of the JSON the daemon returns (the handler's
// spawnProfileJSON is unexported); the *bool toggles let the test assert the
// unset (null/absent) tri-state round-trips.
type wireProfile struct {
	Name         string `json:"name"`
	Harness      string `json:"harness"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Sandbox      string `json:"sandbox"`
	AutoReview   *bool  `json:"auto_review"`
	SyncWorktree *bool  `json:"sync_worktree"`
}

// Scenario: create → get → list → patch (rename + remodel) → delete → 404.
func TestSpawnProfiles_CRUDRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
			map[string]any{"name": "alpha", "model": "sonnet", "role": "worker"})
		require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

		// GET one.
		rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/alpha", nil)
		require.Equalf(t, http.StatusOK, rec.Code, "get body=%s", rec.Body.String())
		var got wireProfile
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, "sonnet", got.Model)

		// LIST contains it.
		rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var list []wireProfile
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Len(t, list, 1)
		assert.Equal(t, "alpha", list[0].Name)

		// PATCH: rename + change model (full-replace semantics).
		rec = profileReq(t, f, http.MethodPatch, "/v1/spawn-profiles/alpha",
			map[string]any{"name": "alpha2", "model": "opus"})
		require.Equalf(t, http.StatusOK, rec.Code, "patch body=%s", rec.Body.String())

		rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/alpha2", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		got = wireProfile{}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, "opus", got.Model, "patched model")

		// The old name is gone.
		rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/alpha", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "old name should 404 after rename")

		// DELETE → 204, then 404.
		rec = profileReq(t, f, http.MethodDelete, "/v1/spawn-profiles/alpha2", nil)
		require.Equal(t, http.StatusNoContent, rec.Code)
		rec = profileReq(t, f, http.MethodDelete, "/v1/spawn-profiles/alpha2", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "delete of a gone profile is 404")
	})
}

// Scenario (#343): the SAME model string is valid for a Codex profile and
// rejected for a Claude profile, because validation keys on the profile's own
// harness. Likewise a launch field a harness can't take (a Claude profile with
// a sandbox) is rejected at save.
func TestSpawnProfiles_HarnessAwareValidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		// Codex harness validates its own model → accepted.
		rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
			map[string]any{"name": "cx", "harness": "codex", "model": "gpt-5-codex", "effort": "high"})
		require.Equalf(t, http.StatusCreated, rec.Code, "codex model on a codex profile body=%s", rec.Body.String())

		// Same model on a blank-harness (→ Claude) profile → 400: the Claude
		// validator rejects an OpenAI model. This is exactly the case the old
		// default_model gate got wrong.
		rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
			map[string]any{"name": "bad", "model": "gpt-5-codex"})
		assert.Equalf(t, http.StatusBadRequest, rec.Code,
			"a codex model on a Claude profile must 400; body=%s", rec.Body.String())

		// A Claude model on a Claude profile → accepted.
		rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
			map[string]any{"name": "cc", "harness": "claude", "model": "opus"})
		require.Equalf(t, http.StatusCreated, rec.Code, "claude model on a claude profile body=%s", rec.Body.String())

		// A sandbox mode on a Claude profile → 400 (Claude has no launch sandbox).
		rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
			map[string]any{"name": "cc-sb", "harness": "claude", "sandbox": "read-only"})
		assert.Equalf(t, http.StatusBadRequest, rec.Code,
			"a sandbox on a Claude profile must 400; body=%s", rec.Body.String())
	})
}

// Scenario: a duplicate name is a 409 — the name is the route key and carries a
// UNIQUE constraint.
func TestSpawnProfiles_NameCollision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		require.Equal(t, http.StatusCreated,
			profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "dup"}).Code)
		rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "dup"})
		assert.Equalf(t, http.StatusConflict, rec.Code, "duplicate name should 409; body=%s", rec.Body.String())
	})
}

// Scenario: the tri-state toggles round-trip. A toggle set explicitly comes
// back set; an omitted toggle comes back unset (null), distinct from false.
func TestSpawnProfiles_ToggleTristateRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		// auto_review only validates true on a harness with a guardian (codex).
		rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles",
			map[string]any{"name": "tg", "harness": "codex", "auto_review": true, "sync_worktree": false})
		require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

		rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/tg", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var got wireProfile
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.NotNil(t, got.AutoReview, "auto_review was set, must round-trip non-nil")
		assert.True(t, *got.AutoReview, "auto_review true round-trips")
		require.NotNil(t, got.SyncWorktree, "sync_worktree was set false, must round-trip non-nil")
		assert.False(t, *got.SyncWorktree, "sync_worktree false round-trips")

		// A profile that omits the toggles reads them back as unset (null).
		require.Equal(t, http.StatusCreated,
			profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "plain"}).Code)
		rec = profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/plain", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		got = wireProfile{}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Nil(t, got.AutoReview, "omitted auto_review stays unset")
		assert.Nil(t, got.SyncWorktree, "omitted sync_worktree stays unset")
	})
}
