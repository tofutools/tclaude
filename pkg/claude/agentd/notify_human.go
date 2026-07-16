package agentd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// humanMsgNotify is the OS-notification seam for notify-human: a desktop
// banner companion to the dashboard Messages tab. Production routes it
// through notify.SendHumanMessage (which self-gates on config and no-ops
// when disabled); flow tests swap in a recorder via
// SetHumanMessageNotifierForTest. The handler fires it through
// goBackground so a slow platform send (WSL spawns PowerShell) never
// blocks the request.
var humanMsgNotify = notify.SendHumanMessage

// notifyHumanRequest is the POST /v1/notify-human body.
type notifyHumanRequest struct {
	Body    string `json:"body"`
	Subject string `json:"subject"`
}

type notifyHumanAttachmentMetadata struct {
	Body    string `json:"body"`
	Subject string `json:"subject"`
	Name    string `json:"name"`
}

// Size caps for a human notification. A notification is a short
// message, not a document — bounding it keeps one looping or
// misbehaving sender from bloating the human_messages table and the
// /api/snapshot payload (every message ships in every 2s snapshot).
const (
	maxNotifyHumanBodyLen    = 16 * 1024
	maxNotifyHumanSubjectLen = 256
)

// maxNotifyHumanRequestBytes bounds the raw POST body the daemon will
// buffer for /v1/notify-human, enforced by http.MaxBytesReader *before*
// the JSON decode. maxNotifyHumanBodyLen / maxNotifyHumanSubjectLen cap
// the *decoded* strings; this caps the *wire* bytes — so a malicious
// local agent cannot stream a multi-GB body into daemon memory before
// the decoded-length check ever runs (the actual DoS the size caps
// imply they address).
//
// JSON escaping inflates content — `"` and `\` double, and control or
// HTML-significant chars expand to a 6-byte \uXXXX — so the wire cap is
// the decoded caps times 6 plus headroom for the JSON envelope. That is
// loose enough that no legitimate body (even a maximally-escaped one) is
// rejected pre-decode, yet still orders of magnitude below the multi-GB
// range that is the real concern.
const maxNotifyHumanRequestBytes = 6*(maxNotifyHumanBodyLen+maxNotifyHumanSubjectLen) + 1024

const maxNotifyHumanAttachmentBytes = 256 << 20

const maxNotifyHumanAttachmentContentTypeBytes = 256

var (
	humanMessageAttachmentsMu             sync.Mutex
	humanMessageAttachmentCleanupInterval       = 10 * time.Minute
	humanMessageAttachmentUploadTimeout         = 5 * time.Minute
	humanMessageAttachmentUploadSlot            = make(chan struct{}, 1)
	errHumanMessageAttachmentQuota              = errors.New("human message attachment storage quota exceeded")
	maxHumanMessageAttachmentSenderBytes  int64 = 512 << 20 // 512 MiB per stable agent
	maxHumanMessageAttachmentTotalBytes   int64 = 2 << 30   // 2 GiB daemon-wide
	maxHumanMessageAttachmentSenderCount        = 100
	maxHumanMessageAttachmentTotalCount         = 1000
)

// humanMessageAttachmentStartUploadTimer is a test seam around the upload's
// real timeout. The returned function preserves time.Timer.Stop semantics at
// the only level the handler needs: best-effort cancellation after io.Copy.
var humanMessageAttachmentStartUploadTimer = func(timeout time.Duration, onTimeout func()) func() {
	timer := time.AfterFunc(timeout, onTimeout)
	return func() { _ = timer.Stop() }
}

