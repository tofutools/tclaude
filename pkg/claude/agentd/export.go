package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// export.go is the daemon half of the per-agent "📋 summary…" export
// (JOH-265). Unlike the group export (groups_export.go), which is a
// SYNCHRONOUS mechanical dump the daemon builds from DB rows + raw .jsonl,
// this is an ASYNCHRONOUS, agent-produced export: the daemon asks a LIVE
// agent to consolidate a curated, shareable artifact, the agent uploads the
// result, and the dashboard downloads it. The round-trip:
//
//	1. Dashboard POST /api/agents/{conv}/export {title, instructions, preset}
//	   → createExportJob: fast-fails if the agent is offline, else inserts a
//	   job (status=requested) and injects a one-line pointer nudge into the
//	   agent's pane. The instructions are NOT injected — only the integer job
//	   id is — keeping send-keys free of arbitrary text (the mail-nudge idiom).
//	2. Agent runs `tclaude agent export show N`  → GET /v1/export-jobs/{id}:
//	   returns the brief and flips the job to running.
//	3. Agent runs `tclaude agent export submit N <files…>` → POST
//	   /v1/export-jobs/{id}/artifact: stores the uploaded bytes under
//	   ~/.tclaude/exports/<id>/ and flips the job to ready.
//	4. Dashboard polls GET /api/export-jobs/{id} until ready, then downloads
//	   GET /api/export-jobs/{id}/artifact (Content-Disposition attachment).
//
// The /v1 endpoints are self-scoped: a confirmed agent may only touch its OWN
// job (conv match); the human operator may touch any. The /api endpoints are
// the cookie-authed dashboard twin.

// maxExportArtifactBytes caps a single uploaded export artifact. A shareable
// summary is small; the cap is a sanity bound against a runaway upload, well
// below the 512 MiB group-import ceiling.
const maxExportArtifactBytes = 256 << 20 // 256 MiB

// exportInstructionsMax / exportTitleMax bound the human's brief. These live in
// the DB and the `export show` JSON only — never in a send-keys injection — so
// the cap is about storage hygiene, not injection safety.
const (
	exportInstructionsMax = 8 << 10 // 8 KiB
	exportTitleMax        = 200     // runes
)

// Cleanup cadence and retention. Package vars so flow tests can shrink them.
var (
	// exportJobStaleTimeout: a requested/running job whose agent never
	// delivered is flipped to failed after this long, so the state stops
	// lying and the TTL sweep can eventually reclaim it.
	exportJobStaleTimeout = 30 * time.Minute
	// exportJobTTL: a terminal (ready/failed) job and its on-disk artifact
	// are deleted this long after they settled, bounding ~/.tclaude/exports.
	exportJobTTL = 24 * time.Hour
	// exportJobsCleanupInterval: how often the sweep runs.
	exportJobsCleanupInterval = 10 * time.Minute
)

// exportsBaseDir is ~/.tclaude/exports — the root for every job's artifact.
func exportsBaseDir() string {
	return filepath.Join(config.ConfigDir(), "exports")
}

// exportJobDir is the per-job artifact directory, ~/.tclaude/exports/<id>.
func exportJobDir(id int64) string {
	return filepath.Join(exportsBaseDir(), strconv.FormatInt(id, 10))
}

