package agentd

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const validProcessSnippetEnvelope = `{"kind":"tclaude/process-selection","version":1,"nodes":[{"id":"done","node":{"type":"end","result":"success"},"position":{"x":10,"y":20}}],"edges":[]}`

func enableProcessSnippetTests(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	withDashboardAuth(t)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Processes: true}}))
}

func serveProcessSnippetRequest(r *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	registerDashboardProcessSnippetRoutes(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, r)
	return recorder
}

func decodeProcessSnippetResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), "body=%s", recorder.Body.String())
	return body
}

func createProcessSnippetRequest(t *testing.T, name string) map[string]any {
	t.Helper()
	body := `{"name":` + strconv.Quote(name) + `,"envelope":` + validProcessSnippetEnvelope + `}`
	recorder := serveProcessSnippetRequest(dashboardRequest(http.MethodPost, "/api/process/snippets", body))
	require.Equal(t, http.StatusCreated, recorder.Code, "body=%s", recorder.Body.String())
	return decodeProcessSnippetResponse(t, recorder)
}

func TestProcessSnippetEnvelopeParityWithClipboardAuthority(t *testing.T) {
	source, err := fs.ReadFile(dashboardAssetsFS, "js/process-editor-clipboard.js")
	require.NoError(t, err)
	text := string(source)
	stringConstant := func(name string) string {
		match := regexp.MustCompile(`export const ` + name + ` = '([^']+)';`).FindStringSubmatch(text)
		require.Len(t, match, 2, name)
		return match[1]
	}
	integerConstant := func(name string) int {
		match := regexp.MustCompile(`export const ` + name + ` = ([0-9_]+);`).FindStringSubmatch(text)
		require.Len(t, match, 2, name)
		value, err := strconv.Atoi(strings.ReplaceAll(match[1], "_", ""))
		require.NoError(t, err)
		return value
	}
	assert.Equal(t, processSnippetEnvelopeKind, stringConstant("PROCESS_CLIPBOARD_KIND"))
	assert.Equal(t, processSnippetEnvelopeVersion, integerConstant("PROCESS_CLIPBOARD_VERSION"))
	assert.Contains(t, text, "export const PROCESS_CLIPBOARD_MAX_BYTES = 256 * 1024;")
	assert.Equal(t, 256*1024, db.MaxProcessSnippetEnvelopeBytes)
	assert.Equal(t, model.MaxNormalizedNodes, integerConstant("PROCESS_CLIPBOARD_MAX_NODES"))
	assert.Equal(t, model.MaxNormalizedEdges, integerConstant("PROCESS_CLIPBOARD_MAX_EDGES"))
	assert.Equal(t, processSnippetMaxNodeIDBytes, integerConstant("PROCESS_CLIPBOARD_MAX_ID"))
	assert.Equal(t, processSnippetMaxOutcomeBytes, integerConstant("PROCESS_CLIPBOARD_MAX_OUTCOME"))
	assert.Equal(t, processSnippetMaxCoordinate, integerConstant("PROCESS_CLIPBOARD_MAX_COORDINATE"))
}

