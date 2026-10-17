package agentd

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A peer's notification file reaches this node's dashboard in chunks over the
// peer view, under node.messages.read, and is served like a local attachment
// (Range included) with a narrowed type. The "peer" here is this same daemon
// answering through its real peer-view handler.
func TestDashboardPeerAttachmentStreamsChunksUnderMessagesRead(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: "forge", Name: "forge", TrustLevel: db.FederationTrustRestricted}))

	// Larger than two chunks, so a full read and a Range crossing a chunk
	// boundary both need several exchanges.
	content := make([]byte, 2*peerAttachmentChunk+12345)
	for i := range content {
		content[i] = byte(i % 251)
	}
	dir := testutil.CanonicalTempDir(t)
	path := filepath.Join(dir, "page.html")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	messageID, err := db.InsertHumanMessageWithAttachments(&db.HumanMessage{FromConv: "conv-a", Subject: "report", Body: "see file"},
		[]*db.HumanMessageAttachment{{Filename: "page.html", ContentType: "text/html", SizeBytes: int64(len(content)), StoragePath: path}})
	require.NoError(t, err)
	attachments, err := db.ListHumanMessageAttachmentsFor(messageID)
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	attachmentID := attachments[0].ID

	exchanges := 0
	previous := fetchPeerAttachmentChunk
	t.Cleanup(func() { fetchPeerAttachmentChunk = previous })
	fetchPeerAttachmentChunk = func(_ context.Context, peer *db.FederationPeer, message, attachment, offset, length int64) (humanAttachmentChunk, int, error) {
		exchanges++
		require.Equal(t, id.ID(), peer.InstanceID)
		uri := "/api/human-attachment-chunk?message=" + strconv.FormatInt(message, 10) + "&attachment=" + strconv.FormatInt(attachment, 10) +
			"&offset=" + strconv.FormatInt(offset, 10) + "&length=" + strconv.FormatInt(length, 10)
		rec := httptest.NewRecorder()
		PeerViewHandler(peer.InstanceID).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, uri, nil))
		require.LessOrEqual(t, rec.Body.Len(), fedPeerViewResponseBodyLimit, "a chunk must fit one peer-view reply")
		if rec.Code != http.StatusOK {
			return humanAttachmentChunk{}, rec.Code, io.ErrUnexpectedEOF
		}
		var c humanAttachmentChunk
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &c))
		return c, http.StatusOK, nil
	}
	mux := http.NewServeMux()
	registerDashboardPeerAttachmentRoutes(mux)
	url := "/api/peer-attachments/" + id.ID() + "/" + strconv.FormatInt(messageID, 10) + "/" + strconv.FormatInt(attachmentID, 10)

	// Without node.messages.read the peer refuses.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, url, ""))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: id.ID(), Slug: PermNodeMessagesRead}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, url, ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, bytes.Equal(content, rec.Body.Bytes()), "the whole file arrives intact")
	require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"), "a peer's HTML is shown as text, never rendered on this origin")
	require.Equal(t, `attachment; filename=page.html`, rec.Header().Get("Content-Disposition"))
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox")

	// A Range across a chunk-size boundary reads from its start: only what it needs.
	exchanges = 0
	req := dashboardRequest(http.MethodGet, url, "")
	req.Header.Set("Range", "bytes="+strconv.Itoa(peerAttachmentChunk-5)+"-"+strconv.Itoa(peerAttachmentChunk+4))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusPartialContent, rec.Code, rec.Body.String())
	require.Equal(t, content[peerAttachmentChunk-5:peerAttachmentChunk+5], rec.Body.Bytes())
	require.Equal(t, 2, exchanges, "metadata, then one chunk read from the range start")

	// A HEAD (the image viewer's preflight) asks only for the metadata.
	exchanges = 0
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodHead, url, ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, strconv.Itoa(len(content)), rec.Header().Get("Content-Length"))
	require.Equal(t, 1, exchanges)

	// An unknown peer is refused before anything is asked of it.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, "/api/peer-attachments/inst_nobody123/1/1", ""))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestPeerAttachmentTypeShowsOnlyInertMedia(t *testing.T) {
	for declared, want := range map[string]string{
		"image/png":              "image/png",
		"video/mp4":              "video/mp4",
		"audio/ogg; codecs=opus": "audio/ogg",
		"text/markdown":          "text/plain; charset=utf-8",
		"text/html":              "text/plain; charset=utf-8",
		"image/svg+xml":          "application/octet-stream",
		"application/javascript": "application/octet-stream",
		"application/pdf":        "application/octet-stream",
		"not a type":             "application/octet-stream",
	} {
		require.Equal(t, want, peerAttachmentType(declared), declared)
	}
}

