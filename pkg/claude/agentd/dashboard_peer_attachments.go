package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A peer's human-notification attachments in this node's dashboard (the fused
// Messages view). The peer view carries JSON replies of at most
// fedPeerViewResponseBodyLimit, so a file crosses as base64 chunks:
//
//	GET /api/human-attachment-chunk?message=N&attachment=M&offset=O[&length=L]
//	    one chunk of a stored attachment (at most L bytes; length=0 asks only
//	    for its name, type and size) with its name, type and size. Served
//	    to this node's dashboard and, through the peer view, to a peer granted
//	    node.messages.read (the grant that already shows it the message).
//
//	GET /api/peer-attachments/{node}/{message}/{attachment}
//	    local-only: reads a trusted peer's attachment chunk by chunk and serves
//	    it like this node's own attachment route, Range requests included, so
//	    the dashboard's previews, players and downloads work unchanged. The
//	    peer's bytes never run on this origin: the type is narrowed to media
//	    and plain text, and the response is a sandboxed download.

// peerAttachmentChunk is the most a chunk carries: base64 and the JSON
// envelope must stay under fedPeerViewResponseBodyLimit.
const peerAttachmentChunk = 4 << 20

type humanAttachmentChunk struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Offset      int64  `json:"offset"`
	Data        []byte `json:"data"`
}

func registerDashboardPeerAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/human-attachment-chunk", handleDashboardHumanAttachmentChunk)
	mux.HandleFunc("GET /api/peer-attachments/{node}/{message}/{attachment}", handleDashboardPeerAttachment)
}

func handleDashboardHumanAttachmentChunk(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	q := r.URL.Query()
	messageID, err1 := strconv.ParseInt(q.Get("message"), 10, 64)
	attachmentID, err2 := strconv.ParseInt(q.Get("attachment"), 10, 64)
	offset, err3 := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || messageID <= 0 || attachmentID <= 0 || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "message, attachment and offset are required")
		return
	}
	length := int64(peerAttachmentChunk)
	if v := q.Get("length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "length must be a non-negative integer")
			return
		}
		length = min(length, n)
	}
	a, err := resolveHumanMessageAttachment(messageID, attachmentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read attachment")
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such attachment")
		return
	}
	f, err := os.Open(a.StoragePath)
	if err != nil {
		writeError(w, http.StatusGone, "missing", "this attachment is no longer available")
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read attachment")
		return
	}
	size := info.Size()
	if offset > size {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid_arg", "offset is past the end of the file")
		return
	}
	data := make([]byte, min(length, size-offset))
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusInternalServerError, "io", "read attachment")
		return
	}
	contentType := a.ContentType
	if videoType := humanMessageAttachmentVideoType(a); videoType != "" {
		contentType = videoType
	}
	writeJSON(w, http.StatusOK, humanAttachmentChunk{Filename: a.Filename, ContentType: contentType, Size: size, Offset: offset, Data: data})
}

