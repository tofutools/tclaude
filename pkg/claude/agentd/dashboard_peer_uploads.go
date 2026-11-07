package agentd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// File uploads to a peer from this node's dashboard (a spawn's attachments on
// another node). A peer view request carries at most fedPeerViewBodyLimit, so
// the file crosses in chunks of peerUploadChunk, staged on the peer, which
// then runs the upload route on the whole file, as downloads cross the other
// way (dashboard_peer_downloads.go):
//
//	POST /api/peer-uploads
//	    opens a staged upload and answers {id}. Only the peer that opened it
//	    appends to it or finishes it (peer_import.go).
//
//	POST /api/peer-uploads/chunk?id=ID&offset=O
//	    appends one chunk (the raw body) at O, the staged size so far.
//
//	POST /api/peer-uploads/finish {"id", "path", "content_type"}
//	    runs one upload route (peerUploadRoutes) with the staged file as its
//	    body and answers that route's reply. A peer view job, held by the
//	    route's own peer rule: unrestricted trust, or its peer gate
//	    (peerUploadGates).
//
//	POST /api/peer-upload/{node}?path=P
//	    local-only: sends this request's body to the peer in chunks and
//	    answers the peer route's reply.
//
// An upload is at most federation.agent_transfer_max_bytes (2 GiB by
// default); the upload route applies its own caps on top.

// peerUploadRoutes are the POSTs a staged upload may run: upload routes the
// peer view serves under unrestricted trust, or under the route's own peer
// grant (peerUploadGates).
var peerUploadRoutes = map[string]bool{
	"/api/spawn-attachments":          true,
	"POST /api/groups/import":         true,
	"POST /api/groups/import/inspect": true,
	// A task board's files (peer_taskboard.go, peerTaskboardUploads).
	"POST /v1/taskboard/tasks/{id}/attachments":    true,
	"POST /v1/taskboard/tasks/{id}/comments":       true,
	"POST /v1/groups/{name}/taskboard/tasks":       true,
	"POST /v1/groups/{name}/taskboard/transfer-in": true,
	"POST /v1/groups/{name}/taskboard/ask":         true,
}

// peerUploadPath reports whether p may name an upload route: a dashboard
// API path or a /v1 route (peerUploadRoutes decides which).
func peerUploadPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/v1/")
}

const (
	// peerUploadChunk is the most one chunk carries. Only the chunk route's
	// request may be this large (peerViewBodyLimit); every other peer view
	// request keeps fedPeerViewBodyLimit.
	peerUploadChunk       = 512 << 10
	peerUploadKeep        = 10 * time.Minute
	peerUploadsKept       = 8
	peerUploadPrefix      = "pul_"
	peerUploadChunkPath   = "/api/peer-uploads/chunk"
	peerUploadFinishPath  = "/api/peer-uploads/finish"
	peerUploadContentType = 256
)

// peerViewBodyLimit is the request body a peer view exchange to path may
// carry.
func peerViewBodyLimit(path string) int {
	if path == peerUploadChunkPath {
		return peerUploadChunk
	}
	return fedPeerViewBodyLimit
}

type peerUpload struct {
	file    string
	size    int64
	written time.Time
	// owner is the peer that opened it ("" when this node's dashboard did).
	owner string
}

var peerUploads = struct {
	sync.Mutex
	byID map[string]*peerUpload
}{byID: map[string]*peerUpload{}}

func registerDashboardPeerUploadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/peer-uploads", handleDashboardPeerUploadOpen)
	mux.HandleFunc("POST /api/peer-uploads/chunk", handleDashboardPeerUploadChunk)
	mux.HandleFunc("/api/peer-uploads/finish", handleDashboardPeerUploadFinish)
	mux.HandleFunc("POST /api/peer-upload/{node}", handleDashboardPeerUpload)
}

// sweepPeerUploadsLocked drops uploads nobody wrote to within
// peerUploadKeep.
func sweepPeerUploadsLocked(now time.Time) {
	for id, u := range peerUploads.byID {
		if now.Sub(u.written) > peerUploadKeep {
			_ = os.Remove(u.file)
			delete(peerUploads.byID, id)
		}
	}
}

// capPeerUploadsLocked drops owner's oldest uploads past peerUploadsKept, so
// one peer's uploads never evict another's.
func capPeerUploadsLocked(owner string) {
	for {
		oldest, count := "", 0
		for id, u := range peerUploads.byID {
			if u.owner != owner {
				continue
			}
			count++
			if oldest == "" || u.written.Before(peerUploads.byID[oldest].written) {
				oldest = id
			}
		}
		if count < peerUploadsKept {
			return
		}
		_ = os.Remove(peerUploads.byID[oldest].file)
		delete(peerUploads.byID, oldest)
	}
}

func handleDashboardPeerUploadOpen(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	openPeerUpload(w, "")
}

