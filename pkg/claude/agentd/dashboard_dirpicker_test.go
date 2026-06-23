package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/dirpicker"
)

// servePickDir routes r through a fresh mux carrying just the
// pick-directory route, the same dispatch a real browser request takes.
func servePickDir(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pick-directory", handleDashboardPickDirAPI)
	mux.ServeHTTP(w, r)
}

// withStubPicker swaps the pickDirectory seam for a stub and restores it
// on cleanup, so the handler is exercised without a real native dialog.
func withStubPicker(t *testing.T, fn func(context.Context, dirpicker.Options) (string, error)) {
	t.Helper()
	prev := pickDirectory
	pickDirectory = fn
	t.Cleanup(func() { pickDirectory = prev })
}

func TestPickDir_Success(t *testing.T) {
	withDashboardAuth(t)
	var gotOpts dirpicker.Options
	withStubPicker(t, func(_ context.Context, o dirpicker.Options) (string, error) {
		gotOpts = o
		return "/Users/me/picked", nil
	})

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/pick-directory", `{"start_dir":"~","title":"Pick"}`))

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp pickDirResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "/Users/me/picked", resp.Path)
	assert.False(t, resp.Canceled)
	assert.Equal(t, "Pick", gotOpts.Title)
	// "~" is expanded to an absolute home path before reaching the picker.
	assert.NotEqual(t, "~", gotOpts.StartDir)
	assert.NotEmpty(t, gotOpts.StartDir)
}

func TestPickDir_Canceled(t *testing.T) {
	withDashboardAuth(t)
	withStubPicker(t, func(_ context.Context, _ dirpicker.Options) (string, error) {
		return "", dirpicker.ErrCanceled
	})

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/pick-directory", ``))

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp pickDirResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Canceled)
	assert.Empty(t, resp.Path)
}

func TestPickDir_Unavailable(t *testing.T) {
	withDashboardAuth(t)
	withStubPicker(t, func(_ context.Context, _ dirpicker.Options) (string, error) {
		return "", dirpicker.ErrUnavailable
	})

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/pick-directory", `{}`))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
}

func TestPickDir_Busy(t *testing.T) {
	withDashboardAuth(t)
	// Hold the in-flight flag as if a dialog were already open.
	require.True(t, dirPickerBusy.CompareAndSwap(false, true))
	t.Cleanup(func() { dirPickerBusy.Store(false) })
	withStubPicker(t, func(_ context.Context, _ dirpicker.Options) (string, error) {
		t.Fatal("picker should not be invoked while one is already open")
		return "", nil
	})

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/pick-directory", `{}`))

	assert.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
}

func TestPickDir_MethodNotAllowed(t *testing.T) {
	withDashboardAuth(t)

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodGet, "/api/pick-directory", ``))

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestPickDir_Unauthed(t *testing.T) {
	// No withDashboardAuth: checkDashboardAuth must reject before any
	// dialog is opened.
	withStubPicker(t, func(_ context.Context, _ dirpicker.Options) (string, error) {
		t.Fatal("picker should not be invoked for an unauthenticated request")
		return "", nil
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/pick-directory", nil)
	servePickDir(w, r)

	assert.NotEqual(t, http.StatusOK, w.Code)
}
