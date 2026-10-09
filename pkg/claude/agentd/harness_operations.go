package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/harnesscredentials"
	"github.com/tofutools/tclaude/pkg/harnessops"
	"github.com/tofutools/tclaude/pkg/nodeinfo"
)

const PermNodeHarnessesInstall = "node.harnesses.install"
const PermNodeCredentialsReceive = "node.credentials.receive"

var harnessOperations struct {
	sync.Mutex
	service *harnessops.Service
}

func canonicalOperatorHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(home)
}
func harnessBusy(name string) (bool, error) {
	states, err := session.ListSessionStates()
	if err != nil {
		return false, err
	}
	for _, s := range states {
		h := s.Harness
		if h == "" {
			h = "claude"
		}
		if h == name && s.Status != session.StatusIdle && s.Status != session.StatusExited {
			return true, nil
		}
	}
	return false, nil
}
func refreshHarnessAvailability() nodeinfo.Availability {
	availabilityCache.Lock()
	availabilityCache.value = nodeinfo.Availability{}
	availabilityCache.Unlock()
	return cachedHarnessAvailability(true)
}
func localHarnessOperations() (*harnessops.Service, error) {
	harnessOperations.Lock()
	defer harnessOperations.Unlock()
	if harnessOperations.service != nil {
		return harnessOperations.service, nil
	}
	home, err := canonicalOperatorHome()
	if err != nil {
		return nil, err
	}
	privateDir, err := filepath.Abs(filepath.Join(common.TclaudeDataDir(), "harness-operations"))
	if err != nil {
		return nil, err
	}
	svc, err := harnessops.New(privateDir, home, harnessops.Hooks{Busy: harnessBusy, Probe: refreshHarnessAvailability, Finished: func(j harnessops.Job) {
		status := 200
		if j.State == "failed" {
			status = 500
		}
		for _, result := range j.Results {
			detail := j.Action + " harness=" + result.Harness + " recipe=" + result.Recipe + " job=" + j.ID + " " + result.State
			recordFederationAudit("federation.harness.manage", j.Actor, "", "", detail, status)
			if result.Credentials != nil {
				recordFederationAudit("federation.credentials.receive", j.Actor, "", "", "harness="+result.Harness+" backup="+result.Credentials.BackupID+" copied="+fmt.Sprint(result.Credentials.Copied), status)
			}
		}
	}})
	if err != nil {
		return nil, err
	}
	harnessops.RefreshLatestAsync()
	harnessOperations.service = svc
	return svc, nil
}
func registerHarnessOperationsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/harnesses/operations", handleDashboardHarnessOperations)
	mux.HandleFunc("POST /api/harnesses/operations", handleDashboardHarnessOperations)
	mux.HandleFunc("POST /api/harnesses/credentials", handleDashboardHarnessOperations)
	mux.HandleFunc("GET /api/harnesses/operations/jobs/{id}", handleDashboardHarnessOperations)
}
func handleDashboardHarnessOperations(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveHarnessOperations(w, r, "operator", nil, nil)
}
func handleLocalHarnessOperations(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage harness installs and updates") {
		return
	}
	serveHarnessOperations(w, r, "operator", nil, nil)
}
func harnessOperationAllowed(v *peerView, rule peerViewRule) bool {
	p, err := db.GetFederationPeer(v.peer.InstanceID)
	return err == nil && p != nil && v.allows(rule, 0)
}
func servePeerHarnessOperations(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !harnessOperationAllowed(v, peerViewRules()["POST /api/harnesses/operations"]) {
		writeError(w, 403, "permission_denied", "node.harnesses.install is not shared")
		return
	}
	authorize := func(credentials bool) bool {
		return harnessOperationAllowed(v, peerViewRules()["POST /api/harnesses/operations"]) && (!credentials || harnessOperationAllowed(v, peerViewRules()["POST /api/harnesses/credentials"]))
	}
	serveHarnessOperations(w, r, "operator@"+v.peer.InstanceID, v, authorize)
}
func decodeHarnessOperation(r *http.Request, peer bool) (harnessops.Request, error) {
	var req harnessops.Request
	dec := json.NewDecoder(io.LimitReader(r.Body, fedPeerViewBodyLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid harness operation request")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return req, fmt.Errorf("only one request object is allowed")
	}
	return req, req.Validate(peer)
}
func serveHarnessOperations(w http.ResponseWriter, r *http.Request, actor string, v *peerView, authorize func(bool) bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	if authorize == nil {
		authorize = func(bool) bool { return true }
	}
	var req harnessops.Request
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, fedPeerViewBodyLimit)
		var err error
		req, err = decodeHarnessOperation(r, v != nil)
		if err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		if req.CopyCredentials && v == nil {
			writeError(w, 400, "invalid_arg", "copy_credentials is a remote install option")
			return
		}
		if !authorize(req.CopyCredentials) {
			writeError(w, 403, "permission_denied", "install and credential receiving grants are required")
			return
		}
	}
	svc, err := localHarnessOperations()
	if err != nil {
		writeError(w, 503, "harness_operations_unavailable", "could not initialize harness operations")
		return
	}
	if r.Method == http.MethodPost {
		job, err := svc.Start(req, actor, authorize)
		if err != nil {
			code := "harness_operation_failed"
			status := 400
			if errors.Is(err, harnessops.ErrBusy) {
				code = "harness_operation_busy"
				status = 409
			}
			if errors.Is(err, harnessops.ErrWorkersBusy) {
				code = "harness_workers_busy"
				status = 409
			}
			writeError(w, status, code, err.Error())
			return
		}
		recordFederationAudit("federation.harness.manage", actor, "", "", req.Action+" harness="+req.Harness+" job="+job.ID+" accepted", 202)
		writeJSON(w, 202, job)
		return
	}
	if id := r.PathValue("id"); id != "" {
		job, err := svc.Job(id)
		if err != nil {
			writeError(w, 404, "not_found", "harness job not found")
			return
		}
		writeJSON(w, 200, job)
		return
	}
	writeJSON(w, 200, map[string]any{"recipes": harnessops.Recipes(), "credential_copy_warning": "Target agents will act as you with this provider; copy is never enabled by default", "modes": []string{"now", "when_idle"}})
}

