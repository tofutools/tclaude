package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/noderun"
)

const PermNodeExec = "node.exec"
const nodeExecWarning = "Full remote code execution as the agentd user; remote scripts also require this node's local accept_remote_scripts switch"

type nodeRunSettings struct {
	AcceptRemoteScripts bool                         `json:"accept_remote_scripts"`
	ResourceLimits      sandboxpolicy.ResourceLimits `json:"resource_limits"`
	Warning             string                       `json:"warning"`
}

func currentNodeRunSettings() (nodeRunSettings, error) {
	settings := nodeRunSettings{Warning: nodeExecWarning}
	if runtime.GOOS == "linux" {
		pids := uint64(256)
		settings.ResourceLimits = sandboxpolicy.ResourceLimits{Memory: "1GiB", PIDs: &pids}
	}
	cfg, err := config.Load()
	if err != nil {
		return settings, err
	}
	if cfg.Federation != nil && cfg.Federation.Scripts != nil {
		v := cfg.Federation.Scripts
		settings.AcceptRemoteScripts = v.AcceptRemoteScripts
		settings.ResourceLimits = sandboxpolicy.ResourceLimits{Memory: v.Memory, CPU: v.CPU, PIDs: v.PIDs}
	}
	settings.ResourceLimits, err = sandboxpolicy.NormalizeResourceLimits(settings.ResourceLimits)
	return settings, err
}
func runNodeScript(ctx context.Context, script, id string, seconds int64) noderun.Result {
	if os.Geteuid() == 0 {
		return noderun.Result{ExitCode: 125, Error: "node scripts cannot run as root"}
	}
	settings, err := currentNodeRunSettings()
	if err != nil {
		return noderun.Result{ExitCode: 125, Error: "node script settings unavailable"}
	}
	home, err := canonicalOperatorHome()
	if err != nil {
		return noderun.Result{ExitCode: 125, Error: "operator home unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	command := nonInteractiveCommand{Argv: []string{"/bin/sh", script}, Cwd: home, Env: append(seanceProcessEnv(nil), "TCLAUDE_AGENT_HINT=1", "TCLAUDE_IGNORE_HOOKS=1"), TimeoutSeconds: seconds, TmuxSessionName: "node-run-" + id, SandboxImplementation: "resource-only", ResourceLimits: settings.ResourceLimits}
	if command.ResourceLimits.Enabled() {
		if err := sandboxpolicy.ValidateResourceLimitTarget(command.ResourceLimits, sandboxpolicy.ImplementationResourceOnly, runtime.GOOS); err != nil {
			return noderun.Result{ExitCode: 125, Error: err.Error()}
		}
		dir, cleanup, err := prepareNonInteractiveResourceCgroup(session.GenerateSessionID(), command.ResourceLimits)
		if err != nil {
			return noderun.Result{ExitCode: 125, Error: "resource cgroup unavailable: " + err.Error()}
		}
		command.ResourceCgroupDir = dir
		defer func() { _ = removeNonInteractiveResourceCgroup(dir); cleanup() }()
	}
	result, failure := runNonInteractiveTmuxCommand(ctx, command)
	if failure != nil {
		return noderun.Result{ExitCode: 125, Error: failure.Msg}
	}
	return noderun.Result{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
}

var nodeRuns struct {
	sync.Mutex
	service *noderun.Service
	dir     string
}

func localNodeRunService() (*noderun.Service, error) {
	nodeRuns.Lock()
	defer nodeRuns.Unlock()
	dir := filepath.Join(common.TclaudeDataDir(), "node-runs")
	if nodeRuns.service != nil && nodeRuns.dir == dir {
		return nodeRuns.service, nil
	}
	if nodeRuns.service != nil {
		nodeRuns.service.Close()
	}
	svc, err := noderun.New(dir, runNodeScript, func(j noderun.Job) { recordNodeRunAudit(j, "result") })
	if err != nil {
		return nil, err
	}
	nodeRuns.service = svc
	nodeRuns.dir = dir
	return svc, nil
}
func stopNodeRuns() {
	nodeRuns.Lock()
	svc := nodeRuns.service
	nodeRuns.service = nil
	nodeRuns.Unlock()
	if svc != nil {
		svc.Close()
	}
}
func recordNodeRunAudit(j noderun.Job, phase string) {
	status := 200
	if j.State == "running" {
		status = 202
	} else if j.ExitCode != 0 {
		status = 500
	}
	recordFederationAudit("federation.node.exec", j.Actor, "", "", fmt.Sprintf("phase=%s peer=%s node=%s job=%s sha256=%s bytes=%d state=%s exit=%d", phase, j.Peer, j.Node, j.ID, j.ScriptSHA256, j.ScriptBytes, j.State, j.ExitCode), status)
}
func registerNodeRunRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/run", handleDashboardNodeRun)
	mux.HandleFunc("POST /api/node/run", handleDashboardNodeRun)
	mux.HandleFunc("GET /api/node/run/jobs/{id}", handleDashboardNodeRun)
	mux.HandleFunc("GET /api/node/run/jobs/{id}/logs", handleDashboardNodeRun)
	mux.HandleFunc("GET /api/node/run/settings", handleDashboardNodeRunSettings)
	mux.HandleFunc("PUT /api/node/run/settings", handleDashboardNodeRunSettings)
}
func handleDashboardNodeRun(w http.ResponseWriter, r *http.Request) {
	if checkDashboardAuth(w, r) {
		serveNodeRun(w, r, "operator", "", func() bool { return true })
	}
}
func handleLocalNodeRun(w http.ResponseWriter, r *http.Request) {
	if requireHuman(w, r, "run operator scripts") {
		serveNodeRun(w, r, "operator", "", func() bool { return true })
	}
}
func servePeerNodeRun(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	authorize := func() bool {
		settings, err := currentNodeRunSettings()
		return err == nil && settings.AcceptRemoteScripts && harnessOperationAllowed(v, rule)
	}
	if !harnessOperationAllowed(v, rule) {
		writeError(w, 403, "permission_denied", "node.exec is not shared")
		return
	}
	// The status read lets a granted peer discover the second lock while closed.
	if r.Method == http.MethodGet && r.PathValue("id") == "" {
		settings, err := currentNodeRunSettings()
		if err != nil {
			writeError(w, 503, "script_settings_unavailable", "node settings unavailable")
			return
		}
		writeJSON(w, 200, settings)
		return
	}
	if !authorize() {
		writeError(w, 403, "remote_scripts_disabled", "receiver has not enabled accept_remote_scripts")
		return
	}
	serveNodeRun(w, r, "operator@"+v.peer.InstanceID, v.peer.InstanceID, authorize)
}
func decodeNodeRunRequest(w http.ResponseWriter, r *http.Request) (noderun.Request, error) {
	var req noderun.Request
	r.Body = http.MaxBytesReader(w, r.Body, fedPeerViewBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		return req, fmt.Errorf("invalid script request")
	}
	return req, req.Validate()
}
func serveNodeRun(w http.ResponseWriter, r *http.Request, actor, peer string, authorize func() bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method == http.MethodGet && r.PathValue("id") == "" {
		settings, err := currentNodeRunSettings()
		if err != nil {
			writeError(w, 503, "script_settings_unavailable", "node settings unavailable")
			return
		}
		writeJSON(w, 200, settings)
		return
	}
	var req noderun.Request
	if r.Method == http.MethodPost {
		var err error
		req, err = decodeNodeRunRequest(w, r)
		if err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
	}
	svc, err := localNodeRunService()
	if err != nil {
		writeError(w, 503, "script_service_unavailable", "private script storage unavailable")
		return
	}
	if r.Method == http.MethodPost {
		node := "local"
		if id, err := federationIdentity(); err == nil {
			node = id.ID()
		}
		j, err := svc.Start(req, actor, peer, node, authorize)
		if err != nil {
			status := 400
			code := "script_failed"
			if errors.Is(err, noderun.ErrBusy) {
				status = 409
				code = "script_workers_busy"
			}
			writeError(w, status, code, err.Error())
			return
		}
		recordNodeRunAudit(j, "accepted")
		writeJSON(w, 202, j)
		return
	}
	j, err := svc.Job(r.PathValue("id"))
	if err != nil || peer != "" && j.Peer != peer {
		writeError(w, 404, "not_found", "script job not found")
		return
	}
	if !authorize() {
		writeError(w, 403, "permission_denied", "script authority revoked")
		return
	}
	if r.URL.Path[len(r.URL.Path)-5:] == "/logs" {
		offset := int64(0)
		if raw := r.URL.Query().Get("offset"); raw != "" {
			offset, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeError(w, 400, "invalid_arg", "invalid log offset")
				return
			}
		}
		chunk, err := svc.Log(j.ID, r.URL.Query().Get("stream"), offset)
		if err != nil {
			writeError(w, 404, "log_unavailable", "log not available; inspect completed job first")
			return
		}
		writeJSON(w, 200, chunk)
		return
	}
	writeJSON(w, 200, j)
}
func handleDashboardNodeRunSettings(w http.ResponseWriter, r *http.Request) {
	if checkDashboardAuth(w, r) {
		serveNodeRunSettings(w, r)
	}
}
func handleLocalNodeRunSettings(w http.ResponseWriter, r *http.Request) {
	if requireHuman(w, r, "configure remote script acceptance") {
		serveNodeRunSettings(w, r)
	}
}
func serveNodeRunSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method == http.MethodPut {
		var req struct {
			Accept *bool                         `json:"accept_remote_scripts"`
			Limits *sandboxpolicy.ResourceLimits `json:"resource_limits"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || req.Accept == nil && req.Limits == nil {
			writeError(w, 400, "invalid_arg", "invalid local script settings")
			return
		}
		current, err := currentNodeRunSettings()
		if err != nil {
			writeError(w, 503, "script_settings_unavailable", "node settings unavailable")
			return
		}
		if req.Accept != nil {
			current.AcceptRemoteScripts = *req.Accept
		}
		if req.Limits != nil {
			current.ResourceLimits, err = sandboxpolicy.NormalizeResourceLimits(*req.Limits)
			if err == nil {
				err = sandboxpolicy.ValidateResourceLimitTarget(current.ResourceLimits, sandboxpolicy.ImplementationResourceOnly, runtime.GOOS)
			}
			if err != nil {
				writeError(w, 400, "invalid_arg", err.Error())
				return
			}
		}
		_, err = config.Update(func(cfg *config.Config, loadErr error) error {
			if loadErr != nil {
				return loadErr
			}
			if cfg.Federation == nil {
				cfg.Federation = &config.FederationConfig{}
			}
			cfg.Federation.Scripts = &config.NodeScriptsConfig{AcceptRemoteScripts: current.AcceptRemoteScripts, Memory: current.ResourceLimits.Memory, CPU: current.ResourceLimits.CPU, PIDs: current.ResourceLimits.PIDs}
			return nil
		})
		if err != nil {
			writeError(w, 500, "script_settings_failed", "could not save local script settings")
			return
		}
		recordFederationAudit("federation.node.exec.settings", "operator", "", "", fmt.Sprintf("accept_remote_scripts=%t", current.AcceptRemoteScripts), 200)
	}
	settings, err := currentNodeRunSettings()
	if err != nil {
		writeError(w, 503, "script_settings_unavailable", "node settings unavailable")
		return
	}
	writeJSON(w, 200, settings)
}
