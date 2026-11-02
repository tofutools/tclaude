package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// gitlabproxy_pipelines.go serves the GitLab CI verbs: listing pipelines,
// reading the trace of what failed in one, and pulling what a job uploaded.
//
// The unit differs from GitHub's. A GitHub run has jobs with steps and one log
// archive; a GitLab pipeline has jobs with one trace each, and artifacts
// belong to a JOB rather than to the pipeline. So `job log-failed` takes a
// pipeline id and reads each failed job's trace, while `job artifacts` and
// `job download` take a job id, because that is what owns the archive.

const (
	// maxGLTraceTransferBytes caps one job trace transfer; the tail buffer is
	// what the caller sees of it, so this bounds time rather than memory.
	maxGLTraceTransferBytes = 512 << 20

	// glJobsPerPage is the jobs-listing page size, GitLab's maximum.
	glJobsPerPage = 100
)

// ---------------------------------------------------------------------------
// pipeline list
// ---------------------------------------------------------------------------

type glProxyPipelineListRequest struct {
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// handleGLProxyPipelineList serves POST /v1/gitlab/pipeline/list — the ids
// `job log-failed` takes.
func handleGLProxyPipelineList(w http.ResponseWriter, r *http.Request) {
	var body glProxyPipelineListRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	limit, fault := validateGHLimitInt(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	query := url.Values{
		"per_page": []string{strconv.Itoa(limit)},
		"order_by": []string{"id"},
		"sort":     []string{"desc"},
	}
	if branch := strings.TrimSpace(body.Branch); branch != "" {
		if fault := validateBranchName(branch); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		query.Set("ref", branch)
	}
	status, fault := validateGLPipelineStatus(body.Status)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if status != "" {
		query.Set("status", status)
	}
	raw, failure, err := g.rest(r.Context(), glAPIRequest{Path: g.projectPath("pipelines"), Query: query})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pipeline.list", failure, err)
		return
	}
	var pipelines []glPipeline
	if err := json.Unmarshal(raw, &pipelines); err != nil {
		g.respond(w, r, "pipeline.list", ProxyResult{}, fmt.Errorf("could not read the pipeline listing: %w", err))
		return
	}
	out := make([]glPipelineOut, 0, len(pipelines))
	for i := range pipelines {
		if p := pipelines[i].project(); p != nil {
			out = append(out, *p)
		}
	}
	g.respondJSON(w, r, "pipeline.list", out)
}

// ---------------------------------------------------------------------------
// job log-failed
// ---------------------------------------------------------------------------

type glProxyPipelineRequest struct {
	Remote     string `json:"remote,omitempty"`
	PipelineID int64  `json:"pipeline_id"`
}

type glProxyJobRequest struct {
	Remote string `json:"remote,omitempty"`
	JobID  int64  `json:"job_id"`
}

// pipelineJobs lists a pipeline's jobs, latest attempt of each, following
// pagination. The truncated flag is returned for the reason runJobs returns
// it: a subset reported as the whole run hides a red job.
func (g *glProxySession) pipelineJobs(ctx context.Context, pipelineID int64) ([]glJob, bool, *ProxyResult, error) {
	var jobs []glJob
	truncated, failure, err := g.restPaginated(ctx, glProxyTimeout, glAPIRequest{
		Path:  g.projectPath("pipelines/%s/jobs", pipelineID),
		Query: url.Values{"per_page": []string{strconv.Itoa(glJobsPerPage)}},
	}, func(page []byte) error {
		var batch []glJob
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("could not read the pipeline's jobs: %w", err)
		}
		jobs = append(jobs, batch...)
		return nil
	})
	return jobs, truncated, failure, err
}

// glIsFailure reports whether a job status is one an agent reading
// `log-failed` wants the trace for. Cancelled is included for the reason the
// GitHub side includes it: the trace says why.
func glIsFailure(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "canceled", "cancelled":
		return true
	}
	return false
}

