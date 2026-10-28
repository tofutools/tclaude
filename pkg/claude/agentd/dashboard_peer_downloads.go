package agentd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A peer's file downloads (a snapshot's or an agent's .paf, a template) in
// this node's dashboard. The peer view carries JSON replies of at most
// fedPeerViewResponseBodyLimit, so the file is staged on the peer and crosses
// as base64 chunks, as a notification's attachment does:
//
//	POST /api/peer-downloads {"path": "/api/snapshots/x/export"}
//	    runs one downloadable GET (peerDownloadRoutes) into a staged file and
//	    answers {id, size, filename}. Through the peer view it is held only
//	    by unrestricted trust, and runs as a peer view job.
//
//	GET /api/peer-downloads/{id}?offset=O[&length=L]
//	    one chunk of a staged file. A staged file goes away
//	    peerDownloadKeep after its last read.
//
//	GET /api/peer-download/{node}?path=P
//	    local-only: stages P on the peer and serves it as a sandboxed
//	    download, chunk by chunk.
//
// A download is at most federation.agent_transfer_max_bytes (2 GiB by
// default), the cap agent archives already cross nodes under.

// peerDownloadRoutes are the GETs a peer may stage: file downloads whose
// route the peer view already serves under unrestricted trust.
var peerDownloadRoutes = map[string]bool{
	"GET /api/snapshots/{name}/export": true,
	"GET /api/agent-bundle/export":     true,
	"GET /api/templates/{name}/export": true,
	"GET /api/groups/{name}/export":    true,
	// A daemon log export: Diagnostics › Logs of a fused view's peer.
	"/api/logs/export": true,
}

const (
	peerDownloadKeep   = 10 * time.Minute
	peerDownloadsKept  = 4
	peerDownloadPrefix = "pdl_"
)

type peerDownload struct {
	file     string
	size     int64
	filename string
	read     time.Time
}

var peerDownloads = struct {
	sync.Mutex
	byID map[string]*peerDownload
}{byID: map[string]*peerDownload{}}

type peerDownloadStaged struct {
	ID       string `json:"id"`
	Size     int64  `json:"size"`
	Filename string `json:"filename"`
}

func registerDashboardPeerDownloadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/peer-downloads", handleDashboardPeerDownloadStage)
	mux.HandleFunc("GET /api/peer-downloads/{id}", handleDashboardPeerDownloadChunk)
	mux.HandleFunc("GET /api/peer-download/{node}", handleDashboardPeerDownload)
}

// sweepPeerDownloadsLocked drops staged files nobody read within
// peerDownloadKeep, then the oldest past peerDownloadsKept.
func sweepPeerDownloadsLocked(now time.Time) {
	for id, d := range peerDownloads.byID {
		if now.Sub(d.read) > peerDownloadKeep {
			_ = os.Remove(d.file)
			delete(peerDownloads.byID, id)
		}
	}
	for len(peerDownloads.byID) >= peerDownloadsKept {
		oldest := ""
		for id, d := range peerDownloads.byID {
			if oldest == "" || d.read.Before(peerDownloads.byID[oldest].read) {
				oldest = id
			}
		}
		_ = os.Remove(peerDownloads.byID[oldest].file)
		delete(peerDownloads.byID, oldest)
	}
}

// peerDownloadFile is the ResponseWriter a staged GET writes its body to.
type peerDownloadFile struct {
	limit  int64
	header http.Header
	status int
	file   *os.File
	size   int64
	err    error
}

func (f *peerDownloadFile) Header() http.Header { return f.header }
func (f *peerDownloadFile) WriteHeader(status int) {
	if f.status == 0 {
		f.status = status
	}
}
func (f *peerDownloadFile) Write(b []byte) (int, error) {
	if f.status == 0 {
		f.status = http.StatusOK
	}
	if f.size+int64(len(b)) > f.limit {
		f.err = errors.New("download exceeds limit")
		return 0, f.err
	}
	n, err := f.file.Write(b)
	f.size += int64(n)
	if err != nil {
		f.err = err
	}
	return n, err
}
func (f *peerDownloadFile) Flush() {}

