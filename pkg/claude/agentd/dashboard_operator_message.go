package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	operatorMessageMaxBody    = 16 << 10
	operatorMessageMaxSubject = 256
)

var operatorMessageAttachmentsBase = filepath.Join(config.APIDir(), "message-files")

func registerDashboardOperatorMessageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/operator-message", handleDashboardOperatorMessage)
}

// handleDashboardOperatorMessage persists human-authored mail with an empty
// FromConv. Unlike /api/message it does not impersonate an agent and never
// types the body into a pane: the normal durable nudge queue owns delivery.
func handleDashboardOperatorMessage(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, operatorMessageMaxBody+4096)
	var req struct {
		To              string `json:"to"`
		Subject         string `json:"subject"`
		Body            string `json:"body"`
		AttachmentToken string `json:"attachment_token"`
		AllLive         bool   `json:"all_live"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid message: "+err.Error())
		return
	}
	req.To = strings.TrimSpace(req.To)
	req.Subject = strings.TrimSpace(req.Subject)
	if req.To == "" && !req.AllLive {
		writeError(w, http.StatusBadRequest, "invalid_arg", "to is required")
		return
	}
	if len([]rune(req.Subject)) > operatorMessageMaxSubject || len([]byte(req.Body)) > operatorMessageMaxBody {
		writeError(w, http.StatusBadRequest, "too_large", "subject or body is too long")
		return
	}
	if req.AllLive {
		if req.To != "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "to must be empty for an all-live announcement")
			return
		}
		if req.AttachmentToken != "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "attachments are not supported for all-live announcements")
			return
		}
		if strings.TrimSpace(req.Body) == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "message body is required")
			return
		}
		handleDashboardLiveAnnouncement(w, r, req.Subject, req.Body)
		return
	}
	target, matches, err := agent.ResolveSelector(req.To)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "target matches multiple conversations", "code": "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
		return
	}
	attachments, durableDir, err := consumeOperatorAttachmentBatch(req.AttachmentToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "attachments", err.Error())
		return
	}
	if strings.TrimSpace(req.Body) == "" && len(attachments) == 0 {
		if durableDir != "" {
			_ = os.RemoveAll(durableDir)
		}
		writeError(w, http.StatusBadRequest, "invalid_arg", "message body or attachment is required")
		return
	}
	id, pending, err := queueRegularAgentMessageWithAttachments(&db.AgentMessage{
		FromConv: "", ToConv: target.ConvID, Subject: req.Subject, Body: req.Body,
		ToRecipients: []string{target.ConvID}, OperatorAuthored: true,
	}, attachments)
	if err != nil {
		if durableDir != "" {
			_ = os.RemoveAll(durableDir)
		}
		if full, ok := agentMessageQueueFull(err); ok {
			writeQueueFull(w, target.ConvID, full)
			return
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if req.AttachmentToken != "" {
		_ = removeDaemonStagedAttachmentBatch(
			filepath.Join(spawnAttachmentsBaseDir(), req.AttachmentToken),
		)
	}
	setAuditTargetLabel(r, agent.TitleFor(target.ConvID))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": id, "queued": true, "pending": pending,
		"to": target.ConvID, "attachments": attachments,
	})
}

// handleDashboardLiveAnnouncement fans one human-authored message out to each
// active agent with a live tmux session at submit time. The roster and tmux
// snapshot are both read here, rather than trusted from the browser's latest
// dashboard poll, so an agent that stopped while the composer was open is not
// accidentally queued and an agent that just came online is included.
//
// This is deliberately actor-level (ListActiveAgents), not group-level: an
// agent in several groups receives one copy, and an ungrouped live agent is
// still part of the dashboard-wide announcement.
func handleDashboardLiveAnnouncement(w http.ResponseWriter, r *http.Request, subject, body string) {
	agents, err := db.ListActiveAgents()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list active agents: "+err.Error())
		return
	}
	// Mutations must not use the dashboard poll cache: even its intentionally
	// short TTL could retain a session that stopped just before the operator
	// pressed Announce, leaving durable mail for an already-offline agent.
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "tmux", "list live agents: "+err.Error())
		return
	}

	recipients := make([]recipient, 0, len(agents))
	for _, active := range agents {
		if active == nil || active.CurrentConvID == "" || !isConvOnlineIn(active.CurrentConvID, alive) {
			continue
		}
		convID := active.CurrentConvID
		id, pending, queueErr := queueRegularAgentMessage(&db.AgentMessage{
			FromConv:         "",
			ToConv:           convID,
			Subject:          subject,
			Body:             body,
			ToRecipients:     []string{convID},
			OperatorAuthored: true,
		})
		row := recipient{
			ConvID: convID, AgentID: active.AgentID, Title: agent.TitleFor(convID),
		}
		if queueErr != nil {
			if full, ok := agentMessageQueueFull(queueErr); ok {
				row.Pending = full.Pending
				row.QueueFull = true
				row.Limit = full.Limit
				row.Error = queueFullHint(full.Pending, full.Limit)
			} else {
				row.Error = "failed to queue message: " + queueErr.Error()
			}
		} else {
			row.MessageID = id
			row.Queued = true
			row.Pending = pending
		}
		recipients = append(recipients, row)
	}
	setAuditTargetLabel(r, "all live agents")
	writeJSON(w, http.StatusOK, map[string]any{
		"all_live":   true,
		"recipients": recipients,
	})
}

func validAttachmentToken(token string) bool {
	if token == "" || len(token) > 80 || filepath.Base(token) != token {
		return false
	}
	for _, r := range token {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// consumeOperatorAttachmentBatch copies browser staging files into durable,
// agent-readable storage. Only an opaque daemon-issued batch token is trusted;
// paths supplied by the browser never cross this boundary.
func consumeOperatorAttachmentBatch(token string) ([]db.AgentMessageAttachment, string, error) {
	if token == "" {
		return nil, "", nil
	}
	if !validAttachmentToken(token) {
		return nil, "", fmt.Errorf("invalid attachment token")
	}
	sourceDir := filepath.Join(spawnAttachmentsBaseDir(), token)
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, "", fmt.Errorf("attachment batch is missing or expired")
	}
	if len(entries) == 0 || len(entries) > spawnAttachmentMaxFiles {
		return nil, "", fmt.Errorf("attachment batch has an invalid file count")
	}
	destDir := filepath.Join(operatorMessageAttachmentsBase, convops.GenerateUUID())
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create durable attachment directory: %w", err)
	}
	cleanup := func(err error) ([]db.AgentMessageAttachment, string, error) {
		_ = os.RemoveAll(destDir)
		return nil, "", err
	}
	var total int64
	out := make([]db.AgentMessageAttachment, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > spawnAttachmentMaxFileBytes {
			return cleanup(fmt.Errorf("attachment %q is not a valid regular file", entry.Name()))
		}
		total += info.Size()
		if total > spawnAttachmentMaxTotalBytes {
			return cleanup(fmt.Errorf("attachment batch exceeds size limit"))
		}
		name := sanitizeAttachmentFilename(entry.Name())
		src := filepath.Join(sourceDir, entry.Name())
		dst := filepath.Join(destDir, name)
		if err := copyOperatorAttachment(src, dst); err != nil {
			return cleanup(err)
		}
		ct := mime.TypeByExtension(filepath.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		out = append(out, db.AgentMessageAttachment{
			Filename: name, ContentType: ct, SizeBytes: info.Size(), StoragePath: dst,
		})
	}
	return out, destDir, nil
}

func copyOperatorAttachment(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open attachment: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create attachment: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy attachment: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close attachment: %w", closeErr)
	}
	return nil
}
