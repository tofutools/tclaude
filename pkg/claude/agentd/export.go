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
	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// export.go is the daemon half of the per-agent "📋 summary…" export
// (JOH-265, made clone-based in JOH-266). Unlike the group export
// (groups_export.go), which is a SYNCHRONOUS mechanical dump the daemon builds
// from DB rows + raw .jsonl, this is an ASYNCHRONOUS, agent-produced export.
//
// To avoid disturbing the live original — interrupting its in-flight work,
// polluting its context, or racing its turns — the export does NOT nudge the
// original. It spawns an isolated CLONE of the conversation (a copy of the
// .jsonl on a fresh conv-id / session), lets the CLONE produce the summary, then
// RETIRES the clone (JOH-267) rather than deleting it. The clone's dollar spend
// is recorded in session_cost_daily, whose conv_id is denormalised at write time
// so it survives the conversation being deleted either way — so retire does NOT
// rescue an otherwise-lost cost. What it preserves is the clone's conv_index
// row, so its cost line keeps a real title (`…-summary-writer-clone`) instead of
// rendering under the `(unknown)` placeholder, and it surfaces the clone in the
// dashboard's Retired group. The summary still attaches to the ORIGINAL: the
// job's conv_id stays the original (history list + download), while
// worker_conv_id is the throwaway clone (who is nudged, who submits, whose
// identity the /v1 gate accepts). The round-trip:
//
//	1. Dashboard POST /api/agents/{conv}/export {title, instructions, preset,
//	   same_group} → createExportJob: validates the conversation is cloneable,
//	   inserts a job (status=cloning, conv_id=original), and returns immediately.
//	   A background goroutine (runExportClone) clones the original, records
//	   worker_conv_id, waits for the clone's pane, renames it, flips the job to
//	   requested, and injects a one-line pointer nudge into the CLONE's pane. The
//	   instructions are NOT injected — only the integer job id is — keeping
//	   send-keys free of arbitrary text (the mail-nudge idiom).
//	2. The clone runs `tclaude agent export show N` → GET /v1/export-jobs/{id}:
//	   returns the brief and flips the job to running.
//	3. The clone runs `tclaude agent export submit N <files…>` → POST
//	   /v1/export-jobs/{id}/artifact: stores the uploaded bytes under
//	   ~/.tclaude/exports/<id>/, flips the job to ready, and RETIRES the clone.
//	4. Dashboard polls GET /api/export-jobs/{id} until ready, then downloads
//	   GET /api/export-jobs/{id}/artifact (Content-Disposition attachment).
//
// By default the clone is standalone (no group) so peers / cron / multicast
// can't touch the throwaway; "Clone into the same group" (same_group) joins the
// original's group(s) and permission posture (but never its group ownership) so
// the summary writer can ping peers.
//
// The /v1 endpoints are self-scoped: a confirmed agent may only touch its OWN
// job (original conv OR the worker clone); the human operator may touch any.
// The /api endpoints are the cookie-authed dashboard twin.

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
	// exportHistoryLimit caps how many past exports the "Previous exports"
	// panel lists per agent. Bounds the payload; older ones still exist
	// until the TTL sweep or a manual clear removes them.
	exportHistoryLimit = 50
)

// Cleanup cadence and retention. Package vars so flow tests can shrink them.
var (
	// exportJobStaleTimeout: a requested/running job whose agent never
	// delivered is flipped to failed after this long, so the state stops
	// lying and the TTL sweep can eventually reclaim it.
	exportJobStaleTimeout = 30 * time.Minute
	// exportJobTTL: a terminal (ready/failed) job and its on-disk artifact
	// are deleted this long after they settled — a generous backstop so the
	// "Previous exports" history stays useful, with manual clear for sooner.
	exportJobTTL = 30 * 24 * time.Hour
	// exportJobsCleanupInterval: how often the sweep runs.
	exportJobsCleanupInterval = 10 * time.Minute
)

// exportsBaseDir is ~/.tclaude/data/exports — the root for every job's
// artifact. Exports hold conversation transcripts, so they live under the
// private data/ subtree denied to sandboxed agents.
func exportsBaseDir() string {
	return filepath.Join(config.DataDir(), "exports")
}

// exportJobDir is the per-job artifact directory, ~/.tclaude/data/exports/<id>.
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

