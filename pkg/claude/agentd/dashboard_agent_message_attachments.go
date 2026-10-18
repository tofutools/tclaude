package agentd

import (
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// Agent-mail attachments in the dashboard's Messages reader. Two kinds of file
// ride along with an agent message:
//
//   - stored attachments (agent_message_attachments): operator mail sent from
//     the dashboard, federated mail, shared profiles. The daemon copied the
//     bytes and owns the row.
//   - briefing files: a spawn's uploaded screenshots and files, which the
//     startup briefing lists by absolute path in its "Attached files" section
//     (startupctx.AttachmentsSection) for the agent to Read. There is no row,
//     only the path in the body.
//
// Both surface in the mailbox API in the human-notification attachment shape,
// so the reader renders them with the same preview, viewer and download
// components. The body text is left as it is: the paths stay readable, the
// files just become usable too.
//
// A briefing path comes from message text, and an agent can write any text
// into a message it sends, so a path is served only from inside one of the
// daemon's spawn-attachment roots and through an os.Root, which refuses a
// symlink that leads out of it. Anything else stays plain text.

func registerDashboardAgentMessageAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent-messages/{message}/attachments/{attachment}", handleDashboardAgentMessageAttachment)
	mux.HandleFunc("GET /api/agent-messages/{message}/files/{index}", handleDashboardAgentMessageBriefingFile)
}

func agentMessageAttachmentURL(messageID, attachmentID int64) string {
	return fmt.Sprintf("/api/agent-messages/%d/attachments/%d", messageID, attachmentID)
}

func agentMessageBriefingFileURL(messageID int64, index int) string {
	return fmt.Sprintf("/api/agent-messages/%d/files/%d", messageID, index)
}

// agentMessageAttachmentViews builds the reader's attachment list for each
// message on a mailbox page: stored attachments first, then the briefing
// files its body lists. A lookup failure costs the previews, never the page.
func agentMessageAttachmentViews(rows []*db.AgentMessage) map[int64][]*dashboardHumanMessageAttachment {
	out := make(map[int64][]*dashboardHumanMessageAttachment)
	ids := make([]int64, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	stored, err := db.ListAgentMessageAttachmentsBatch(ids)
	if err != nil {
		slog.Warn("mailbox: agent message attachment lookup failed", "rows", len(ids), "error", err)
	}
	for _, m := range rows {
		for _, a := range stored[m.ID] {
			view := dashboardHumanMessageAttachmentView(m.ID, &db.HumanMessageAttachment{
				ID: a.ID, MessageID: m.ID, Filename: a.Filename, ContentType: a.ContentType,
				SizeBytes: a.SizeBytes, StoragePath: a.StoragePath,
			})
			view.URL = agentMessageAttachmentURL(m.ID, a.ID)
			out[m.ID] = append(out[m.ID], view)
		}
		for i, path := range briefingAttachmentPaths(m.Body) {
			f, a := briefingFile(path)
			if f == nil {
				continue
			}
			_ = f.Close()
			view := dashboardHumanMessageAttachmentView(m.ID, a)
			// Briefing files have no row id. A negative position keeps the
			// frontend's per-file element ids unique without colliding with a
			// row id, and reads as "no peer route" to withPeerAttachments.
			view.ID = int64(-(i + 1))
			view.URL = agentMessageBriefingFileURL(m.ID, i)
			out[m.ID] = append(out[m.ID], view)
		}
	}
	return out
}

// briefingAttachmentPaths returns the absolute paths a message body lists in
// an "Attached files:" section, in order. Index i in the result is the
// briefing-file route's {index}, so listing and serving parse the same way.
func briefingAttachmentPaths(body string) []string {
	var paths []string
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "Attached files:" {
			continue
		}
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		for ; j < len(lines); j++ {
			item, ok := strings.CutPrefix(strings.TrimSpace(lines[j]), "- ")
			if !ok {
				break
			}
			if item = strings.TrimSpace(item); filepath.IsAbs(item) {
				paths = append(paths, item)
			}
		}
		i = j - 1
	}
	return paths
}