// peerAttachmentType narrows a peer-declared type to what the dashboard can
// show inline without running it: raster images, video, audio and plain text.
// Anything else is served as an opaque download.
func peerAttachmentType(declared string) string {
	media, _, err := mime.ParseMediaType(declared)
	if err != nil {
		return "application/octet-stream"
	}
	switch {
	case media == "image/svg+xml":
		return "application/octet-stream"
	case strings.HasPrefix(media, "image/"), strings.HasPrefix(media, "video/"), strings.HasPrefix(media, "audio/"):
		return media
	case strings.HasPrefix(media, "text/"):
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// peerAttachmentReader reads a peer's attachment through chunk requests. It
// seeks, so http.ServeContent can answer Range and HEAD requests with only
// the chunks they need.
type peerAttachmentReader struct {
	fetch    func(offset int64) (humanAttachmentChunk, error)
	first    humanAttachmentChunk
	pos      int64
	buf      []byte
	bufStart int64
}

func (p *peerAttachmentReader) Read(out []byte) (int, error) {
	if p.pos >= p.first.Size {
		return 0, io.EOF
	}
	if p.pos < p.bufStart || p.pos >= p.bufStart+int64(len(p.buf)) {
		c, err := p.fetch(p.pos)
		if err != nil {
			return 0, err
		}
		if c.Offset != p.pos || len(c.Data) == 0 || c.Size != p.first.Size {
			return 0, errors.New("peer returned an inconsistent attachment chunk")
		}
		p.buf, p.bufStart = c.Data, c.Offset
	}
	n := copy(out, p.buf[p.pos-p.bufStart:])
	p.pos += int64(n)
	return n, nil
}

func (p *peerAttachmentReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += p.pos
	case io.SeekEnd:
		offset += p.first.Size
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	p.pos = offset
	return offset, nil
}

// fetchPeerAttachmentChunk asks a peer for one chunk over the peer view:
// up to length bytes from offset (length 0: only the file's metadata).
var fetchPeerAttachmentChunk = func(ctx context.Context, peer *db.FederationPeer, messageID, attachmentID, offset, length int64) (humanAttachmentChunk, int, error) {
	rt := currentFederation()
	if rt == nil {
		return humanAttachmentChunk{}, http.StatusBadGateway, errPeerViewOffline
	}
	q := url.Values{"message": {strconv.FormatInt(messageID, 10)}, "attachment": {strconv.FormatInt(attachmentID, 10)},
		"offset": {strconv.FormatInt(offset, 10)}, "length": {strconv.FormatInt(length, 10)}}
	reply, err := exchangePeerView(ctx, rt, peer, fedPeerViewRequest{Method: http.MethodGet, URI: "/api/human-attachment-chunk?" + q.Encode(), Header: http.Header{}})
	if err != nil {
		if errors.Is(err, errPeerViewBusy) {
			return humanAttachmentChunk{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, errPeerViewTimeout) {
			return humanAttachmentChunk{}, http.StatusGatewayTimeout, err
		}
		return humanAttachmentChunk{}, http.StatusBadGateway, err
	}
	if !validPeerViewReply(reply) {
		return humanAttachmentChunk{}, http.StatusBadGateway, errors.New("peer returned an invalid response")
	}
	if reply.Status != http.StatusOK {
		var e struct{ Error string }
		_ = json.Unmarshal(reply.Body, &e)
		return humanAttachmentChunk{}, reply.Status, fmt.Errorf("peer refused the attachment: %s", e.Error)
	}
	var c humanAttachmentChunk
	if err := json.Unmarshal(reply.Body, &c); err != nil || c.Size < 0 || int64(len(c.Data)) > c.Size-c.Offset {
		return humanAttachmentChunk{}, http.StatusBadGateway, errors.New("peer returned an invalid attachment chunk")
	}
	return c, http.StatusOK, nil
}

func handleDashboardPeerAttachment(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	messageID, err1 := strconv.ParseInt(r.PathValue("message"), 10, 64)
	attachmentID, err2 := strconv.ParseInt(r.PathValue("attachment"), 10, 64)
	if err1 != nil || err2 != nil || messageID <= 0 || attachmentID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "message and attachment must be positive IDs")
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
	// Each chunk gets the peer view's own timeout; a whole large file may take
	// longer than one exchange.
	fetch := func(offset, length int64) (humanAttachmentChunk, int, error) {
		ctx, cancel := context.WithTimeout(r.Context(), fedPeerViewTimeout)
		defer cancel()
		return fetchPeerAttachmentChunk(ctx, peer, messageID, attachmentID, offset, length)
	}
	// The first exchange carries only the name, type and size: a HEAD or a
	// Range request then fetches just the chunks it reads.
	first, status, err := fetch(0, 0)
	if err != nil {
		writeError(w, status, "peer_attachment", err.Error())
		return
	}
	reader := &peerAttachmentReader{first: first, fetch: func(offset int64) (humanAttachmentChunk, error) {
		c, _, err := fetch(offset, peerAttachmentChunk)
		return c, err
	}}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": first.Filename}))
	w.Header().Set("Content-Type", peerAttachmentType(first.ContentType))
	http.ServeContent(w, r, "", time.Time{}, reader)
}
