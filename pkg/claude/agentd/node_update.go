package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

const PermNodeUpdate = "node.update"

// PermNodeUpdateSettings lets a peer change this node's self-update settings
// (method, schedule, post-update setup). Instance-wide, default off.
const PermNodeUpdateSettings = "node.update.settings"

var nodeUpdates struct {
	sync.Mutex
	service *selfupdate.Service
}

func localUpdateService() (*selfupdate.Service, error) {
	nodeUpdates.Lock()
	defer nodeUpdates.Unlock()
	if nodeUpdates.service != nil {
		return nodeUpdates.service, nil
	}
	binaries, err := selfupdate.Discover()
	if err != nil {
		return nil, err
	}
	svc, err := selfupdate.New(filepath.Join(common.TclaudeDataDir(), "updates"), binaries, selfupdate.Hooks{Restart: requestSelfUpdateRestart, Finished: func(job selfupdate.Job) {
		status := 200
		if job.State == "failed" {
			status = 500
		}
		recordFederationAudit("federation.node.update", job.Actor, "", "", job.Action+" "+job.Version+" job="+job.ID+" "+job.State, status)
		selfUpdateJobFinished(job)
	}, ToolEnv: selfUpdateToolEnv})
	if err != nil {
		return nil, err
	}
	nodeUpdates.service = svc
	// A job finished by this process's restart verification may still owe
	// its post-update setup and notification.
	go finishRestartedUpdate(context.Background(), svc)
	return svc, nil
}
func registerNodeUpdateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/update", handleDashboardNodeUpdate)
	mux.HandleFunc("POST /api/node/update", handleDashboardNodeUpdate)
	mux.HandleFunc("GET /api/node/update/jobs/{id}", handleDashboardNodeUpdate)
	mux.HandleFunc("GET /api/node/update/preflight", handleDashboardNodeUpdatePreflight)
	registerNodeUpdateSettingsRoutes(mux)
}

// GET <base>/preflight is a dry run of Update now under the node's saved
// update settings: whether the method may run here, what it would run, and
// the local preconditions checked without installing (selfupdate.Plan).
func handleDashboardNodeUpdatePreflight(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveNodeUpdatePreflight(w, r, false)
}
func handleLocalNodeUpdatePreflight(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "test the tclaude update method") {
		return
	}
	serveNodeUpdatePreflight(w, r, false)
}

// servePeerNodeUpdatePreflight serves a peer holding node.update, as the
// update status read is. The custom command is the operator's free text: only
// an unrestricted peer sees it, as with the settings view.
func servePeerNodeUpdatePreflight(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !peerNodeUpdateAllowed(v, rule) {
		writeError(w, 403, "permission_denied", "node.update is not shared")
		return
	}
	serveNodeUpdatePreflight(w, r, !v.unrestricted())
}

