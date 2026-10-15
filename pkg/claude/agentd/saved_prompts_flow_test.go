package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The shared library survives through the production HTTP/SQLite path, with
// rename collisions and missing names distinguished from successful updates.
func TestSavedPromptsCRUD(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodGet, "/v1/saved-prompts", nil).Code)
	body := map[string]any{"name": "review", "body": "  Review this.\r\nKeep context.\n"}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/saved-prompts", body).Code)
	require.Equal(t, http.StatusConflict, humanReq(t, f, http.MethodPost, "/v1/saved-prompts", body).Code)
	var prompts []db.SavedPrompt
	testharness.DecodeJSON(t, humanReq(t, f, http.MethodGet, "/v1/saved-prompts", nil), &prompts)
	require.Equal(t, []db.SavedPrompt{{Name: "review", Body: "  Review this.\nKeep context.\n"}}, prompts)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/saved-prompts", map[string]any{"name": "other", "body": "Other"}).Code)
	require.Equal(t, http.StatusConflict, humanReq(t, f, http.MethodPatch, "/v1/saved-prompts/review", map[string]any{"name": "other", "body": "changed"}).Code)
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/saved-prompts/review", map[string]any{"name": "audit", "body": "New text"}).Code)
	require.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodGet, "/v1/saved-prompts/review", nil).Code)
	var prompt db.SavedPrompt
	testharness.DecodeJSON(t, humanReq(t, f, http.MethodGet, "/v1/saved-prompts/audit", nil), &prompt)
	require.Equal(t, db.SavedPrompt{Name: "audit", Body: "New text"}, prompt)
	require.Equal(t, http.StatusNoContent, humanReq(t, f, http.MethodDelete, "/v1/saved-prompts/audit", nil).Code)
	require.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodDelete, "/v1/saved-prompts/audit", nil).Code)
	require.Equal(t, http.StatusNotFound, humanReq(t, f, http.MethodPatch, "/v1/saved-prompts/missing", body).Code)
}

func TestSavedPromptsValidationAndPermissions(t *testing.T) {
	f := newFlow(t)
	for _, body := range []map[string]any{
		{"name": "", "body": "text"}, {"name": "bad/name", "body": "text"},
		{"name": "empty", "body": " \n"}, {"name": "large", "body": strings.Repeat("x", 16385)},
	} {
		require.Equal(t, http.StatusBadRequest, humanReq(t, f, http.MethodPost, "/v1/saved-prompts", body).Code)
	}
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/saved-prompts", map[string]any{"name": "review", "body": "Text"}).Code)
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		path := "/v1/saved-prompts"
		if method != http.MethodPost {
			path += "/review"
		}
		req := agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, map[string]any{"name": "review", "body": "unauthorized"}), "unprivileged")
		rec := testharness.Serve(f.Mux, req)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/saved-prompts/review", nil), "unprivileged"))
	require.Equal(t, http.StatusOK, rec.Code)
	var prompt db.SavedPrompt
	testharness.DecodeJSON(t, rec, &prompt)
	require.Equal(t, "Text", prompt.Body)
}
