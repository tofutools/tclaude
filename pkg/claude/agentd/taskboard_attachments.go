package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Files on a task (tcl-jy91ej, docs/taskboard.md). Names and types pass the
// same allow-list as mail attachments across nodes (fedAttachmentName): no
// HTML, SVG, archives or executables, so a file is safe to show inline. The
// bytes live in a content-addressed store under the daemon's private data
// directory, which no sandboxed agent reaches; a reader gets them through
// the task's routes, under taskboard.read.
//
//	POST   /v1/taskboard/tasks/{id}/attachments        multipart "file" parts (taskboard.write)
//	GET    /v1/taskboard/tasks/{id}/attachments        the list (taskboard.read)
//	GET    /v1/taskboard/tasks/{id}/attachments/{aid}  the bytes; ?format=json base64 for peer views
//	DELETE /v1/taskboard/tasks/{id}/attachments/{aid}  detach (taskboard.write)
//
// A create, a transfer-in and an ask take the same "file" parts beside a
// "task" part that carries their JSON body (decodeTaskboardRequest).

const (
	// taskAttachmentMaxBytes caps one file. A screenshot fits, and a file's
	// base64 still fits one peer view reply (fedPeerViewResponseBodyLimit).
	taskAttachmentMaxBytes = 4 << 20
	// taskAttachmentBlobGrace keeps a fresh blob no row names yet: a request
	// may be between storing the bytes and committing its row.
	taskAttachmentBlobGrace = 30 * time.Minute
	// taskboardMultipartMax bounds a multipart request: every file at its
	// cap, the JSON part, and the framing.
	taskboardMultipartMax = db.GroupTaskAttachmentsCap*taskAttachmentMaxBytes + 1<<20
)

// taskboardFilesDir holds the blobs, by sha256: <dir>/<first two>/<sha256>.
func taskboardFilesDir() string { return filepath.Join(config.DataDir(), "taskboard-files") }

func taskBlobPath(sha string) (string, error) {
	if len(sha) != sha256.Size*2 || strings.ToLower(sha) != sha {
		return "", fmt.Errorf("bad blob id")
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", fmt.Errorf("bad blob id")
	}
	return filepath.Join(taskboardFilesDir(), sha[:2], sha), nil
}

// storeTaskBlob writes data to the store, once per content, and returns its
// sha256. An existing blob is touched so the collector's grace covers the
// row about to name it.
func storeTaskBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	path, err := taskBlobPath(sha)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == int64(len(data)) {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return sha, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return "", werr
	}
	return sha, nil
}

// touchTaskBlobs refreshes the collector's grace on the files' blobs.
func touchTaskBlobs(files []db.GroupTaskAttachment) {
	now := time.Now()
	for _, f := range files {
		if path, err := taskBlobPath(f.SHA256); err == nil {
			_ = os.Chtimes(path, now, now)
		}
	}
}

// readTaskBlob reads a stored file, checking it is the size its row says.
func readTaskBlob(a *db.GroupTaskAttachment) ([]byte, error) {
	path, err := taskBlobPath(a.SHA256)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != a.SizeBytes {
		return nil, fmt.Errorf("stored file %s has the wrong size", a.Name)
	}
	return data, nil
}

// collectTaskBlobs removes the blobs no task's file names, past the grace.
// It fails closed: without an authoritative list nothing is removed.
func collectTaskBlobs(now time.Time) {
	referenced, err := db.GroupTaskAttachmentBlobs()
	if err != nil {
		slog.Warn("taskboard attachments: list blobs failed", "error", err)
		return
	}
	removed := 0
	_ = filepath.WalkDir(taskboardFilesDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if referenced[d.Name()] {
			return nil
		}
		// Stat afresh: a store or a clone may have just touched it.
		if info, err := os.Stat(path); err != nil || now.Sub(info.ModTime()) < taskAttachmentBlobGrace {
			return nil
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	if removed > 0 {
		slog.Info("taskboard attachments: collected unreferenced files", "removed", removed)
	}
}

// taskboardUpload is one file a request carries, its name and type
// already through the allow-list.
type taskboardUpload struct {
	Name        string
	ContentType string
	Data        []byte
}

// cleanTaskUpload gates one file's name, type and size.
func cleanTaskUpload(name string, data []byte) (taskboardUpload, error) {
	clean, ct, err := fedAttachmentName(name)
	if err != nil {
		return taskboardUpload{}, err
	}
	if len(data) == 0 {
		return taskboardUpload{}, fmt.Errorf("attachment %q is empty", clean)
	}
	if len(data) > taskAttachmentMaxBytes {
		return taskboardUpload{}, &taskUploadTooLarge{name: clean}
	}
	return taskboardUpload{Name: clean, ContentType: ct, Data: data}, nil
}

type taskUploadTooLarge struct{ name string }

func (e *taskUploadTooLarge) Error() string {
	return fmt.Sprintf("attachment %q is larger than %d MiB", e.name, taskAttachmentMaxBytes>>20)
}

func isMultipart(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "multipart/form-data"
}

// readTaskboardMultipart reads a multipart request: the "task" part's JSON
// (nil when absent) and the "file" parts. It writes the refusal itself.
func readTaskboardMultipart(w http.ResponseWriter, r *http.Request) ([]byte, []taskboardUpload, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, taskboardMultipartMax)
	task, files, err := parseTaskboardMultipart(r)
	if err != nil {
		var tooLarge *taskUploadTooLarge
		var maxBytes *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.As(err, &maxBytes) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		}
		return nil, nil, false
	}
	return task, files, true
}

