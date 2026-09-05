package agentd

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDashboardSpawnTiming(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	t.Setenv("TCLAUDE_STARTUP_TIMING", "1")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	report := `{"label":"spawn-test","closed":true,"prepared_ms":10,"worktree_ready_ms":100,"attachments_ready_ms":101,"request_sent_ms":102,"response_received_ms":900,"elapsed_ms":901}`
	w := httptest.NewRecorder()
	handleDashboardSpawnTiming(w, dashboardRequest(http.MethodPost, "/api/spawn-timing", report))
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Contains(t, logs.String(), `"stage":"dialog_close_requested"`)
	require.Contains(t, logs.String(), `"elapsed_ms":901`)
	logs.Reset()
	t.Setenv("TCLAUDE_STARTUP_TIMING", "")
	w = httptest.NewRecorder()
	handleDashboardSpawnTiming(w, dashboardRequest(http.MethodPost, "/api/spawn-timing", report))
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Empty(t, logs.String())
	t.Setenv("TCLAUDE_STARTUP_TIMING", "1")
	for _, invalid := range []string{`{"elapsed_ms":-1}`, `{"elapsed_ms":1e100}`, `{"prompt":"do not log me"}`} {
		w = httptest.NewRecorder()
		handleDashboardSpawnTiming(w, dashboardRequest(http.MethodPost, "/api/spawn-timing", invalid))
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
	require.Empty(t, logs.String())
	w = httptest.NewRecorder()
	handleDashboardSpawnTiming(w, httptest.NewRequest(http.MethodPost, "/api/spawn-timing", nil))
	require.NotEqual(t, http.StatusNoContent, w.Code)
}