// openPeerUpload opens a staged upload for owner.
func openPeerUpload(w http.ResponseWriter, owner string) {
	f, err := os.CreateTemp("", "tclaude-peer-upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "stage upload")
		return
	}
	_ = f.Close()
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	id := peerUploadPrefix + hex.EncodeToString(raw)
	now := time.Now()
	peerUploads.Lock()
	sweepPeerUploadsLocked(now)
	capPeerUploadsLocked(owner)
	peerUploads.byID[id] = &peerUpload{file: f.Name(), written: now, owner: owner}
	peerUploads.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func handleDashboardPeerUploadChunk(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	appendPeerUpload(w, r, "")
}

// appendPeerUpload appends one chunk to an upload owner opened.
func appendPeerUpload(w http.ResponseWriter, r *http.Request, owner string) {
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "offset is required")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, peerUploadChunk+1))
	if err != nil || len(data) > peerUploadChunk {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a chunk is at most 512 KiB")
		return
	}
	// One chunk at a time per upload: the lock covers the append.
	peerUploads.Lock()
	defer peerUploads.Unlock()
	sweepPeerUploadsLocked(time.Now())
	u := peerUploads.byID[r.URL.Query().Get("id")]
	if u == nil || u.owner != owner {
		writeError(w, http.StatusNotFound, "not_found", "no such upload")
		return
	}
	if offset != u.size {
		writeError(w, http.StatusConflict, "offset", "offset must be the upload's size so far")
		return
	}
	if u.size+int64(len(data)) > agentTransferLimit() {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "upload exceeds federation.agent_transfer_max_bytes")
		return
	}
	f, err := os.OpenFile(u.file, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		writeError(w, http.StatusGone, "upload", "this upload is no longer available")
		return
	}
	n, err := f.Write(data)
	closeErr := f.Close()
	u.size += int64(n)
	u.written = time.Now()
	if err != nil || closeErr != nil {
		writeError(w, http.StatusInternalServerError, "io", "write upload")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"size": u.size})
}

func handleDashboardPeerUploadFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if !checkDashboardAuth(w, r) {
		return
	}
	finishPeerUpload(w, r, "", func(w http.ResponseWriter, sub *http.Request, _ string) {
		peerDashboardMux().ServeHTTP(w, sub)
	})
}

