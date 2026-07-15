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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid message: "+err.Error())
		return
	}
	req.To = strings.TrimSpace(req.To)
	req.Subject = strings.TrimSpace(req.Subject)
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "to is required")
		return
	}
	if len([]rune(req.Subject)) > operatorMessageMaxSubject || len([]byte(req.Body)) > operatorMessageMaxBody {
		writeError(w, http.StatusBadRequest, "too_large", "subject or body is too long")
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
	id, err := db.InsertAgentMessageWithAttachments(&db.AgentMessage{
		FromConv: "", ToConv: target.ConvID, Subject: req.Subject, Body: req.Body,
		ToRecipients: []string{target.ConvID}, OperatorAuthored: true,
	}, attachments)
	if err != nil {
		if durableDir != "" {
			_ = os.RemoveAll(durableDir)
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if req.AttachmentToken != "" {
		_ = os.RemoveAll(filepath.Join(spawnAttachmentsBaseDir(), req.AttachmentToken))
	}
	setAuditTargetLabel(r, agent.TitleFor(target.ConvID))
	enqueueDeliveryForConv(target.ConvID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": id, "queued": true, "pending": queueDepthFor(target.ConvID, false),
		"to": target.ConvID, "attachments": attachments,
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
