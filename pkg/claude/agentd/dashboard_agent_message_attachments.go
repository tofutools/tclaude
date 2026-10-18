package agentd

import (
	"errors"
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
// A briefing path comes from message text, so it counts only in a briefing
// tclaude wrote itself (db.BriefingMessagesBatch: not operator mail, not a
// federation peer's, whatever its subject), and is served only from inside one
// of the daemon's spawn-attachment roots and through an os.Root, which refuses
// a symlink that leads out of it. Anything else stays plain text.

//
// One id names either kind: a stored attachment by its positive row id, a
// briefing file by the negative of its 1-based position among the body's
// listed paths (briefingFileID). A peer granted node.messages.read reads both
// through /api/agent-attachment-chunk (dashboard_peer_attachments.go), the
// same way it reads a notification's files.

func registerDashboardAgentMessageAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent-messages/{message}/attachments/{attachment}", handleDashboardAgentMessageAttachment)
}

func agentMessageAttachmentURL(messageID, attachmentID int64) string {
	return fmt.Sprintf("/api/agent-messages/%d/attachments/%d", messageID, attachmentID)
}

// briefingFileID is the attachment id of the body's index-th listed path.
func briefingFileID(index int) int64 { return -int64(index + 1) }

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
	briefings, err := db.BriefingMessagesBatch(ids)
	if err != nil {
		slog.Warn("mailbox: briefing lookup failed", "rows", len(ids), "error", err)
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
		if !briefings[m.ID] {
			continue
		}
		for i, path := range briefingAttachmentPaths(m.Body) {
			f, a := briefingFile(path)
			if f == nil {
				continue
			}
			_ = f.Close()
			view := dashboardHumanMessageAttachmentView(m.ID, a)
			view.ID = briefingFileID(i)
			view.URL = agentMessageAttachmentURL(m.ID, view.ID)
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

// openAgentMessageAttachment opens one file of an agent message by its
// attachment id (see briefingFileID). status is the HTTP status to answer with
// when f is nil.
func openAgentMessageAttachment(messageID, attachmentID int64) (f *os.File, a *db.HumanMessageAttachment, status int, err error) {
	missing := errors.New("this attachment is no longer available")
	if attachmentID > 0 {
		row, err := db.GetAgentMessageAttachment(messageID, attachmentID)
		if err != nil {
			return nil, nil, http.StatusInternalServerError, errors.New("read attachment")
		}
		if row == nil {
			return nil, nil, http.StatusNotFound, errors.New("no such attachment")
		}
		f, err := os.Open(row.StoragePath)
		if err != nil {
			return nil, nil, http.StatusGone, missing
		}
		return f, &db.HumanMessageAttachment{
			ID: row.ID, MessageID: messageID, Filename: row.Filename, ContentType: row.ContentType,
			SizeBytes: row.SizeBytes, StoragePath: row.StoragePath,
		}, http.StatusOK, nil
	}
	m, err := db.GetAgentMessage(messageID)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, errors.New("read message")
	}
	briefing, err := db.BriefingMessagesBatch([]int64{messageID})
	if err != nil {
		return nil, nil, http.StatusInternalServerError, errors.New("read message")
	}
	index := -attachmentID - 1
	var paths []string
	if m != nil && briefing[messageID] {
		paths = briefingAttachmentPaths(m.Body)
	}
	if attachmentID == 0 || index >= int64(len(paths)) {
		return nil, nil, http.StatusNotFound, errors.New("no such attachment")
	}
	f, a = briefingFile(paths[index])
	if f == nil {
		return nil, nil, http.StatusGone, missing
	}
	return f, a, http.StatusOK, nil
}

func handleDashboardAgentMessageAttachment(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	messageID, err1 := strconv.ParseInt(r.PathValue("message"), 10, 64)
	attachmentID, err2 := strconv.ParseInt(r.PathValue("attachment"), 10, 64)
	if err1 != nil || err2 != nil || messageID <= 0 {
		http.NotFound(w, r)
		return
	}
	f, a, status, err := openAgentMessageAttachment(messageID, attachmentID)
	if f == nil {
		writeError(w, status, "attachment", err.Error())
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
