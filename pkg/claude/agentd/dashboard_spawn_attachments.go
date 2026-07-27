package agentd

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// Spawn-attachment upload — the dashboard's "📎 Attach files" + paste-a-
// screenshot affordance on the spawn modal. The browser uploads the chosen
// files (and clipboard images, packaged client-side as PNGs) here BEFORE it
// POSTs the spawn; this endpoint writes them to a per-batch temp dir and hands
// the absolute paths back. The dashboard then passes those paths as the spawn
// request's `attachments`, and the daemon folds them into the new agent's
// startup briefing (buildSpawnAttachmentsSection) so it can open them on its
// first turn.
//
// The files live in a temp dir, not the user's repo, so a spawn that never
// completes leaves nothing behind in the working tree; a best-effort sweep of
// stale batches keeps the temp dir from accumulating. The endpoint is
// human-only (cookie + Origin pinned, same as every other /api/ route) — the
// dashboard is the human's surface by definition.

const (
	// spawnAttachmentMaxFileBytes caps a single uploaded file. Screenshots and
	// short text/code files are the expected payloads; 25 MiB is generous for
	// those while still rejecting an accidental giant upload.
	spawnAttachmentMaxFileBytes = 25 << 20
	// spawnAttachmentMaxTotalBytes caps one upload batch across all its files.
	spawnAttachmentMaxTotalBytes = 100 << 20
	// spawnAttachmentMaxFiles caps how many files one batch may carry. Matches
	// the daemon-side maxSpawnAttachments backstop on the spawn request.
	spawnAttachmentMaxFiles = maxSpawnAttachments
	// spawnAttachmentBatchTTL is how long an uploaded batch survives before the
	// stale-batch sweep removes it. A spawn reads its attachments within seconds
	// of upload, so a day is far more than enough headroom for a slow operator
	// who opens the picker and finishes the spawn later.
	spawnAttachmentBatchTTL = 24 * time.Hour
)

// spawnAttachmentsBase is the parent of all per-batch upload dirs. Kept under
// the OS temp dir (per-user on macOS) so uploads never touch the user's repo
// and the OS reclaims them eventually even if the sweep never runs. A var (not
// a const) so tests can repoint it at a t.TempDir() — cross-platform isolation
// that an env override can't give (Windows os.TempDir() ignores $TMPDIR).
var spawnAttachmentsBase = filepath.Join(os.TempDir(), "tclaude-spawn-attachments")

func spawnAttachmentsBaseDir() string { return spawnAttachmentsBase }

var daemonStagedAttachments = struct {
	sync.RWMutex
	paths map[string]os.FileInfo
}{paths: make(map[string]os.FileInfo)}

func registerDaemonStagedAttachment(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	daemonStagedAttachments.Lock()
	daemonStagedAttachments.paths[path] = info
	daemonStagedAttachments.Unlock()
}

func daemonStagedAttachmentIdentity(path string) (os.FileInfo, bool) {
	daemonStagedAttachments.RLock()
	info, issued := daemonStagedAttachments.paths[path]
	daemonStagedAttachments.RUnlock()
	return info, issued
}

func forgetDaemonStagedBatch(dir string) {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	daemonStagedAttachments.Lock()
	for path := range daemonStagedAttachments.paths {
		if strings.HasPrefix(path, prefix) {
			delete(daemonStagedAttachments.paths, path)
		}
	}
	daemonStagedAttachments.Unlock()
}

// spawnAttachmentFile is one stored upload in the JSON response.
type spawnAttachmentFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// spawnAttachmentsResponse is the upload endpoint's reply: the batch token (the
// per-batch dir name), the absolute dir, and one entry per stored file. The
// dashboard reads files[].path into the spawn request's `attachments`.
type spawnAttachmentsResponse struct {
	Token string                `json:"token"`
	Dir   string                `json:"dir"`
	Files []spawnAttachmentFile `json:"files"`
}

func registerDashboardSpawnAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/spawn-attachments", handleDashboardSpawnAttachments)
	mux.HandleFunc("/api/terminal-attachments", handleDashboardTerminalAttachments)
}

// handleDashboardSpawnAttachments receives a multipart/form-data POST whose
// "file" parts are the attachments, writes each to a fresh per-batch temp dir,
// and returns the stored paths. It streams the parts (MultipartReader) rather
// than buffering the whole form, enforcing the per-file / per-batch / count
// caps as it goes so an oversized upload is rejected without first landing on
// disk in full.
func handleDashboardSpawnAttachments(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}

	// Best-effort hygiene: drop batches from earlier spawns before adding a new
	// one. A failure here is non-fatal — the OS reclaims the temp dir anyway.
	sweepStaleAttachmentBatches(spawnAttachmentsBaseDir())
	sweepStalePrivateAttachmentRoots()
	storeDashboardAttachments(w, r, spawnAttachmentsBaseDir(), true)
}

