package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/harnesscredentials"
)

const credentialShareWarning = "Remote node agents will act as you with this provider; confirm_share is required to push your credentials"

type credentialRequest struct {
	Harness      string                     `json:"harness"`
	ConfirmShare bool                       `json:"confirm_share,omitempty"`
	Backup       string                     `json:"backup,omitempty"`
	Credentials  *harnesscredentials.Bundle `json:"credentials,omitempty"`
}

func decodeCredentialRequest(r *http.Request, peer bool) (credentialRequest, error) {
	var req credentialRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, fedPeerViewBodyLimit+1))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		return req, fmt.Errorf("invalid credential request")
	}
	switch req.Harness {
	case "claude", "codex", "gemini", "opencode":
	default:
		return req, fmt.Errorf("file credentials unsupported for this harness; use its login flow")
	}
	if req.Backup != "" && !harnesscredentials.ValidBackupID(req.Backup) {
		return req, fmt.Errorf("invalid backup ID")
	}
	if req.Credentials != nil {
		if !peer {
			return req, fmt.Errorf("credential contents cannot be supplied by local clients")
		}
		if req.Credentials.Harness != req.Harness {
			return req, fmt.Errorf("credential harness mismatch")
		}
		if err := req.Credentials.Validate(); err != nil {
			return req, err
		}
	}
	return req, nil
}
func registerHarnessCredentialRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/harnesses/credentials/backups", handleDashboardHarnessCredentials)
	mux.HandleFunc("POST /api/harnesses/credentials/push", handleDashboardHarnessCredentials)
	mux.HandleFunc("POST /api/harnesses/credentials/backup", handleDashboardHarnessCredentials)
	mux.HandleFunc("POST /api/harnesses/credentials/restore", handleDashboardHarnessCredentials)
}
func handleDashboardHarnessCredentials(w http.ResponseWriter, r *http.Request) {
	if checkDashboardAuth(w, r) {
		serveHarnessCredentials(w, r, "operator", nil)
	}
}
func handleLocalHarnessCredentials(w http.ResponseWriter, r *http.Request) {
	if requireHuman(w, r, "manage harness credentials") {
		serveHarnessCredentials(w, r, "operator", nil)
	}
}
func servePeerHarnessCredentials(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !harnessOperationAllowed(v, rule) {
		writeError(w, 403, "permission_denied", "node.credentials.receive is not shared")
		return
	}
	serveHarnessCredentials(w, r, "operator@"+v.peer.InstanceID, func() bool { return harnessOperationAllowed(v, rule) })
}
func credentialBackupDir() (string, error) {
	return filepath.Abs(filepath.Join(common.TclaudeDataDir(), "harness-operations", "credential-backups"))
}
func serveHarnessCredentials(w http.ResponseWriter, r *http.Request, actor string, authorize func() bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	peer := authorize != nil
	if authorize == nil {
		authorize = func() bool { return true }
	}
	dir, err := credentialBackupDir()
	if err != nil {
		writeError(w, 503, "credentials_unavailable", "private credential directory unavailable")
		return
	}
	if r.Method == http.MethodGet {
		entries, err := harnesscredentials.ListBackups(dir, r.URL.Query().Get("harness"))
		if err != nil {
			writeError(w, 400, "credential_backup_failed", err.Error())
			return
		}
		if !authorize() {
			writeError(w, 403, "permission_denied", "credential receiving authority revoked")
			return
		}
		writeJSON(w, 200, map[string]any{"backups": entries, "credential_share_warning": credentialShareWarning})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, fedPeerViewBodyLimit)
	req, err := decodeCredentialRequest(r, peer)
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if action == "push" && (!peer || !req.ConfirmShare || req.Credentials == nil) {
		writeError(w, 400, "confirmation_required", credentialShareWarning+" through the local peer proxy")
		return
	}
	if action != "push" && (req.Credentials != nil || req.ConfirmShare) || action != "restore" && req.Backup != "" {
		writeError(w, 400, "invalid_arg", "incompatible credential request fields")
		return
	}
	home, err := canonicalOperatorHome()
	if err != nil {
		writeError(w, 503, "credentials_unavailable", "operator credential root unavailable")
		return
	}
	var result any
	var receipt harnesscredentials.Receipt
	switch action {
	case "push":
		receipt, err = harnesscredentials.Receive(home, dir, *req.Credentials, true, authorize)
		result = receipt
	case "backup":
		receipt, err = harnesscredentials.Backup(home, dir, req.Harness, authorize)
		result = receipt
	case "restore":
		var restored harnesscredentials.RestoreReceipt
		restored, err = harnesscredentials.Restore(home, dir, req.Harness, req.Backup, authorize)
		receipt = restored.Receipt
		result = restored
	default:
		writeError(w, 404, "not_found", "credential operation not found")
		return
	}
	status := 200
	if err != nil {
		status = 409
	}
	recordFederationAudit("federation.credentials."+action, actor, "", "", "harness="+req.Harness+" backup="+receipt.BackupID+" result="+fmt.Sprint(err == nil), status)
	if err != nil {
		writeJSON(w, status, map[string]any{"error": "credential_operation_failed", "message": err.Error(), "receipt": result})
		return
	}
	availability := refreshHarnessAvailability()
	if !authorize() {
		writeError(w, 403, "permission_denied", "credential receiving authority revoked")
		return
	}
	writeJSON(w, 200, map[string]any{"receipt": result, "availability": availability})
}
func prepareStandaloneCredentialPush(r *http.Request, body []byte) ([]byte, error) {
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	req, err := decodeCredentialRequest(clone, false)
	if err != nil {
		return nil, err
	}
	if !req.ConfirmShare {
		return nil, fmt.Errorf("%s", credentialShareWarning)
	}
	if req.Backup != "" {
		return nil, fmt.Errorf("backup is only valid for restore")
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