// handleNotifyHuman serves POST /v1/notify-human — the daemon side of
// `tclaude agent notify-human`. It gates via requireNotifyHumanPermission,
// then persists the message to the human_messages table, where the
// dashboard Messages tab surfaces it.
//
// from_title / group_name are snapshotted at insert (notifyHumanCaller*)
// so a later rename or deletion of the sending agent cannot blank an
// already-delivered message.
func handleNotifyHuman(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	callerConv, ok := requireNotifyHumanPermission(w, r)
	if !ok {
		return
	}
	// Cap the buffered request body before decoding — see
	// maxNotifyHumanRequestBytes. An over-cap body fails the Decode below
	// with http.MaxBytesReader's error, handled as a 400 like any other
	// malformed request.
	r.Body = http.MaxBytesReader(w, r.Body, maxNotifyHumanRequestBytes)
	var body notifyHumanRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	body.Subject = strings.TrimSpace(body.Subject)
	if body.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required")
		return
	}
	if len(body.Body) > maxNotifyHumanBodyLen {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("body too long: %d bytes, max %d", len(body.Body), maxNotifyHumanBodyLen))
		return
	}
	if len(body.Subject) > maxNotifyHumanSubjectLen {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("subject too long: %d bytes, max %d", len(body.Subject), maxNotifyHumanSubjectLen))
		return
	}
	id, err := recordHumanMessage(callerConv, body.Subject, body.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"failed to record message: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "delivered": true})
}

// handleNotifyHumanAttachment receives one binary artifact plus base64url JSON
// metadata in X-Tclaude-Notify-Metadata. Keeping metadata out of the binary body
// lets the daemon stream/cap the file and avoids exposing an agent filesystem
// path to the dashboard. Multiple agent paths arrive as one CLI-built zip.
func handleNotifyHumanAttachment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	callerConv, ok := requireNotifyHumanPermission(w, r)
	if !ok {
		return
	}
	rawMeta, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Tclaude-Notify-Metadata"))
	if err != nil || len(rawMeta) > maxNotifyHumanRequestBytes {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid attachment metadata")
		return
	}
	var meta notifyHumanAttachmentMetadata
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid attachment metadata: "+err.Error())
		return
	}
	meta.Body = strings.TrimSpace(meta.Body)
	meta.Subject = strings.TrimSpace(meta.Subject)
	meta.Name = sanitizeExportFilename(meta.Name)
	if meta.Body == "" || len(meta.Body) > maxNotifyHumanBodyLen {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required and must fit the notification limit")
		return
	}
	if len(meta.Subject) > maxNotifyHumanSubjectLen {
		writeError(w, http.StatusBadRequest, "invalid_arg", "subject is too long")
		return
	}
	contentType, err := normalizeHumanMessageAttachmentContentType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	select {
	case humanMessageAttachmentUploadSlot <- struct{}{}:
		defer func() { <-humanMessageAttachmentUploadSlot }()
	case <-r.Context().Done():
		writeError(w, http.StatusRequestTimeout, "cancelled", "attachment upload cancelled")
		return
	}
	if r.ContentLength > maxNotifyHumanAttachmentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "attachment exceeds the 256 MiB limit")
		return
	}
	agentID, _ := db.AgentIDForConv(callerConv)
	humanMessageAttachmentsMu.Lock()
	err = checkHumanMessageAttachmentQuota(agentID, callerConv, max(r.ContentLength, 0))
	humanMessageAttachmentsMu.Unlock()
	if err != nil {
		writeHumanMessageAttachmentQuotaError(w, err)
		return
	}
	incoming := humanMessageAttachmentsIncomingDir()
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create attachment directory: "+err.Error())
		return
	}
	f, err := os.CreateTemp(incoming, "artifact-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create attachment file: "+err.Error())
		return
	}
	path := f.Name()
	var written int64
	var timedOut atomic.Bool
	stopUploadTimer := humanMessageAttachmentStartUploadTimer(humanMessageAttachmentUploadTimeout, func() {
		timedOut.Store(true)
		_ = r.Body.Close()
	})
	if err = f.Chmod(0o600); err == nil {
		written, err = io.Copy(f, http.MaxBytesReader(w, r.Body, maxNotifyHumanAttachmentBytes))
		if err == nil {
			err = f.Sync()
		}
	}
	stopUploadTimer()
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		if timedOut.Load() {
			writeError(w, http.StatusRequestTimeout, "timeout", "attachment upload timed out")
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "attachment exceeds the 256 MiB limit")
			return
		}
		writeError(w, http.StatusInternalServerError, "io", "store attachment: "+err.Error())
		return
	}
	// Best-effort on platforms/filesystems that permit directory fsync. File
	// sync above is mandatory; reconciliation below repairs a missing/truncated
	// referenced file after a system crash if directory durability lagged.
	_ = syncDirectory(incoming)
	humanMessageAttachmentsMu.Lock()
	defer humanMessageAttachmentsMu.Unlock()
	if err := checkHumanMessageAttachmentQuota(agentID, callerConv, written); err != nil {
		_ = os.Remove(path)
		writeHumanMessageAttachmentQuotaError(w, err)
		return
	}
	message, fromTitle, groupName := newHumanMessageRow(callerConv, meta.Subject, meta.Body, "", "", "")
	id, err := db.InsertHumanMessageWithAttachment(message, &db.HumanMessageAttachment{
		Filename: meta.Name, ContentType: contentType,
		SizeBytes: written, StoragePath: path,
	})
	if err != nil {
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, "io", "record attachment: "+err.Error())
		return
	}
	dispatchHumanMessageNotification(callerConv, fromTitle, groupName, meta.Subject, meta.Body)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "delivered": true, "attachment": meta.Name})
}