func parseTaskboardMultipart(r *http.Request) (task []byte, files []taskboardUpload, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid multipart body: %w", err)
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return task, files, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("invalid multipart body: %w", err)
		}
		switch part.FormName() {
		case "task":
			if task != nil {
				err = errors.New("give one task part")
				break
			}
			task, err = io.ReadAll(io.LimitReader(part, 64<<10+1))
			if err == nil && len(task) > 64<<10 {
				err = errors.New("the task part is at most 64 KiB")
			}
		case "file":
			if len(files) >= db.GroupTaskAttachmentsCap {
				err = fmt.Errorf("at most %d files", db.GroupTaskAttachmentsCap)
				break
			}
			var data []byte
			if data, err = io.ReadAll(io.LimitReader(part, taskAttachmentMaxBytes+1)); err == nil {
				var up taskboardUpload
				if up, err = cleanTaskUpload(part.FileName(), data); err == nil {
					files = append(files, up)
				}
			}
		default:
			err = fmt.Errorf("unexpected part %q: give a task part and file parts", guideText(part.FormName(), 40))
		}
		_ = part.Close()
		if err != nil {
			return nil, nil, err
		}
	}
}

// decodeTaskboardRequest decodes a route's JSON body into v: the body
// itself, or a multipart request's "task" part, whose "file" parts it
// returns.
func decodeTaskboardRequest(w http.ResponseWriter, r *http.Request, v any) ([]taskboardUpload, bool) {
	if !isMultipart(r) {
		return nil, decodeTaskboardBody(w, r, v)
	}
	task, files, ok := readTaskboardMultipart(w, r)
	if !ok {
		return nil, false
	}
	if task == nil {
		task = []byte("{}")
	}
	if err := json.NewDecoder(bytes.NewReader(task)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON in the task part: "+err.Error())
		return nil, false
	}
	return files, true
}

// storeTaskUploads stores the files' bytes and returns their rows, ready to
// insert.
func storeTaskUploads(files []taskboardUpload, actor string) ([]db.GroupTaskAttachment, error) {
	rows := make([]db.GroupTaskAttachment, 0, len(files))
	for _, f := range files {
		sha, err := storeTaskBlob(f.Data)
		if err != nil {
			return nil, err
		}
		rows = append(rows, db.GroupTaskAttachment{Name: f.Name, SHA256: sha, SizeBytes: int64(len(f.Data)),
			ContentType: f.ContentType, AddedBy: actor})
	}
	return rows, nil
}

// storeTaskUploadsOrFail is storeTaskUploads writing its own refusal.
func storeTaskUploadsOrFail(w http.ResponseWriter, files []taskboardUpload, actor string) ([]db.GroupTaskAttachment, bool) {
	rows, err := storeTaskUploads(files, actor)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "store attachment: "+err.Error())
		return nil, false
	}
	return rows, true
}

// ---- wire ----

type taskboardAttachmentView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	Image       bool   `json:"image,omitempty"`
	AddedBy     string `json:"added_by"`
	AddedAt     string `json:"added_at"`
}