// briefingAttachmentRoots are the daemon-owned directories a spawn's uploaded
// files live in: the shared staging dir, and the private per-session roots a
// tclaude-layer sandbox sees (current and pre-relocation).
func briefingAttachmentRoots() []string {
	return []string{
		spawnAttachmentsBaseDir(),
		tclcommon.SpawnAttachmentsPrivateBase(),
		tclcommon.LegacySpawnAttachmentsPrivateBase(),
	}
}

// openBriefingFile opens path for reading when it is a regular file inside a
// briefing attachment root, resolving it through an os.Root so no symlink can
// lead outside that root. ok is false for anything else, including a file the
// stale-batch sweep has since removed.
func openBriefingFile(path string) (f *os.File, ok bool) {
	clean := filepath.Clean(path)
	for _, base := range briefingAttachmentRoots() {
		rel, err := filepath.Rel(base, clean)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			continue
		}
		f, err := root.Open(rel)
		_ = root.Close()
		if err != nil {
			return nil, false
		}
		if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, false
		}
		return f, true
	}
	return nil, false
}

// briefingFile opens a servable briefing file and describes it in the
// attachment shape the preview verdicts take, or returns nil when it is not
// servable. The caller closes the file.
func briefingFile(path string) (*os.File, *db.HumanMessageAttachment) {
	f, ok := openBriefingFile(path)
	if !ok {
		return nil, nil
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil
	}
	name := filepath.Base(path)
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return f, &db.HumanMessageAttachment{
		Filename: name, ContentType: contentType, SizeBytes: info.Size(), StoragePath: filepath.Clean(path),
	}
}

func handleDashboardAgentMessageAttachment(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	messageID, err1 := strconv.ParseInt(r.PathValue("message"), 10, 64)
	attachmentID, err2 := strconv.ParseInt(r.PathValue("attachment"), 10, 64)
	if err1 != nil || err2 != nil || messageID <= 0 || attachmentID <= 0 {
		http.NotFound(w, r)
		return
	}
	a, err := db.GetAgentMessageAttachment(messageID, attachmentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if a == nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(a.StoragePath)
	if err != nil {
		writeError(w, http.StatusGone, "missing", "this attachment is no longer available")
		return
	}
	defer func() { _ = f.Close() }()
	serveMessageAttachmentFile(w, r, f, &db.HumanMessageAttachment{
		Filename: a.Filename, ContentType: a.ContentType, SizeBytes: a.SizeBytes, StoragePath: a.StoragePath,
	})
}

func handleDashboardAgentMessageBriefingFile(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	messageID, err1 := strconv.ParseInt(r.PathValue("message"), 10, 64)
	index, err2 := strconv.Atoi(r.PathValue("index"))
	if err1 != nil || err2 != nil || messageID <= 0 || index < 0 {
		http.NotFound(w, r)
		return
	}
	m, err := db.GetAgentMessage(messageID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if m == nil {
		http.NotFound(w, r)
		return
	}
	paths := briefingAttachmentPaths(m.Body)
	if index >= len(paths) {
		http.NotFound(w, r)
		return
	}
	f, a := briefingFile(paths[index])
	if f == nil {
		writeError(w, http.StatusGone, "missing", "this attachment is no longer available")
		return
	}
	defer func() { _ = f.Close() }()
	serveMessageAttachmentFile(w, r, f, a)
}

// serveMessageAttachmentFile writes one attachment as a download, with the
// same headers as a human notification's file: a confirmed video goes out as
// its sniffed container type so the player gets it, and nosniff keeps the
// browser from second-guessing the declared type.
func serveMessageAttachmentFile(w http.ResponseWriter, r *http.Request, f *os.File, a *db.HumanMessageAttachment) {
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename}))
	contentType := a.ContentType
	if videoType := humanMessageAttachmentVideoType(a); videoType != "" {
		contentType = videoType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// ServeContent answers Range requests (and HEAD) for the video player.
	http.ServeContent(w, r, "", time.Time{}, f)
}