func humanMessageAttachmentsBaseDir() string {
	return filepath.Join(config.DataDir(), "human-message-files")
}

func humanMessageAttachmentsIncomingDir() string {
	return filepath.Join(humanMessageAttachmentsBaseDir(), ".incoming")
}

func normalizeHumanMessageAttachmentContentType(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "application/octet-stream"
	}
	if len(raw) > maxNotifyHumanAttachmentContentTypeBytes {
		return "", fmt.Errorf("content type too long (max %d bytes)", maxNotifyHumanAttachmentContentTypeBytes)
	}
	mediaType, params, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", fmt.Errorf("invalid content type: %w", err)
	}
	canonical := mime.FormatMediaType(mediaType, params)
	if canonical == "" || len(canonical) > maxNotifyHumanAttachmentContentTypeBytes {
		return "", fmt.Errorf("invalid content type")
	}
	return canonical, nil
}

func checkHumanMessageAttachmentQuota(agentID, convID string, incoming int64) error {
	totalBytes, senderBytes, totalCount, senderCount, err := db.HumanMessageAttachmentUsage(agentID, convID)
	if err != nil {
		return fmt.Errorf("check attachment quota: %w", err)
	}
	if totalCount >= maxHumanMessageAttachmentTotalCount || senderCount >= maxHumanMessageAttachmentSenderCount ||
		quotaWouldExceed(totalBytes, incoming, maxHumanMessageAttachmentTotalBytes) ||
		quotaWouldExceed(senderBytes, incoming, maxHumanMessageAttachmentSenderBytes) {
		return errHumanMessageAttachmentQuota
	}
	return nil
}

func writeHumanMessageAttachmentQuotaError(w http.ResponseWriter, err error) {
	if errors.Is(err, errHumanMessageAttachmentQuota) {
		writeError(w, http.StatusRequestEntityTooLarge, "quota",
			"attachment storage quota is full; delete older message attachments first")
		return
	}
	writeError(w, http.StatusInternalServerError, "io", err.Error())
}

func quotaWouldExceed(current, incoming, limit int64) bool {
	return incoming < 0 || current > limit-incoming
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func removeHumanMessageAttachmentPaths(paths []string) {
	humanMessageAttachmentsMu.Lock()
	defer humanMessageAttachmentsMu.Unlock()
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("human message attachment: remove failed; reconciler will retry",
				"path", path, "error", err)
		}
	}
}

// startHumanMessageAttachmentCleanup reconciles private files against the DB
// immediately at daemon startup and periodically thereafter. It recovers both
// sides of the filesystem/SQLite crash boundary: an upload that wrote bytes but
// never committed metadata, and a deletion whose post-commit Remove failed.
func startHumanMessageAttachmentCleanup(stop <-chan struct{}) {
	go func() {
		runHumanMessageAttachmentCleanup()
		ticker := time.NewTicker(humanMessageAttachmentCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runHumanMessageAttachmentCleanup()
			}
		}
	}()
}