func serveNodeUpdatePreflight(w http.ResponseWriter, r *http.Request, redact bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	service, err := localUpdateService()
	if err != nil {
		writeError(w, 503, "update_unavailable", err.Error())
		return
	}
	plan := service.Plan(r.Context(), selfUpdateApplyOptions(loadSelfUpdateSettings()), selfupdate.PlanOptions{RedactCommand: redact})
	writeJSON(w, 200, plan)
}
func handleDashboardNodeUpdate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveNodeUpdate(w, r, "operator", func() bool { return true })
}
func handleLocalNodeUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage tclaude updates") {
		return
	}
	serveNodeUpdate(w, r, "operator", func() bool { return true })
}
func peerNodeUpdateAllowed(v *peerView, rule peerViewRule) bool {
	p, err := db.GetFederationPeer(v.peer.InstanceID)
	return err == nil && p != nil && v.allows(rule, 0)
}
func servePeerNodeUpdate(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	authorize := func() bool { return peerNodeUpdateAllowed(v, rule) }
	if !authorize() {
		writeError(w, 403, "permission_denied", "node.update is not shared")
		return
	}
	serveNodeUpdate(w, r, "operator@"+v.peer.InstanceID, authorize)
}
func serveNodeUpdate(w http.ResponseWriter, r *http.Request, actor string, authorize func() bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	var req selfupdate.Request
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, 400, "invalid_arg", "invalid update request")
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			writeError(w, 400, "invalid_arg", "only one update JSON object is allowed")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
	}
	service, err := localUpdateService()
	if err != nil {
		writeError(w, 503, "update_unavailable", err.Error())
		return
	}
	if r.Method == http.MethodPost {
		var opts selfupdate.ApplyOptions
		if req.Action != "rollback" {
			// A check needs the method too: a source checkout that follows a
			// branch checks for new commits, not a newer release.
			opts = selfUpdateApplyOptions(loadSelfUpdateSettings())
			// The operator asked for this update: a custom command runs
			// even at the latest release (automatic runs do not).
			opts.RunWhenCurrent = true
		}
		if req.Action != "check" {
			opts.Launch = selfUpdateLaunchPlan()
		}
		job, err := service.StartWith(req, actor, authorize, opts)
		if err != nil {
			status := 500
			code := "update_failed"
			var managed *selfupdate.PackageManagedError
			switch {
			case errors.Is(err, selfupdate.ErrBusy):
				status, code = 409, "update_busy"
			case errors.Is(err, selfupdate.ErrNotifyOnly):
				status, code = 409, "update_method_notify_only"
			case errors.As(err, &managed) && managed.NeedsAck:
				status, code = 409, "update_package_manager_ack_required"
			case errors.As(err, &managed):
				status, code = 409, "update_method_package_managed"
			}
			writeError(w, status, code, err.Error())
			return
		}
		recordFederationAudit("federation.node.update", actor, "", "", req.Action+" "+req.Version+" job="+job.ID+" accepted", 202)
		writeJSON(w, 202, job)
		return
	}
	if id := r.PathValue("id"); id != "" {
		job, err := service.Job(id)
		if err != nil {
			writeError(w, 404, "not_found", "update job not found")
			return
		}
		writeJSON(w, 200, job)
		return
	}
	status := service.Status()
	status.ChosenMethod = loadSelfUpdateSettings().Method
	status.DetectedMethod = selfupdate.Detect(status.Binaries).Method
	if rt := currentFederation(); rt != nil {
		for _, p := range rt.cl.Directory() {
			if p.Version != "" && p.Version != status.CurrentVersion {
				status.Warnings = append(status.Warnings, "Version skew: "+p.InstanceID+" runs "+p.Version)
			}
		}
	}
	writeJSON(w, 200, status)
}

var updateRestartState struct {
	sync.Mutex
	stop func()
	// path is the binary a restart starts; startPath the one this process
	// started from (a self-update may move path away from it).
	path, startPath string
	requested       bool
	plan            restartPlan
}

// restartPlan is how performSelfUpdateRestart restarts; the zero value
// re-executes in place.
type restartPlan struct {
	// exit: end the process with exitCode instead of re-executing, for the
	// supervisor's Restart= to start it again (launcher.RestartExit).
	exit     bool
	exitCode int
	// relaunch: start agentd in a fresh process through the relaunch helper
	// (launcher.RestartRelaunch); detach starts it detached.
	relaunch, detach bool
}

// trayShown: this serve shows a tray icon, so on macOS a restart must start
// a fresh process rather than exec in place (launcher.RestartRelaunch).
var trayShown atomic.Bool

// restartExit ends the process for an exit restart; tests swap it.
var restartExit = os.Exit

func configureSelfUpdateRestart(stop func()) {
	path, _ := os.Executable()
	path, _ = filepath.EvalSymlinks(path)
	updateRestartState.Lock()
	updateRestartState.stop = stop
	updateRestartState.path = path
	updateRestartState.startPath = path
	updateRestartState.requested = false
	updateRestartState.Unlock()
}