func handleDashboardTerminalAttachments(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	base, createBase, status, err := terminalAttachmentBase(
		r.URL.Query().Get("terminal"),
		session.IsTmuxSessionAlive,
	)
	if err != nil {
		writeError(w, status, "terminal", err.Error())
		return
	}
	if !createBase {
		info, statErr := os.Stat(base)
		if statErr != nil || !info.IsDir() {
			writeError(w, http.StatusConflict, "terminal",
				"live tclaude-layer attachment root is unavailable")
			return
		}
	}
	sweepStalePrivateAttachmentRoots()
	sweepStaleAttachmentBatches(base)
	storeDashboardAttachments(w, r, base, createBase)
}

func terminalAttachmentBase(
	rawTerminalPath string,
	isTmuxSessionAlive func(string) bool,
) (string, bool, int, error) {
	rawTerminalPath = strings.TrimSpace(rawTerminalPath)
	if rawTerminalPath == "" {
		return "", false, http.StatusBadRequest, fmt.Errorf("terminal path is required")
	}
	terminalURL, err := url.ParseRequestURI(rawTerminalPath)
	if err != nil || terminalURL.IsAbs() || terminalURL.Host != "" ||
		terminalURL.Fragment != "" || !strings.HasPrefix(terminalURL.Path, "/api/") {
		return "", false, http.StatusBadRequest, fmt.Errorf("invalid same-origin terminal path")
	}
	path := terminalURL.Path
	switch {
	case strings.HasPrefix(path, "/api/term-ws/"),
		strings.HasPrefix(path, "/api/group-term-ws/"):
		return spawnAttachmentsBaseDir(), true, http.StatusOK, nil
	}

	var sess *db.SessionRow
	switch {
	case strings.HasPrefix(path, "/api/open-window-ws/"):
		selector, unescapeErr := url.PathUnescape(strings.TrimPrefix(path, "/api/open-window-ws/"))
		if unescapeErr != nil || selector == "" || strings.Contains(selector, "/") {
			return "", false, http.StatusBadRequest, fmt.Errorf("invalid agent terminal path")
		}
		resolved, _, resolveErr := agent.ResolveSelectorCached(selector)
		if resolveErr != nil {
			return "", false, http.StatusNotFound, fmt.Errorf("terminal agent is unavailable")
		}
		candidates, findErr := db.FindSessionsByConvID(resolved.ConvID)
		if findErr == nil {
			for _, candidate := range candidates {
				if candidate.TmuxSession != "" && isTmuxSessionAlive(candidate.TmuxSession) {
					sess = candidate
					break
				}
			}
		}
	case strings.HasPrefix(path, "/api/spawn-focus-ws/"):
		label, unescapeErr := url.PathUnescape(strings.TrimPrefix(path, "/api/spawn-focus-ws/"))
		if unescapeErr != nil || label == "" || strings.Contains(label, "/") {
			return "", false, http.StatusBadRequest, fmt.Errorf("invalid spawn terminal path")
		}
		sess, err = db.LoadSession(label)
		if err != nil {
			sess = nil
		}
		if sess != nil && (sess.TmuxSession == "" || !isTmuxSessionAlive(sess.TmuxSession)) {
			sess = nil
		}
	default:
		return "", false, http.StatusBadRequest, fmt.Errorf("unknown terminal path")
	}
	if sess == nil {
		return "", false, http.StatusNotFound, fmt.Errorf("terminal session is not live")
	}
	implementation, normalizeErr := sandboxpolicy.NormalizeImplementation(
		sess.SandboxImplementation,
	)
	if normalizeErr != nil {
		return "", false, http.StatusConflict, fmt.Errorf(
			"terminal session has unknown sandbox implementation %q",
			sess.SandboxImplementation,
		)
	}
	switch implementation {
	case sandboxpolicy.ImplementationHarnessBuiltin:
		return spawnAttachmentsBaseDir(), true, http.StatusOK, nil
	case sandboxpolicy.ImplementationTclaudeLayer:
		privateRoot := tclcommon.SpawnAttachmentsPrivateDir(sess.ID)
		if !filepath.IsAbs(privateRoot) {
			return "", false, http.StatusConflict, fmt.Errorf(
				"terminal session private attachment root is unavailable",
			)
		}
		return privateRoot, false, http.StatusOK, nil
	}
	return "", false, http.StatusConflict, fmt.Errorf("terminal session sandbox is unavailable")
}