// The reader's ⤓ download for a peer's notification: the same Markdown the
// peer's own export serves, and a zip assembled here from its files.
func TestDashboardPeerMessageExportMatchesThePeersOwn(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: "forge", Name: "forge", TrustLevel: db.FederationTrustRestricted}))
	dir := testutil.CanonicalTempDir(t)
	path := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello from forge"), 0o600))
	messageID, err := db.InsertHumanMessageWithAttachments(&db.HumanMessage{FromConv: "conv-a", Subject: "Build report", Body: "see file"},
		[]*db.HumanMessageAttachment{{Filename: "notes.txt", ContentType: "text/plain", SizeBytes: 16, StoragePath: path}})
	require.NoError(t, err)

	serve := func(uri string, out any) (int, error) {
		rec := httptest.NewRecorder()
		PeerViewHandler(id.ID()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, uri, nil))
		if rec.Code != http.StatusOK {
			return rec.Code, io.ErrUnexpectedEOF
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
		return http.StatusOK, nil
	}
	prevParts, prevChunk := fetchPeerMessageExportParts, fetchPeerAttachmentChunk
	t.Cleanup(func() { fetchPeerMessageExportParts, fetchPeerAttachmentChunk = prevParts, prevChunk })
	fetchPeerMessageExportParts = func(_ context.Context, peer *db.FederationPeer, message int64) (humanMessageExportParts, int, error) {
		var parts humanMessageExportParts
		status, err := serve("/api/human-message-export-parts?message="+strconv.FormatInt(message, 10), &parts)
		return parts, status, err
	}
	fetchPeerAttachmentChunk = func(_ context.Context, peer *db.FederationPeer, message, attachment, offset, length int64) (humanAttachmentChunk, int, error) {
		var c humanAttachmentChunk
		status, err := serve("/api/human-attachment-chunk?message="+strconv.FormatInt(message, 10)+"&attachment="+strconv.FormatInt(attachment, 10)+
			"&offset="+strconv.FormatInt(offset, 10)+"&length="+strconv.FormatInt(length, 10), &c)
		return c, status, err
	}
	mux := http.NewServeMux()
	registerDashboardPeerAttachmentRoutes(mux)
	url := "/api/peer-message-export/" + id.ID() + "/" + strconv.FormatInt(messageID, 10)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, url, ""))
	require.Equal(t, http.StatusForbidden, rec.Code, "without node.messages.read the peer refuses: "+rec.Body.String())

	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: id.ID(), Slug: PermNodeMessagesRead}))
	local := httptest.NewRecorder()
	serveHumanMessageExport(local, httptest.NewRequest(http.MethodGet, "/api/human-messages/1/export", nil), messageID)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, url, ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, local.Body.String(), rec.Body.String(), "the same Markdown as the peer's own download")
	require.Equal(t, local.Header().Get("Content-Disposition"), rec.Header().Get("Content-Disposition"))

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, url+"?attachments=1", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	entries := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		entries[f.Name] = string(data)
	}
	require.Equal(t, "hello from forge", entries["attachments/notes.txt"])
	require.Contains(t, entries["message.md"], "[notes.txt](<attachments/notes.txt>)")
}
