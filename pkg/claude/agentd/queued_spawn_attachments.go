package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Queued-spawn attachments. A queued spawn may wait for days and across daemon
// restarts, longer than a spawn-attachment staging batch lives, so queueing
// copies the dialog's staged uploads into a durable per-queued-spawn dir under
// the private data tree. The draft's `attachments` lists them by stored file
// name. Launching re-stages the chosen ones as a fresh daemon-issued batch
// (handleQueuedSpawnStageAttachments) and spawns with those paths, exactly as
// a fresh upload would. The dir goes away with its queued spawn.

// queuedSpawnAttachmentsBase holds one dir per queued spawn id. A var so tests
// can repoint it.
var queuedSpawnAttachmentsBase = filepath.Join(config.DataDir(), "queued-spawn-attachments")

// queuedSpawnAttachmentsMu serialises the queue's attachment-dir writes with
// the row writes that reference them.
var queuedSpawnAttachmentsMu sync.Mutex

func queuedSpawnAttachmentsDir(id int64) string {
	return filepath.Join(queuedSpawnAttachmentsBase, strconv.FormatInt(id, 10))
}

// queuedSpawnAttachment is one draft.attachments entry. Staged, set only on
// the way in, is a daemon-issued spawn-attachment path to copy into the
// queued spawn's dir; without it Name names a file already stored there.
type queuedSpawnAttachment struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Staged string `json:"staged,omitempty"`
}

// storeQueuedSpawnAttachments reconciles the queued spawn's dir with the
// draft's attachments: staged uploads are copied in, stored files the draft no
// longer lists are removed. It returns the draft with its attachments rewritten
// to the stored names. On error the dir is left as it was.
func storeQueuedSpawnAttachments(id int64, draft string) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(draft), &fields); err != nil {
		return "", err
	}
	dir := queuedSpawnAttachmentsDir(id)
	raw, listed := fields["attachments"]
	var entries []queuedSpawnAttachment
	if listed && string(raw) != "null" {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return "", fmt.Errorf("draft.attachments: %w", err)
		}
	}
	if len(entries) > maxSpawnAttachments {
		return "", fmt.Errorf("too many attachments: %d (max %d)", len(entries), maxSpawnAttachments)
	}
	if len(entries) == 0 {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if !listed {
			return draft, nil
		}
		delete(fields, "attachments")
		out, err := json.Marshal(fields)
		return string(out), err
	}

	existing := map[string]bool{}
	if dirEntries, err := os.ReadDir(dir); err == nil {
		for _, e := range dirEntries {
			existing[e.Name()] = true
		}
	}
	// Kept files must already be stored here; new copies avoid every existing
	// name so a failure part-way never clobbers a file the old draft lists.
	used := map[string]bool{}
	for name := range existing {
		used[name] = true
	}
	kept := map[string]bool{}
	for i, entry := range entries {
		if entry.Staged != "" {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, entry.Name))
		if entry.Name == "" || sanitizeAttachmentFilename(entry.Name) != entry.Name || err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("attachment %d: %q is not stored with this queued spawn", i, entry.Name)
		}
		entries[i].Size = info.Size()
		kept[entry.Name] = true
	}

	var created []string
	rollback := func() {
		for _, path := range created {
			_ = os.Remove(path)
		}
	}
	stagingBase, stagingErr := filepath.EvalSymlinks(spawnAttachmentsBaseDir())
	for i, entry := range entries {
		if entry.Staged == "" {
			continue
		}
		if stagingErr != nil {
			rollback()
			return "", fmt.Errorf("resolve daemon attachment staging root: %w", stagingErr)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			rollback()
			return "", err
		}
		source, err := openDaemonStagedAttachment(stagingBase, entry.Staged)
		if err != nil {
			rollback()
			return "", fmt.Errorf("attachment %d: %w", i, err)
		}
		name := entry.Name
		if name == "" {
			name = filepath.Base(entry.Staged)
		}
		name = uniqueAttachmentName(sanitizeAttachmentFilename(name), used)
		dest := filepath.Join(dir, name)
		size, werr := writeSpawnAttachmentPart(dest, source, spawnAttachmentMaxFileBytes, spawnAttachmentMaxFileBytes)
		_ = source.Close()
		if werr != nil {
			rollback()
			return "", fmt.Errorf("attachment %d: %s", i, werr.msg)
		}
		created = append(created, dest)
		used[name] = true
		kept[name] = true
		entries[i] = queuedSpawnAttachment{Name: name, Size: size}
	}

	for name := range existing {
		if !kept[name] {
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		rollback()
		return "", err
	}
	fields["attachments"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		rollback()
		return "", err
	}
	return string(out), nil
}

