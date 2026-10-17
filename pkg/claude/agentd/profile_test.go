package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func withProfileStore(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(testutil.CanonicalTempDir(t), "profiles")
	prev := profileStoreDir
	profileStoreDir = func() string { return dir }
	t.Cleanup(func() { profileStoreDir = prev })
	return dir
}

// burnCPU keeps one goroutine busy until the test ends so a short CPU
// profile has samples for pprof to render.
func burnCPU(t *testing.T) {
	t.Helper()
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		x := 0
		for !stop.Load() {
			for i := range 100_000 {
				x += i * i
			}
		}
		_ = x
	}()
	t.Cleanup(func() { stop.Store(true); <-done })
}

func waitProfileIdle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for runningProfileID() != "" {
		select {
		case <-ctx.Done():
			t.Fatal("profile capture did not finish")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func profileRequestTo(t *testing.T, mux http.Handler, method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if cookie != nil {
		r.AddCookie(cookie)
		r.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func TestDashboardProfiles_CaptureListViewDelete(t *testing.T) {
	store := withProfileStore(t)
	cookie, origin := withDashboardAuthForTest(t)
	mux := http.NewServeMux()
	registerProfileRoutes(mux)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		return profileRequestTo(t, mux, method, path, body, cookie, origin)
	}

	rec := profileRequestTo(t, mux, http.MethodGet, "/api/profiles", "", nil, "")
	assert.Equal(t, http.StatusForbidden, rec.Code, "the capture list sits behind dashboard auth")

	burnCPU(t)
	rec = do(http.MethodPost, "/api/profiles", `{"seconds":1,"trace_seconds":1,"label":"  flow   test "}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var started profileCapture
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &started))
	assert.Equal(t, profileStatusRunning, started.Status)
	assert.Equal(t, "flow test", started.Label)

	rec = do(http.MethodPost, "/api/profiles", `{}`)
	assert.Equal(t, http.StatusConflict, rec.Code, "only one in-process capture at a time")
	rec = do(http.MethodDelete, "/api/profiles/"+started.ID, "")
	assert.Equal(t, http.StatusConflict, rec.Code, "a running capture cannot be deleted")

	waitProfileIdle(t)
	rec = do(http.MethodGet, "/api/profiles", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var list profileListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Captures, 1)
	got := list.Captures[0]
	assert.Equal(t, started.ID, got.ID)
	assert.Equal(t, profileStatusDone, got.Status, got.Error)
	require.NotNil(t, got.Headline)
	assert.Greater(t, got.Headline.WindowSeconds, 0.5)
	names := map[string]bool{}
	for _, f := range got.Files {
		names[f.Name] = f.Bytes > 0
	}
	for _, want := range []string{"cpu.pprof", "trace.out", "heap.pprof", "goroutine.pprof", "goroutines.txt", "summary.txt", "stats-after.json"} {
		assert.True(t, names[want], "capture should contain non-empty %s", want)
	}

	rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/files/summary.txt", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "agentd profile summary")
	assert.Empty(t, rec.Header().Get("Content-Disposition"), "the summary opens inline")
	rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/files/cpu.pprof", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/files/meta.json", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "only known artifacts are served")
	rec = do(http.MethodGet, "/api/profiles/..%2Fetc/files/summary.txt", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// A pasted or bookmarked viewer link carries the cookie but neither
	// Origin nor Referer; read-only GETs must still open.
	direct := httptest.NewRequest(http.MethodGet, "/api/profiles/"+got.ID+"/files/summary.txt", nil)
	direct.AddCookie(cookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, direct)
	assert.Equal(t, http.StatusOK, rec.Code, "cookie-only GET opens the artifact")
	rec = profileRequestTo(t, mux, http.MethodGet, "/api/profiles/"+got.ID+"/ui/cpu/top", "", nil, "")
	assert.Equal(t, http.StatusForbidden, rec.Code, "the viewer still requires the session cookie")

	// pprof's own web UI, mounted under the capture.
	rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/ui/cpu/", "")
	require.Equal(t, http.StatusMovedPermanently, rec.Code, rec.Body.String())
	assert.Equal(t, "flamegraph", rec.Header().Get("Location"), "relative redirect keeps the prefix")
	for _, view := range []string{"flamegraph", "top"} {
		rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/ui/cpu/"+view, "")
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", view, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `href="./top"`, "%s keeps pprof's relative navigation", view)
	}
	rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/ui/heap/top", "")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/ui/nope/", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = do(http.MethodDelete, "/api/profiles/"+got.ID, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err := os.Stat(filepath.Join(store, got.ID))
	assert.True(t, os.IsNotExist(err), "delete removes the capture directory")
	rec = do(http.MethodGet, "/api/profiles/"+got.ID+"/ui/cpu/top", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "a deleted capture's cached viewer is dropped")
}

func TestDashboardProfiles_InterruptedCapturesAndValidation(t *testing.T) {
	store := withProfileStore(t)
	id, dir, err := createProfileDir(time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, writeProfileMeta(dir, &profileCapture{
		ID: id, Source: "dashboard", Status: profileStatusRunning, Seconds: 30, StartedAt: time.Now().Add(-time.Hour),
	}))
	list, err := listProfileCaptures()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, profileStatusInterrupted, list[0].Status,
		"a running record this daemon is not running ended with an earlier process")
	require.NoError(t, os.WriteFile(filepath.Join(store, "not-a-capture"), nil, 0o600))
	list, err = listProfileCaptures()
	require.NoError(t, err)
	assert.Len(t, list, 1, "stray entries in the store are ignored")

	zero := 0
	secs, trace, _, err := profileRequest{TraceSeconds: &zero}.normalize()
	require.NoError(t, err)
	assert.Equal(t, profileDefaultSeconds, secs)
	assert.Equal(t, 0, trace, "an explicit 0 skips the trace")
	_, trace, _, err = profileRequest{Seconds: 3}.normalize()
	require.NoError(t, err)
	assert.Equal(t, 3, trace, "the default trace is clipped to the CPU window")
	_, _, _, err = profileRequest{Seconds: profileMaxSeconds + 1}.normalize()
	assert.Error(t, err)
}

func TestValidatePprofAddr(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:6060", "localhost:6060", "[::1]:6060"} {
		assert.NoError(t, validatePprofAddr(ok), ok)
	}
	for _, bad := range []string{":6060", "0.0.0.0:6060", "192.168.1.5:6060", "127.0.0.1"} {
		assert.Error(t, validatePprofAddr(bad), bad)
	}
}

// TestProfileCLI_CapturesOverHTTP drives the `agentd profile` capture path
// against the real profiling mux and checks it lands in the shared store in
// the same shape the dashboard lists.
func TestProfileCLI_CapturesOverHTTP(t *testing.T) {
	withProfileStore(t)
	burnCPU(t)
	srv := httptest.NewServer(profilingMux())
	t.Cleanup(srv.Close)

	id, dir, err := createProfileDir(time.Now())
	require.NoError(t, err)
	c := &profileCapture{ID: id, Source: "cli", Status: profileStatusRunning, Seconds: 1, StartedAt: time.Now()}
	var out bytes.Buffer
	require.NoError(t, captureProfileOverHTTP(srv.Client(), srv.URL, dir, c, &out), out.String())

	list, err := listProfileCaptures()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, profileStatusDone, list[0].Status)
	assert.Equal(t, "cli", list[0].Source)
	summary, err := os.ReadFile(filepath.Join(dir, profileSummaryFile))
	require.NoError(t, err)
	assert.Contains(t, string(summary), "agentd CPU:")

	resp, err := srv.Client().Get(srv.URL + statsPath)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(raw), `"subprocesses"`)
}