func storeDashboardAttachments(
	w http.ResponseWriter,
	r *http.Request,
	base string,
	createBase bool,
) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "multipart",
			"expected a multipart/form-data upload: "+err.Error())
		return
	}

	token := convops.GenerateUUID()
	dir := filepath.Join(base, token)
	if createBase {
		err = os.MkdirAll(dir, 0o700)
	} else {
		err = os.Mkdir(dir, 0o700)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"failed to create attachment dir: "+err.Error())
		return
	}

	var (
		files []spawnAttachmentFile
		total int64
		used  = map[string]bool{}
	)
	// Roll back the whole batch on any error — a half-written batch must not
	// reach the spawn briefing.
	cleanup := func() { _ = os.RemoveAll(dir) }

	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			cleanup()
			writeError(w, http.StatusBadRequest, "multipart", "read part: "+perr.Error())
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		if len(files) >= spawnAttachmentMaxFiles {
			_ = part.Close()
			cleanup()
			writeError(w, http.StatusBadRequest, "too_many",
				fmt.Sprintf("too many files: max %d per upload", spawnAttachmentMaxFiles))
			return
		}

		name := uniqueAttachmentName(sanitizeAttachmentFilename(part.FileName()), used)
		dest := filepath.Join(dir, name)
		size, werr := writeSpawnAttachmentPart(dest, part, spawnAttachmentMaxFileBytes, spawnAttachmentMaxTotalBytes-total)
		_ = part.Close()
		if werr != nil {
			cleanup()
			writeError(w, werr.status, werr.code, werr.msg)
			return
		}
		used[name] = true
		total += size
		files = append(files, spawnAttachmentFile{Name: name, Path: dest, Size: size})
	}

	if len(files) == 0 {
		cleanup()
		writeError(w, http.StatusBadRequest, "empty", "no files in the upload")
		return
	}
	if filepath.Clean(base) == filepath.Clean(spawnAttachmentsBaseDir()) {
		for _, file := range files {
			registerDaemonStagedAttachment(file.Path)
		}
	}

	writeJSON(w, http.StatusOK, spawnAttachmentsResponse{Token: token, Dir: dir, Files: files})
}

// attachmentWriteError is a typed write failure carrying the HTTP mapping so
// the handler can surface a per-file cap breach distinctly from an IO error.
type attachmentWriteError struct {
	status int
	code   string
	msg    string
}

// writeSpawnAttachmentPart streams one multipart part to dest, enforcing both
// the per-file cap (perFileMax) and the batch's remaining budget (remaining).
// It reads one byte past the smaller limit so it can tell "exactly at the cap"
// from "over it", and removes the partial file on any failure. Returns the
// bytes written on success.
func writeSpawnAttachmentPart(dest string, src io.Reader, perFileMax, remaining int64) (int64, *attachmentWriteError) {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, &attachmentWriteError{http.StatusInternalServerError, "io", "create file: " + err.Error()}
	}
	limit := min(remaining, perFileMax)
	// LimitReader to limit+1: a read that yields more than `limit` bytes means
	// the source exceeded whichever cap was tighter.
	n, copyErr := io.Copy(f, io.LimitReader(src, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(dest)
		return 0, &attachmentWriteError{http.StatusInternalServerError, "io", "write file: " + copyErr.Error()}
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return 0, &attachmentWriteError{http.StatusInternalServerError, "io", "close file: " + closeErr.Error()}
	}
	if n > limit {
		_ = os.Remove(dest)
		if limit == remaining && remaining < perFileMax {
			return 0, &attachmentWriteError{http.StatusBadRequest, "too_large",
				fmt.Sprintf("upload exceeds the %d MiB total cap", spawnAttachmentMaxTotalBytes>>20)}
		}
		return 0, &attachmentWriteError{http.StatusBadRequest, "too_large",
			fmt.Sprintf("file exceeds the %d MiB per-file cap", spawnAttachmentMaxFileBytes>>20)}
	}
	return n, nil
}

// sanitizeAttachmentFilename reduces an uploaded filename to a safe basename:
// path components stripped, control chars and separators dropped, length
// bounded, and a fallback applied for an empty / "." / ".." result. The cleaned
// name is only ever joined under the per-batch dir, but defending here keeps a
// crafted multipart filename (e.g. "../../etc/foo") from escaping it.
func sanitizeAttachmentFilename(name string) string {
	// Drop any directory part the client included (both separators, since a
	// Windows client may send backslashes).
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	// Bound the length while preserving a short extension.
	const maxNameLen = 128
	if len(name) > maxNameLen {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:maxNameLen-len(ext)] + ext
	}
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

// uniqueAttachmentName ensures the (already sanitised) name is unique within a
// batch, appending "-1", "-2", … before the extension on a collision so two
// "screenshot.png" uploads don't clobber each other.
func uniqueAttachmentName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := stem + "-" + strconv.Itoa(i) + ext
		if !used[cand] {
			return cand
		}
	}
}

// sweepStaleSpawnAttachmentBatches removes per-batch dirs whose mtime is older
// than spawnAttachmentBatchTTL. Best-effort: every error is logged at debug and
// swallowed — a leaked temp dir is harmless and the OS reclaims it eventually.
func sweepStaleSpawnAttachmentBatches() {
	sweepStaleAttachmentBatches(spawnAttachmentsBaseDir())
}

func sweepStaleAttachmentBatches(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return // base doesn't exist yet, or is unreadable — nothing to sweep
	}
	cutoff := time.Now().Add(-spawnAttachmentBatchTTL)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(base, e.Name())); rerr != nil {
			slog.Debug("spawn-attachments: failed to sweep stale batch",
				"dir", e.Name(), "error", rerr)
		} else if filepath.Clean(base) == filepath.Clean(spawnAttachmentsBaseDir()) {
			forgetDaemonStagedBatch(filepath.Join(base, e.Name()))
		}
	}
}
