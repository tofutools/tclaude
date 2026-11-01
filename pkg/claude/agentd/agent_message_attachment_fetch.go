package agentd

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// callerSentMessage reports whether callerConv, in any generation, sent m.
func callerSentMessage(callerConv string, m *db.AgentMessage) bool {
	if m.FromConv != "" && m.FromConv == callerConv {
		return true
	}
	a := peerAgentID(callerConv)
	return a != "" && m.FromAgent == a
}

// messageForAttachments returns the message when the caller is its recipient
// or sender, and answers the request otherwise.
func messageForAttachments(w http.ResponseWriter, r *http.Request, idStr string) (*db.AgentMessage, bool) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return nil, false
	}
	caller, _, ok := requireInboxAccess(w, r)
	if !ok {
		return nil, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid id")
		return nil, false
	}
	m, err := db.GetAgentMessage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return nil, false
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no message #%d", id))
		return nil, false
	}
	if !callerIsRecipient(caller, m) && !callerSentMessage(caller, m) {
		writeError(w, http.StatusForbidden, "auth", "message is not addressed to you")
		return nil, false
	}
	return m, true
}

// handleMessageAttachmentList serves GET /v1/messages/{id}/attachments.
func handleMessageAttachmentList(w http.ResponseWriter, r *http.Request, idStr string) {
	m, ok := messageForAttachments(w, r, idStr)
	if !ok {
		return
	}
	attachments, err := db.ListAgentMessageAttachments(m.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": attachments})
}

// handleMessageAttachment serves GET /v1/messages/{id}/attachments/{attachment}.
// Stored files are out of a sandboxed agent's reach, so this is how one gets
// to a file, and only the message's recipient or sender does.
func handleMessageAttachment(w http.ResponseWriter, r *http.Request, messageStr, attachmentStr string) {
	m, ok := messageForAttachments(w, r, messageStr)
	if !ok {
		return
	}
	attachmentID, err := strconv.ParseInt(attachmentStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid id")
		return
	}
	row, err := db.GetAgentMessageAttachment(m.ID, attachmentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such attachment")
		return
	}
	f, err := os.Open(row.StoragePath)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "this attachment is no longer available")
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not_found", "this attachment is no longer available")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// migrateLegacyMessageAttachments moves stored files out of the agent-visible
// tree and repoints their rows. Agents could write there, so only a regular
// file of the recorded size moves: anything else loses its row rather than
// have the daemon serve it. The tree goes once nothing in it is referenced.
func migrateLegacyMessageAttachments() {
	if info, err := os.Lstat(legacyMessageAttachmentsBase); err != nil || !info.IsDir() {
		return
	}
	attachments, err := db.ListAllAgentMessageAttachments()
	if err != nil {
		slog.Warn("message attachment migration: list failed", "error", err)
		return
	}
	stranded := false
	for _, a := range attachments {
		rel, err := filepath.Rel(legacyMessageAttachmentsBase, filepath.Clean(a.StoragePath))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if err := migrateMessageAttachment(a, rel); err != nil {
			stranded = true
			slog.Warn("message attachment migration: file left in place", "path", a.StoragePath, "error", err)
		}
	}
	if !stranded {
		if err := os.RemoveAll(legacyMessageAttachmentsBase); err != nil {
			slog.Warn("message attachment migration: remove old tree failed", "error", err)
		}
	}
}

func migrateMessageAttachment(a db.AgentMessageAttachment, rel string) error {
	info, err := os.Lstat(a.StoragePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != a.SizeBytes {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return db.DeleteAgentMessageAttachment(a.ID)
	}
	dst := filepath.Join(operatorMessageAttachmentsBase, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Rename(a.StoragePath, dst); err != nil {
		return err
	}
	if err := db.UpdateAgentMessageAttachmentPath(a.ID, dst); err != nil {
		return errors.Join(err, os.Rename(dst, a.StoragePath))
	}
	return nil
}