// prepareHarnessCredentialPush runs only after local human/cookie auth and a
// pinned trusted destination lookup. Incoming peers never call this function.
func prepareHarnessCredentialPush(r *http.Request, body []byte) ([]byte, error) {
	if r.Method != http.MethodPost || (r.PathValue("tail") != "harnesses/operations" && r.PathValue("tail") != "harnesses/credentials") {
		return body, nil
	}
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	req, err := decodeHarnessOperation(clone, false)
	if err != nil {
		return nil, err
	}
	if !req.CopyCredentials {
		return body, nil
	}
	home, err := canonicalOperatorHome()
	if err != nil {
		return nil, fmt.Errorf("operator credential location unavailable")
	}
	bundle, err := harnesscredentials.Capture(home, req.Harness)
	if err != nil {
		return nil, err
	}
	req.Credentials = &bundle
	out, err := json.Marshal(req)
	if err != nil || len(out) > fedPeerViewBodyLimit {
		return nil, fmt.Errorf("credential request exceeds encrypted peer transport limit")
	}
	return out, nil
}

func startBackgroundHarnessOperations(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				harnessOperations.Lock()
				svc := harnessOperations.service
				harnessOperations.Unlock()
				if svc != nil {
					svc.Close()
				}
				return
			case <-ticker.C:
				harnessops.RefreshLatestAsync()
			}
		}
	}()
}

func stopHarnessOperations() {
	harnessOperations.Lock()
	svc := harnessOperations.service
	harnessOperations.Unlock()
	if svc != nil {
		svc.Close()
	}
}