func runHumanMessageAttachmentCleanup() {
	humanMessageAttachmentsMu.Lock()
	defer humanMessageAttachmentsMu.Unlock()
	attachments, err := db.ListHumanMessageAttachments()
	if err != nil {
		slog.Warn("human message attachment: reconciliation list failed", "error", err)
		return // fail closed: never delete files without an authoritative mark set
	}
	referenced := make(map[string]struct{}, len(attachments))
	for _, attachment := range attachments {
		path := filepath.Clean(attachment.StoragePath)
		info, statErr := os.Stat(path)
		valid := statErr == nil && info.Mode().IsRegular() && info.Size() == attachment.SizeBytes
		if valid {
			referenced[path] = struct{}{}
			continue
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			slog.Warn("human message attachment: validate referenced file failed",
				"message", attachment.MessageID, "path", path, "error", statErr)
			referenced[path] = struct{}{} // transient error: fail closed
			continue
		}
		if err := db.DeleteHumanMessageAttachment(attachment.MessageID); err != nil {
			slog.Warn("human message attachment: drop broken metadata failed",
				"message", attachment.MessageID, "path", path, "error", err)
			referenced[path] = struct{}{}
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("human message attachment: remove broken file failed", "path", path, "error", err)
		}
	}
	base := humanMessageAttachmentsBaseDir()
	var dirs []string
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != base {
				dirs = append(dirs, path)
			}
			return nil
		}
		if _, ok := referenced[filepath.Clean(path)]; ok {
			return nil
		}
		if filepath.Clean(filepath.Dir(path)) == filepath.Clean(humanMessageAttachmentsIncomingDir()) {
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) < 2*humanMessageAttachmentUploadTimeout {
				return nil // an upload may still be streaming; stale crash remnants age out
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("human message attachment: remove orphan failed", "path", path, "error", err)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("human message attachment: reconciliation walk failed", "error", err)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // succeeds only when the legacy directory is empty
	}
}

// recordHumanMessage is the daemon-internal half of notify-human. Process
// obligations and escalations use it without inventing an authorization
// bypass: the permission gate remains exclusively at the external HTTP
// boundary, while trusted daemon code shares the same persistence and desktop
// notification path.
func recordHumanMessage(fromConv, subject, body string) (int64, error) {
	return recordHumanMessageWithProcess(fromConv, subject, body, "", "", "")
}

func recordHumanMessageWithProcess(fromConv, subject, body, runID, nodeID, commandID string) (int64, error) {
	id, fromTitle, groupName, err := insertHumanMessageRow(fromConv, subject, body, runID, nodeID, commandID)
	if err != nil {
		return 0, err
	}
	dispatchHumanMessageNotification(fromConv, fromTitle, groupName, subject, body)
	return id, nil
}

func insertHumanMessageRow(fromConv, subject, body, runID, nodeID, commandID string) (int64, string, string, error) {
	m, fromTitle, groupName := newHumanMessageRow(fromConv, subject, body, runID, nodeID, commandID)
	id, err := db.InsertHumanMessage(m)
	if err != nil {
		return 0, "", "", err
	}
	return id, fromTitle, groupName, nil
}

func newHumanMessageRow(fromConv, subject, body, runID, nodeID, commandID string) (*db.HumanMessage, string, string) {
	fromTitle := notifyHumanCallerTitle(fromConv)
	groupName := notifyHumanCallerGroup(fromConv)
	return &db.HumanMessage{
		FromConv:         fromConv,
		FromTitle:        fromTitle,
		GroupName:        groupName,
		Subject:          subject,
		Body:             body,
		CreatedAt:        time.Now(),
		ProcessRunID:     runID,
		ProcessNodeID:    nodeID,
		ProcessCommandID: commandID,
	}, fromTitle, groupName
}

