package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	mux.HandleFunc("/api/browse-directories", handleDashboardBrowseDirsAPI)
	mux.HandleFunc("/api/create-directory", handleDashboardCreateDirAPI)
	mux.HandleFunc("/api/rename-directory", handleDashboardRenameDirAPI)
	mux.HandleFunc("/api/delete-directory", handleDashboardDeleteDirAPI)
	mux.ServeHTTP(w, r)
}

func TestDirectoryMutations_CreateRenameDelete(t *testing.T) {
	withDashboardAuth(t)
	root := t.TempDir()

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/create-directory",
		`{"parent":`+strconv.Quote(root)+`,"name":"created"}`))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	created := filepath.Join(root, "created")
	require.DirExists(t, created)

	require.NoError(t, os.WriteFile(filepath.Join(created, "kept-until-delete.txt"), []byte("x"), 0o644))
	w = httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/rename-directory",
		`{"path":`+strconv.Quote(created)+`,"name":"renamed"}`))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	renamed := filepath.Join(root, "renamed")
	require.DirExists(t, renamed)
	assert.NoDirExists(t, created)

	w = httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/delete-directory",
		`{"path":`+strconv.Quote(renamed)+`,"confirm":`+strconv.Quote(renamed)+`}`))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.NoDirExists(t, renamed)
}

func TestDirectoryMutations_ValidateNamesTargetsAndConfirmation(t *testing.T) {
	withDashboardAuth(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "source"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "occupied"), 0o755))

	for _, tc := range []struct {
		name, route, body string
		status            int
	}{
		{"create nested", "/api/create-directory", `{"parent":` + strconv.Quote(root) + `,"name":"nested/child"}`, http.StatusBadRequest},
		{"create existing", "/api/create-directory", `{"parent":` + strconv.Quote(root) + `,"name":"source"}`, http.StatusConflict},
		{"rename nested", "/api/rename-directory", `{"path":` + strconv.Quote(filepath.Join(root, "source")) + `,"name":"../moved"}`, http.StatusBadRequest},
		{"rename occupied", "/api/rename-directory", `{"path":` + strconv.Quote(filepath.Join(root, "source")) + `,"name":"occupied"}`, http.StatusConflict},
		{"delete wrong confirmation", "/api/delete-directory", `{"path":` + strconv.Quote(filepath.Join(root, "source")) + `,"confirm":"source"}`, http.StatusBadRequest},
		{"delete root", "/api/delete-directory", `{"path":"/","confirm":"/"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			servePickDir(w, dashboardRequest(http.MethodPost, tc.route, tc.body))
			assert.Equal(t, tc.status, w.Code, "body=%s", w.Body.String())
		})
	}
	require.DirExists(t, filepath.Join(root, "source"))
}

func TestDirectoryMutations_PreserveTrailingSpaceInTargetPath(t *testing.T) {
	withDashboardAuth(t)
	root := t.TempDir()
	plain := filepath.Join(root, "folder")
	spaced := filepath.Join(root, "folder ")
	require.NoError(t, os.Mkdir(plain, 0o755))
	require.NoError(t, os.Mkdir(spaced, 0o755))

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/delete-directory",
		`{"path":`+strconv.Quote(spaced)+`,"confirm":`+strconv.Quote(spaced)+`}`))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.DirExists(t, plain)
	assert.NoDirExists(t, spaced)
}

func TestDirectoryMutations_RequireAuthAndPost(t *testing.T) {
	root := t.TempDir()
	w := httptest.NewRecorder()
	servePickDir(w, httptest.NewRequest(http.MethodPost, "/api/create-directory", nil))
	assert.NotEqual(t, http.StatusOK, w.Code)

	withDashboardAuth(t)
	w = httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodGet, "/api/delete-directory",
		`{"path":`+strconv.Quote(root)+`}`))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestBrowseDirs_ListsOnlyDirectories(t *testing.T) {
	withDashboardAuth(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "alpha"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, ".hidden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("not a folder"), 0o644))
	if err := os.Symlink(filepath.Join(root, "alpha"), filepath.Join(root, "linked")); err != nil {
		t.Logf("symlink unavailable: %v", err)
	}

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/browse-directories", `{"path":`+strconv.Quote(root)+`}`))

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp browseDirsResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, root, resp.Path)
	assert.Equal(t, filepath.Dir(root), resp.Parent)
	assert.NotEmpty(t, resp.Home)
	assert.Contains(t, resp.Directories, browseDirEntry{Name: "alpha", Path: filepath.Join(root, "alpha")})
	assert.Contains(t, resp.Directories, browseDirEntry{Name: ".hidden", Path: filepath.Join(root, ".hidden")})
	assert.NotContains(t, resp.Directories, browseDirEntry{Name: "notes.txt", Path: filepath.Join(root, "notes.txt")})
}

func TestBrowseDirs_DefaultsToHome(t *testing.T) {
	withDashboardAuth(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/browse-directories", `{}`))

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var resp browseDirsResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, home, resp.Path)
	assert.Equal(t, home, resp.Home)
	assert.NotNil(t, resp.Directories)
}

func TestBrowseDirs_RejectsFileAndWrongMethod(t *testing.T) {
	withDashboardAuth(t)
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	w := httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodPost, "/api/browse-directories", `{"path":`+strconv.Quote(file)+`}`))
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())

	w = httptest.NewRecorder()
	servePickDir(w, dashboardRequest(http.MethodGet, "/api/browse-directories", ``))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
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