// finishPeerUpload takes owner's staged upload off the stage and runs its
// upload route through serve with the staged file as sub's body; pattern is
// the route's.
func finishPeerUpload(w http.ResponseWriter, r *http.Request, owner string, serve func(w http.ResponseWriter, sub *http.Request, pattern string)) {
	var body struct {
		ID          string `json:"id"`
		Path        string `json:"path"`
		ContentType string `json:"content_type"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil || len(body.ContentType) > peerUploadContentType {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id and path are required")
		return
	}
	peerUploads.Lock()
	u := peerUploads.byID[body.ID]
	if u != nil && u.owner != owner {
		u = nil
	}
	if u != nil {
		delete(peerUploads.byID, body.ID)
	}
	peerUploads.Unlock()
	if u == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such upload")
		return
	}
	defer func() { _ = os.Remove(u.file) }()
	target, err := url.ParseRequestURI(body.Path)
	if err != nil || len(body.Path) > 4096 || target.IsAbs() || target.Host != "" || !peerUploadPath(target.Path) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "path must be a dashboard API path")
		return
	}
	f, err := os.Open(u.file)
	if err != nil {
		writeError(w, http.StatusGone, "upload", "this upload is no longer available")
		return
	}
	defer func() { _ = f.Close() }()
	sub := r.Clone(r.Context())
	sub.Method, sub.URL, sub.RequestURI, sub.Body, sub.ContentLength = http.MethodPost, target, target.RequestURI(), f, u.size
	sub.Header = http.Header{}
	if body.ContentType != "" {
		sub.Header.Set("Content-Type", body.ContentType)
	}
	_, pattern := peerDashboardMux().Handler(sub)
	if !peerUploadRoutes[pattern] {
		writeError(w, http.StatusForbidden, "not_uploadable", "that route is not an upload")
		return
	}
	serve(w, sub, pattern)
}

// peerUploadExchange runs one request of an upload on the peer and decodes
// its JSON reply into out (when given). The upload route's own refusal comes
// back as a peerDownloadRefused, for the caller to pass on as it is.
var peerUploadExchange = func(ctx context.Context, peer *db.FederationPeer, req fedPeerViewRequest, out any) (int, error) {
	rt := currentFederation()
	if rt == nil {
		return http.StatusBadGateway, errPeerViewOffline
	}
	reply, err := exchangePeerView(ctx, rt, peer, req)
	if err == nil && req.URI == peerUploadFinishPath {
		reply, _, err = awaitPeerViewJob(ctx, rt, peer, peerUploadFinishPath, reply)
	}
	if err != nil {
		if errors.Is(err, errPeerViewBusy) {
			return http.StatusServiceUnavailable, err
		}
		if errors.Is(err, errPeerViewTimeout) {
			return http.StatusGatewayTimeout, err
		}
		return http.StatusBadGateway, err
	}
	if !validPeerViewReply(reply) {
		return http.StatusBadGateway, errors.New("peer returned an invalid response")
	}
	// A route may answer 201 (a task created with its files): any 2xx is
	// its success.
	if reply.Status < 200 || reply.Status > 299 {
		return reply.Status, &peerDownloadRefused{body: reply.Body}
	}
	if out != nil && json.Unmarshal(reply.Body, out) != nil {
		return http.StatusBadGateway, errors.New("peer returned an invalid reply")
	}
	return http.StatusOK, nil
}

// handleDashboardPeerUpload sends this request's body to a trusted peer's
// upload route in chunks and answers that route's reply.
func handleDashboardPeerUpload(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	peer, err := db.GetFederationPeer(r.PathValue("node"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "peer_unavailable", "could not read peer trust")
		return
	}
	if peer == nil {
		writeError(w, http.StatusForbidden, "not_trusted", "linked trusted peer required")
		return
	}
	sendPeerUpload(w, r, peer)
}

// handleFederationPeerUpload is handleDashboardPeerUpload for the CLI
// (POST /v1/federation/peer-upload/{node}?path=P): the operator's, by the
// peer's label or instance id. A task board's files reach a linked node
// through it (`tclaude agent taskboard attach … --peer`).
func handleFederationPeerUpload(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "upload to a linked node") {
		return
	}
	peer, err := resolveFederationPeerOpt(r.PathValue("node"), false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	sendPeerUpload(w, r, peer)
}

// sendPeerUpload sends r's body to peer's upload route in chunks and
// answers that route's reply.
func sendPeerUpload(w http.ResponseWriter, r *http.Request, peer *db.FederationPeer) {
	target := r.URL.Query().Get("path")
	if !peerUploadPath(target) || len(target) > 4096 || len(r.Header.Get("Content-Type")) > peerUploadContentType {
		writeError(w, http.StatusBadRequest, "invalid_arg", "path must be a dashboard API path")
		return
	}
	fail := func(status int, err error) {
		var refused *peerDownloadRefused
		if errors.As(err, &refused) && len(refused.body) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(refused.body)
			return
		}
		if status == http.StatusForbidden || status == http.StatusNotFound {
			// An older peer has no upload routes; a restricted one refuses them.
			writeError(w, status, "peer_upload", "the peer takes uploads only under unrestricted trust, from a current tclaude")
			return
		}
		writeError(w, status, "peer_upload", err.Error())
	}
	exchange := func(req fedPeerViewRequest, out any) (int, error) {
		ctx, cancel := context.WithTimeout(r.Context(), fedPeerViewTimeout)
		defer cancel()
		if req.URI == peerUploadFinishPath {
			// The route's own work runs as a job, which outlasts one exchange.
			return peerUploadExchange(r.Context(), peer, req, out)
		}
		return peerUploadExchange(ctx, peer, req, out)
	}
	var opened struct {
		ID string `json:"id"`
	}
	if status, err := exchange(fedPeerViewRequest{Method: http.MethodPost, URI: "/api/peer-uploads", Header: http.Header{}}, &opened); err != nil {
		fail(status, err)
		return
	}
	if !strings.HasPrefix(opened.ID, peerUploadPrefix) {
		writeError(w, http.StatusBadGateway, "peer_upload", "peer returned an invalid reply")
		return
	}
	// A failed upload drops what the peer staged: finishing it with no
	// route removes it there.
	abort := func() {
		raw, _ := json.Marshal(map[string]string{"id": opened.ID})
		_, _ = exchange(fedPeerViewRequest{Method: http.MethodPost, URI: peerUploadFinishPath, Header: http.Header{"Content-Type": {"application/json"}}, Body: raw}, nil)
	}
	body := http.MaxBytesReader(w, r.Body, agentTransferLimit())
	buf := make([]byte, peerUploadChunk)
	var offset int64
	for {
		n, readErr := io.ReadFull(body, buf)
		if n > 0 {
			q := url.Values{"id": {opened.ID}, "offset": {strconv.FormatInt(offset, 10)}}
			req := fedPeerViewRequest{Method: http.MethodPost, URI: peerUploadChunkPath + "?" + q.Encode(), Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: buf[:n]}
			if status, err := exchange(req, nil); err != nil {
				abort()
				fail(status, err)
				return
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			abort()
			if tooLarge := new(http.MaxBytesError); errors.As(readErr, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "too_large", "upload exceeds federation.agent_transfer_max_bytes")
				return
			}
			writeError(w, http.StatusBadRequest, "upload", "read upload: "+readErr.Error())
			return
		}
	}
	raw, _ := json.Marshal(map[string]string{"id": opened.ID, "path": target, "content_type": r.Header.Get("Content-Type")})
	var reply json.RawMessage
	if status, err := exchange(fedPeerViewRequest{Method: http.MethodPost, URI: peerUploadFinishPath, Header: http.Header{"Content-Type": {"application/json"}}, Body: raw}, &reply); err != nil {
		fail(status, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply)
}