func dispatchHumanMessageNotification(fromConv, fromTitle, groupName, subject, body string) {
	// Also raise a desktop notification (off the request goroutine — a
	// platform send can spawn a subprocess). Self-gates on config, so this
	// is a no-op unless the human opted in. The per-agent / per-group
	// notification filters apply here too: a muted sender's ping still
	// lands in the Messages tab (with the unread badge), it just skips
	// the OS banner. Checked outside the seam so flow tests observe it.
	senderSession := notifyHumanSenderSessionID(fromConv)
	goBackground(func() {
		if fromConv != "" && !notify.AllowedForConv(fromConv) {
			return
		}
		humanMsgNotify(senderSession, fromTitle, groupName, subject, body)
	})
}

// notifyHumanSenderSessionID resolves the caller conv-id to its tclaude
// session ID so the desktop notification can click-to-focus the sending
// agent's terminal — the OS-notification twin of the dashboard's
// per-message Focus button. Empty for the human path (callerConv == "")
// or when the sender has no recorded session; the notification still
// fires, just non-clickable.
func notifyHumanSenderSessionID(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	if row, err := db.FindSessionByConvID(callerConv); err == nil && row != nil {
		return row.ID
	}
	return ""
}

// requireNotifyHumanPermission gates POST /v1/notify-human. The caller
// passes if ANY of:
//
//   - they are the human, or hold the human.notify slug (config default
//     / per-conv grant / sudo), or clear the X-Tclaude-Ask-Human popup;
//   - they own at least one group — a group owner is a trusted
//     coordinating role and gets human.notify by default, slug or not.
//
// The owner default is realised as a structural bypass at the
// permUndecided level (via requirePermissionEx), so the universal
// precedence holds: a permAllow grant passes, and an explicit deny
// override is authoritative and suppresses the owner default too — deny
// always wins, the same as every other gate. Returns (callerConvID, ok);
// callerConvID is "" for the human path. On failure the response is
// already written.
func requireNotifyHumanPermission(w http.ResponseWriter, r *http.Request) (string, bool) {
	return requirePermissionEx(w, r, PermHumanNotify, func(convID string) bool {
		owned, err := db.ListGroupsOwnedBy(convID)
		return err == nil && len(owned) > 0
	})
}

// notifyHumanCallerTitle resolves a caller conv-id to its display title
// for the message's sender attribution. Empty for the human path
// (callerConv == "") or when the conv has no resolvable title.
func notifyHumanCallerTitle(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	title := agent.FreshTitle(callerConv)
	if title == agent.UnknownTitle {
		return ""
	}
	return title
}

// notifyHumanCallerGroup returns one group name the caller belongs to,
// for the message's "which project" context. Empty when the caller is
// ungrouped or is the human. When the caller is in several groups the
// first is used — the attribution is a hint, not an audit.
func notifyHumanCallerGroup(callerConv string) string {
	if callerConv == "" {
		return ""
	}
	groups, err := db.ListGroupsForConv(callerConv)
	if err != nil || len(groups) == 0 {
		return ""
	}
	return groups[0].Name
}

// dashboardHumanMessage is the wire shape of one Messages-tab row in the
// dashboard snapshot.
type dashboardHumanMessage struct {
	ID         int64                            `json:"id"`
	FromConv   string                           `json:"from_conv"`
	FromAgent  string                           `json:"from_agent"`
	FromTitle  string                           `json:"from_title"`
	Group      string                           `json:"group"`
	Subject    string                           `json:"subject"`
	Body       string                           `json:"body"`
	CreatedAt  string                           `json:"created_at"`
	Read       bool                             `json:"read"`
	Attachment *dashboardHumanMessageAttachment `json:"attachment,omitempty"`
}

type dashboardHumanMessageAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// buildHumanMessagesSnapshot loads the human_messages rows for the
// dashboard snapshot, newest first, plus the unread count that drives
// the Messages tab badge.
func buildHumanMessagesSnapshot() ([]dashboardHumanMessage, int) {
	rows, err := db.ListHumanMessages()
	if err != nil {
		slog.Warn("dashboard: list human messages failed", "error", err)
		// Empty (not nil) slice so the snapshot serializes [] — the
		// dashboard JS calls .map() on it directly.
		return []dashboardHumanMessage{}, 0
	}
	// A short-lived Codex sender can notify the human before its conversation
	// index exists. Older send paths therefore snapshotted an empty from_title
	// even though the actor's spawn-time pending_name was already durable in
	// SQLite. Resolve only those blank snapshots by stable agent_id so existing
	// messages heal too; a non-empty historical snapshot remains immutable.
	missingTitleAgents := make([]string, 0)
	seenAgents := make(map[string]struct{})
	for _, m := range rows {
		if m.FromTitle == "" && m.FromAgent != "" {
			if _, seen := seenAgents[m.FromAgent]; !seen {
				seenAgents[m.FromAgent] = struct{}{}
				missingTitleAgents = append(missingTitleAgents, m.FromAgent)
			}
		}
	}
	pendingNames, err := db.PendingNamesByAgent(missingTitleAgents)
	if err != nil {
		slog.Warn("dashboard: resolve human message sender names failed", "error", err)
		pendingNames = map[string]string{}
	}

	out := make([]dashboardHumanMessage, 0, len(rows))
	unread := 0
	for _, m := range rows {
		if !m.IsRead() {
			unread++
		}
		fromTitle := m.FromTitle
		if fromTitle == "" {
			fromTitle = pendingNames[m.FromAgent]
		}
		view := dashboardHumanMessage{
			ID:        m.ID,
			FromConv:  m.FromConv,
			FromAgent: m.FromAgent,
			FromTitle: fromTitle,
			Group:     m.GroupName,
			Subject:   m.Subject,
			Body:      m.Body,
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
			Read:      m.IsRead(),
		}
		if m.Attachment != nil {
			view.Attachment = &dashboardHumanMessageAttachment{
				Filename: m.Attachment.Filename, ContentType: m.Attachment.ContentType, SizeBytes: m.Attachment.SizeBytes,
			}
		}
		out = append(out, view)
	}
	return out, unread
}

// handleDashboardHumanMessageAttachment is the cookie-authenticated download
// surface. The browser receives only an attachment URL, never its daemon path.
func handleDashboardHumanMessageAttachment(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/human-messages/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "attachment" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	a, err := db.GetHumanMessageAttachment(id)
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
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Type", a.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("human message attachment: stream failed", "message", id, "error", err)
	}
}

