package agentd

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
//
//	GET /api/agent-attachment-chunk?message=N&attachment=M&offset=O[&length=L]
//	GET /api/peer-agent-attachments/{node}/{message}/{attachment}
//	    the same pair for agent mail's files, under the same grant (the one
//	    that shows the peer its mailboxes). M is an agent-mail attachment id:
//	    negative for a startup briefing's listed file (briefingFileID).
//
//	GET /api/human-message-export-parts?message=N
//	    the reader's "⤓ download" as JSON: the Markdown the export route would
//	    serve, alone and as a zip's message.md, with the files the zip holds.
//	    Served like the chunk route, under node.messages.read.
//
//	GET /api/peer-message-export/{node}/{message}[?attachments=1]
//	    local-only: a peer's notification as this node's export route serves
//	    its own, the zip assembled here from the peer's parts and chunks.

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

// humanMessageExportParts is a notification's export, split so another node
// can rebuild the .md and .zip downloads: ZipMarkdown links the files in Files.
type humanMessageExportParts struct {
	Basename    string                   `json:"basename"`
	Markdown    string                   `json:"markdown"`
	ZipMarkdown string                   `json:"zip_markdown,omitempty"`
	Files       []humanMessageExportFile `json:"files,omitempty"`
}

type humanMessageExportFile struct {
	AttachmentID int64  `json:"attachment_id"`
	Name         string `json:"name"`
}

func registerDashboardPeerAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/human-attachment-chunk", handleDashboardHumanAttachmentChunk)
	mux.HandleFunc("GET /api/human-message-export-parts", handleDashboardHumanMessageExportParts)
	mux.HandleFunc("GET /api/peer-attachments/{node}/{message}/{attachment}", handleDashboardPeerAttachment)
	mux.HandleFunc("GET /api/agent-attachment-chunk", handleDashboardAgentAttachmentChunk)
	mux.HandleFunc("GET /api/peer-agent-attachments/{node}/{message}/{attachment}", handleDashboardPeerAgentAttachment)
	mux.HandleFunc("GET /api/peer-message-export/{node}/{message}", handleDashboardPeerMessageExport)
}

func handleDashboardHumanMessageExportParts(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	messageID, err := strconv.ParseInt(r.URL.Query().Get("message"), 10, 64)
	if err != nil || messageID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "message is required")
		return
	}
	m, err := db.GetHumanMessage(messageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read message")
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such message")
		return
	}
	sender := humanMessageSenderLabel(m)
	parts := humanMessageExportParts{Basename: humanMessageExportBasename(m), Markdown: humanMessageMarkdown(m, sender, nil)}
	if len(m.Attachments) > 0 {
		// As the local zip does: a file no longer on disk is named in
		// message.md and left out of the archive.
		names := humanMessageExportNames(m.Attachments)
		for i, a := range m.Attachments {
			if _, statErr := os.Stat(a.StoragePath); statErr != nil {
				names[i] = ""
				continue
			}
			parts.Files = append(parts.Files, humanMessageExportFile{AttachmentID: a.ID, Name: names[i]})
		}
		parts.ZipMarkdown = humanMessageMarkdown(m, sender, names)
	}
	writeJSON(w, http.StatusOK, parts)
}

func handleDashboardHumanAttachmentChunk(w http.ResponseWriter, r *http.Request) {
	serveAttachmentChunk(w, r, false, func(messageID, attachmentID int64) (*os.File, *db.HumanMessageAttachment, int, error) {
		a, err := resolveHumanMessageAttachment(messageID, attachmentID)
		if err != nil {
			return nil, nil, http.StatusInternalServerError, errors.New("read attachment")
		}
		if a == nil {
			return nil, nil, http.StatusNotFound, errors.New("no such attachment")
		}
		f, err := os.Open(a.StoragePath)
		if err != nil {
			return nil, nil, http.StatusGone, errors.New("this attachment is no longer available")
		}
		return f, a, http.StatusOK, nil
	})
}

func handleDashboardAgentAttachmentChunk(w http.ResponseWriter, r *http.Request) {
	serveAttachmentChunk(w, r, true, openAgentMessageAttachment)
}