// exportJobView is the JSON shape the dashboard sees from create + poll. It is
// deliberately a projection of db.ExportJob — no artifact_path (an internal
// on-disk detail), and a derived `ready` flag for the poller's convenience.
type exportJobView struct {
	ID           int64  `json:"id"`
	ConvID       string `json:"conv_id"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	Title        string `json:"title,omitempty"`
	Preset       string `json:"preset,omitempty"`
	ArtifactName string `json:"artifact_name,omitempty"`
	ArtifactSize int64  `json:"artifact_size,omitempty"`
	Ready        bool   `json:"ready"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

func exportJobToView(j *db.ExportJob) exportJobView {
	v := exportJobView{
		ID:           j.ID,
		ConvID:       j.ConvID,
		Status:       j.Status,
		Error:        j.Error,
		Title:        j.Title,
		Preset:       j.Preset,
		ArtifactName: j.ArtifactName,
		ArtifactSize: j.ArtifactSize,
		Ready:        j.Status == db.ExportStatusReady && j.ArtifactPath != "",
	}
	if !j.CreatedAt.IsZero() {
		v.CreatedAt = j.CreatedAt.Format(time.RFC3339Nano)
	}
	if !j.UpdatedAt.IsZero() {
		v.UpdatedAt = j.UpdatedAt.Format(time.RFC3339Nano)
	}
	return v
}

// --- create (dashboard) ---

// dashboardCreateExport is the cookie-authed entry point reached from
// handleDashboardAgentsAPI for `POST /api/agents/{conv}/export`. It resolves
// the selector to a conv-id and delegates to createExportJob.
func dashboardCreateExport(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	createExportJob(w, r, res.ConvID)
}

// createExportJob inserts a new export request for convID and nudges the
// agent's pane. Fast-fails when the agent has no live tmux session: an export
// needs a running agent to produce it, so there is no point queuing one.
func createExportJob(w http.ResponseWriter, r *http.Request, convID string) {
	var body struct {
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
		Preset       string `json:"preset"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "decode body: "+err.Error())
		return
	}

	title := strings.TrimSpace(body.Title)
	if len([]rune(title)) > exportTitleMax {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("title too long (max %d characters)", exportTitleMax))
		return
	}
	instructions := strings.TrimSpace(body.Instructions)
	if len(instructions) > exportInstructionsMax {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("instructions too long (max %d bytes)", exportInstructionsMax))
		return
	}

	// An export needs a live agent. Resolve the pane up front and fail fast
	// with a clear message rather than creating a job nobody will service.
	sess := aliveSessionForConv(convID)
	if sess == nil {
		writeError(w, http.StatusConflict, "agent_offline",
			"this agent has no running session — start it before exporting")
		return
	}

	id, err := db.InsertExportJob(&db.ExportJob{
		ConvID:       convID,
		Title:        title,
		Instructions: instructions,
		Preset:       strings.TrimSpace(body.Preset),
		Status:       db.ExportStatusRequested,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create export job: "+err.Error())
		return
	}

	// Inject only a fixed-format pointer — the agent fetches the actual brief
	// via `export show`. Only the integer id is interpolated, so no arbitrary
	// text rides send-keys (the mail-nudge safety property).
	nudge := fmt.Sprintf(
		"[system: the human requested an export of this conversation (request #%d). "+
			"Run: tclaude agent export show %d — it explains what to produce and how to deliver it.]",
		id, id)
	if err := injectTextAndSubmit(sess.TmuxSession+":0.0", nudge); err != nil {
		slog.Warn("export nudge failed", "error", err, "tmux", sess.TmuxSession, "job", id)
		if _, ferr := db.FailExportJob(id, "could not reach the agent's pane to start the export"); ferr != nil {
			slog.Warn("export: failed to record nudge failure", "error", ferr, "job", id)
		}
		writeError(w, http.StatusBadGateway, "nudge_failed",
			"could not reach the agent's pane to start the export")
		return
	}

	job, err := db.GetExportJob(id)
	if err != nil {
		// The job exists and the nudge landed; just echo a minimal view.
		writeJSON(w, http.StatusOK, exportJobView{ID: id, ConvID: convID, Status: db.ExportStatusRequested})
		return
	}
	writeJSON(w, http.StatusOK, exportJobToView(job))
}

// --- show (agent, /v1) ---

// handleExportShow serves `GET /v1/export-jobs/{id}` — `tclaude agent export
// show`. It returns the brief and flips a requested job to running so the
// dashboard can tell "the agent picked it up" from "still waiting".
func handleExportShow(w http.ResponseWriter, r *http.Request) {
	job, ok := exportJobFromPath(w, r)
	if !ok {
		return
	}
	if !requireExportJobAccess(w, r, job) {
		return
	}
	if moved, err := db.MarkExportJobRunning(job.ID); err != nil {
		slog.Warn("export: mark running failed", "error", err, "job", job.ID)
	} else if moved {
		job.Status = db.ExportStatusRunning
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           job.ID,
		"conv_id":      job.ConvID,
		"status":       job.Status,
		"title":        job.Title,
		"instructions": job.Instructions,
		"preset":       job.Preset,
		"submit_hint": fmt.Sprintf(
			"tclaude agent export submit %d <file> [more files…]", job.ID),
	})
}

// --- submit (agent, /v1) ---

// handleExportSubmit serves `POST /v1/export-jobs/{id}/artifact` — `tclaude
// agent export submit`. The raw request body is the artifact bytes (the CLI
// has already zipped multiple files into one); the download filename rides the
// `name` query param. The job flips to ready.
func handleExportSubmit(w http.ResponseWriter, r *http.Request) {
	job, ok := exportJobFromPath(w, r)
	if !ok {
		return
	}
	if !requireExportJobAccess(w, r, job) {
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxExportArtifactBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("artifact exceeds the %d MiB limit", maxExportArtifactBytes>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "io", "read upload: "+err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty", "the uploaded artifact is empty")
		return
	}

	name := sanitizeExportFilename(r.URL.Query().Get("name"))
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	dir := exportJobDir(job.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create export dir: "+err.Error())
		return
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "write artifact: "+err.Error())
		return
	}

	if _, err := db.SetExportJobReady(job.ID, path, name, int64(len(data)), contentType); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "record artifact: "+err.Error())
		return
	}
	slog.Info("export artifact received", "job", job.ID, "conv", job.ConvID,
		"name", name, "bytes", len(data))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     job.ID,
		"status": db.ExportStatusReady,
		"name":   name,
		"size":   len(data),
	})
}

// --- poll + download (dashboard, /api) ---