// handleGLProxyJobLogFailed serves POST /v1/gitlab/job/log-failed — the trace
// of every failed job in a pipeline.
//
// There is no per-step log in GitLab; a job's trace is the unit, so this is
// the whole trace of each failed job, tailed. A job that is allowed to fail
// is still reported, labelled as such, because its trace is still where the
// explanation is.
func handleGLProxyJobLogFailed(w http.ResponseWriter, r *http.Request) {
	var body glProxyPipelineRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	pipelineID, fault := validateGLID("pipeline", body.PipelineID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glProxyLogTimeout)
	defer cancel()

	jobs, partial, failure, err := g.pipelineJobs(ctx, pipelineID)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "job.log-failed", failure, err)
		return
	}
	var b strings.Builder
	running := 0
	// A trace that cannot be read keeps its placeholder in the output, and
	// the run reports failure, as the GitHub proxy does when half a read
	// fails: exit 0 would tell the agent it has every failure's trace.
	unreadable := 0
	var firstUnreadable string
	traceTrimmed := false
	for _, job := range jobs {
		switch strings.ToLower(job.Status) {
		case "running", "pending", "created", "waiting_for_resource", "preparing":
			running++
		}
		if !glIsFailure(job.Status) {
			continue
		}
		label := job.Stage
		if job.AllowFailure {
			label += " (allowed to fail)"
		}
		text, trimmed, failure, err := g.jobTrace(ctx, job.ID)
		traceTrimmed = traceTrimmed || trimmed
		if err != nil || failure != nil {
			fmt.Fprintf(&b, "%s\t%s\t(the trace for this failed job could not be read)\n", job.Name, label)
			if unreadable == 0 {
				reason := ""
				if err != nil {
					reason = err.Error()
				} else {
					reason = strings.TrimSpace(failure.Stderr)
				}
				firstUnreadable = fmt.Sprintf("the trace of job %s (%d) could not be read: %s", job.Name, job.ID, reason)
			}
			unreadable++
			continue
		}
		writePrefixedLog(&b, job.Name, label, text)
	}
	if running > 0 {
		fmt.Fprintf(&b, "\n(%d job(s) in this pipeline have not finished; a failure among them is not reported yet)\n", running)
	}
	if partial {
		fmt.Fprintf(&b, "\n(this pipeline has more than %d jobs; the rest were not inspected)\n",
			maxGLPaginatedPages*glJobsPerPage)
	}
	tail, truncated := ghTailText(b.String(), maxGHProxyTextBytes)
	out := ProxyResult{Stdout: tail, Truncated: truncated || traceTrimmed}
	if unreadable > 0 {
		out.ExitCode = ghExitFailure
		out.Stderr = firstUnreadable
		if unreadable > 1 {
			out.Stderr += fmt.Sprintf(" (and %d more)", unreadable-1)
		}
	}
	g.respond(w, r, "job.log-failed", out, nil)
}

// jobTrace reads one job's trace, keeping the tail, through the stream seam
// for the reason jobLog does: a trace has no size limit and the failure is at
// the end.
func (g *glProxySession) jobTrace(ctx context.Context, jobID int64) (string, bool, *ProxyResult, error) {
	runCtx, cancel := ghCallContext(ctx, glProxyTimeout)
	defer cancel()

	tail := newProxyTail(maxGHProxyTextBytes)
	res, err := glStream(runCtx, g.base, g.token, glAPIRequest{
		Path:   g.projectPath("jobs/%s/trace", jobID),
		Accept: "text/plain",
	}, tail, maxGLTraceTransferBytes)
	if err != nil {
		return "", false, nil, fmt.Errorf("could not read the job's trace: %w", err)
	}
	if res.Status < 200 || res.Status > 299 {
		return "", false, glFailureFor(glAPIResult{Status: res.Status, Body: res.Body, Header: http.Header{}}), nil
	}
	// The tail keeps the END of the trace; whether it dropped the start is
	// the caller's to report, since the text it returns no longer shows it.
	return tail.String(), tail.Truncated(), nil, nil
}

// ---------------------------------------------------------------------------
// job artifacts
// ---------------------------------------------------------------------------

// glJobArtifactsOut is what a job uploaded: GitLab's per-type artifact
// entries for it (the archive, its metadata, the trace, a junit report…) with
// sizes, so a caller can decide before downloading. It does not open the
// archive to list the files inside.
type glJobArtifactsOut struct {
	JobID     int64  `json:"jobId"`
	Name      string `json:"name"`
	Stage     string `json:"stage"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expiresAt"`
	Expired   bool   `json:"expired"`
	Archive   *struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"archive"`
	Files []glArtifactFileOut `json:"files"`
}

type glArtifactFileOut struct {
	FileType string `json:"fileType"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// job reads one job.
func (g *glProxySession) job(ctx context.Context, jobID int64) (*glJob, *ProxyResult, error) {
	raw, failure, err := g.restBounded(ctx, glProxyTimeout, glAPIRequest{Path: g.projectPath("jobs/%s", jobID)})
	if failure != nil || err != nil {
		return nil, failure, err
	}
	var job glJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, nil, fmt.Errorf("could not read the job: %w", err)
	}
	if job.ID == 0 {
		notFound := ghResultFromError(fmt.Sprintf("no job %d in %s", jobID, g.project))
		return nil, &notFound, nil
	}
	return &job, nil, nil
}

// glArtifactsExpired reports whether the job's artifacts are past their
// expiry. An unparseable or absent stamp counts as live: GitLab omits it for
// artifacts kept indefinitely.
func glArtifactsExpired(job *glJob, now time.Time) bool {
	stamp := strings.TrimSpace(job.ArtifactsExpireAt)
	if stamp == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return false
	}
	return t.Before(now)
}