func taskboardAttachmentViews(atts []db.GroupTaskAttachment) []taskboardAttachmentView {
	out := make([]taskboardAttachmentView, 0, len(atts))
	for _, a := range atts {
		out = append(out, taskboardAttachmentView{ID: a.ID, Name: a.Name, Size: a.SizeBytes, ContentType: a.ContentType,
			SHA256: a.SHA256, Image: strings.HasPrefix(a.ContentType, "image/"), AddedBy: a.AddedBy, AddedAt: rfc3339(a.AddedAt)})
	}
	return out
}

func registerTaskboardAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/attachments", handleTaskboardTask(taskboardOwnGate, handleTaskboardAttach))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}/attachments", handleTaskboardTask(taskboardReadAccess, handleTaskboardAttachments))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}/attachments/{aid}", handleTaskboardTask(taskboardReadAccess, handleTaskboardAttachmentGet))
	mux.HandleFunc("DELETE /v1/taskboard/tasks/{id}/attachments/{aid}", handleTaskboardTask(taskboardOwnGate, handleTaskboardDetach))
}

// POST /v1/taskboard/tasks/{id}/attachments — multipart "file" parts.
func handleTaskboardAttach(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	if !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	if !isMultipart(r) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "send the files as multipart/form-data file parts")
		return
	}
	task, files, ok := readTaskboardMultipart(w, r)
	if !ok {
		return
	}
	if task != nil || len(files) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "give one or more file parts, and no task part")
		return
	}
	actor := taskboardActor(r)
	rows, ok := storeTaskUploadsOrFail(w, files, actor)
	if !ok {
		return
	}
	out, added, err := db.AddGroupTaskAttachments(t.ID, rows, actor)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	names := make([]string, 0, len(added))
	for _, a := range added {
		names = append(names, a.Name)
	}
	setAuditDetail(r, fmt.Sprintf("attached %s to %s", strings.Join(names, ", "), t.ID))
	writeTaskboardTask(w, http.StatusCreated, g, out)
}

// GET /v1/taskboard/tasks/{id}/attachments
func handleTaskboardAttachments(w http.ResponseWriter, _ *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	atts, err := db.ListGroupTaskAttachments(t.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "group": g.Name, "attachments": taskboardAttachmentViews(atts)})
}

// taskboardAttachmentOf resolves {aid} on t, writing the 404 itself.
func taskboardAttachmentOf(w http.ResponseWriter, r *http.Request, t *db.GroupTask) (*db.GroupTaskAttachment, bool) {
	raw := r.PathValue("aid")
	aid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || aid <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such attachment "+guideText(raw, 20))
		return nil, false
	}
	a, err := db.GetGroupTaskAttachment(t.ID, aid)
	if errors.Is(err, db.ErrGroupTaskAttachmentNotFound) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("task %s has no attachment %d", t.ID, aid))
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return nil, false
	}
	return a, true
}

// GET /v1/taskboard/tasks/{id}/attachments/{aid}[?format=json] — the file.
// An image is served inline, so the dashboard can show it; anything else
// is a download. A peer view carries JSON only: format=json answers the
// bytes as base64.
func handleTaskboardAttachmentGet(w http.ResponseWriter, r *http.Request, t *db.GroupTask, _ *db.AgentGroup) {
	a, ok := taskboardAttachmentOf(w, r, t)
	if !ok {
		return
	}
	data, err := readTaskBlob(a)
	if err != nil {
		writeError(w, http.StatusGone, "missing", "the stored file is missing: "+a.Name)
		return
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "task": t.ID, "name": a.Name, "content_type": a.ContentType,
			"size": a.SizeBytes, "sha256": a.SHA256, "data": data})
		return
	}
	disposition := "attachment"
	if strings.HasPrefix(a.ContentType, "image/") {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", a.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// DELETE /v1/taskboard/tasks/{id}/attachments/{aid}
func handleTaskboardDetach(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	if !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	a, ok := taskboardAttachmentOf(w, r, t)
	if !ok {
		return
	}
	out, _, err := db.RemoveGroupTaskAttachment(t.ID, a.ID, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("detached %s from %s", a.Name, t.ID))
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// taskAttachmentPayloads turns a request's files into mail attachments
// (an ask's files go to the asked agent's inbox).
func taskAttachmentPayloads(files []taskboardUpload) []proto.AttachmentPayload {
	out := make([]proto.AttachmentPayload, 0, len(files))
	for _, f := range files {
		out = append(out, proto.AttachmentPayload{Name: f.Name, Data: f.Data})
	}
	return out
}
