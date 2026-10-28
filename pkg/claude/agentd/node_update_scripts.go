package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/updatescripts"
)

// The operator's own update scripts (config updates.scripts) sit under the
// node.update grant with tclaude's self-update: a peer allowed to update this
// node may run the status and update commands this node's operator wrote, by
// name. Neither the dashboard nor a peer can supply or change a command.

var nodeUpdateScripts struct {
	sync.Mutex
	service *updatescripts.Service
}

func configuredUpdateScripts() ([]updatescripts.Script, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Updates == nil {
		return nil, nil
	}
	if errs := cfg.Updates.Validate(); len(errs) > 0 {
		return nil, errors.New(errs[0])
	}
	out := make([]updatescripts.Script, 0, len(cfg.Updates.Scripts))
	for _, s := range cfg.Updates.Scripts {
		out = append(out, updatescripts.Script{Name: s.Name, Label: s.Label, Status: s.Status, Update: s.Update,
			StatusTimeout: time.Duration(s.StatusTimeoutSeconds) * time.Second, UpdateTimeout: time.Duration(s.UpdateTimeoutSeconds) * time.Second})
	}
	return out, nil
}

// harnessUpdateOverride is the operator's updates.harnesses command for a
// harness, or "" for the default recipe.
func harnessUpdateOverride(name string) string {
	cfg, err := config.Load()
	if err != nil || cfg.Updates == nil {
		return ""
	}
	return cfg.Updates.Harnesses[name].Update
}

func localUpdateScripts() *updatescripts.Service {
	nodeUpdateScripts.Lock()
	defer nodeUpdateScripts.Unlock()
	if nodeUpdateScripts.service == nil {
		home, _ := canonicalOperatorHome()
		run := updatescripts.ShellRunner(home)
		nodeUpdateScripts.service = updatescripts.New(updatescripts.Hooks{Scripts: configuredUpdateScripts, Finished: func(j updatescripts.Job) {
			status := 200
			if j.State == "failed" {
				status = 500
			}
			recordFederationAudit("federation.node.update", j.Actor, "", "", fmt.Sprintf("script=%s job=%s %s exit=%d", j.Script, j.ID, j.State, j.ExitCode), status)
		}}, run)
	}
	return nodeUpdateScripts.service
}

func stopNodeUpdateScripts() {
	nodeUpdateScripts.Lock()
	svc := nodeUpdateScripts.service
	nodeUpdateScripts.service = nil
	nodeUpdateScripts.Unlock()
	if svc != nil {
		svc.Close()
	}
}

// refreshUpdateScriptStatus re-runs every configured status script; the
// fleet metadata loop calls it hourly while federation runs.
func refreshUpdateScriptStatus() {
	if scripts, err := configuredUpdateScripts(); err != nil || len(scripts) == 0 {
		return
	}
	_ = localUpdateScripts().Check("")
}

func registerNodeUpdateScriptRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/update-scripts", handleDashboardNodeUpdateScripts)
	mux.HandleFunc("POST /api/node/update-scripts", handleDashboardNodeUpdateScripts)
	mux.HandleFunc("GET /api/node/update-scripts/jobs/{id}", handleDashboardNodeUpdateScripts)
}
func handleDashboardNodeUpdateScripts(w http.ResponseWriter, r *http.Request) {
	if checkDashboardAuth(w, r) {
		serveNodeUpdateScripts(w, r, "operator")
	}
}
func servePeerNodeUpdateScripts(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !peerNodeUpdateAllowed(v, rule) {
		writeError(w, 403, "permission_denied", "node.update is not shared")
		return
	}
	serveNodeUpdateScripts(w, r, "operator@"+v.peer.InstanceID)
}
func serveNodeUpdateScripts(w http.ResponseWriter, r *http.Request, actor string) {
	w.Header().Set("Cache-Control", "private, no-store")
	svc := localUpdateScripts()
	if id := r.PathValue("id"); id != "" {
		job, ok := svc.Job(id)
		if !ok {
			writeError(w, 404, "not_found", "update script job not found")
			return
		}
		writeJSON(w, 200, job)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 200, svc.Overview())
		return
	}
	var req updatescripts.Request
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid_arg", "invalid update script request")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	if req.Action == "check" {
		if err := svc.Check(req.Name); err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		writeJSON(w, 202, svc.Overview())
		return
	}
	job, err := svc.Start(req.Name, actor)
	if err != nil {
		if errors.Is(err, updatescripts.ErrBusy) {
			writeError(w, 409, "update_busy", err.Error())
			return
		}
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	recordFederationAudit("federation.node.update", actor, "", "", "script="+req.Name+" job="+job.ID+" accepted", 202)
	writeJSON(w, 202, job)
}