func handleDashboardPeerDownloadStage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if !checkDashboardAuth(w, r) {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "path is required")
		return
	}
	target, err := url.ParseRequestURI(body.Path)
	if err != nil || len(body.Path) > 4096 || target.IsAbs() || target.Host != "" || !strings.HasPrefix(target.Path, "/api/") {
		writeError(w, http.StatusBadRequest, "invalid_arg", "path must be a dashboard API path")
		return
	}
	sub := r.Clone(r.Context())
	sub.Method, sub.URL, sub.RequestURI, sub.Body, sub.ContentLength = http.MethodGet, target, target.RequestURI(), http.NoBody, 0
	sub.Header = http.Header{}
	mux := peerDashboardMux()
	if _, pattern := mux.Handler(sub); !peerDownloadRoutes[pattern] {
		writeError(w, http.StatusForbidden, "not_downloadable", "that route is not a download")
		return
	}
	f, err := os.CreateTemp("", "tclaude-peer-download-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "stage download")
		return
	}
	out := &peerDownloadFile{limit: agentTransferLimit(), header: make(http.Header), file: f}
	mux.ServeHTTP(out, sub)
	closeErr := f.Close()
	if out.status == 0 {
		out.status = http.StatusOK
	}
	if out.status != http.StatusOK || out.err != nil || closeErr != nil {
		raw, _ := os.ReadFile(f.Name())
		_ = os.Remove(f.Name())
		if out.err != nil || closeErr != nil {
			writeError(w, http.StatusInternalServerError, "io", "stage download")
			return
		}
		// The route's own refusal, when it is one.
		if json.Valid(raw) && len(raw) <= 64<<10 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(out.status)
			_, _ = w.Write(raw)
			return
		}
		writeError(w, out.status, "download", http.StatusText(out.status))
		return
	}
	filename := ""
	if _, params, err := mime.ParseMediaType(out.header.Get("Content-Disposition")); err == nil {
		filename = params["filename"]
	}
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	id := peerDownloadPrefix + hex.EncodeToString(raw)
	now := time.Now()
	peerDownloads.Lock()
	sweepPeerDownloadsLocked(now)
	peerDownloads.byID[id] = &peerDownload{file: f.Name(), size: out.size, filename: filename, read: now}
	peerDownloads.Unlock()
	writeJSON(w, http.StatusOK, peerDownloadStaged{ID: id, Size: out.size, Filename: filename})
}

func handleDashboardPeerDownloadChunk(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "offset is required")
		return
	}
	length := int64(peerAttachmentChunk)
	if v := r.URL.Query().Get("length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "length must be a non-negative integer")
			return
		}
		length = min(length, n)
	}
	peerDownloads.Lock()
	sweepPeerDownloadsLocked(time.Now())
	d := peerDownloads.byID[r.PathValue("id")]
	if d != nil {
		d.read = time.Now()
	}
	peerDownloads.Unlock()
	if d == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such download")
		return
	}
	defer func() {
		// The last chunk read, the staged file goes.
		if offset+length >= d.size {
			peerDownloads.Lock()
			if peerDownloads.byID[r.PathValue("id")] == d {
				delete(peerDownloads.byID, r.PathValue("id"))
				_ = os.Remove(d.file)
			}
			peerDownloads.Unlock()
		}
	}()
	if offset > d.size {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid_arg", "offset is past the end of the file")
		return
	}
	f, err := os.Open(d.file)
	if err != nil {
		writeError(w, http.StatusGone, "download", "this download is no longer available")
		return
	}
	defer func() { _ = f.Close() }()
	data := make([]byte, min(length, d.size-offset))
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusInternalServerError, "io", "read download")
		return
	}
	writeJSON(w, http.StatusOK, humanAttachmentChunk{Filename: d.filename, ContentType: "application/octet-stream", Size: d.size, Offset: offset, Data: data})
}

// peerDownloadRefused is the download route's own refusal on the peer (a
// flagged export's findings), which the dialog reads as it would locally.
type peerDownloadRefused struct{ body []byte }

func (e *peerDownloadRefused) Error() string {
	var body struct{ Error string }
	_ = json.Unmarshal(e.body, &body)
	return "peer refused: " + body.Error
}

