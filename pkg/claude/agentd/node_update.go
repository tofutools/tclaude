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
	"path/filepath"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

const PermNodeUpdate = "node.update"

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
	}})
	if err != nil {
		return nil, err
	}
	nodeUpdates.service = svc
	return svc, nil
}
func registerNodeUpdateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/update", handleDashboardNodeUpdate)
	mux.HandleFunc("POST /api/node/update", handleDashboardNodeUpdate)
	mux.HandleFunc("GET /api/node/update/jobs/{id}", handleDashboardNodeUpdate)
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
		job, err := service.Start(req, actor, authorize)
		if err != nil {
			status := 500
			code := "update_failed"
			if errors.Is(err, selfupdate.ErrBusy) {
				status = 409
				code = "update_busy"
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
	stop      func()
	path      string
	requested bool
}

func configureSelfUpdateRestart(stop func()) {
	path, _ := os.Executable()
	path, _ = filepath.EvalSymlinks(path)
	updateRestartState.Lock()
	updateRestartState.stop = stop
	updateRestartState.path = path
	updateRestartState.requested = false
	updateRestartState.Unlock()
}
func requestSelfUpdateRestart() error {
	updateRestartState.Lock()
	defer updateRestartState.Unlock()
	if updateRestartState.stop == nil || updateRestartState.path == "" {
		return fmt.Errorf("restart hook unavailable; restart agentd manually")
	}
	updateRestartState.requested = true
	time.AfterFunc(500*time.Millisecond, updateRestartState.stop)
	return nil
}
func performSelfUpdateRestart() error {
	updateRestartState.Lock()
	requested, path := updateRestartState.requested, updateRestartState.path
	updateRestartState.stop = nil
	updateRestartState.Unlock()
	if !requested {
		return nil
	}
	db.Close()
	err := execSelfUpdate(path, os.Args, os.Environ())
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

func startBackgroundNodeUpdates(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	go func() {
		defer cancel()
		service, err := localUpdateService()
		if err != nil {
			slog.Debug("self-update metadata unavailable", "error", err)
			return
		}
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if err := service.RefreshMetadata(ctx); err != nil && ctx.Err() == nil {
				slog.Debug("self-update release check failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
