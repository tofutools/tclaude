package agentd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/tofutools/tclaude/pkg/claude/common/dirpicker"
)

// --- POST /api/pick-directory ---
//
// The dashboard's "Browse…" buttons can't open a native folder picker
// from the browser — a web page has no access to one. So they ask the
// daemon: agentd runs as the human, on the human's desktop, outside any
// agent sandbox, so it can pop the OS directory chooser and hand the
// chosen path back over the loopback API. The browser fetch stays pending
// while the dialog is open, then drops the path into the form field.
//
// Same threat model as the rest of /api/* — the dashboard cookie + Origin
// pin is the human-consent layer (see dashboard.go's checkDashboardAuth).
// The endpoint neither reads nor mutates tclaude state; it only shows a
// dialog and echoes back what the human picked.

// pickDirectory is the seam for opening a native directory picker.
// Production uses dirpicker.Pick (osascript / zenity / PowerShell); tests
// swap in a stub so they exercise the handler without a real dialog.
// Mirrors openTerminal in dir.go.
var pickDirectory = dirpicker.Pick

// dirPickerBusy guards against stacking dialogs: a native folder chooser
// is a modal window on the human's desktop, so a second concurrent
// request is refused (409) rather than opening a second dialog behind the
// first.
var dirPickerBusy atomic.Bool

// pickDirResp is the wire shape for POST /api/pick-directory.
type pickDirResp struct {
	Path     string `json:"path,omitempty"`     // chosen path
	Canceled bool   `json:"canceled,omitempty"` // human dismissed the dialog
	Error    string `json:"error,omitempty"`    // human-readable failure
}

// browseDirEntry is one child directory in the web picker's current folder.
// Path is resolved by agentd so the browser never has to guess host path
// separators or join rules.
type browseDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// browseDirsResp is the wire shape for POST /api/browse-directories.
type browseDirsResp struct {
	Path        string           `json:"path"`
	Parent      string           `json:"parent,omitempty"`
	Home        string           `json:"home,omitempty"`
	Directories []browseDirEntry `json:"directories"`
	Error       string           `json:"error,omitempty"`
}

// handleDashboardBrowseDirsAPI lists the direct child directories of a path
// on the agentd host. The authenticated dashboard already accepts host paths
// for spawn/group/template operations; this endpoint gives a remote browser a
// usable way to choose one without trying to pop a native dialog on the host.
func handleDashboardBrowseDirsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			writeJSON(w, http.StatusBadRequest, browseDirsResp{Error: "malformed JSON body: " + err.Error()})
			return
		}
	}

	home, _ := os.UserHomeDir()
	requested := strings.TrimSpace(body.Path)
	if requested == "" {
		requested = home
	}
	if requested == "" {
		requested = "."
	}
	abs, err := filepath.Abs(expandTilde(requested))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, browseDirsResp{Error: "resolve directory: " + err.Error()})
		return
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, browseDirsResp{Error: "open directory: " + err.Error()})
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, browseDirsResp{Error: "not a directory: " + abs})
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, http.StatusForbidden, browseDirsResp{Error: "read directory: " + err.Error()})
		return
	}

	directories := make([]browseDirEntry, 0, len(entries))
	for _, entry := range entries {
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			if target, statErr := os.Stat(filepath.Join(abs, entry.Name())); statErr == nil {
				isDir = target.IsDir()
			}
		}
		if !isDir {
			continue
		}
		directories = append(directories, browseDirEntry{
			Name: entry.Name(),
			Path: filepath.Join(abs, entry.Name()),
		})
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	if home != "" {
		home, _ = filepath.Abs(home)
	}
	writeJSON(w, http.StatusOK, browseDirsResp{
		Path: abs, Parent: parent, Home: home, Directories: directories,
	})
}

// handleDashboardPickDirAPI opens a native directory picker and returns
// the chosen path. Registered as POST /api/pick-directory.
func handleDashboardPickDirAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		StartDir string `json:"start_dir"`
		Title    string `json:"title"`
	}
	// Body is optional — empty (io.EOF) means "use the defaults"; any
	// other decode error is malformed JSON.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			http.Error(w, "malformed JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// One dialog at a time.
	if !dirPickerBusy.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, pickDirResp{Error: "a directory picker is already open"})
		return
	}
	defer dirPickerBusy.Store(false)

	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "Select a directory"
	}
	path, err := pickDirectory(r.Context(), dirpicker.Options{
		Title:    title,
		StartDir: expandTilde(strings.TrimSpace(body.StartDir)),
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, pickDirResp{Path: path})
	case errors.Is(err, dirpicker.ErrCanceled):
		writeJSON(w, http.StatusOK, pickDirResp{Canceled: true})
	case errors.Is(err, dirpicker.ErrUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, pickDirResp{
			Error: "no native directory picker on this machine — type the path instead",
		})
	default:
		writeJSON(w, http.StatusInternalServerError, pickDirResp{Error: err.Error()})
	}
}
