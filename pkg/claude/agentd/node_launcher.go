package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/launcher"
)

// App launcher: `tclaude service launcher install|uninstall` and the Config
// tab's Installation page write a desktop entry (Linux) or an app bundle in
// ~/Applications (macOS) that opens the dashboard, starting agentd first
// when it does not answer. The launcher runs the tclaude binary this daemon
// runs; a managed launcher naming another binary or version is rewritten
// when agentd starts (refreshStaleAppLauncher), which covers a self-update
// (it restarts agentd) and a relocated binary (started from its new path).

// appLauncherFactory builds the production AppLauncher; tests swap it so
// they never write into the real HOME or run lsregister.
var appLauncherFactory = func() launcher.AppLauncher {
	home, _ := os.UserHomeDir()
	spec, _ := currentAppLauncherSpec()
	return launcher.AppLauncher{
		GOOS:    runtime.GOOS,
		Paths:   launcher.AppLauncherPathsFor(runtime.GOOS, home, os.Getenv("XDG_DATA_HOME")),
		Run:     launcherRunner,
		FS:      launcher.OSFS{},
		Current: spec,
	}
}

// currentAppLauncherSpec is what an install now writes: this daemon's
// tclaude binary, version and PATH, with warnings for PATH entries the
// launcher script cannot carry.
func currentAppLauncherSpec() (launcher.AppLauncherSpec, []string) {
	exe, _ := tclaudeExecutable(os.Args[0], exec.LookPath, fileExists)
	var keep, warnings []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || strings.ContainsAny(dir, "\"'`$\\%") || strings.ContainsFunc(dir, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			if dir != "" {
				warnings = append(warnings, "Left "+dir+" out of the launcher's PATH: it holds a quote, $, % or backslash.")
			}
			continue
		}
		keep = append(keep, dir)
	}
	return launcher.AppLauncherSpec{
		Exe:     exe,
		Version: buildversion.AppVersion(),
		PATH:    strings.Join(keep, string(os.PathListSeparator)),
	}, warnings
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// tclaudeExecutable is the absolute tclaude binary for argv0: the binary
// itself, or for the standalone tclaude-agentd the tclaude next to it (else
// on PATH). It keeps a symlinked install path (a package manager's bin
// link) rather than resolving it, so an upgrade behind the link does not
// leave the launcher pointing at the old version's file.
func tclaudeExecutable(argv0 string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	exe := argv0
	if !filepath.IsAbs(exe) {
		resolved, err := lookPath(exe)
		if err != nil {
			return "", fmt.Errorf("cannot resolve the tclaude binary %q: %w", argv0, err)
		}
		if exe, err = filepath.Abs(resolved); err != nil {
			return "", err
		}
	}
	if filepath.Base(exe) != "tclaude-agentd" {
		return filepath.Clean(exe), nil
	}
	if sibling := filepath.Join(filepath.Dir(exe), "tclaude"); exists(sibling) {
		return sibling, nil
	}
	found, err := lookPath("tclaude")
	if err != nil {
		return "", fmt.Errorf("agentd runs as tclaude-agentd and no tclaude binary is next to it or on PATH")
	}
	return filepath.Abs(found)
}

// AppLauncherRequest is the POST .../launcher/install body.
type AppLauncherRequest struct {
	// StartDaemon (default true): the launcher starts agentd when it does
	// not answer.
	StartDaemon *bool `json:"start_daemon,omitempty"`
}

// NodeAppLauncher is the GET /api/node/launcher and /v1/node/launcher body:
// the launcher status, and what the caller may do (full, manage or read, as
// for the service routes).
type NodeAppLauncher struct {
	launcher.AppLauncherStatus
	Access string `json:"access"`
}

func registerNodeLauncherRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/launcher", handleDashboardNodeService)
	mux.HandleFunc("POST /api/node/launcher/install", handleDashboardNodeService)
	mux.HandleFunc("POST /api/node/launcher/uninstall", handleDashboardNodeService)
}

func nodeLauncherStatus(access string) NodeAppLauncher {
	return NodeAppLauncher{AppLauncherStatus: appLauncherFactory().Status(), Access: access}
}

// serveNodeLauncher serves the launcher routes; serveNodeService routes
// them here after the caller's access is settled.
func serveNodeLauncher(w http.ResponseWriter, r *http.Request, actor, access string) {
	w.Header().Set("Cache-Control", "private, no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, http.StatusOK, nodeLauncherStatus(access))
		return
	case http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET to read and POST to act")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req AppLauncherRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid launcher request")
		return
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.More() {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid launcher request")
			return
		}
	}
	l := appLauncherFactory()
	var warnings []string
	switch {
	case strings.HasSuffix(r.URL.Path, "/install"):
		spec := l.Current
		_, specWarnings := currentAppLauncherSpec()
		if spec.Exe == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "cannot tell which tclaude binary this agentd runs")
			return
		}
		spec.Options.StartDaemon = req.StartDaemon == nil || *req.StartDaemon
		if err := l.Install(r.Context(), spec); err != nil {
			writeLauncherError(w, err)
			return
		}
		warnings = specWarnings
		recordFederationAudit("node.launcher", actor, "", "", fmt.Sprintf("install %s: %s (start_daemon=%t)", l.Status().Path, spec.Exe, spec.Options.StartDaemon), http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/uninstall"):
		if req.StartDaemon != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "uninstall takes no options")
			return
		}
		if err := l.Uninstall(r.Context()); err != nil {
			writeLauncherError(w, err)
			return
		}
		recordFederationAudit("node.launcher", actor, "", "", "uninstall "+l.Status().Path, http.StatusOK)
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown launcher action")
		return
	}
	st := nodeLauncherStatus(access)
	st.Warnings = append(st.Warnings, warnings...)
	writeJSON(w, http.StatusOK, st)
}

func writeLauncherError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, launcher.ErrLauncherNotManaged):
		writeError(w, http.StatusConflict, "launcher_not_managed", err.Error())
	case errors.Is(err, launcher.ErrLauncherUnsupported):
		writeError(w, http.StatusBadRequest, "unsupported", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "launcher_failed", err.Error())
	}
}

// refreshStaleAppLauncher rewrites a managed launcher that runs another
// tclaude binary or names another version than this daemon, keeping its
// options. It never installs one that is not there, and leaves a launcher
// alone when this daemon runs from a throwaway build (go run, a test).
func refreshStaleAppLauncher(ctx context.Context) {
	l := appLauncherFactory()
	st := l.Status()
	if !st.Managed || !st.Stale || l.Current.Exe == "" || throwawayBinary(l.Current.Exe) {
		return
	}
	spec := l.Current
	spec.Options = st.Options
	if err := l.Install(ctx, spec); err != nil {
		slog.Warn("app launcher: could not rewrite the stale launcher", "path", st.Path, "reason", st.StaleReason, "error", err)
		return
	}
	slog.Info("app launcher: rewritten for this tclaude", "path", st.Path, "reason", st.StaleReason)
}

func throwawayBinary(exe string) bool {
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	return strings.HasPrefix(exe, tmp) || strings.Contains(exe, string(filepath.Separator)+"go-build")
}
