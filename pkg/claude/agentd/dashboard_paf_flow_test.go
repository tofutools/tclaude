package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// dashBody is a dashboard request whose body is raw bytes (a PAF upload).
func dashBody(method, path string, body []byte) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Origin", "http://localhost")
	return r
}

// The dashboard reaches PAF export, redact and import only through its own
// /api twins: a cookie-authed human caller, the same shared handlers.
func TestDashboardPAFExportRedactImport(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2b8"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "original", "original-pane", cwd)
	dash := agentd.BuildDashboardHandlerForTest()

	rec := httptest.NewRecorder()
	dash.ServeHTTP(rec, dashBody(http.MethodGet, "/api/agent-bundle/export?agent="+source+"&history=true&format=paf", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, paf.MediaType, rec.Header().Get("Content-Type"))
	raw := rec.Body.Bytes()

	rec = httptest.NewRecorder()
	dash.ServeHTTP(rec, dashBody(http.MethodPost, "/api/paf/redact?preview=true", raw))
	require.Equal(t, http.StatusBadRequest, rec.Code, "no redaction chosen is refused: %s", rec.Body.String())

	rec = httptest.NewRecorder()
	dash.ServeHTTP(rec, dashBody(http.MethodPost, "/api/paf/redact?preview=true&mask_flagged=true&paths_to_placeholders=true", raw))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var report struct {
		Redactions []json.RawMessage `json:"redactions"`
		Findings   []json.RawMessage `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	assert.NotNil(t, report.Findings, "the report scans the redacted output")

	rec = httptest.NewRecorder()
	dash.ServeHTTP(rec, dashBody(http.MethodPost, "/api/paf/redact?mask_flagged=true&paths_to_placeholders=true", raw))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	redacted := rec.Body.Bytes()
	a, err := paf.Open(bytes.NewReader(redacted), int64(len(redacted)), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err, "the redacted copy is a valid PAF")
	require.Equal(t, paf.KindAgent, a.Manifest.Kind)

	dest := testutil.CanonicalTempDir(t)
	rec = httptest.NewRecorder()
	dash.ServeHTTP(rec, dashBody(http.MethodPost, "/api/agent-bundle/import?group=receiver&cwd="+dest, raw))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preview struct {
		Applied bool `json:"applied"`
		Landing struct {
			Rung    string `json:"rung"`
			Harness string `json:"harness"`
		} `json:"history_landing"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	assert.False(t, preview.Applied, "without apply the import is only a preview")
	assert.Equal(t, "native", preview.Landing.Rung)
	assert.Equal(t, "claude", preview.Landing.Harness)
}

// The Snapshots view drives the library through the dashboard twins: create,
// list, export, spawn and remove, plus the resilience status read.
func TestDashboardSnapshotLibrary(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("trial")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2c9"
	f.HaveAliveSession(source, "original", "original-pane", testutil.CanonicalTempDir(t))
	dash := agentd.BuildDashboardHandlerForTest()
	serve := func(method, path string, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := dashBody(method, path, []byte(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		dash.ServeHTTP(rec, r)
		return rec
	}

	rec := serve(http.MethodPost, "/api/snapshots", `{"agent":"`+source+`","name":"known-good","note":"before refactor"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = serve(http.MethodGet, "/api/snapshots", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Snapshots []struct {
			Name string `json:"name"`
			Note string `json:"note"`
		} `json:"snapshots"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Snapshots, 1)
	assert.Equal(t, "before refactor", list.Snapshots[0].Note, "the human sees the note")

	rec = serve(http.MethodGet, "/api/snapshots/known-good/export", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, paf.MediaType, rec.Header().Get("Content-Type"))

	dest := testutil.CanonicalTempDir(t)
	rec = serve(http.MethodPost, "/api/snapshots/known-good/spawn", `{"group":"trial","name":"replay","cwd":"`+dest+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var spawned struct {
		Spawned []struct {
			ConvID string `json:"conv_id"`
		} `json:"spawned"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &spawned))
	require.Len(t, spawned.Spawned, 1)
	assert.NotEqual(t, source, spawned.Spawned[0].ConvID)

	rec = serve(http.MethodGet, "/api/resilience/policies", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"policies"`)

	rec = serve(http.MethodDelete, "/api/snapshots/known-good", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = serve(http.MethodGet, "/api/snapshots/known-good", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
