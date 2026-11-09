package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/launcher"
)

// Service management: how this agentd was launched (systemd, launchd, tmux,
// a shell) and a restart that goes through that launcher, so the unit's own
// arguments and environment survive a self-update or a Config-tab restart.

// launcherRunner probes the supervisor (systemctl show / launchctl print).
// launcherRestartRunner asks it to restart us; it runs detached from our
// session so a supervisor that signals our process group cannot kill it
// mid-request. Tests swap both: they must never reach the real supervisor.
var (
	launcherRunner        launcher.Runner = launcher.ExecRunner
	launcherRestartRunner launcher.Runner = detachedLauncherRun
	launcherProcessEnv                    = func() launcher.Env { return launcher.FromProcess(runtime.GOOS) }
)

// restartDelay lets the HTTP response that asked for a restart reach the
// client before the daemon goes away.
var restartDelay = 500 * time.Millisecond

// supervisorRestartDeadline is how long a queued supervisor restart may take
// before a new restart request is allowed to retry.
var supervisorRestartDeadline = 2 * time.Minute

var launcherState struct {
	sync.Mutex
	info    *launcher.Info
	pending string // restart method of an accepted, not yet carried out restart
}

// currentLauncher returns the cached launcher detection, detecting on first
// use or when refresh is set.
func currentLauncher(refresh bool) launcher.Info {
	launcherState.Lock()
	cached := launcherState.info
	launcherState.Unlock()
	if cached != nil && !refresh {
		return *cached
	}
	info := launcher.Detect(context.Background(), launcherProcessEnv(), launcherRunner)
	launcherState.Lock()
	launcherState.info = &info
	launcherState.Unlock()
	return info
}

// RequestDaemonRestart asks for a graceful restart of this agentd that keeps
// its launch parameters. Under a systemd unit or a launchd job it asks the
// supervisor to restart the service (systemctl --user --no-block restart,
// launchctl kickstart -k), so the unit's own ExecStart and environment apply.
// Otherwise, or where a supervisor restart would stop the agents' tmux server
// (KillMode=control-group) or is not permitted (a system unit, not root), it
// stops gracefully and re-executes in place with the same argv and
// environment. It never relaunches with different arguments.
//
// The restart happens shortly after the call returns. It returns the method
// it chose.
func RequestDaemonRestart(reason string) (string, error) {
	info := currentLauncher(false)
	launcherState.Lock()
	if launcherState.pending != "" {
		method := launcherState.pending
		launcherState.Unlock()
		return method, nil
	}
	name, args, supervised := launcher.RestartCommand(info)
	method := launcher.RestartExec
	if supervised {
		method = launcher.RestartSupervisor
	}
	if !supervised {
		if err := scheduleExecRestart(); err != nil {
			launcherState.Unlock()
			return "", err
		}
		launcherState.pending = method
		launcherState.Unlock()
		slog.Info("agentd restart: re-executing in place", "reason", reason, "launcher", info.Kind)
		return method, nil
	}
	launcherState.pending = method
	launcherState.Unlock()
	slog.Info("agentd restart: asking the supervisor", "reason", reason, "launcher", info.Kind, "command", name+" "+strings.Join(args, " "))
	time.AfterFunc(restartDelay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := launcherRestartRunner(ctx, name, args...)
		if err == nil {
			// systemctl --no-block returns once the job is queued. If the
			// supervisor never stops us, let a later restart try again
			// instead of short-circuiting on a stale pending flag.
			time.AfterFunc(supervisorRestartDeadline, func() {
				launcherState.Lock()
				if launcherState.pending == launcher.RestartSupervisor {
					launcherState.pending = ""
					slog.Warn("agentd restart: the supervisor did not restart agentd; a new restart request may retry")
				}
				launcherState.Unlock()
			})
		}
		if err != nil {
			// The supervisor refused; restart in place with the same
			// argv rather than leave a staged update unapplied.
			slog.Warn("agentd restart: supervisor restart failed; re-executing in place", "err", err)
			launcherState.Lock()
			if err := scheduleExecRestart(); err != nil {
				launcherState.pending = ""
				slog.Error("agentd restart: in-place restart unavailable", "err", err)
				failSelfUpdateRestart(fmt.Errorf("supervisor restart failed: %w; restart agentd manually", err))
			} else {
				launcherState.pending = launcher.RestartExec
			}
			launcherState.Unlock()
		}
	})
	return method, nil
}

// scheduleExecRestart arms the graceful stop + in-place re-exec that
// performSelfUpdateRestart carries out once serve returns.
func scheduleExecRestart() error {
	updateRestartState.Lock()
	defer updateRestartState.Unlock()
	if updateRestartState.stop == nil || updateRestartState.path == "" {
		return fmt.Errorf("restart hook unavailable; restart agentd manually")
	}
	updateRestartState.requested = true
	time.AfterFunc(restartDelay, updateRestartState.stop)
	return nil
}

// updateApplying reports whether a self-update job is mid-apply: stopping
// the daemon then would leave the binaries half replaced.
func updateApplying() bool {
	nodeUpdates.Lock()
	service := nodeUpdates.service
	nodeUpdates.Unlock()
	if service == nil {
		return false
	}
	job := service.Status().Job
	return job != nil && job.State == "running"
}