// removeQueuedSpawnAttachments drops a deleted queued spawn's dir.
func removeQueuedSpawnAttachments(id int64) {
	_ = os.RemoveAll(queuedSpawnAttachmentsDir(id))
}

// sweepOrphanQueuedSpawnAttachments removes dirs whose queued spawn no longer
// exists (a delete that crashed between the row and the dir). Callers hold
// queuedSpawnAttachmentsMu so a dir being filled for a new row is never swept.
func sweepOrphanQueuedSpawnAttachments() {
	entries, err := os.ReadDir(queuedSpawnAttachmentsBase)
	if err != nil {
		return
	}
	rows, err := db.ListQueuedSpawns()
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, q := range rows {
		live[strconv.FormatInt(q.ID, 10)] = true
	}
	for _, e := range entries {
		if !live[e.Name()] {
			_ = os.RemoveAll(filepath.Join(queuedSpawnAttachmentsBase, e.Name()))
		}
	}
}

// handleQueuedSpawnStageAttachments copies the named stored attachments of a
// queued spawn into a fresh daemon-issued staging batch and returns it in the
// upload endpoint's shape, so the launch spawns with paths the spawn path
// accepts. The dir is not consumed: a failed launch can stage again.
func handleQueuedSpawnStageAttachments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be a positive integer")
		return
	}
	var req struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, queuedSpawnMaxBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if len(req.Names) == 0 || len(req.Names) > maxSpawnAttachments {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("names must list 1 to %d attachments", maxSpawnAttachments))
		return
	}

	queuedSpawnAttachmentsMu.Lock()
	defer queuedSpawnAttachmentsMu.Unlock()
	if q, err := db.GetQueuedSpawn(id); err != nil || q == nil {
		writeQueuedSpawnError(w, firstNonNil(err, db.ErrQueuedSpawnNotFound))
		return
	}
	sweepStaleAttachmentBatches(spawnAttachmentsBaseDir())
	token := convops.GenerateUUID()
	batch := filepath.Join(spawnAttachmentsBaseDir(), token)
	if err := os.MkdirAll(batch, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "failed to create attachment dir: "+err.Error())
		return
	}
	dir := queuedSpawnAttachmentsDir(id)
	files := make([]spawnAttachmentFile, 0, len(req.Names))
	used := map[string]bool{}
	for _, name := range req.Names {
		source, status, msg := openQueuedSpawnAttachment(dir, name)
		if source == nil {
			_ = os.RemoveAll(batch)
			writeError(w, status, "attachment", msg)
			return
		}
		staged := uniqueAttachmentName(name, used)
		dest := filepath.Join(batch, staged)
		size, werr := writeSpawnAttachmentPart(dest, source, spawnAttachmentMaxFileBytes, spawnAttachmentMaxFileBytes)
		_ = source.Close()
		if werr != nil {
			_ = os.RemoveAll(batch)
			writeError(w, werr.status, werr.code, werr.msg)
			return
		}
		used[staged] = true
		files = append(files, spawnAttachmentFile{Name: staged, Path: dest, Size: size})
	}
	for _, file := range files {
		registerDaemonStagedAttachment(file.Path)
	}
	writeJSON(w, http.StatusOK, spawnAttachmentsResponse{Token: token, Dir: batch, Files: files})
}

// openQueuedSpawnAttachment opens one stored file, refusing anything but a
// plain, non-symlinked regular file directly in the queued spawn's dir.
func openQueuedSpawnAttachment(dir, name string) (*os.File, int, string) {
	if name == "" || sanitizeAttachmentFilename(name) != name {
		return nil, http.StatusBadRequest, fmt.Sprintf("invalid attachment name %q", name)
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, http.StatusNotFound, fmt.Sprintf("attachment %q is no longer stored with this queued spawn", name)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, http.StatusInternalServerError, "open attachment: " + err.Error()
	}
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, http.StatusConflict, fmt.Sprintf("attachment %q changed while opening", name)
	}
	return f, 0, ""
}
