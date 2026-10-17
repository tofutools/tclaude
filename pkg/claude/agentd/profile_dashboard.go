package agentd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Dashboard surface for daemon profiling (Activity › Performance):
//
//	GET    /api/profiles                         list captures, newest first
//	POST   /api/profiles                         start an in-process capture
//	DELETE /api/profiles/{id}                    delete a finished capture
//	GET    /api/profiles/{id}/files/{name}       download one artifact
//	       /api/profiles/{id}/ui/{kind}/...      pprof web UI for one profile
//
// Captures run inside this daemon, so they need neither `serve --pprof` nor
// a second port; they work wherever the dashboard does, remote access
// included.

var errProfileNotFound = errors.New("profile not found")

func registerProfileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/profiles", dashboardAuthed(handleProfileList))
	mux.HandleFunc("POST /api/profiles", dashboardAuthed(handleProfileStart))
	mux.HandleFunc("DELETE /api/profiles/{id}", dashboardAuthed(handleProfileDelete))
	mux.HandleFunc("GET /api/profiles/{id}/files/{name}", dashboardReadAuthed(handleProfileFile))
	mux.HandleFunc("/api/profiles/{id}/ui/{kind}/", dashboardReadAuthed(func(w http.ResponseWriter, r *http.Request) {
		serveProfileViewer(w, r, r.PathValue("id"), r.PathValue("kind"))
	}))
}

func dashboardAuthed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		h(w, r)
	}
}

// dashboardReadAuthed admits read-only GET/HEAD navigations on the session
// cookie alone, the way the dashboard page itself is served. The viewer and
// artifacts open in new tabs, from bookmarks or pasted links, where browsers
// send neither Origin nor Referer; the SameSite=Strict cookie already keeps
// cross-site pages from using them. Anything else (pprof's config writes)
// goes through the full cookie + Origin check.
func dashboardReadAuthed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && dashboardSessionToken != "" {
			if dashboardPreAuthed(r) {
				h(w, r)
				return
			}
			if _, valid, refresh := dashboardRequestSessionMatch(r); valid {
				if refresh {
					setDashboardSessionCookie(w)
				}
				h(w, r)
				return
			}
		}
		if !checkDashboardAuth(w, r) {
			return
		}
		h(w, r)
	}
}

type profileListResponse struct {
	Captures   []profileCapture `json:"captures"`
	Running    string           `json:"running,omitempty"`
	MaxSeconds int              `json:"max_seconds"`
	Defaults   struct {
		Seconds      int `json:"seconds"`
		TraceSeconds int `json:"trace_seconds"`
	} `json:"defaults"`
}

func handleProfileList(w http.ResponseWriter, _ *http.Request) {
	captures, err := listProfileCaptures()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := profileListResponse{Captures: captures, Running: runningProfileID(), MaxSeconds: profileMaxSeconds}
	resp.Defaults.Seconds = profileDefaultSeconds
	resp.Defaults.TraceSeconds = profileDefaultTraceSec
	writeJSON(w, http.StatusOK, resp)
}

func handleProfileStart(w http.ResponseWriter, r *http.Request) {
	var req profileRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	c, err := startInProcessProfile(req)
	switch {
	case errors.Is(err, errProfileBusy):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

func handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	err := deleteProfileCapture(r.PathValue("id"))
	switch {
	case errors.Is(err, errProfileBusy):
		http.Error(w, "capture is still running", http.StatusConflict)
	case errors.Is(err, os.ErrNotExist):
		http.NotFound(w, r)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleProfileFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dir, err := profileDir(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name, ok := profileFileByName(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(name) {
	case ".txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// Text artifacts open inline (the summary is meant to be read in a tab);
	// binary profiles download under a name that keeps the capture id.
	if r.URL.Query().Get("download") == "1" || filepath.Ext(name) != ".txt" {
		w.Header().Set("Content-Disposition", `attachment; filename="agentd-`+id+`-`+name+`"`)
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
}