func TestProcessSnippetAPIAuthCRUDCASAndNamePolicy(t *testing.T) {
	enableProcessSnippetTests(t)

	unauthorized := httptest.NewRequest(http.MethodGet, "/api/process/snippets", nil)
	recorder := serveProcessSnippetRequest(unauthorized)
	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "snippets")
	foreignOrigin := dashboardRequest(http.MethodPost, "/api/process/snippets",
		`{"name":"CSRF probe","envelope":`+validProcessSnippetEnvelope+`}`)
	foreignOrigin.Header.Set("Origin", "http://evil.example")
	recorder = serveProcessSnippetRequest(foreignOrigin)
	assert.Equal(t, http.StatusForbidden, recorder.Code, "cross-origin mutation must fail before storage")

	unexpectedRevision := serveProcessSnippetRequest(dashboardRequest(http.MethodPost, "/api/process/snippets",
		`{"name":"Unexpected revision","revision":1,"envelope":`+validProcessSnippetEnvelope+`}`))
	assert.Equal(t, http.StatusBadRequest, unexpectedRevision.Code)

	created := createProcessSnippetRequest(t, "  Review gate  ")
	snippet := created["snippet"].(map[string]any)
	assert.Equal(t, "Review gate", snippet["name"])
	assert.Equal(t, true, snippet["available"])
	assert.NotEmpty(t, snippet["envelope"])
	id := snippet["id"].(string)

	duplicate := serveProcessSnippetRequest(dashboardRequest(http.MethodPost, "/api/process/snippets",
		`{"name":"review GATE","envelope":`+validProcessSnippetEnvelope+`}`))
	assert.Equal(t, http.StatusConflict, duplicate.Code)

	rename := serveProcessSnippetRequest(dashboardRequest(http.MethodPatch, "/api/process/snippets/"+id,
		`{"name":"Approval gate","revision":1}`))
	require.Equal(t, http.StatusOK, rename.Code, "body=%s", rename.Body.String())
	renamed := decodeProcessSnippetResponse(t, rename)["snippet"].(map[string]any)
	assert.Equal(t, float64(2), renamed["revision"])

	stale := serveProcessSnippetRequest(dashboardRequest(http.MethodPatch, "/api/process/snippets/"+id,
		`{"name":"Stale","revision":1}`))
	assert.Equal(t, http.StatusConflict, stale.Code)
	missing := serveProcessSnippetRequest(dashboardRequest(http.MethodDelete,
		"/api/process/snippets/psn_00000000000000000000000000000000", `{"revision":1}`))
	assert.Equal(t, http.StatusNotFound, missing.Code)

	deleted := serveProcessSnippetRequest(dashboardRequest(http.MethodDelete, "/api/process/snippets/"+id, `{"revision":2}`))
	assert.Equal(t, http.StatusOK, deleted.Code)
	list := serveProcessSnippetRequest(dashboardRequest(http.MethodGet, "/api/process/snippets", ""))
	require.Equal(t, http.StatusOK, list.Code)
	assert.Empty(t, decodeProcessSnippetResponse(t, list)["snippets"].([]any))
}

func TestProcessSnippetAPIRejectsMalformedAndOversizedWithoutMutation(t *testing.T) {
	enableProcessSnippetTests(t)

	cases := []string{
		`{"name":"","envelope":` + validProcessSnippetEnvelope + `}`,
		`{"name":"bad\nname","envelope":` + validProcessSnippetEnvelope + `}`,
		`{"name":"unknown","extra":true,"envelope":` + validProcessSnippetEnvelope + `}`,
		`{"name":"missing-position","envelope":{"kind":"tclaude/process-selection","version":1,"nodes":[{"id":"done","node":{"type":"end"},"position":{"x":1}}],"edges":[]}}`,
		`{"name":"nested-topology","envelope":{"kind":"tclaude/process-selection","version":1,"nodes":[{"id":"done","node":{"type":"end","next":{"pass":"done"}},"position":{"x":1,"y":2}}],"edges":[]}}`,
	}
	for _, body := range cases {
		recorder := serveProcessSnippetRequest(dashboardRequest(http.MethodPost, "/api/process/snippets", body))
		assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, recorder.Code, "body=%s", recorder.Body.String())
	}
	oversized := `{"name":"large","envelope":"` + strings.Repeat("x", db.MaxProcessSnippetEnvelopeBytes+processSnippetRequestOverhead+1) + `"}`
	recorder := serveProcessSnippetRequest(dashboardRequest(http.MethodPost, "/api/process/snippets", oversized))
	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	list := serveProcessSnippetRequest(dashboardRequest(http.MethodGet, "/api/process/snippets", ""))
	require.Equal(t, http.StatusOK, list.Code)
	assert.Empty(t, decodeProcessSnippetResponse(t, list)["snippets"].([]any))
}

func TestProcessSnippetAPICorruptRowIsolation(t *testing.T) {
	enableProcessSnippetTests(t)
	createProcessSnippetRequest(t, "Healthy")
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO process_snippets
		(id, name, name_key, envelope_json, revision, created_at, updated_at)
		VALUES (?, 'Broken', 'broken', '{not-json', 1, ?, ?)`,
		"psn_11111111111111111111111111111111", "2026-07-18T00:00:00Z", "2026-07-18T00:00:00Z")
	require.NoError(t, err)

	recorder := serveProcessSnippetRequest(dashboardRequest(http.MethodGet, "/api/process/snippets", ""))
	require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
	rows := decodeProcessSnippetResponse(t, recorder)["snippets"].([]any)
	require.Len(t, rows, 2)
	var healthy, broken map[string]any
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["name"] == "Healthy" {
			healthy = row
		} else {
			broken = row
		}
	}
	assert.Equal(t, true, healthy["available"])
	assert.NotEmpty(t, healthy["envelope"])
	assert.Equal(t, false, broken["available"])
	assert.NotEmpty(t, broken["unavailableReason"])
	assert.NotContains(t, recorder.Body.String(), "{not-json")
}