// stagePeerDownload asks a peer to stage one download; it runs as a peer view
// job there, so a large export outlasts one exchange.
var stagePeerDownload = func(ctx context.Context, peer *db.FederationPeer, target string) (peerDownloadStaged, int, error) {
	rt := currentFederation()
	if rt == nil {
		return peerDownloadStaged{}, http.StatusBadGateway, errPeerViewOffline
	}
	raw, _ := json.Marshal(map[string]string{"path": target})
	req := fedPeerViewRequest{Method: http.MethodPost, URI: "/api/peer-downloads", Header: http.Header{"Content-Type": {"application/json"}}, Body: raw}
	reply, err := exchangePeerView(ctx, rt, peer, req)
	if err == nil {
		reply, _, err = awaitPeerViewJob(ctx, rt, peer, "/api/peer-downloads", reply)
	}
	if err != nil {
		if errors.Is(err, errPeerViewBusy) {
			return peerDownloadStaged{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, errPeerViewTimeout) {
			return peerDownloadStaged{}, http.StatusGatewayTimeout, err
		}
		return peerDownloadStaged{}, http.StatusBadGateway, err
	}
	if !validPeerViewReply(reply) {
		return peerDownloadStaged{}, http.StatusBadGateway, errors.New("peer returned an invalid response")
	}
	if reply.Status != http.StatusOK {
		return peerDownloadStaged{}, reply.Status, &peerDownloadRefused{body: reply.Body}
	}
	var staged peerDownloadStaged
	if json.Unmarshal(reply.Body, &staged) != nil || !strings.HasPrefix(staged.ID, peerDownloadPrefix) || staged.Size < 0 || staged.Size > agentTransferLimit() {
		return peerDownloadStaged{}, http.StatusBadGateway, errors.New("peer returned an invalid reply")
	}
	return staged, http.StatusOK, nil
}

// fetchPeerDownloadChunk reads one chunk of a download the peer staged.
var fetchPeerDownloadChunk = func(ctx context.Context, peer *db.FederationPeer, id string, offset int64) (humanAttachmentChunk, int, error) {
	q := url.Values{"offset": {strconv.FormatInt(offset, 10)}, "length": {strconv.Itoa(peerAttachmentChunk)}}
	var c humanAttachmentChunk
	if status, err := peerViewGetJSON(ctx, peer, "/api/peer-downloads/"+url.PathEscape(id)+"?"+q.Encode(), &c); err != nil {
		return humanAttachmentChunk{}, status, err
	}
	if c.Size < 0 || int64(len(c.Data)) > c.Size-c.Offset {
		return humanAttachmentChunk{}, http.StatusBadGateway, errors.New("peer returned an invalid download chunk")
	}
	return c, http.StatusOK, nil
}

// handleDashboardPeerDownload serves a trusted peer's download as this node's
// own route serves it. The peer's bytes never run on this origin: the type is
// opaque and the response a sandboxed attachment.
func handleDashboardPeerDownload(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	peer, err := db.GetFederationPeer(r.PathValue("node"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "peer_unavailable", "could not read peer trust")
		return
	}
	if peer == nil {
		writeError(w, http.StatusForbidden, "not_trusted", "linked trusted peer required")
		return
	}
	target := r.URL.Query().Get("path")
	if !strings.HasPrefix(target, "/api/") || len(target) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "path must be a dashboard API path")
		return
	}
	staged, status, err := stagePeerDownload(r.Context(), peer, target)
	var refused *peerDownloadRefused
	if errors.As(err, &refused) && len(refused.body) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(refused.body)
		return
	}
	if err != nil {
		writeError(w, status, "peer_download", err.Error())
		return
	}
	name := path.Base(strings.ReplaceAll(staged.Filename, "\\", "/"))
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	reader := &peerAttachmentReader{first: humanAttachmentChunk{Size: staged.Size}, fetch: func(offset int64) (humanAttachmentChunk, error) {
		ctx, cancel := context.WithTimeout(r.Context(), fedPeerViewTimeout)
		defer cancel()
		c, _, err := fetchPeerDownloadChunk(ctx, peer, staged.ID, offset)
		return c, err
	}}
	http.ServeContent(w, r, "", time.Time{}, reader)
}
