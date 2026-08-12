package agentd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
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

// maxNotifyHumanAttachmentsPerMessage bounds how many separate files one
// notification may publish. The CLI packages anything larger as a single
// archive (see notifyHumanAutoZipFileCount), so this is the daemon-side floor
// under that policy — a message stays a message, not a file browser.
const maxNotifyHumanAttachmentsPerMessage = 20

var (
	humanMessageAttachmentsMu             sync.Mutex
	humanMessageAttachmentCleanupInterval       = 10 * time.Minute
	humanMessageAttachmentUploadTimeout         = 5 * time.Minute
	humanMessageAttachmentUploadSlot            = make(chan struct{}, 1)
	errHumanMessageAttachmentQuota              = errors.New("human message attachment storage quota exceeded")
	maxHumanMessageAttachmentSenderBytes  int64 = 512 << 20 // 512 MiB per stable agent
	maxHumanMessageAttachmentTotalBytes   int64 = 2 << 30   // 2 GiB daemon-wide
	// Count quotas guard database rows and filesystem inodes, not bytes. They
	// are per-FILE, and one notification may now publish up to
	// maxNotifyHumanAttachmentsPerMessage of them, so they are sized so that a
	// sender still gets roughly the same number of NOTIFICATIONS as when an
	// attachment was always a single artifact.
	maxHumanMessageAttachmentSenderCount = 1000
	maxHumanMessageAttachmentTotalCount  = 10000
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

// handleNotifyHumanAttachment receives the published artifact(s) plus base64url
// JSON metadata in X-Tclaude-Notify-Metadata. Keeping metadata out of the binary
// body lets the daemon stream/cap the files and avoids exposing an agent
// filesystem path to the dashboard.
//
// Two body shapes are accepted. A raw body is one artifact named by
// metadata.name — the original single-file (and CLI-built zip) upload.
// A multipart/form-data body carries several artifacts, one per file part,
// each keeping its own filename and content type: the CLI sends a small file
// set that way so the dashboard can show — and preview — each file instead of
// one opaque export.zip.
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
	// The body may be empty here — and only here. This route always carries at
	// least one published file, so a subject plus the attachment is already a
	// complete message; the bodiless notification is "here is the artifact,
	// this is what it is". A message with neither is still nothing to read.
	if meta.Body == "" && meta.Subject == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a body or a subject is required")
		return
	}
	if len(meta.Body) > maxNotifyHumanBodyLen {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body is too long")
		return
	}
	if len(meta.Subject) > maxNotifyHumanSubjectLen {
		writeError(w, http.StatusBadRequest, "invalid_arg", "subject is too long")
		return
	}
	multipartBoundary, err := notifyHumanMultipartBoundary(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	var singleContentType string
	if multipartBoundary == "" {
		singleContentType, err = normalizeHumanMessageAttachmentContentType(r.Header.Get("Content-Type"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
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
	err = checkHumanMessageAttachmentQuota(agentID, callerConv, max(r.ContentLength, 0), 1)
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
	// One deadline and one byte budget for the whole upload, however many parts
	// it carries: a multipart body must not buy a sender extra time or bytes.
	var timedOut atomic.Bool
	stopUploadTimer := humanMessageAttachmentStartUploadTimer(humanMessageAttachmentUploadTimeout, func() {
		timedOut.Store(true)
		_ = r.Body.Close()
	})
	capped := http.MaxBytesReader(w, r.Body, maxNotifyHumanAttachmentBytes)
	var attachments []*db.HumanMessageAttachment
	if multipartBoundary == "" {
		attachments, err = receiveSingleNotifyHumanAttachment(capped, incoming, meta.Name, singleContentType)
	} else {
		attachments, err = receiveMultipartNotifyHumanAttachments(capped, multipartBoundary, incoming)
	}
	stopUploadTimer()
	if err != nil {
		removeStoredAttachmentFiles(attachments)
		writeNotifyHumanAttachmentUploadError(w, err, timedOut.Load())
		return
	}
	// Best-effort on platforms/filesystems that permit directory fsync. The
	// per-file sync above is mandatory; reconciliation repairs a missing or
	// truncated referenced file after a system crash if directory durability
	// lagged.
	_ = syncDirectory(incoming)
	var written int64
	for _, a := range attachments {
		written += a.SizeBytes
	}
	humanMessageAttachmentsMu.Lock()
	defer humanMessageAttachmentsMu.Unlock()
	if err := checkHumanMessageAttachmentQuota(agentID, callerConv, written, len(attachments)); err != nil {
		removeStoredAttachmentFiles(attachments)
		writeHumanMessageAttachmentQuotaError(w, err)
		return
	}
	message, fromTitle, groupName := newHumanMessageRow(callerConv, meta.Subject, meta.Body, "", "", "")
	id, err := db.InsertHumanMessageWithAttachments(message, attachments)
	if err != nil {
		removeStoredAttachmentFiles(attachments)
		writeError(w, http.StatusInternalServerError, "io", "record attachment: "+err.Error())
		return
	}
	dispatchHumanMessageNotification(callerConv, fromTitle, groupName, meta.Subject, meta.Body)
	names := make([]string, 0, len(attachments))
	for _, a := range attachments {
		names = append(names, a.Filename)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "delivered": true, "attachment": names[0], "attachments": names,
	})
}

// errBadNotifyHumanUpload marks a malformed upload — the sender's fault, so it
// answers 400 rather than being reported as a daemon failure.
var errBadNotifyHumanUpload = errors.New("malformed attachment upload")

// errTooManyNotifyHumanAttachments is the per-message file-count refusal. The
// CLI zips large sets itself, so hitting this means a sender bypassed that.
var errTooManyNotifyHumanAttachments = fmt.Errorf("%w: a notification carries at most %d files"+
	" — package a larger set as one archive",
	errBadNotifyHumanUpload, maxNotifyHumanAttachmentsPerMessage)

func notifyHumanMultipartBoundary(contentType string) (string, error) {
	if strings.TrimSpace(contentType) == "" {
		return "", nil
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("invalid content type: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return "", nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", fmt.Errorf("multipart upload is missing its boundary")
	}
	return boundary, nil
}

func receiveSingleNotifyHumanAttachment(body io.Reader, incoming, name, contentType string) ([]*db.HumanMessageAttachment, error) {
	a, err := storeNotifyHumanAttachment(body, incoming, name, contentType)
	if a == nil {
		return nil, err
	}
	return []*db.HumanMessageAttachment{a}, err
}

func receiveMultipartNotifyHumanAttachments(body io.Reader, boundary, incoming string) ([]*db.HumanMessageAttachment, error) {
	reader := multipart.NewReader(body, boundary)
	var out []*db.HumanMessageAttachment
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A body the daemon cannot parse is a malformed request; an
			// over-budget one still unwraps to MaxBytesError and stays a 413.
			return out, fmt.Errorf("%w: %w", errBadNotifyHumanUpload, err)
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue // non-file form field: no artifact to store
		}
		if len(out) >= maxNotifyHumanAttachmentsPerMessage {
			_ = part.Close()
			return out, errTooManyNotifyHumanAttachments
		}
		contentType, err := normalizeHumanMessageAttachmentContentType(part.Header.Get("Content-Type"))
		if err != nil {
			_ = part.Close()
			return out, fmt.Errorf("%w: %w", errBadNotifyHumanUpload, err)
		}
		a, storeErr := storeNotifyHumanAttachment(part, incoming, sanitizeExportFilename(part.FileName()), contentType)
		_ = part.Close()
		if a != nil {
			out = append(out, a)
		}
		if storeErr != nil {
			return out, storeErr
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: it carried no files", errBadNotifyHumanUpload)
	}
	return out, nil
}

// storeNotifyHumanAttachment streams one artifact into the daemon's private
// directory and returns its (not yet committed) metadata. A non-nil attachment
// is returned even alongside an error so the caller can clean up the partial
// file it names.
func storeNotifyHumanAttachment(src io.Reader, incoming, name, declaredType string) (*db.HumanMessageAttachment, error) {
	f, err := os.CreateTemp(incoming, "artifact-*")
	if err != nil {
		return nil, fmt.Errorf("create attachment file: %w", err)
	}
	a := &db.HumanMessageAttachment{Filename: name, ContentType: declaredType, StoragePath: f.Name()}
	if err = f.Chmod(0o600); err == nil {
		a.SizeBytes, err = io.Copy(f, src)
		if err == nil {
			err = f.Sync()
		}
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return a, err
}

func removeStoredAttachmentFiles(attachments []*db.HumanMessageAttachment) {
	for _, a := range attachments {
		if a != nil && a.StoragePath != "" {
			_ = os.Remove(a.StoragePath)
		}
	}
}

func writeNotifyHumanAttachmentUploadError(w http.ResponseWriter, err error, timedOut bool) {
	if timedOut {
		writeError(w, http.StatusRequestTimeout, "timeout", "attachment upload timed out")
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "attachment exceeds the 256 MiB limit")
		return
	}
	// A malformed body is the sender's error, not the daemon's — the single-body
	// path already answers 400 for the same mistakes.
	if errors.Is(err, errBadNotifyHumanUpload) {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "io", "store attachment: "+err.Error())
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

// checkHumanMessageAttachmentQuota rejects an upload of incoming bytes spread
// over incomingFiles new rows when it would push the daemon-wide or per-sender
// storage past its limit.
func checkHumanMessageAttachmentQuota(agentID, convID string, incoming int64, incomingFiles int) error {
	totalBytes, senderBytes, totalCount, senderCount, err := db.HumanMessageAttachmentUsage(agentID, convID)
	if err != nil {
		return fmt.Errorf("check attachment quota: %w", err)
	}
	if incomingFiles < 1 {
		incomingFiles = 1
	}
	if totalCount+incomingFiles > maxHumanMessageAttachmentTotalCount ||
		senderCount+incomingFiles > maxHumanMessageAttachmentSenderCount ||
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
		if err := db.DeleteHumanMessageAttachment(attachment.ID); err != nil {
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
	return requirePermission(w, r, PermHumanNotify)
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
	ID        int64  `json:"id"`
	FromConv  string `json:"from_conv"`
	FromAgent string `json:"from_agent"`
	FromTitle string `json:"from_title"`
	Group     string `json:"group"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
	// Attachment is the first published file, kept for surfaces that show a
	// single download card; Attachments carries the full set.
	Attachment  *dashboardHumanMessageAttachment   `json:"attachment,omitempty"`
	Attachments []*dashboardHumanMessageAttachment `json:"attachments,omitempty"`
}

// dashboardHumanMessageAttachment is one downloadable file on a notification.
// URL is the download route for this exact file; Previewable is the daemon's
// verdict (raster image, content-sniff confirmed) on whether the dashboard may
// render it inline instead of only offering a download. Markdown is the
// equivalent verdict for the Markdown document viewer.
type dashboardHumanMessageAttachment struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	URL         string `json:"url"`
	Previewable bool   `json:"previewable,omitempty"`
	Markdown    bool   `json:"markdown,omitempty"`
}

func dashboardHumanMessageAttachmentView(messageID int64, a *db.HumanMessageAttachment) *dashboardHumanMessageAttachment {
	if a == nil {
		return nil
	}
	return &dashboardHumanMessageAttachment{
		ID:          a.ID,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		URL:         humanMessageAttachmentURL(messageID, a.ID),
		Previewable: humanMessageAttachmentPreviewable(a),
		Markdown:    humanMessageAttachmentMarkdown(a),
	}
}

// humanMessageAttachmentURL is the download route for one published file. The
// legacy /attachment route stays as an alias for a message's first file.
func humanMessageAttachmentURL(messageID, attachmentID int64) string {
	return fmt.Sprintf("/api/human-messages/%d/attachments/%d", messageID, attachmentID)
}

// humanMessageAttachmentPreviewable is deliberately decided by the daemon,
// not by the browser-facing Content-Type alone. Agents control that header;
// accepting image/* here would let a non-image payload opt into the preview
// surface. Keep this raster-only until the dashboard has an explicit safe SVG
// policy, and require the bytes to match the declared type as well.
func humanMessageAttachmentPreviewable(a *db.HumanMessageAttachment) bool {
	if a == nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(a.ContentType)
	if err != nil {
		return false
	}
	if _, ok := previewableHumanImageTypes[mediaType]; !ok {
		return false
	}
	cacheKey := humanMessageAttachmentPreviewCacheKey(a.StoragePath, mediaType)
	humanMessageAttachmentPreviewCache.Lock()
	previewable, ok := humanMessageAttachmentPreviewCache.entries[cacheKey]
	humanMessageAttachmentPreviewCache.Unlock()
	if ok {
		return previewable
	}

	// Attachment paths are daemon-owned and immutable after upload. Cache a
	// completed probe so the 2-second dashboard snapshot poll does not reopen
	// every historical image. A cached positive result intentionally survives
	// file cleanup: the preview then reaches the authenticated route and shows
	// its missing-file state instead of tearing the thumbnail out of the UI.
	f, err := os.Open(a.StoragePath)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var header [32]byte
	n, err := io.ReadFull(f, header[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	previewable = humanImageHeaderMatches(mediaType, header[:n])
	humanMessageAttachmentPreviewCache.Lock()
	if len(humanMessageAttachmentPreviewCache.entries) >= maxHumanMessageAttachmentPreviewCacheEntries {
		for key := range humanMessageAttachmentPreviewCache.entries {
			delete(humanMessageAttachmentPreviewCache.entries, key)
			break
		}
	}
	humanMessageAttachmentPreviewCache.entries[cacheKey] = previewable
	humanMessageAttachmentPreviewCache.Unlock()
	return previewable
}

var previewableHumanImageTypes = map[string]struct{}{
	"image/avif": {},
	"image/gif":  {},
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

// humanMessageAttachmentMarkdown is the daemon's verdict on whether the
// dashboard should offer its Markdown document viewer for this file. It is
// decided here for the same reason Previewable is: the browser-facing
// Content-Type comes from the publishing agent, so it is a claim, not evidence.
//
// The bar is lower than an image's, because the viewer treats the file as text
// either way — markdown-it parses with HTML disabled and the dashboard builds
// the document out of allowlisted elements, so a mislabelled payload renders as
// visible characters rather than as markup. What the checks below are for is
// avoiding a useless offer: a binary shown as mojibake, or a file large enough
// to lock the tab up while it renders. Either way the download stays.
func humanMessageAttachmentMarkdown(a *db.HumanMessageAttachment) bool {
	if a == nil || a.SizeBytes > maxMarkdownHumanAttachmentBytes {
		return false
	}
	if !declaresMarkdown(a) {
		return false
	}
	cacheKey := humanMessageAttachmentMarkdownCacheKey(a.StoragePath)
	humanMessageAttachmentPreviewCache.Lock()
	markdown, ok := humanMessageAttachmentPreviewCache.entries[cacheKey]
	humanMessageAttachmentPreviewCache.Unlock()
	if ok {
		return markdown
	}

	// Same caching contract as the image probe: attachment paths are
	// daemon-owned and immutable after upload, so one completed sniff serves
	// every later dashboard poll, and a cached positive survives file cleanup
	// so the viewer reaches the authenticated route and shows its own
	// missing-file state.
	f, err := os.Open(a.StoragePath)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var header [512]byte
	n, err := io.ReadFull(f, header[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	markdown = looksLikeUTF8Text(header[:n], n == len(header))
	humanMessageAttachmentPreviewCache.Lock()
	if len(humanMessageAttachmentPreviewCache.entries) >= maxHumanMessageAttachmentPreviewCacheEntries {
		for key := range humanMessageAttachmentPreviewCache.entries {
			delete(humanMessageAttachmentPreviewCache.entries, key)
			break
		}
	}
	humanMessageAttachmentPreviewCache.entries[cacheKey] = markdown
	humanMessageAttachmentPreviewCache.Unlock()
	return markdown
}

// declaresMarkdown reports whether the attachment presents itself as Markdown,
// by media type or by the extension a human reads in the filename. Either alone
// is enough: `tclaude agent notify-human` types .md as text/markdown, but a file
// published through another path can arrive as text/plain or octet-stream and
// still be the document the operator wants rendered.
func declaresMarkdown(a *db.HumanMessageAttachment) bool {
	if mediaType, _, err := mime.ParseMediaType(a.ContentType); err == nil {
		if _, ok := markdownHumanAttachmentTypes[mediaType]; ok {
			return true
		}
	}
	_, ok := markdownHumanAttachmentExtensions[strings.ToLower(filepath.Ext(a.Filename))]
	return ok
}

// looksLikeUTF8Text rejects the payloads that would render as mojibake: a NUL
// byte is the classic binary tell, and invalid UTF-8 means the browser cannot
// decode the file as the text the viewer would present. truncated says the
// sample stopped at the buffer rather than at end-of-file, in which case a
// final incomplete rune is the sample's fault, not the file's.
func looksLikeUTF8Text(sample []byte, truncated bool) bool {
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	if truncated {
		// Drop a trailing partial rune (at most 3 bytes) before validating.
		for i := 0; i < utf8.UTFMax-1 && len(sample) > 0; i++ {
			if utf8.Valid(sample) {
				return true
			}
			sample = sample[:len(sample)-1]
		}
	}
	return utf8.Valid(sample)
}

// maxMarkdownHumanAttachmentBytes bounds what the viewer will offer to parse
// and lay out in the operator's browser. A Markdown document that exceeds it is
// still downloadable; it is just not worth freezing the tab over.
const maxMarkdownHumanAttachmentBytes = 1 << 20 // 1 MiB

var markdownHumanAttachmentTypes = map[string]struct{}{
	"text/markdown":   {},
	"text/x-markdown": {},
}

var markdownHumanAttachmentExtensions = map[string]struct{}{
	".md":       {},
	".markdown": {},
	".mdown":    {},
	".mkd":      {},
	".mkdn":     {},
}

// humanMessageAttachmentMarkdownCacheKey shares the preview cache with the
// image probe. The discriminator cannot collide with an image key, whose second
// field is always a parsed media type.
func humanMessageAttachmentMarkdownCacheKey(storagePath string) string {
	return storagePath + "\x00#markdown"
}

const maxHumanMessageAttachmentPreviewCacheEntries = 2048

var humanMessageAttachmentPreviewCache = struct {
	sync.Mutex
	entries map[string]bool
}{entries: make(map[string]bool)}

func humanMessageAttachmentPreviewCacheKey(storagePath, mediaType string) string {
	return storagePath + "\x00" + mediaType
}

func humanImageHeaderMatches(mediaType string, header []byte) bool {
	switch mediaType {
	case "image/avif":
		// AVIF is an ISO-BMFF file. The major brand is normally at bytes 8–12;
		// accepting the compatible brand at the same position covers the common
		// avif/avis variants without treating arbitrary ftyp files as images.
		return len(header) >= 12 && bytes.Equal(header[4:8], []byte("ftyp")) &&
			(bytes.Equal(header[8:12], []byte("avif")) || bytes.Equal(header[8:12], []byte("avis")))
	case "image/gif":
		return len(header) >= 6 && (bytes.Equal(header[:6], []byte("GIF87a")) || bytes.Equal(header[:6], []byte("GIF89a")))
	case "image/jpeg":
		return len(header) >= 3 && header[0] == 0xff && header[1] == 0xd8 && header[2] == 0xff
	case "image/png":
		return len(header) >= 8 && bytes.Equal(header[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case "image/webp":
		return len(header) >= 12 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP"))
	default:
		return false
	}
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
	// Heal pre-stable-id rows too. Replaced conversation generations remain
	// mapped to their actor in SQLite, so this restores rotation-stable
	// attribution without rewriting historical notification rows.
	agentByConv := make(map[string]string)
	for _, m := range rows {
		if m.FromAgent == "" && m.FromConv != "" {
			if _, seen := agentByConv[m.FromConv]; !seen {
				agentByConv[m.FromConv], _ = db.AgentIDForConv(m.FromConv)
			}
		}
	}

	missingTitleAgents := make([]string, 0)
	seenAgents := make(map[string]struct{})
	for _, m := range rows {
		fromAgent := m.FromAgent
		if fromAgent == "" {
			fromAgent = agentByConv[m.FromConv]
		}
		if m.FromTitle == "" && fromAgent != "" {
			if _, seen := seenAgents[fromAgent]; !seen {
				seenAgents[fromAgent] = struct{}{}
				missingTitleAgents = append(missingTitleAgents, fromAgent)
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
		fromAgent := m.FromAgent
		if fromAgent == "" {
			fromAgent = agentByConv[m.FromConv]
		}
		if fromTitle == "" {
			fromTitle = pendingNames[fromAgent]
		}
		view := dashboardHumanMessage{
			ID:        m.ID,
			FromConv:  m.FromConv,
			FromAgent: fromAgent,
			FromTitle: fromTitle,
			Group:     m.GroupName,
			Subject:   m.Subject,
			Body:      m.Body,
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
			Read:      m.IsRead(),
		}
		for _, a := range m.Attachments {
			view.Attachments = append(view.Attachments, dashboardHumanMessageAttachmentView(m.ID, a))
		}
		if len(view.Attachments) > 0 {
			view.Attachment = view.Attachments[0]
		}
		out = append(out, view)
	}
	return out, unread
}

// handleDashboardHumanMessageAttachment is the cookie-authenticated download
// surface. The browser receives only an attachment URL, never its daemon path.
//
// Routes:
//
//	/api/human-messages/{id}/attachments/{attachmentID}  one specific file
//	/api/human-messages/{id}/attachment                  the first file (legacy)
func handleDashboardHumanMessageAttachment(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	// HEAD is served like GET, minus the body: the dashboard's image viewer
	// preflights an attachment before deciding to display it.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
		return
	}
	messageID, attachmentID, ok := parseHumanMessageAttachmentPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, err := resolveHumanMessageAttachment(messageID, attachmentID)
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("human message attachment: stream failed", "message", messageID, "error", err)
	}
}

// parseHumanMessageAttachmentPath returns the message id and, for the explicit
// per-file route, the attachment id (0 means "the message's first attachment").
func parseHumanMessageAttachmentPath(path string) (messageID, attachmentID int64, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/human-messages/"), "/")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, false
	}
	messageID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || messageID <= 0 {
		return 0, 0, false
	}
	switch {
	case len(parts) == 2 && parts[1] == "attachment":
		return messageID, 0, true
	case len(parts) == 3 && parts[1] == "attachments":
		attachmentID, err = strconv.ParseInt(parts[2], 10, 64)
		if err != nil || attachmentID <= 0 {
			return 0, 0, false
		}
		return messageID, attachmentID, true
	}
	return 0, 0, false
}

func resolveHumanMessageAttachment(messageID, attachmentID int64) (*db.HumanMessageAttachment, error) {
	if attachmentID > 0 {
		return db.GetHumanMessageAttachment(messageID, attachmentID)
	}
	attachments, err := db.ListHumanMessageAttachmentsFor(messageID)
	if err != nil || len(attachments) == 0 {
		return nil, err
	}
	return attachments[0], nil
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
// carries no subject, so we fall back to a line that identifies both the
// speaker and the notification being answered; when there is one we prefix
// "Re: " (bounded so a long original can't blow past the inbox subject cap).
func replySubjectFor(orig string, originalMessageID int64) string {
	orig = strings.TrimSpace(orig)
	if orig == "" {
		return fmt.Sprintf("Reply from the human operator to original message #%d", originalMessageID)
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
		// The replacement-engine seam is deliberately nil-capable even though
		// its temporary no-engine stub below always returns an error.
		if err := resolveProcessHumanMessage(r.Context(), orig, body.Body); err != nil { //nolint:staticcheck
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
	id, pending, err := queueRegularAgentMessage(&db.AgentMessage{
		GroupID:          0,
		FromConv:         "",
		ToConv:           target,
		Subject:          replySubjectFor(orig.Subject, orig.ID),
		Body:             body.Body,
		ToRecipients:     []string{target},
		OperatorAuthored: true,
	})
	if err != nil {
		if full, ok := agentMessageQueueFull(err); ok {
			writeQueueFull(w, target, full)
			return
		}
		writeError(w, http.StatusInternalServerError, "io", "queue reply: "+err.Error())
		return
	}
	// Replying means the operator has handled this notification — mark the
	// original read (idempotent). Merely opening the reader deliberately does
	// NOT mark read, so for a replied-to notification this is usually the write
	// that clears it. Best-effort: a failure here must not fail the delivered
	// reply.
	if _, err := db.MarkHumanMessageRead(body.ID); err != nil {
		slog.Warn("reply: mark original human message read failed", "id", body.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message_id": id,
		"conv_id":    target,
		"queued":     true,
		"pending":    pending,
	})
}

func resolveProcessHumanMessage(_ context.Context, _ *db.HumanMessage, _ string) error { //nolint:staticcheck // Temporary stub intentionally always errors behind the nil-capable seam.
	// Retain process command metadata for the replacement engine, but never
	// feed a reply into the removed filesystem runtime. Old obligation rows are
	// inert until a new engine explicitly adopts a migration policy.
	return fmt.Errorf("process runtime is temporarily unavailable: no engine is installed")
}