// serveAttachmentChunk answers one chunk request for a file open resolves.
// negativeIDs admits agent mail's briefing-file ids.
func serveAttachmentChunk(w http.ResponseWriter, r *http.Request, negativeIDs bool,
	open func(messageID, attachmentID int64) (*os.File, *db.HumanMessageAttachment, int, error)) {
	if !checkDashboardAuth(w, r) {
		return
	}
	q := r.URL.Query()
	messageID, err1 := strconv.ParseInt(q.Get("message"), 10, 64)
	attachmentID, err2 := strconv.ParseInt(q.Get("attachment"), 10, 64)
	offset, err3 := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || messageID <= 0 || attachmentID == 0 || (attachmentID < 0 && !negativeIDs) || offset < 0 {
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
	f, a, status, err := open(messageID, attachmentID)
	if f == nil {
		writeError(w, status, "attachment", err.Error())
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

// fetchPeerAttachmentChunk asks a peer for one chunk of a notification's file
// over the peer view: up to length bytes from offset (length 0: only the
// file's metadata).
var fetchPeerAttachmentChunk = func(ctx context.Context, peer *db.FederationPeer, messageID, attachmentID, offset, length int64) (humanAttachmentChunk, int, error) {
	return fetchPeerChunk(ctx, peer, "/api/human-attachment-chunk", messageID, attachmentID, offset, length)
}

// fetchPeerAgentAttachmentChunk is fetchPeerAttachmentChunk for agent mail.
var fetchPeerAgentAttachmentChunk = func(ctx context.Context, peer *db.FederationPeer, messageID, attachmentID, offset, length int64) (humanAttachmentChunk, int, error) {
	return fetchPeerChunk(ctx, peer, "/api/agent-attachment-chunk", messageID, attachmentID, offset, length)
}

func fetchPeerChunk(ctx context.Context, peer *db.FederationPeer, route string, messageID, attachmentID, offset, length int64) (humanAttachmentChunk, int, error) {
	q := url.Values{"message": {strconv.FormatInt(messageID, 10)}, "attachment": {strconv.FormatInt(attachmentID, 10)},
		"offset": {strconv.FormatInt(offset, 10)}, "length": {strconv.FormatInt(length, 10)}}
	var c humanAttachmentChunk
	if status, err := peerViewGetJSON(ctx, peer, route+"?"+q.Encode(), &c); err != nil {
		return humanAttachmentChunk{}, status, err
	}
	if c.Size < 0 || int64(len(c.Data)) > c.Size-c.Offset {
		return humanAttachmentChunk{}, http.StatusBadGateway, errors.New("peer returned an invalid attachment chunk")
	}
	return c, http.StatusOK, nil
}

// fetchPeerMessageExportParts asks a peer for a notification's export parts.
var fetchPeerMessageExportParts = func(ctx context.Context, peer *db.FederationPeer, messageID int64) (humanMessageExportParts, int, error) {
	var parts humanMessageExportParts
	status, err := peerViewGetJSON(ctx, peer, "/api/human-message-export-parts?message="+strconv.FormatInt(messageID, 10), &parts)
	return parts, status, err
}

// peerViewGetJSON runs one GET over the peer view and decodes its JSON reply,
// mapping transport failures to the status this node answers with.
func peerViewGetJSON(ctx context.Context, peer *db.FederationPeer, uri string, out any) (int, error) {
	rt := currentFederation()
	if rt == nil {
		return http.StatusBadGateway, errPeerViewOffline
	}
	reply, err := exchangePeerView(ctx, rt, peer, fedPeerViewRequest{Method: http.MethodGet, URI: uri, Header: http.Header{}})
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
	if reply.Status != http.StatusOK {
		var e struct{ Error string }
		_ = json.Unmarshal(reply.Body, &e)
		return reply.Status, fmt.Errorf("peer refused: %s", e.Error)
	}
	if err := json.Unmarshal(reply.Body, out); err != nil {
		return http.StatusBadGateway, errors.New("peer returned an invalid reply")
	}
	return http.StatusOK, nil
}

// peerExportRequest reads the {node} and {message} of a local peer route and
// looks the peer up; ok is false when it has already answered.
func peerExportRequest(w http.ResponseWriter, r *http.Request) (peer *db.FederationPeer, messageID int64, ok bool) {
	messageID, err := strconv.ParseInt(r.PathValue("message"), 10, 64)
	if err != nil || messageID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "message must be a positive ID")
		return nil, 0, false
	}
	peer, err = db.GetFederationPeer(r.PathValue("node"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "peer_unavailable", "could not read peer trust")
		return nil, 0, false
	}
	if peer == nil {
		writeError(w, http.StatusForbidden, "not_trusted", "linked trusted peer required")
		return nil, 0, false
	}
	return peer, messageID, true
}

// handleDashboardPeerMessageExport serves a peer's notification the way
// serveHumanMessageExport serves this node's own: Markdown, or with
// attachments=1 a zip of message.md plus attachments/, read chunk by chunk.
func handleDashboardPeerMessageExport(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	peer, messageID, ok := peerExportRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), fedPeerViewTimeout)
	parts, status, err := fetchPeerMessageExportParts(ctx, peer, messageID)
	cancel()
	if err != nil {
		writeError(w, status, "peer_export", err.Error())
		return
	}
	base := exportSlugUnsafe.ReplaceAllString(strings.ToLower(parts.Basename), "-")
	if base == "" || base == "-" {
		base = fmt.Sprintf("msg-%d", messageID)
	}
	if r.URL.Query().Get("attachments") != "1" || parts.ZipMarkdown == "" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".md"}))
		_, _ = w.Write([]byte(parts.Markdown))
		return
	}
	fetch := func(attachmentID, offset, length int64) (humanAttachmentChunk, error) {
		ctx, cancel := context.WithTimeout(r.Context(), fedPeerViewTimeout)
		defer cancel()
		c, _, err := fetchPeerAttachmentChunk(ctx, peer, messageID, attachmentID, offset, length)
		return c, err
	}
	// Every file's size is known before the first byte goes out, so a file
	// the peer no longer has fails the request rather than a half-sent zip.
	readers := make([]*peerAttachmentReader, len(parts.Files))
	for i, f := range parts.Files {
		first, err := fetch(f.AttachmentID, 0, 0)
		if err != nil {
			writeError(w, http.StatusBadGateway, "peer_export", err.Error())
			return
		}
		readers[i] = &peerAttachmentReader{first: first, fetch: func(offset int64) (humanAttachmentChunk, error) {
			return fetch(f.AttachmentID, offset, peerAttachmentChunk)
		}}
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".zip"}))
	zw := zip.NewWriter(w)
	modified := time.Now()
	if err := writeZipEntry(zw, "message.md", modified, strings.NewReader(parts.ZipMarkdown)); err != nil {
		return
	}
	// The peer named the files, but the archive is unpacked elsewhere: the
	// names are made plain and unique again here, as the local export does.
	named := make([]*db.HumanMessageAttachment, len(parts.Files))
	for i, f := range parts.Files {
		named[i] = &db.HumanMessageAttachment{Filename: f.Name}
	}
	names := humanMessageExportNames(named)
	for i := range parts.Files {
		name := names[i]
		if err := writeZipEntry(zw, "attachments/"+name, modified, readers[i]); err != nil {
			slog.Warn("dashboard: peer message export failed", "peer", peer.InstanceID, "message", messageID, "error", err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		slog.Warn("dashboard: peer message export failed", "peer", peer.InstanceID, "message", messageID, "error", err)
	}
}

func handleDashboardPeerAttachment(w http.ResponseWriter, r *http.Request) {
	servePeerAttachment(w, r, false, func(ctx context.Context, peer *db.FederationPeer, messageID, attachmentID, offset, length int64) (humanAttachmentChunk, int, error) {
		return fetchPeerAttachmentChunk(ctx, peer, messageID, attachmentID, offset, length)
	})
}

func handleDashboardPeerAgentAttachment(w http.ResponseWriter, r *http.Request) {
	servePeerAttachment(w, r, true, func(ctx context.Context, peer *db.FederationPeer, messageID, attachmentID, offset, length int64) (humanAttachmentChunk, int, error) {
		return fetchPeerAgentAttachmentChunk(ctx, peer, messageID, attachmentID, offset, length)
	})
}

// servePeerAttachment serves a trusted peer's file read chunk by chunk.
// negativeIDs admits agent mail's briefing-file ids.
func servePeerAttachment(w http.ResponseWriter, r *http.Request, negativeIDs bool,
	fetchChunk func(ctx context.Context, peer *db.FederationPeer, messageID, attachmentID, offset, length int64) (humanAttachmentChunk, int, error)) {
	if !checkDashboardAuth(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	messageID, err1 := strconv.ParseInt(r.PathValue("message"), 10, 64)
	attachmentID, err2 := strconv.ParseInt(r.PathValue("attachment"), 10, 64)
	if err1 != nil || err2 != nil || messageID <= 0 || attachmentID == 0 || (attachmentID < 0 && !negativeIDs) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "message and attachment must be valid IDs")
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
		return fetchChunk(ctx, peer, messageID, attachmentID, offset, length)
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