// dashboardExportJob is the Jobs-tab view of one export job: the modal's poll
// projection plus a resolved display label for the ORIGINAL conversation — the
// unified /api/jobs listing (dashboard_jobs.go) spans all agents, so each row
// carries its own label instead of the front-end doing a per-row lookup.
type dashboardExportJob struct {
	exportJobView
	ConvLabel string `json:"conv_label,omitempty"`
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

// createExportJob inserts a new export request for convID and kicks off the
// clone that will produce it (JOH-266). It does NOT require the original to be
// online — a clone resumes from the .jsonl — so instead of the old offline
// fast-fail it validates that the conversation is locatable to clone. The clone
// is spawned + nudged asynchronously (runExportClone); this returns immediately
// with a status=cloning job for the dashboard to poll.
func createExportJob(w http.ResponseWriter, r *http.Request, convID string) {
	var body struct {
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
		Preset       string `json:"preset"`
		// SameGroup opts the clone into the original's group(s) so the summary
		// writer can ping peers. Default false → a standalone, isolated throwaway.
		SameGroup bool `json:"same_group"`
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

	// The export runs on an isolated clone, so the original need not be online —
	// but we DO need to be able to locate the conversation (its cwd) to spawn the
	// clone into. Fail fast with a clear message if it can't be resolved.
	cwd, effort, model, _, ok := resolveConvLaunchMetadata(convID)
	if !ok || strings.TrimSpace(cwd) == "" {
		writeError(w, http.StatusConflict, "not_cloneable",
			"can't locate this conversation to clone for export "+
				"(no session, conversation index, or harness metadata with a working directory)")
		return
	}

	id, err := db.InsertExportJob(&db.ExportJob{
		ConvID:       convID,
		Title:        title,
		Instructions: instructions,
		Preset:       strings.TrimSpace(body.Preset),
		Status:       db.ExportStatusCloning,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create export job: "+err.Error())
		return
	}

	// Clone + nudge asynchronously: copying the conversation and waiting for the
	// clone's pane to come online takes seconds. The dashboard shows a spinner
	// over the 'cloning' phase and polls the job (keyed on the original).
	goBackground(func() {
		runExportClone(id, convID, cwd, effort, model, body.SameGroup)
	})

	job, err := db.GetExportJob(id)
	if err != nil {
		// The job exists and the clone is being spawned; echo a minimal view.
		writeJSON(w, http.StatusOK, exportJobView{ID: id, ConvID: convID, Status: db.ExportStatusCloning})
		return
	}
	writeJSON(w, http.StatusOK, exportJobToView(job))
}

// exportCloneTitleSuffix is appended to the original's title to name the
// throwaway summary writer (`<original-title>-summary-writer-clone`). The suffix
// also makes the clone recognisable at a glance in the roster / `agent ls`.
const exportCloneTitleSuffix = "-summary-writer-clone"

// runExportClone is the async body behind a clone-based export (JOH-266). It
// clones the original conversation, records the clone as the job's worker, waits
// for the clone's pane, renames it, flips the job to requested, and nudges the
// CLONE to produce the summary. Any failure flips the job to failed AND reaps the
// clone so a throwaway never leaks. Runs under goBackground so flow tests can drain it.
func runExportClone(jobID int64, originalConv, cwd, effort, model string, sameGroup bool) {
	// 1. Clone the original (copy path → the clone carries the full history) and
	// spawn it as its own session. Reuses the internal clone spawn so the race
	// handling (poll for the new conv-id + tmux registration) is shared.
	srcHarness := harnessForConv(originalConv).Name
	cloneSandbox := sandboxForHarness(srcHarness)
	codexGitCommonDir, gerr := spawnGitCommonDir(srcHarness, cloneSandbox, cwd)
	if gerr != nil {
		slog.Warn("export clone: resolve codex git common dir failed", "job", jobID, "orig", originalConv, "error", gerr)
		failExportJobAndReap(jobID, "", "could not clone the conversation to export it: "+gerr.Error())
		return
	}
	newConv, _, _, warn, spawnErr := cloneSpawnOnce(originalConv, cwd, false /* copy conv */, effort, model, "", false, nil, codexGitCommonDir, nil)
	if spawnErr != nil {
		slog.Warn("export clone: spawn failed", "job", jobID, "orig", originalConv, "error", spawnErr.Msg)
		failExportJobAndReap(jobID, "", "could not clone the conversation to export it: "+spawnErr.Msg)
		return
	}
	if warn != "" {
		// The conv-id + .jsonl exist but the clone's tmux session registered
		// slowly; waitForConvAlive below decides whether it actually came up.
		slog.Warn("export clone: spawn registered slowly", "job", jobID, "conv", newConv, "warning", warn)
	}
	// Record the clone as the job's worker ASAP — before it is nudged. This is
	// MANDATORY, not best-effort: an unrecorded clone is rejected by the /v1
	// ownership gate (it matches neither conv_id nor worker_conv_id) AND is
	// invisible to the cleanup sweep, so nudging it would both doom the export
	// and leak the clone. On a write error, or if the job was deleted/cleared
	// out from under us (zero rows updated), reap the clone instead of nudging.
	// (There remains an irreducible narrow window between the spawn above and
	// this write where a daemon crash leaves a never-recorded clone to reap by
	// hand — but a live daemon never proceeds past an unconfirmed assignment.)
	if updated, err := db.SetExportJobWorkerConv(jobID, newConv); err != nil {
		slog.Warn("export clone: record worker conv failed", "job", jobID, "conv", newConv, "error", err)
		failExportJobAndReap(jobID, newConv, "could not record the export clone for this job")
		return
	} else if !updated {
		slog.Warn("export clone: job vanished before worker conv recorded; reaping clone",
			"job", jobID, "conv", newConv)
		deleteSummaryWriterClone(newConv)
		return
	}

	// 2. Wait for the clone's pane to come online before touching it.
	if !waitForConvAlive(newConv) {
		slog.Warn("export clone: never came online", "job", jobID, "conv", newConv)
		failExportJobAndReap(jobID, newConv, "the export clone never came online")
		return
	}

	// 3. By default the clone is standalone (no group / identity) so peers, cron
	// and multicast can't touch the throwaway. same_group joins the original's
	// group(s) and inherits its permission posture so the summary writer can ping
	// peers — but NOT group OWNERSHIP: a throwaway summary writer needs no
	// administrative control over the group. Done only after the pane is alive so
	// a clone that never comes up is never briefly a (dead) group member.
	if sameGroup {
		if _, _, err := db.EnsureAgentForConv(newConv, "export-clone"); err != nil {
			slog.Warn("export clone: ensure actor failed", "job", jobID, "conv", newConv, "error", err)
		}
		members, perms := snapshotConvIdentity(originalConv)
		applyClonedIdentity(newConv, "system:export-clone", members, perms, nil /* never inherit ownership */)
	}

	// 4. Rename the clone so it is identifiable as the throwaway summary writer —
	// a clean line of its own, settled before the nudge (same ordering trap as
	// the clone post-init). Best-effort: a rename miss doesn't block the export.
	title := exportCloneTitle(originalConv)
	if isValidRenameTitle(title) {
		if !deliverRename(newConv, title) {
			slog.Warn("export clone: rename failed", "job", jobID, "conv", newConv, "title", title)
		}
		time.Sleep(reincarnateReadyDelay)
	}

	// 5. Flip cloning → requested BEFORE the nudge, so the clone's `export show`
	// (requested → running) lands on a job already past 'cloning'.
	if _, err := db.MarkExportJobRequested(jobID); err != nil {
		slog.Warn("export clone: mark requested failed", "job", jobID, "error", err)
	}

	// 6. Queue the request through the universal inbox. A transient pane or
	// input failure now follows the normal durable retry path instead of
	// destroying the export job and clone after one send-keys attempt.
	body := fmt.Sprintf(
		"The human requested an export of this conversation (request #%d). "+
			"Run: tclaude agent export show %d — it explains what to produce and how to deliver it.",
		jobID, jobID)
	if _, err := queueAgentMessage(&db.AgentMessage{
		FromConv:         "",
		ToConv:           newConv,
		Subject:          "Conversation export request",
		Body:             body,
		ToRecipients:     []string{newConv},
		OperatorAuthored: true,
	}); err != nil {
		slog.Warn("export clone: queue request failed", "job", jobID, "conv", newConv, "error", err)
		failExportJobAndReap(jobID, newConv, "could not queue the export request for its clone")
		return
	}
	slog.Info("export clone ready to summarize", "job", jobID, "orig", originalConv, "clone", newConv)
}

// exportCloneTitle is the clone's title: `<original-title>-summary-writer-clone`,
// or a bare `summary-writer-clone` when the original has no resolvable title.
// Resolved through the harness-native store first (Codex titles), then the
// conv_index path (CC) — the same precedence the clone handler uses.
func exportCloneTitle(originalConv string) string {
	base := ""
	if t, ok := harnessNativeTitle(originalConv); ok {
		base = t
	} else if row := agent.FreshConvRowResolved(originalConv); row != nil {
		base = agent.DisplayTitle(row)
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return strings.TrimPrefix(exportCloneTitleSuffix, "-")
	}
	return base + exportCloneTitleSuffix
}

// snapshotConvIdentity reads a conversation's group memberships and permission
// overrides — the identity a same_group export clone inherits. Group OWNERSHIP
// is deliberately NOT snapshotted: a throwaway summary writer should be able to
// message peers (membership) and act with the original's permission posture, but
// never administer the group. Best-effort: a read error logs and yields an empty
// slice for that facet rather than failing the export (the background goroutine
// has no HTTP response to fail to). The clone handler does the equivalent
// snapshot inline with fatal HTTP errors; the export path is best-effort.
func snapshotConvIdentity(convID string) (members []*db.AgentGroupMember, perms map[string]string) {
	groups, err := db.ListGroupsForConv(convID)
	if err != nil {
		slog.Warn("export clone: snapshot groups failed", "conv", convID, "error", err)
	}
	for _, g := range groups {
		m, err := db.FindMemberInGroup(g.ID, convID)
		if err != nil {
			slog.Warn("export clone: snapshot membership failed", "group", g.ID, "conv", convID, "error", err)
			continue
		}
		if m != nil {
			members = append(members, m)
		}
	}
	if perms, err = db.ListAgentPermissionOverridesForConv(convID); err != nil {
		slog.Warn("export clone: snapshot perms failed", "conv", convID, "error", err)
	}
	return members, perms
}

// failExportJobAndReap flips a job to failed with reason AND tears down its clone
// (when known) so a throwaway never leaks. The shared failure path for every
// runExportClone bail-out.
func failExportJobAndReap(jobID int64, cloneConv, reason string) {
	if _, err := db.FailExportJob(jobID, reason); err != nil {
		slog.Warn("export clone: record failure failed", "job", jobID, "error", err)
	}
	deleteSummaryWriterClone(cloneConv)
}

// retireSummaryWriterClone tears down a summary-writer clone that did real work
// (it ran a summary turn, so it has a cost line) by force-killing the pane and
// then RETIRING the conversation instead of deleting it (JOH-267): retireAgentConv
// keeps conv_index + sessions + the .jsonl and only demotes the agent (unjoin
// groups, revoke perms, flip enrollment → retired).
//
// The point is NOT to rescue the cost — the dollar figure lives in
// session_cost_daily (conv_id denormalised there precisely so it survives a
// delete), so the spend stays in the Costs totals either way. Keeping conv_index
// is what lets the Costs line show the clone's title instead of the `(unknown)`
// placeholder, and the clone shows up in the dashboard's "Retired" group.
//
// EnsureAgentForConv first (a same_group clone already has an actor; a
// standalone one is minted here) so RetireAgent has an active actor to flip.
// Retire is non-destructive, so a same_group clone's peer agent_messages /
// history rows are left behind (delete would have purged them) — harmless
// residue, and the reason a retired same_group clone can appear as a Retired
// mailbox folder.
//
// Best-effort and idempotent. NEVER call with the original conv-id: callers guard
// worker_conv_id != conv_id so the original is never touched.
func retireSummaryWriterClone(cloneConv string) {
	if cloneConv == "" {
		return
	}
	stopOneConv(cloneConv, true /* force kill — the clone is done */)
	if _, _, err := db.EnsureAgentForConv(cloneConv, "export-clone"); err != nil {
		slog.Warn("export clone: ensure-actor-before-retire failed", "conv", cloneConv, "error", err)
	}
	if _, _, err := retireAgentConv(cloneConv, "system:export-clone",
		"export complete — retired to preserve cost"); err != nil {
		slog.Warn("export clone: retire failed", "conv", cloneConv, "error", err)
	} else {
		cleanupAgentDirectoriesAfterRetire(cloneConv, true)
	}
}

// deleteSummaryWriterClone tears down a summary-writer clone that did NOT do
// billable work — an early failure (never came online, spawn failed, the job
// vanished before the worker was recorded). It force-kills the pane, then purges
// the conversation (DB rows + .jsonl): there is no cost worth preserving, so a
// retired entry would just be clutter. Compare retireSummaryWriterClone, used on
// the success + timeout paths. Best-effort and idempotent — an empty id or an
// already-gone session/conv is a no-op. NEVER call with the original conv-id:
// callers guard worker_conv_id != conv_id so the original is never deleted.
func deleteSummaryWriterClone(cloneConv string) {
	if cloneConv == "" {
		return
	}
	stopOneConv(cloneConv, true /* force kill */)
	if _, err := removeAgentDirectoriesForConv(cloneConv); err != nil {
		slog.Warn("export clone: agent-owned directory cleanup failed", "conv", cloneConv, "error", err)
		return
	}
	if _, err := conv.DeleteConvByID(cloneConv); err != nil {
		slog.Warn("export clone: delete conv failed", "conv", cloneConv, "error", err)
	}
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
	// A job that has already delivered an artifact is done — refuse a
	// duplicate submit up front (before reading/writing the body) rather than
	// clobbering the delivered artifact. A late submit on a requested/running/
	// failed job is still accepted (it can revive a timed-out export).
	if job.Status == db.ExportStatusReady {
		writeError(w, http.StatusConflict, "already_delivered",
			"this export has already been delivered")
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

	updated, err := db.SetExportJobReady(job.ID, path, name, int64(len(data)), contentType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "record artifact: "+err.Error())
		return
	}
	if !updated {
		// The job vanished (deleted / cleared) between our fetch and this
		// write, so nothing owns the bytes we just wrote — drop them and
		// report it rather than claiming a success the dashboard can't poll.
		if rerr := os.RemoveAll(dir); rerr != nil {
			slog.Warn("export: cleanup after lost job failed", "error", rerr, "job", job.ID)
		}
		writeError(w, http.StatusGone, "job_gone",
			"the export job no longer exists — nothing to deliver to")
		return
	}
	slog.Info("export artifact received", "job", job.ID, "conv", job.ConvID,
		"worker", job.WorkerConvID, "name", name, "bytes", len(data))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     job.ID,
		"status": db.ExportStatusReady,
		"name":   name,
		"size":   len(data),
	})
	// The export is delivered — tear down the throwaway clone that produced it.
	// RETIRE it rather than delete (JOH-267): it ran a summary turn, so keeping
	// its conv_index keeps that cost line labelled with the clone's title (instead
	// of `(unknown)`) and surfaces it as a retired agent. Done AFTER the response
	// (in the background) so the clone's `export submit` sees its 200 before its
	// pane is killed. The worker != conv guard makes it impossible to ever touch
	// the ORIGINAL conversation.
	if job.WorkerConvID != "" && job.WorkerConvID != job.ConvID {
		clone := job.WorkerConvID
		goBackground(func() { retireSummaryWriterClone(clone) })
	}
}

// --- poll + download (dashboard, /api) ---

// handleDashboardExportJobsAPI dispatches the cookie-authed dashboard routes:
//
//	GET    /api/export-jobs/{id}          → poll the job status (JSON)
//	GET    /api/export-jobs/{id}/artifact → download the artifact (attachment)
//	DELETE /api/export-jobs/{id}          → delete one export + its artifact
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
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		serveExportArtifact(w, job)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, exportJobToView(job))
	case http.MethodDelete:
		deleteExportJobAndArtifact(job.ID)
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": job.ID})
	default:
		http.Error(w, "GET or DELETE only", http.StatusMethodNotAllowed)
	}
}