// requestSelfUpdateRestart is the self-updater's restart hook. It restarts
// through the launcher (see RequestDaemonRestart), so a systemd or launchd
// service keeps its own arguments.
func requestSelfUpdateRestart() error {
	// An update may have moved the daemon's binary (an install next to a
	// package-managed copy, a new Homebrew Cellar version); an in-place
	// re-exec must start the new location.
	updateRestartState.Lock()
	nodeUpdates.Lock()
	relocated := false
	if svc := nodeUpdates.service; svc != nil && updateRestartState.path != "" {
		moved := svc.RestartPath(updateRestartState.path)
		relocated = moved != updateRestartState.path
		updateRestartState.path = moved
	}
	nodeUpdates.Unlock()
	updateRestartState.Unlock()
	// A supervisor restart or an exit would start the unit's own ExecStart,
	// the old location; only the re-exec starts the moved binary.
	_, err := requestDaemonRestart("self-update", relocated)
	return err
}

// markLaunchEnvRefreshed tells the running update job, if any, that the
// restart refreshes the environment.
func markLaunchEnvRefreshed() {
	nodeUpdates.Lock()
	service := nodeUpdates.service
	nodeUpdates.Unlock()
	if service == nil {
		return
	}
	if err := service.MarkLaunchEnvRefreshed(); err != nil {
		slog.Warn("self-update: could not record that the restart refreshes the environment", "error", err)
	}
}

func performSelfUpdateRestart() error {
	updateRestartState.Lock()
	requested, path, startPath, plan := updateRestartState.requested, updateRestartState.path, updateRestartState.startPath, updateRestartState.plan
	exit, exitCode := plan.exit, plan.exitCode
	updateRestartState.stop = nil
	updateRestartState.Unlock()
	if !requested {
		return nil
	}
	if exit && currentLauncher(true).RestartMethod != launcher.RestartExit {
		// Something started inside the unit since the restart was asked
		// for (tmux, an agent runtime): an exit would stop it with us.
		exit = false
		slog.Warn("agentd restart: exiting is no longer safe; re-executing in place")
	}
	db.Close()
	if exit {
		// The supervisor starts agentd again (Restart=), through the unit's
		// own ExecStart.
		restartExit(exitCode)
		return nil
	}
	argv := os.Args
	if plan.relaunch && relaunchHandBack(path, startPath) {
		// The waiting helper that started this daemon starts it again; a
		// helper of our own would nest a second one in the shell job.
		// Exiting also takes the tray icon down with this PID. The helper
		// refreshes the environment (runRelaunch), which the update's
		// launch comparison expects rather than reports.
		if restartRefreshesEnv(currentLauncher(false)) {
			markLaunchEnvRefreshed()
		}
		slog.Info("agentd restart: handing back to the relaunch helper", "helper_pid", relaunchHelperPID)
		restartExit(relaunchHandBackExit)
		return nil
	}
	if plan.relaunch {
		helper, ok := relaunchHelperArgv(path, os.Args, plan.detach)
		switch {
		case !ok:
			slog.Warn("agentd restart: cannot relaunch from this command line; re-executing in place")
		case !relaunchAvailable(path, helper):
			slog.Warn("agentd restart: the binary has no relaunch helper; re-executing in place", "path", path)
		default:
			argv = helper
		}
	}
	if runtime.GOOS == "darwin" && trayShown.Load() {
		// The exec keeps this PID, and with it the old tray icon's slot in
		// the menu bar: a waiting helper would show it blank for the rest
		// of the shell job.
		releaseTrayStatusItem()
	}
	// A restart starts like a fresh one, with a fresh environment from the
	// login shell, built from the environment this process started with,
	// not os.Environ(): what startup exported into it is derived again
	// from the same flags. A Ctrl+C or SIGTERM while the shell is asked
	// stops agentd instead: it was meant to.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	env, refreshed := daemonRestartEnviron(sigCtx, launchEnviron)
	signaled := sigCtx.Err() != nil
	stopSignals()
	if signaled {
		slog.Info("agentd restart: stopped by a signal while asking the login shell; not restarting")
		restartExit(0)
		return nil
	}
	if refreshed {
		// The update's launch comparison expects it rather than reports it.
		markLaunchEnvRefreshed()
	}
	err := execSelfUpdate(path, argv, env)
	if err != nil {
		nodeUpdates.Lock()
		service := nodeUpdates.service
		nodeUpdates.Unlock()
		if service != nil {
			service.RestartFailed(err)
		}
	}
	return err
}