func failSelfUpdateRestart(err error) {
	nodeUpdates.Lock()
	service := nodeUpdates.service
	nodeUpdates.Unlock()
	if service != nil {
		service.RestartFailed(err)
	}
}

// NodeService is the GET /api/node/service and /v1/node/service body.
type NodeService struct {
	Launcher       launcher.Info            `json:"launcher"`
	Autostart      launcher.AutostartStatus `json:"autostart"`
	RestartPending string                   `json:"restart_pending,omitempty"`
	// Access is what the caller may do: full (this node's operator or an
	// unrestricted peer), manage (a peer holding node.update.settings:
	// restart and autostart, with the observed serve flags only) or read.
	Access string `json:"access"`
	// Redacted names what a restricted peer's view withholds.
	Redacted []string `json:"redacted,omitempty"`
}

func nodeServiceStatus(refresh bool) NodeService {
	info := currentLauncher(refresh)
	auto := autostartStatus()
	launcherState.Lock()
	defer launcherState.Unlock()
	return NodeService{Launcher: info, Autostart: auto, RestartPending: launcherState.pending}
}

// NodeRestartResult is the POST .../service/restart body.
type NodeRestartResult struct {
	Method   string `json:"method"`
	Launcher string `json:"launcher"`
}

func registerNodeServiceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/service", handleDashboardNodeService)
	mux.HandleFunc("POST /api/node/service/restart", handleDashboardNodeService)
	mux.HandleFunc("POST /api/node/service/autostart", handleDashboardNodeService)
}

func handleDashboardNodeService(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveNodeService(w, r, "operator", serviceAccessFull)
}

func handleLocalNodeService(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage the tclaude service") {
		return
	}
	serveNodeService(w, r, "operator", serviceAccessFull)
}

// What a caller of the service routes may do (NodeService.Access).
const (
	serviceAccessFull   = "full"
	serviceAccessManage = "manage"
	serviceAccessRead   = "read"
)

// servePeerNodeService serves a peer: reading needs node.read, restarting
// and autostart need node.update.settings as well, both node-wide and both
// re-checked here. Only an unrestricted peer acts as this node's operator;
// a restricted one sees no environment values and installs autostart with
// the observed serve flags only.
func servePeerNodeService(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if p, err := db.GetFederationPeer(v.peer.InstanceID); err != nil || p == nil {
		writeError(w, http.StatusForbidden, "permission_denied", "node.read is not shared")
		return
	}
	peer := v.peer.InstanceID
	if !v.unrestricted() && !fedPeerHoldsUnscoped(peer, PermNodeRead) {
		writeError(w, http.StatusForbidden, "permission_denied", "node.read is not shared")
		return
	}
	access := serviceAccessRead
	switch {
	case v.unrestricted():
		access = serviceAccessFull
	case fedPeerHoldsUnscoped(peer, PermNodeUpdateSettings):
		access = serviceAccessManage
	}
	if r.Method != http.MethodGet && access == serviceAccessRead {
		writeError(w, http.StatusForbidden, "permission_denied", "node.update.settings is not shared")
		return
	}
	serveNodeService(w, r, "operator@"+peer, access)
}

// serviceStatusFor is the status as the caller may see it: a restricted
// peer gets the variable names of the launcher and the autostart unit, not
// their values (the unit environment carries the harnesses' API keys).
func serviceStatusFor(st NodeService, access string) NodeService {
	st.Access = access
	if access == serviceAccessFull {
		return st
	}
	withhold := func(env []launcher.EnvVar) []launcher.EnvVar {
		out := make([]launcher.EnvVar, len(env))
		for i, e := range env {
			out[i] = launcher.EnvVar{Key: e.Key, From: e.From}
		}
		return out
	}
	st.Launcher.Env = withhold(st.Launcher.Env)
	st.Autostart.Env = withhold(st.Autostart.Env)
	st.Redacted = []string{"launcher.env", "autostart.env"}
	return st
}

func serveNodeService(w http.ResponseWriter, r *http.Request, actor, access string) {
	w.Header().Set("Cache-Control", "private, no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, http.StatusOK, serviceStatusFor(nodeServiceStatus(r.URL.Query().Get("refresh") == "1"), access))
		return
	case http.MethodPost:
	default:
		// The GET pattern also matches HEAD; nothing but POST acts.
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET to read and POST to act")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/autostart") {
		serveNodeServiceAutostart(w, r, actor, access)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid restart request")
		return
	}
	if s := strings.TrimSpace(string(body)); s != "" && s != "{}" {
		var probe map[string]json.RawMessage
		if json.Unmarshal(body, &probe) != nil || len(probe) != 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "restart takes no parameters")
			return
		}
	}
	if updateApplying() {
		writeError(w, http.StatusConflict, "update_busy", "an update is being applied; it restarts agentd when it is done")
		return
	}
	method, err := RequestDaemonRestart("operator request")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "restart_unavailable", err.Error())
		return
	}
	info := currentLauncher(false)
	recordFederationAudit("node.restart", actor, "", "", "restart via "+method+" ("+info.Kind+")", http.StatusAccepted)
	writeJSON(w, http.StatusAccepted, NodeRestartResult{Method: method, Launcher: info.Kind})
}