// dashboardListExports serves `GET /api/agents/{conv}/exports` — the modal's
// "Previous exports" history for one agent, newest first.
func dashboardListExports(w http.ResponseWriter, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	jobs, err := db.ListExportJobsForConv(res.ConvID, exportHistoryLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	views := make([]exportJobView, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, exportJobToView(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"exports": views})
}

// dashboardClearExports serves `DELETE /api/agents/{conv}/exports` — the
// "clear all" control: removes every export job + artifact for the agent.
func dashboardClearExports(w http.ResponseWriter, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	ids, err := db.DeleteExportJobsForConv(res.ConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	for _, id := range ids {
		if rerr := os.RemoveAll(exportJobDir(id)); rerr != nil {
			slog.Warn("export clear: remove artifact dir failed", "error", rerr, "job", id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(ids)})
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
// always passes; a confirmed agent passes for its OWN job — either as the
// ORIGINAL conversation (conv_id) or as the worker CLONE that actually produces
// the export (worker_conv_id, JOH-266). Anything else fails closed. Mirrors
// requirePermissionEx's class handling.
func requireExportJobAccess(w http.ResponseWriter, r *http.Request, job *db.ExportJob) bool {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return true
	case classAgent:
		// The clone is the normal caller (it was nudged to run show/submit); the
		// original conv is allowed too (harmless — the export is about it).
		if p.ConvID == job.ConvID || (job.WorkerConvID != "" && p.ConvID == job.WorkerConvID) {
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
		writeUnconfirmed(w, r)
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
	// Drop path separators, NUL, control chars, and the quote/semicolon that
	// would break the Content-Disposition header this name is interpolated into.
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == 0 || r == '"' || r == ';' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	// Reject the path-y leftovers Base can return for degenerate input.
	if name == "" || name == "." || name == ".." {
		return "export.zip"
	}
	// Bound the length on rune boundaries so truncation never splits a
	// multi-byte character into invalid UTF-8.
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
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
		// A job stuck mid-flight — cloning (clone never came up), requested or
		// running (the clone never delivered) — is timed out so the state stops
		// lying and the TTL sweep can reclaim it.
		if j.Status != db.ExportStatusCloning &&
			j.Status != db.ExportStatusRequested &&
			j.Status != db.ExportStatusRunning {
			continue
		}
		moved, err := db.FailExportJob(j.ID,
			fmt.Sprintf("the agent did not deliver an export within %s", exportJobStaleTimeout))
		if err != nil {
			slog.Warn("export cleanup: timeout fail failed", "error", err, "job", j.ID)
			continue
		}
		// Reap the throwaway clone we spawned for this job so it never leaks —
		// only when we actually transitioned it to failed (a job that raced to
		// ready meanwhile keeps its clone reaped by the submit path). The
		// worker != conv guard makes touching the original impossible.
		//
		// A requested/running clone was nudged and most likely ran a summary turn,
		// so it has a labelled cost line worth keeping → RETIRE (JOH-267). A clone
		// still in 'cloning' was never nudged (a daemon-crash orphan, since a live
		// waitForConvAlive failure would already have failed+deleted it inline), so
		// it ran no summary turn → DELETE; nothing to label. (A requested clone
		// that crashed between the requested-flip and the nudge is the imperfect
		// edge — it gets retired with no cost line; rare and harmless.)
		if moved && j.WorkerConvID != "" && j.WorkerConvID != j.ConvID {
			if j.Status == db.ExportStatusRequested || j.Status == db.ExportStatusRunning {
				retireSummaryWriterClone(j.WorkerConvID)
			} else {
				deleteSummaryWriterClone(j.WorkerConvID)
			}
		}
	}

	old, err := db.ListStaleExportJobs(now.Add(-exportJobTTL), true)
	if err != nil {
		slog.Warn("export cleanup: list terminal failed", "error", err)
	}
	for _, j := range old {
		deleteExportJobAndArtifact(j.ID)
	}
}

// deleteExportJobAndArtifact removes a job's on-disk artifact directory and its
// DB row — the shared teardown used by the TTL sweep and the manual delete /
// clear-all controls. Best-effort: a failure is logged, never fatal.
func deleteExportJobAndArtifact(id int64) {
	if err := os.RemoveAll(exportJobDir(id)); err != nil {
		slog.Warn("export: remove artifact dir failed", "error", err, "job", id)
	}
	if _, err := db.DeleteExportJob(id); err != nil {
		slog.Warn("export: delete row failed", "error", err, "job", id)
	}
}