// handleDashboardHumanMessagesRead serves POST /api/human-messages/read
// — sets read-state on one message ({"id": N}, optionally
// {"read": false} to mark it unread) or marks every message read
// ({"all": true}). The "read" field defaults to true when omitted, so
// existing {"id": N} callers keep marking read; {"id": N, "read": false}
// is the reader's "mark unread" opt-out. Cookie-authed (dashboard-only).
func handleDashboardHumanMessagesRead(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// The body is a tiny {"id":N} / {"all":true} envelope; cap it well
	// below anything legitimate so a stray huge POST cannot be buffered.
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var body struct {
		ID   int64 `json:"id"`
		All  bool  `json:"all"`
		Read *bool `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.All {
		// "all" is the "mark all read" control; there's no "mark all
		// unread" affordance, so it ignores the read field.
		n, err := db.MarkAllHumanMessagesRead()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"marked": n})
		return
	}
	if body.ID <= 0 {
		http.Error(w, "id is required (or pass {\"all\":true})", http.StatusBadRequest)
		return
	}
	// read defaults to true when omitted, so {"id": N} keeps marking read;
	// {"id": N, "read": false} marks the message unread.
	read := body.Read == nil || *body.Read
	var err error
	if read {
		_, err = db.MarkHumanMessageRead(body.ID)
	} else {
		_, err = db.MarkHumanMessageUnread(body.ID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": 1})
}

// handleDashboardHumanMessagesClear serves POST /api/human-messages/clear
// — hard-deletes every message that has been marked read (the manual
// "clear read" control). Unread messages survive. Cookie-authed.
func handleDashboardHumanMessagesClear(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	n, paths, err := db.DeleteReadHumanMessagesWithAttachments()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	removeHumanMessageAttachmentPaths(paths)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// handleDashboardHumanMessagesDelete serves POST /api/human-messages/delete
// — hard-deletes one message ({"id": N}) or several ({"ids": [...]}),
// read or unread. The per-message and multi-select delete controls on
// the tab, distinct from the bulk "clear read" sweep. Cookie-authed
// (dashboard-only).
func handleDashboardHumanMessagesDelete(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// A {"id":N} or {"ids":[...]} envelope — cap the body generously
	// above a "select all then delete" list but well below anything that
	// could blow up memory.
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	var body struct {
		ID  int64   `json:"id"`
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.IDs) > 0 {
		n, paths, err := db.DeleteHumanMessagesWithAttachments(body.IDs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		removeHumanMessageAttachmentPaths(paths)
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
		return
	}
	if body.ID <= 0 {
		http.Error(w, "id or ids is required", http.StatusBadRequest)
		return
	}
	n, paths, err := db.DeleteHumanMessagesWithAttachments([]int64{body.ID})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	removeHumanMessageAttachmentPaths(paths)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// replySubjectFor derives the subject of an operator reply from the
// subject of the notification being answered. A notify-human ping often
// carries no subject, so we fall back to a fixed line that still tells
// the agent WHO is speaking; when there is one we prefix "Re: " (bounded
// so a long original can't blow past the inbox subject cap).
func replySubjectFor(orig string) string {
	orig = strings.TrimSpace(orig)
	if orig == "" {
		return "Reply from the human operator"
	}
	// Bound the echoed subject. Truncate on a RUNE boundary — a byte slice
	// could split a multi-byte character and leave invalid UTF-8 in the
	// subject (the original is capped in bytes, so a long unicode subject
	// can reach here).
	const maxRe = 200
	if r := []rune(orig); len(r) > maxRe {
		orig = string(r[:maxRe]) + "…"
	}
	return "Re: " + orig
}

// handleDashboardHumanMessagesReply serves POST /api/human-messages/reply
// — the operator's answer to a `notify-human` ping, sent back to the
// agent that raised it. Body: {"id": N, "body": "..."} where id is the
// human_messages row being replied to.
//
// The reply target is resolved AUTHORITATIVELY from the stored row (the
// browser passes only the message id + text), so a reply can only route
// to the notification's real sender. It is delivered as a sender-less
// operator message — the same universal-inbox transport the dashboard's
// self-reincarnate request uses (FromConv ""). The async dispatcher owns
// readiness and retries; the mail UI renders a sender-less row as the
// human/operator, which is exactly what this is.
//
// The operator asked that a reply be BLOCKED when the agent is offline —
// an offline agent has no live session, and answering a question into the
// void reads as delivered when it isn't. So this gates on a live tmux
// session and rejects (409) when the target is offline; the dashboard
// disables Send and shows the same reason, but the gate is enforced here
// too so a stale snapshot (agent went offline between poll and click)
// still can't slip a reply through. Cookie-authed (dashboard-only).
func handleDashboardHumanMessagesReply(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// The reply body is capped exactly like a notify-human message — same
	// inbox, same reason to bound it. Cap the wire bytes before decode.
	r.Body = http.MaxBytesReader(w, r.Body, maxNotifyHumanRequestBytes)
	var body struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	if body.ID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id is required")
		return
	}
	if body.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is required (the reply text)")
		return
	}
	if len(body.Body) > maxNotifyHumanBodyLen {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("body too long: %d bytes, max %d", len(body.Body), maxNotifyHumanBodyLen))
		return
	}
	orig, err := db.GetHumanMessage(body.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "load message: "+err.Error())
		return
	}
	if orig == nil {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no human message #%d to reply to", body.ID))
		return
	}
	if orig.ProcessCommandID != "" {
		if err := resolveProcessHumanMessage(r.Context(), orig, body.Body); err != nil {
			writeError(w, http.StatusConflict, "process_resolve", err.Error())
			return
		}
		if _, err := db.MarkHumanMessageRead(body.ID); err != nil {
			slog.Warn("reply: mark process obligation message read failed", "id", body.ID, "error", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"resolved": true, "run_id": orig.ProcessRunID, "node_id": orig.ProcessNodeID})
		return
	}
	// Resolve who to reply to. Lead with the stable agent_id (rotation-immune
	// across reincarnation), falling back to the raw from_conv for old rows /
	// a sender that never became an actor. ResolveSelector then walks any
	// succession chain forward, so the reply reaches the live generation.
	selector := orig.FromAgent
	if selector == "" {
		selector = orig.FromConv
	}
	if selector == "" {
		writeError(w, http.StatusConflict, "no_sender", "this notification has no sender to reply to")
		return
	}
	res, _, err := agent.ResolveSelector(selector)
	if err != nil || res == nil || res.ConvID == "" {
		writeError(w, http.StatusConflict, "unresolved", "cannot resolve the sending agent — it may have been deleted")
		return
	}
	target := res.ConvID
	// Online gate — the reply is blocked when the target has no live tmux
	// session (see the doc comment). One tmux ls; a map lookup against it. A
	// failed enumeration fails closed (treated as offline) — but log it, so an
	// actual tmux/exec failure is diagnosable rather than masquerading as a
	// plain "agent offline" 409 the operator can't tell apart.
	aliveSessions, err := session.LiveTmuxSessions()
	if err != nil {
		slog.Warn("reply: enumerate live tmux sessions failed; treating target as offline", "error", err)
	}
	if !isConvOnlineIn(target, aliveSessions) {
		writeError(w, http.StatusConflict, "offline", "the agent is offline — it has no live session to receive a reply")
		return
	}
	// Deliver as a sender-less operator message on the universal inbox.
	// The async worker owns readiness/hold checks, retries, and consumption.
	id, err := queueAgentMessage(&db.AgentMessage{
		GroupID:          0,
		FromConv:         "",
		ToConv:           target,
		Subject:          replySubjectFor(orig.Subject),
		Body:             body.Body,
		ToRecipients:     []string{target},
		OperatorAuthored: true,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "queue reply: "+err.Error())
		return
	}
	// Replying means the operator has handled this notification — mark the
	// original read (idempotent; opening it in the reader usually already
	// did). Best-effort: a failure here must not fail the delivered reply.
	if _, err := db.MarkHumanMessageRead(body.ID); err != nil {
		slog.Warn("reply: mark original human message read failed", "id", body.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message_id": id,
		"conv_id":    target,
		"queued":     true,
		"pending":    queueDepthFor(target, false),
	})
}

func resolveProcessHumanMessage(ctx context.Context, message *db.HumanMessage, reply string) error {
	fields := strings.Fields(strings.TrimSpace(reply))
	if len(fields) == 0 {
		return fmt.Errorf("reply must begin with a verdict")
	}
	verdict := fields[0]
	feedback := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(reply), verdict))
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return err
	}
	schema, err := fs.RunStateSchemaVersion(ctx, message.ProcessRunID)
	if err != nil {
		return err
	}
	if schema == pathv1.CheckpointStateSchemaVersion {
		_, err := processexec.NewExclusiveV7(fs, nil).RecordObservation(ctx, message.ProcessRunID, message.ProcessNodeID, message.ProcessCommandID, processexec.Observation{
			Actor:       state.ActorRef("human:operator"),
			Verdict:     verdict,
			Feedback:    feedback,
			EvidenceRef: fmt.Sprintf("human-message:%d:reply", message.ID),
		})
		return err
	}
	if schema <= 0 || schema > pathv1.LegacyMaxSchemaVersion {
		return fmt.Errorf("unsupported process state schema %d", schema)
	}
	snapshot, err := fs.LoadRun(ctx, message.ProcessRunID)
	if err != nil {
		return err
	}
	command, ok := snapshot.State.OutstandingCommands[message.ProcessCommandID]
	if !ok || command.NodeID != message.ProcessNodeID {
		return fmt.Errorf("process obligation command no longer matches run/node")
	}
	_, err = processexec.New(fs, nil).RecordOutstandingObservation(ctx, message.ProcessRunID, message.ProcessCommandID, processexec.Observation{
		Actor:       state.ActorRef("human:operator"),
		Verdict:     verdict,
		Feedback:    feedback,
		EvidenceRef: fmt.Sprintf("human-message:%d:reply", message.ID),
	})
	return err
}