// handleDashboardExportJobsAPI dispatches the cookie-authed dashboard routes:
//
//	GET /api/export-jobs/{id}          → poll the job status (JSON)
//	GET /api/export-jobs/{id}/artifact → download the artifact (attachment)
func handleDashboardExportJobsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/export-jobs/")
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "expected /api/export-jobs/{id}", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	job, err := db.GetExportJob(id)
	if errors.Is(err, db.ErrExportJobNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such export job")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if len(parts) > 1 && parts[1] == "artifact" {
		serveExportArtifact(w, job)
		return
	}
	writeJSON(w, http.StatusOK, exportJobToView(job))
}

// serveExportArtifact streams a ready job's artifact as a browser download.
func serveExportArtifact(w http.ResponseWriter, job *db.ExportJob) {
	if job.Status != db.ExportStatusReady || job.ArtifactPath == "" {
		writeError(w, http.StatusConflict, "not_ready",
			"this export is not ready yet (status: "+job.Status+")")
		return
	}
	f, err := os.Open(job.ArtifactPath)
	if err != nil {
		writeError(w, http.StatusGone, "missing", "the export artifact is no longer available")
		return
	}
	defer func() { _ = f.Close() }()

	name := job.ArtifactName
	if name == "" {
		name = "export.bin"
	}
	ct := job.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Tclaude-Export-Filename", name)
	if job.ArtifactSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(job.ArtifactSize, 10))
	}
	if _, err := io.Copy(w, f); err != nil {
		slog.Warn("export artifact: write response failed", "job", job.ID, "error", err)
	}
}

// --- helpers ---

// exportJobFromPath reads {id} from a /v1/export-jobs/{id}[/...] route and
// loads the job, writing a 400/404 and returning ok=false on failure.
func exportJobFromPath(w http.ResponseWriter, r *http.Request) (*db.ExportJob, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "bad export job id")
		return nil, false
	}
	job, err := db.GetExportJob(id)
	if errors.Is(err, db.ErrExportJobNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such export job")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return nil, false
	}
	return job, true
}

// requireExportJobAccess authorizes a /v1 export endpoint. The human operator
// always passes; a confirmed agent passes only for its OWN job (conv match).
// Anything else fails closed. Mirrors requirePermissionEx's class handling.
func requireExportJobAccess(w http.ResponseWriter, r *http.Request, job *db.ExportJob) bool {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return true
	case classAgent:
		if p.ConvID == job.ConvID {
			return true
		}
		writeError(w, http.StatusForbidden, "auth",
			"this export job belongs to another conversation")
		return false
	case classUnidentified:
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to evaluate access")
		return false
	case classAgentUnknown:
		writeError(w, http.StatusForbidden, "auth",
			"caller has a Claude Code ancestor but no resolvable conv-id")
		return false
	default: // classUnconfirmed
		writeUnconfirmed(w)
		return false
	}
}

// sanitizeExportFilename reduces a caller-supplied name to a safe base
// filename: path separators stripped, control characters dropped, length
// bounded, with a sensible default. The result is only ever joined under the
// per-job directory, so this is defence-in-depth against traversal.
func sanitizeExportFilename(name string) string {
	name = strings.TrimSpace(name)
	// Take the base only — kill any directory components (incl. Windows "\").
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == 0 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	// Reject the path-y leftovers Base can return for degenerate input.
	if name == "" || name == "." || name == ".." {
		return "export.zip"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// --- cleanup sweep ---

// startExportJobsCleanup runs runExportJobsCleanup immediately and then on a
// ticker until stop closes — the same shape as startSudoGrantsCleanup.
func startExportJobsCleanup(stop <-chan struct{}) {
	go func() {
		runExportJobsCleanup(time.Now())
		t := time.NewTicker(exportJobsCleanupInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runExportJobsCleanup(now)
			}
		}
	}()
}

// runExportJobsCleanup (1) times out requested/running jobs whose agent never
// delivered, and (2) deletes terminal jobs + their artifacts past the TTL.
func runExportJobsCleanup(now time.Time) {
	stale, err := db.ListStaleExportJobs(now.Add(-exportJobStaleTimeout), false)
	if err != nil {
		slog.Warn("export cleanup: list stale failed", "error", err)
	}
	for _, j := range stale {
		if j.Status != db.ExportStatusRequested && j.Status != db.ExportStatusRunning {
			continue
		}
		if _, err := db.FailExportJob(j.ID,
			fmt.Sprintf("the agent did not deliver an export within %s", exportJobStaleTimeout)); err != nil {
			slog.Warn("export cleanup: timeout fail failed", "error", err, "job", j.ID)
		}
	}

	old, err := db.ListStaleExportJobs(now.Add(-exportJobTTL), true)
	if err != nil {
		slog.Warn("export cleanup: list terminal failed", "error", err)
	}
	for _, j := range old {
		if err := os.RemoveAll(exportJobDir(j.ID)); err != nil {
			slog.Warn("export cleanup: remove artifact dir failed", "error", err, "job", j.ID)
		}
		if _, err := db.DeleteExportJob(j.ID); err != nil {
			slog.Warn("export cleanup: delete row failed", "error", err, "job", j.ID)
		}
	}
}