func projectGLJobArtifacts(job *glJob, now time.Time) glJobArtifactsOut {
	out := glJobArtifactsOut{
		JobID: job.ID, Name: job.Name, Stage: job.Stage, Status: job.Status,
		ExpiresAt: job.ArtifactsExpireAt, Expired: glArtifactsExpired(job, now),
		Archive: job.ArtifactsFile, Files: []glArtifactFileOut{},
	}
	for _, f := range job.Artifacts {
		out.Files = append(out.Files, glArtifactFileOut{FileType: f.FileType, Filename: f.Filename, Size: f.Size})
	}
	return out
}

// handleGLProxyJobArtifacts serves POST /v1/gitlab/job/artifacts.
func handleGLProxyJobArtifacts(w http.ResponseWriter, r *http.Request) {
	var body glProxyJobRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	jobID, fault := validateGLID("job", body.JobID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	job, failure, err := g.job(r.Context(), jobID)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "job.artifacts", failure, err)
		return
	}
	g.respondJSON(w, r, "job.artifacts", projectGLJobArtifacts(job, time.Now()))
}

// ---------------------------------------------------------------------------
// job download
// ---------------------------------------------------------------------------

// handleGLProxyJobDownload serves POST /v1/gitlab/job/download — a job's
// artifact archive, unpacked into the agent's own work tree under
// .tclaude-artifacts/job-<id>/, with every bound the GitHub download has: a
// computed destination, a size preflight from the job's own manifest, and an
// unpack budget enforced as bytes are written.
func handleGLProxyJobDownload(w http.ResponseWriter, r *http.Request) {
	var body glProxyJobRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	jobID, fault := validateGLID("job", body.JobID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glProxyDownloadTimeout)
	defer cancel()

	job, failure, err := g.job(ctx, jobID)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "job.download", failure, err)
		return
	}
	if job.ArtifactsFile == nil {
		writeProxyFault(w, faultf(http.StatusNotFound, "no_artifact",
			"job %d (%s) has no artifact archive; a job that failed early may never have uploaded one, "+
				"and GitLab removes the archive once it expires", job.ID, job.Name))
		return
	}
	if glArtifactsExpired(job, time.Now()) {
		writeProxyFault(w, faultf(http.StatusNotFound, "artifact_expired",
			"the artifacts of job %d (%s) expired at %s; GitLab deleted the archive and retrying will not bring it back",
			job.ID, job.Name, job.ArtifactsExpireAt))
		return
	}
	if job.ArtifactsFile.Size > maxGHArtifactBytes {
		writeProxyFault(w, faultf(http.StatusRequestEntityTooLarge, "artifact_too_large",
			"that is %s of artifacts and the proxy will not download more than %s at once",
			humanBytes(job.ArtifactsFile.Size), humanBytes(maxGHArtifactBytes)))
		return
	}
	dest, fault := prepareArtifactDest(g.repoRoot, "job-"+strconv.FormatInt(jobID, 10))
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}

	f, err := os.CreateTemp("", "tclaude-glartifact-*.zip")
	if err != nil {
		g.respond(w, r, "job.download", ProxyResult{}, fmt.Errorf("could not stage the artifact download: %w", err))
		return
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()
	res, err := glStream(ctx, g.base, g.token, glAPIRequest{
		Path: g.projectPath("jobs/%s/artifacts", jobID),
	}, f, maxGHArtifactBytes)
	if err != nil {
		discardArtifactDest(dest)
		g.respond(w, r, "job.download", ProxyResult{}, fmt.Errorf("could not download the artifacts of job %d: %w", jobID, err))
		return
	}
	if res.Status < 200 || res.Status > 299 {
		discardArtifactDest(dest)
		g.respond(w, r, "job.download", *glFailureFor(glAPIResult{Status: res.Status, Body: res.Body, Header: http.Header{}}), nil)
		return
	}
	if err := f.Close(); err != nil {
		discardArtifactDest(dest)
		g.respond(w, r, "job.download", ProxyResult{}, fmt.Errorf("could not finish staging the artifact download: %w", err))
		return
	}
	if _, err := extractZip(tmp, dest, "", maxGHArtifactUnpackedBytes); err != nil {
		discardArtifactDest(dest)
		if errors.Is(err, errArtifactUnpackTooLarge) {
			writeProxyFault(w, faultf(http.StatusRequestEntityTooLarge, "artifact_too_large",
				"the archive was under the %s download limit compressed but unpacked past the %s the proxy "+
					"will leave on disk, so it has been deleted",
				humanBytes(maxGHArtifactBytes), humanBytes(maxGHArtifactUnpackedBytes)))
			return
		}
		g.respond(w, r, "job.download", ProxyResult{}, err)
		return
	}
	listing, _ := artifactListing(ctx, dest)
	g.respond(w, r, "job.download", ProxyResult{Stdout: listing}, nil)
}
