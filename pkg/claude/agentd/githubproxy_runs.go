package agentd

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// githubproxy_runs.go serves the GitHub Actions verbs: listing runs, reading
// the log of what failed, and pulling what a run uploaded.
//
// These are the three verbs that do not merely relay a JSON document. `run
// log-failed` assembles one answer from a jobs listing and a zip archive;
// `run download` writes bulk bytes into the agent's work tree. Both used to be
// one `gh` invocation each, and doing them here is what lets the daemon bound
// an unpack while it is happening rather than measuring the wreckage after.

const (
	// maxGHLogTransferBytes caps a CI-log transfer: the run's whole log
	// archive, or one job's log on the fallback path. A run's archive is
	// downloaded in full because GitHub offers no per-step endpoint, so the
	// bound has to sit on the transfer.
	//
	// It is generous on purpose, and it costs no memory to be: the archive goes
	// to a temp file and a job log goes into a fixed-size tail buffer, so the
	// only thing this figure bounds is time and disk. A tight bound would turn
	// a verbose matrix leg — exactly the run someone is reading `log-failed`
	// for — into silence.
	maxGHLogTransferBytes = 512 << 20

	// maxGHJobLogBytes caps a single step's log read out of the run archive.
	// It feeds straight into the response, so it is held in memory.
	maxGHJobLogBytes = 32 << 20

	// ghJobsPerPage is the jobs-listing page size. GitHub's maximum.
	ghJobsPerPage = 100
)

// ---------------------------------------------------------------------------
// run list
// ---------------------------------------------------------------------------

type ghProxyRunListRequest struct {
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ghRunListWire is the REST shape of a workflow-run listing.
type ghRunListWire struct {
	Runs []struct {
		ID           int64  `json:"id"`
		HeadSHA      string `json:"head_sha"`
		RunAttempt   int    `json:"run_attempt"`
		Conclusion   string `json:"conclusion"`
		Status       string `json:"status"`
		Name         string `json:"name"`
		DisplayTitle string `json:"display_title"`
		HeadBranch   string `json:"head_branch"`
		Event        string `json:"event"`
		CreatedAt    string `json:"created_at"`
		HTMLURL      string `json:"html_url"`
	} `json:"workflow_runs"`
}

// ghRunListEntry is the rendered shape.
//
// `databaseId` leads rather than sitting alphabetically among the rest because
// it is the field that matters: it is the id every other run verb takes.
// `headSha` and `attempt` follow because they are what distinguish a superseded
// run from a current one, and a re-run from a first try.
type ghRunListEntry struct {
	DatabaseID   int64  `json:"databaseId"`
	HeadSHA      string `json:"headSha"`
	Attempt      int    `json:"attempt"`
	Conclusion   string `json:"conclusion"`
	Status       string `json:"status"`
	WorkflowName string `json:"workflowName"`
	DisplayTitle string `json:"displayTitle"`
	HeadBranch   string `json:"headBranch"`
	Event        string `json:"event"`
	CreatedAt    string `json:"createdAt"`
	URL          string `json:"url"`
}

// handleGHProxyRunList serves POST /v1/github/run/list — the run ids
// `run log-failed` needs.
//
// Without it the only route to a run id is regexing one out of the detailsUrl
// buried in a `pr checks` rollup, which works and reads like a workaround.
//
// It also reaches runs `pr checks` cannot show, though not the ones you might
// expect. A status-check rollup is scoped to the pull request's HEAD COMMIT, so
// a force-push or an amend takes every run against the superseded commit out
// of `pr checks` entirely, while `run ls --branch` still lists them. Re-runs
// are NOT such a case: re-running does not create a new run, it adds an
// attempt to the same run id, and both `pr checks` and `run list` then report
// that latest attempt's conclusion.
func handleGHProxyRunList(w http.ResponseWriter, r *http.Request) {
	var body ghProxyRunListRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	limit, fault := validateGHLimitInt(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	query := url.Values{"per_page": []string{strconv.Itoa(limit)}}
	if branch := strings.TrimSpace(body.Branch); branch != "" {
		// The same gate every other ref parameter passes. It reaches a query
		// string rather than argv now, but a branch name is still the one
		// caller-supplied string here with structure worth checking.
		if fault := validateBranchName(branch); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		query.Set("branch", branch)
	}
	status, fault := validateGHRunStatus(body.Status)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if status != "" {
		query.Set("status", status)
	}
	raw, failure, err := g.rest(r.Context(), ghAPIRequest{
		Path:  g.repoPath("actions/runs"),
		Query: query,
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "run.list", failure, err)
		return
	}
	var wire ghRunListWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		g.respond(w, r, "run.list", ProxyResult{},
			fmt.Errorf("could not read the run listing: %w", err))
		return
	}
	out := make([]ghRunListEntry, 0, len(wire.Runs))
	for _, run := range wire.Runs {
		out = append(out, ghRunListEntry{
			DatabaseID: run.ID, HeadSHA: run.HeadSHA, Attempt: run.RunAttempt,
			Conclusion: run.Conclusion, Status: run.Status, WorkflowName: run.Name,
			DisplayTitle: run.DisplayTitle, HeadBranch: run.HeadBranch, Event: run.Event,
			CreatedAt: run.CreatedAt, URL: run.HTMLURL,
		})
	}
	g.respondJSON(w, r, "run.list", out)
}

// ---------------------------------------------------------------------------
// run log-failed
// ---------------------------------------------------------------------------

// ghProxyRunRequest names one GitHub Actions workflow run.
type ghProxyRunRequest struct {
	Remote string `json:"remote,omitempty"`
	RunID  int64  `json:"run_id"`
}

type ghRunStatusWire struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ghJobWire struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Steps      []struct {
		Name       string `json:"name"`
		Number     int    `json:"number"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

// handleGHProxyRunLogFailed serves POST /v1/github/run/log-failed — the log of
// whatever steps failed in a workflow run.
//
// It is the other half of `pr checks`: checks says WHICH job went red, this
// says why. Only the failed steps, never the whole run: the full log of a green
// matrix build is megabytes of noise that would blow the response bound and
// tell the agent nothing it did not already know from the check rollup.
//
// The run id is not derived from anything — an agent takes it from `run ls` or
// from the `detailsUrl` in a `pr checks` rollup — but it is still bounded by
// the same repository, because every request is built under the derived slug
// and a run id belonging to another repository simply 404s.
func handleGHProxyRunLogFailed(w http.ResponseWriter, r *http.Request) {
	var body ghProxyRunRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	runID, fault := validateGHRunID(body.RunID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ghProxyLogTimeout)
	defer cancel()

	// The run's own status first. A run still in progress has no complete log
	// archive, and saying so is a different answer from "nothing failed" —
	// which is what an empty result would otherwise be read as.
	raw, failure, err := g.restBounded(ctx, ghProxyTimeout, ghAPIRequest{
		Path: g.repoPath("actions/runs/%s", runID),
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "run.log-failed", failure, err)
		return
	}
	var run ghRunStatusWire
	if err := json.Unmarshal(raw, &run); err != nil {
		g.respond(w, r, "run.log-failed", ProxyResult{},
			fmt.Errorf("could not read the run: %w", err))
		return
	}
	if run.Status != "completed" {
		g.respond(w, r, "run.log-failed", ghResultFromError(fmt.Sprintf(
			"run %d is %s; its log is only available once it completes",
			runID, strings.ReplaceAll(run.Status, "_", " "))), nil)
		return
	}

	jobs, partial, failure, err := g.runJobs(ctx, runID)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "run.log-failed", failure, err)
		return
	}
	text, err := g.failedStepLogs(ctx, runID, jobs)
	if err != nil {
		g.respond(w, r, "run.log-failed", ProxyResult{}, err)
		return
	}
	if partial {
		text += fmt.Sprintf(
			"\n(this run has more than %d jobs; the rest were not inspected, so a failure among "+
				"them is not reported here)\n", maxGHPaginatedPages*ghJobsPerPage)
	}
	// A run with no failed steps prints nothing and succeeds. Silence means the
	// run is green, not that the read failed — the same answer this verb has
	// always given, and the docs say so.
	tail, truncated := ghTailText(text, maxGHProxyTextBytes)
	g.respond(w, r, "run.log-failed", ProxyResult{Stdout: tail, Truncated: truncated}, nil)
}

// runJobs lists every job in a run, following pagination.
//
// The truncated flag is returned rather than dropped: a run past the page cap
// loses jobs, and a `log-failed` that reported a subset as though it were the
// whole run would have an agent conclude a red job it never saw was green.
func (g *ghProxySession) runJobs(ctx context.Context, runID int64) ([]ghJobWire, bool, *ProxyResult, error) {
	var jobs []ghJobWire
	truncated, failure, err := g.restPaginated(ctx, ghProxyTimeout, ghAPIRequest{
		Path: g.repoPath("actions/runs/%s/jobs", runID),
		Query: url.Values{
			"per_page": []string{strconv.Itoa(ghJobsPerPage)},
			// The latest attempt, which is what a re-run leaves behind and
			// what the check rollup reports. Reading an earlier attempt's log
			// is not something this proxy offers.
			"filter": []string{"latest"},
		},
	}, func(page []byte) error {
		var batch struct {
			Jobs []ghJobWire `json:"jobs"`
		}
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("could not read the run's jobs: %w", err)
		}
		jobs = append(jobs, batch.Jobs...)
		return nil
	})
	return jobs, truncated, failure, err
}

// ghLogArchive indexes a downloaded run-log archive by entry name.
type ghLogArchive struct {
	reader *zip.ReadCloser
	path   string
	files  map[string]*zip.File
}

func (a *ghLogArchive) close() {
	if a == nil {
		return
	}
	if a.reader != nil {
		_ = a.reader.Close()
	}
	if a.path != "" {
		_ = os.Remove(a.path)
	}
}

// failedStepLogs renders the log of every failed step across the run's jobs.
//
// The archive is fetched once and consulted per step, with a per-job fallback
// for the case GitHub does not put a job's steps in it — which happens, and
// which would otherwise report a red job as having no explanation.
func (g *ghProxySession) failedStepLogs(ctx context.Context, runID int64, jobs []ghJobWire) (string, error) {
	var failed []ghJobWire
	for _, job := range jobs {
		if ghIsFailure(job.Conclusion) {
			failed = append(failed, job)
		}
	}
	if len(failed) == 0 {
		return "", nil
	}
	// Only downloaded once there is something to look for. A green run should
	// not cost a log-archive transfer.
	archive, err := g.fetchRunLogArchive(ctx, runID)
	if err != nil {
		// Not fatal: the per-job fallback below reaches the same text one
		// request at a time, and a run whose archive has expired or was never
		// assembled is exactly when it earns its place.
		archive = nil
	}
	defer archive.close()

	var b strings.Builder
	for _, job := range failed {
		wrote := false
		for _, step := range job.Steps {
			if !ghIsFailure(step.Conclusion) {
				continue
			}
			entry := archive.find(job.Name, step.Number)
			if entry == nil {
				continue
			}
			text, err := readZipEntry(entry, maxGHJobLogBytes)
			if err != nil {
				continue
			}
			writePrefixedLog(&b, job.Name, step.Name, text)
			wrote = true
		}
		if wrote {
			continue
		}
		// Nothing in the archive for this job's failed steps. Fall back to the
		// job's own log, which is the whole job rather than the failed step —
		// more than was asked for, and far better than reporting a red job with
		// no explanation at all.
		text, failure, err := g.jobLog(ctx, job.ID)
		if err != nil || failure != nil {
			// Said out loud rather than skipped. A red job that contributes
			// nothing to the output is indistinguishable from a run where
			// nothing failed, which is the one conclusion that would make an
			// agent stop looking.
			fmt.Fprintf(&b, "%s\t%s\t(the log for this failed job could not be read)\n",
				job.Name, ghFailedStepName(job))
			continue
		}
		writePrefixedLog(&b, job.Name, ghFailedStepName(job), text)
	}
	return b.String(), nil
}

// ghFailedStepName names the step a fallback log is attributed to. The first
// failed step is the useful label; a job that failed without any step doing so
// (a runner that died, a cancelled matrix leg) gets a name that says as much
// rather than an empty column.
func ghFailedStepName(job ghJobWire) string {
	for _, step := range job.Steps {
		if ghIsFailure(step.Conclusion) {
			return step.Name
		}
	}
	return "(job)"
}

// ghIsFailure reports whether a conclusion is one an agent reading
// `log-failed` wants the log for. `cancelled` and `timed_out` are included
// because both leave a log that says why, and a timeout in particular is the
// case where the log is the entire diagnosis.
func ghIsFailure(conclusion string) bool {
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "failure", "timed_out", "startup_failure", "cancelled":
		return true
	}
	return false
}

// writePrefixedLog renders a step's log with each line labelled by job and
// step, so a matrix build's interleaved output stays attributable.
func writePrefixedLog(b *strings.Builder, jobName, stepName, text string) {
	// An empty log is not one blank labelled line. SplitSeq over "" yields a
	// single empty element, which would render as `job\tstep\t` and read like
	// a step that logged a blank line rather than one that logged nothing.
	if strings.TrimRight(text, "\n") == "" {
		return
	}
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(jobName)
		b.WriteByte('\t')
		b.WriteString(stepName)
		b.WriteByte('\t')
		b.WriteString(strings.TrimRight(line, "\r"))
		b.WriteByte('\n')
	}
}

// fetchRunLogArchive downloads the run's log archive to a temp file and opens
// it.
//
// To a file rather than to memory: the archive is the one response in this verb
// whose size is set by how much CI logged, and holding a matrix build's whole
// log in the daemon's heap to read a few kilobytes out of it is the wrong
// trade.
func (g *ghProxySession) fetchRunLogArchive(ctx context.Context, runID int64) (*ghLogArchive, error) {
	f, err := os.CreateTemp("", "tclaude-ghlogs-*.zip")
	if err != nil {
		return nil, fmt.Errorf("could not stage the run log archive: %w", err)
	}
	name := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(name)
	}
	res, err := ghStream(ctx, g.token, ghAPIRequest{
		Path: g.repoPath("actions/runs/%s/logs", runID),
	}, f, maxGHLogTransferBytes)
	if err != nil {
		cleanup()
		return nil, err
	}
	if res.Status < 200 || res.Status > 299 {
		cleanup()
		return nil, fmt.Errorf("the run's log archive could not be read (HTTP %d)", res.Status)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return nil, fmt.Errorf("could not finish staging the run log archive: %w", err)
	}
	reader, err := zip.OpenReader(name)
	if err != nil {
		_ = os.Remove(name)
		return nil, fmt.Errorf("the run's log archive could not be opened: %w", err)
	}
	archive := &ghLogArchive{reader: reader, path: name, files: map[string]*zip.File{}}
	for _, file := range reader.File {
		archive.files[file.Name] = file
	}
	return archive, nil
}

// ghLogNameSanitizer matches the characters GitHub cannot put in an archive
// entry name. It replaces each with the same placeholder on both sides of a
// comparison, so a job called "test (ubuntu/latest)" still matches the entry
// GitHub wrote it to.
var ghLogNameSanitizer = regexp.MustCompile(`[/\\:<>|*?"]`)

func ghNormalizeLogName(s string) string {
	return strings.ToLower(ghLogNameSanitizer.ReplaceAllString(s, "_"))
}

// find locates the archive entry holding one step's log.
//
// GitHub names it `<job name>/<step number>_<step name>.txt`, but neither half
// is a literal copy of what the API reports: characters illegal in a filename
// are substituted, and a long name is truncated. So the lookup goes from exact
// to increasingly forgiving, and stops at the first hit:
//
//  1. the directory and step-number prefix, compared literally;
//  2. the same, compared with both sides sanitized the same way;
//  3. the only entry in the whole archive carrying that step number, when no
//     directory matched the job at all — which is what a truncated job name
//     looks like from here. "Only" is load-bearing: in a matrix build several
//     jobs have a step 3, and guessing between them would attribute one leg's
//     failure to another.
//
// Returning nil is a normal outcome, not a failure: the caller falls back to
// the job's own log, which needs no name matching at all.
//
// Entry names are walked in sorted order throughout. Ranging a map would make
// the winner between two equally-good candidates depend on hash order, and a
// log read that differs run to run is worse than one that is merely wrong.
func (a *ghLogArchive) find(jobName string, stepNumber int) *zip.File {
	if a == nil {
		return nil
	}
	names := make([]string, 0, len(a.files))
	for name := range a.files {
		if strings.HasSuffix(name, ".txt") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	prefix := fmt.Sprintf("%s/%d_", jobName, stepNumber)
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			return a.files[name]
		}
	}
	wantDir := ghNormalizeLogName(jobName)
	wantStep := strconv.Itoa(stepNumber) + "_"
	var loose []*zip.File
	for _, name := range names {
		dir, base := path.Split(name)
		if !strings.HasPrefix(base, wantStep) {
			continue
		}
		if ghNormalizeLogName(strings.TrimSuffix(dir, "/")) == wantDir {
			return a.files[name]
		}
		loose = append(loose, a.files[name])
	}
	// No directory matched the job, and exactly one entry in the archive
	// carries this step number. That is the truncated-job-name case, and it is
	// unambiguous.
	if len(loose) == 1 {
		return loose[0]
	}
	return nil
}

// readZipEntry reads one archive entry, bounded.
func readZipEntry(file *zip.File, max int64) (string, error) {
	rc, err := file.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, max))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// jobLog reads one job's complete log as text, keeping the TAIL.
//
// It streams into a tail buffer rather than going through the ordinary bounded
// read, and the difference is the whole point. A job log has no size limit —
// a verbose matrix leg runs to hundreds of megabytes — and an in-memory read
// bounded from the FRONT would hand back the first N bytes of a log whose
// failure is at the end. Nothing downstream could recover from that: the caller
// tails the assembled text, and tailing a head-truncated log yields the middle
// of a log rather than the end of one.
//
// The tail buffer is the same size as the response's own bound, because that is
// all the caller will ever see of it — which is also why the transfer cap can
// afford to be generous: the bytes stream through a fixed-size window.
func (g *ghProxySession) jobLog(ctx context.Context, jobID int64) (string, *ProxyResult, error) {
	runCtx, cancel := ghCallContext(ctx, ghProxyTimeout)
	defer cancel()

	tail := newProxyTail(maxGHProxyTextBytes)
	res, err := ghStream(runCtx, g.token, ghAPIRequest{
		Path:   g.repoPath("actions/jobs/%s/logs", jobID),
		Accept: "application/vnd.github.raw",
	}, tail, maxGHLogTransferBytes)
	if err != nil {
		return "", nil, fmt.Errorf("could not read the job's log: %w", err)
	}
	if res.Status < 200 || res.Status > 299 {
		return "", ghFailureFor(ghAPIResult{Status: res.Status, Body: res.Body, Header: http.Header{}}), nil
	}
	return tail.String(), nil, nil
}

// ---------------------------------------------------------------------------
// run artifacts
// ---------------------------------------------------------------------------

// ghProxyRunDownloadRequest names one run's artifacts to fetch.
//
// There is deliberately no destination field — see artifactDest for why a
// caller-supplied path is the one parameter this proxy cannot accept.
type ghProxyRunDownloadRequest struct {
	Remote string `json:"remote,omitempty"`
	RunID  int64  `json:"run_id"`
	Name   string `json:"name,omitempty"`
}

// ghArtifactsPerPage is how many artifacts one manifest read covers. It is the
// endpoint's own maximum; a run with more is reported honestly rather than
// paginated, because a partial page is a fact the caller needs to know about
// (see checkArtifactBudget) rather than something to paper over.
const ghArtifactsPerPage = 100

// ghArtifactManifest is the manifest, projected down to the fields that decide
// anything: what an artifact is called, how big it is, and whether it is still
// there.
//
// A projection rather than a passthrough because each raw entry embeds the
// complete `workflow_run` object plus several API urls, which together dwarf
// the six fields a caller acts on.
//
// `total` is the endpoint's own `total_count` and is NOT decoration. This reads
// ONE page, so `total` above that many is how both the daemon and the caller
// learn that the array is a page and not the run — and for `run download`,
// which without a `--name` fetches every artifact in the run rather than every
// artifact on the page, it is the difference between a real size check and one
// made on a fraction of the bytes.
type ghArtifactManifest struct {
	Total     int              `json:"total"`
	Artifacts []ghArtifactInfo `json:"artifacts"`
}

type ghArtifactInfo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size_in_bytes"`
	Expired   bool   `json:"expired"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

// ghArtifactsWire is the endpoint's own shape, before projection.
type ghArtifactsWire struct {
	TotalCount int              `json:"total_count"`
	Artifacts  []ghArtifactInfo `json:"artifacts"`
}

// handleGHProxyRunArtifacts serves POST /v1/github/run/artifacts — what a run
// produced, before deciding whether to pull it.
//
// It is a separate verb rather than an implementation detail of `run download`
// because the sizes are the whole point: an artifact is routinely hundreds of
// megabytes, `run download` refuses past maxGHArtifactBytes, and "list, then
// choose" is how a caller avoids finding that out the slow way.
func handleGHProxyRunArtifacts(w http.ResponseWriter, r *http.Request) {
	var body ghProxyRunRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	runID, fault := validateGHRunID(body.RunID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	manifest, failure, err := g.artifactManifest(r.Context(), runID)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "run.artifacts", failure, err)
		return
	}
	g.respondJSON(w, r, "run.artifacts", manifest)
}

// artifactManifest reads one run's artifact list.
func (g *ghProxySession) artifactManifest(ctx context.Context, runID int64) (ghArtifactManifest, *ProxyResult, error) {
	raw, failure, err := g.restBounded(ctx, ghProxyTimeout, ghAPIRequest{
		Path:  g.repoPath("actions/runs/%s/artifacts", runID),
		Query: url.Values{"per_page": []string{strconv.Itoa(ghArtifactsPerPage)}},
	})
	if failure != nil || err != nil {
		return ghArtifactManifest{}, failure, err
	}
	var wire ghArtifactsWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		// The daemon cannot make the size decision it is here to make, and must
		// not proceed as though it had.
		return ghArtifactManifest{}, nil,
			fmt.Errorf("the run's artifact list could not be understood: %w", err)
	}
	artifacts := wire.Artifacts
	if artifacts == nil {
		artifacts = []ghArtifactInfo{}
	}
	return ghArtifactManifest{Total: wire.TotalCount, Artifacts: artifacts}, nil, nil
}

// ---------------------------------------------------------------------------
// run download
// ---------------------------------------------------------------------------

// handleGHProxyRunDownload serves POST /v1/github/run/download — a run's
// artifacts, on disk, in the agent's own work tree.
//
// This is the one verb whose effect is a filesystem write rather than a
// response, which shapes everything about it:
//
//   - The destination is computed, never named (artifactDest). A caller who
//     could name it could aim the unsandboxed daemon at any path it can reach.
//   - The manifest is read FIRST, and the download is refused if the total
//     exceeds maxGHArtifactBytes. Without that preflight the only bound on what
//     lands on the operator's disk is what CI happened to upload.
//   - The preflight also answers "no artifact by that name" and "it expired"
//     precisely, where a bare download reports both as one unhelpful failure.
//
// Every call shares one budget, so the daemon's worst case stays the number the
// CLI is waiting on however the work divides between them.
func handleGHProxyRunDownload(w http.ResponseWriter, r *http.Request) {
	var body ghProxyRunDownloadRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	runID, fault := validateGHRunID(body.RunID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	name, fault := validateGHArtifactName(body.Name)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ghProxyDownloadTimeout)
	defer cancel()

	manifest, failure, err := g.artifactManifest(ctx, runID)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "run.download", failure, err)
		return
	}
	if fault := checkArtifactBudget(manifest, name); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	dest, fault := g.artifactDest(strconv.FormatInt(runID, 10))
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}

	wanted := selectArtifacts(manifest, name)
	// The remaining unpacked budget travels down through the extraction, so a
	// zip bomb is stopped WHILE it expands rather than measured afterwards.
	// That is the one thing the daemon could not do while a child process
	// owned the unzip, and it closes the residual risk the docs used to name.
	budget := maxGHArtifactUnpackedBytes
	for _, artifact := range wanted {
		// Empty means "unpack straight into the download directory", which is
		// what naming one artifact asks for.
		into := ""
		if name == "" {
			// Without a name every artifact is fetched, each into its own
			// subdirectory, so two artifacts holding a file of the same name
			// do not overwrite one another.
			into = ghArtifactSubdir(artifact.Name)
		}
		written, failure, err := g.downloadArtifact(ctx, dest, into, artifact, budget)
		if failure != nil || err != nil {
			discardArtifactDest(dest)
			if errors.Is(err, errArtifactUnpackTooLarge) {
				writeProxyFault(w, faultf(http.StatusRequestEntityTooLarge, "artifact_too_large",
					"that artifact was under the %s download limit compressed but unpacked past the %s "+
						"the proxy will leave on disk, so it has been deleted; a small archive that "+
						"expands like this is usually machine-generated data rather than something to "+
						"read — take a narrower artifact with `--name`",
					humanBytes(maxGHArtifactBytes), humanBytes(maxGHArtifactUnpackedBytes)))
				return
			}
			g.respondOrFail(w, r, "run.download", failure, err)
			return
		}
		budget -= written
	}

	listing, _ := artifactListing(ctx, dest)
	g.respond(w, r, "run.download", ProxyResult{Stdout: listing}, nil)
}

// ghArtifactSubdir renders the per-artifact subdirectory name for a
// whole-run download. Artifact names are already gated by
// validateGHArtifactName on the way in, but the names here come from GITHUB
// rather than from the caller, and an artifact uploaded by a fork's pull
// request is named by whoever opened it — so they are sanitized rather than
// trusted.
func ghArtifactSubdir(name string) string {
	cleaned := ghLogNameSanitizer.ReplaceAllString(name, "_")
	cleaned = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, cleaned)
	cleaned = strings.Trim(cleaned, " .")
	if cleaned == "" {
		return "artifact"
	}
	return cleaned
}

// selectArtifacts picks the live artifacts a download covers.
func selectArtifacts(manifest ghArtifactManifest, name string) []ghArtifactInfo {
	var out []ghArtifactInfo
	for _, a := range manifest.Artifacts {
		if a.Expired || (name != "" && a.Name != name) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// downloadArtifact fetches one artifact's zip and unpacks it under dest.
//
// root is the download's own directory and `into` is the relative path inside
// it — "" for a named download, which unpacks directly, or the artifact's own
// subdirectory for a whole-run one. Everything is written through an os.Root
// anchored at the download directory, so a zip entry naming `../../.ssh` fails
// rather than escaping (see extractZip).
func (g *ghProxySession) downloadArtifact(
	ctx context.Context, root, into string, artifact ghArtifactInfo, budget int64,
) (int64, *ProxyResult, error) {
	f, err := os.CreateTemp("", "tclaude-ghartifact-*.zip")
	if err != nil {
		return 0, nil, fmt.Errorf("could not stage the artifact download: %w", err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	res, err := ghStream(ctx, g.token, ghAPIRequest{
		Path: g.repoPath("actions/artifacts/%s/zip", artifact.ID),
	}, f, maxGHArtifactBytes)
	if err != nil {
		return 0, nil, fmt.Errorf("could not download the artifact %q: %w", artifact.Name, err)
	}
	if res.Status < 200 || res.Status > 299 {
		failure := ghFailureFor(ghAPIResult{Status: res.Status, Body: res.Body, Header: http.Header{}})
		return 0, failure, nil
	}
	if err := f.Close(); err != nil {
		return 0, nil, fmt.Errorf("could not finish staging the artifact download: %w", err)
	}
	written, err := extractZip(tmp, root, into, budget)
	if err != nil {
		return written, nil, err
	}
	return written, nil, nil
}

// errArtifactUnpackTooLarge is the sentinel for a download that expanded past
// what the proxy will leave on disk. A sentinel rather than a message, because
// the caller has to answer it with a 413 and a deletion rather than with the
// 502 every other extraction failure earns.
var errArtifactUnpackTooLarge = errors.New("artifact unpacked past the on-disk limit")

// extractZip unpacks an archive under root/into, refusing to write more than
// budget bytes.
//
// Two properties matter here and neither is optional:
//
//   - Every write goes through an os.Root anchored at the download directory,
//     which refuses a traversal out of it. A zip entry named `../../../.ssh/
//     authorized_keys` is a real archive an untrusted CI job can upload, and
//     the daemon that unpacks it is unsandboxed.
//   - The budget is checked AS BYTES ARE WRITTEN, not after. A declared
//     uncompressed size in a zip header is attacker-controlled, and measuring
//     afterwards means the disk is already full.
func extractZip(archivePath, root, into string, budget int64) (int64, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return 0, fmt.Errorf("the downloaded artifact is not a readable zip archive: %w", err)
	}
	defer func() { _ = reader.Close() }()

	dir, err := os.OpenRoot(root)
	if err != nil {
		return 0, fmt.Errorf("could not open the download directory: %w", err)
	}
	defer func() { _ = dir.Close() }()

	if into != "" {
		if err := dir.MkdirAll(into, 0o755); err != nil {
			return 0, fmt.Errorf("could not create %s: %w", into, err)
		}
	}

	var written int64
	for _, file := range reader.File {
		// A zip entry name is always slash-separated, whatever the platform
		// that wrote it. path.Clean plus the os.Root below is what keeps an
		// absolute or traversing name from landing anywhere but here.
		rel := path.Clean("/" + file.Name)[1:]
		if rel == "" || rel == "." {
			continue
		}
		if into != "" {
			rel = path.Join(into, rel)
		}
		if file.FileInfo().IsDir() {
			if err := dir.MkdirAll(rel, 0o755); err != nil {
				return written, fmt.Errorf("could not create %s: %w", rel, err)
			}
			continue
		}
		if parent := path.Dir(rel); parent != "." {
			if err := dir.MkdirAll(parent, 0o755); err != nil {
				return written, fmt.Errorf("could not create %s: %w", parent, err)
			}
		}
		n, err := extractZipFile(dir, rel, file, budget-written)
		written += n
		if err != nil {
			return written, err
		}
		if written > budget {
			return written, errArtifactUnpackTooLarge
		}
	}
	return written, nil
}

// extractZipFile writes one entry, bounded.
//
// Every non-directory entry becomes a REGULAR FILE, whatever mode bits the
// archive declares. A zip can describe a symlink, and creating one would hand
// an untrusted archive a second route out of the destination — this time
// through a link the agent could later follow rather than through the entry
// name os.Root already guards. A symlink entry therefore lands as an ordinary
// file containing its target path, which is inert.
func extractZipFile(dir *os.Root, rel string, file *zip.File, remaining int64) (int64, error) {
	rc, err := file.Open()
	if err != nil {
		return 0, fmt.Errorf("could not read %s from the artifact: %w", file.Name, err)
	}
	defer func() { _ = rc.Close() }()

	// O_EXCL is not used: an archive may legitimately overwrite within itself,
	// and the directory was emptied before this download began.
	out, err := dir.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("could not write %s: %w", rel, err)
	}
	defer func() { _ = out.Close() }()

	// remaining+1, so the caller sees the overrun rather than a file silently
	// cut off at the budget.
	n, err := io.Copy(out, io.LimitReader(rc, remaining+1))
	if err != nil {
		return n, fmt.Errorf("could not write %s: %w", rel, err)
	}
	return n, nil
}

// checkArtifactBudget decides whether a download may proceed, from the
// manifest alone — before a byte is fetched.
//
// An empty name means every artifact, so the budget is the sum. Naming one
// narrows the budget to that one, which is the point of naming it.
//
// The two cases differ in more than arithmetic. `--name X` fetches exactly the
// artifact this function measured, so the check is exact even when the manifest
// is only a page. Without a name every artifact IN THE RUN is fetched, which is
// not the same set as every artifact on the page — so a run with more artifacts
// than one page holds is refused rather than sized against a fraction of what
// it would pull.
func checkArtifactBudget(manifest ghArtifactManifest, name string) *proxyFault {
	var (
		total        int64
		matched      int
		expiredMatch int
		liveNames    []string
	)
	for _, a := range manifest.Artifacts {
		if !a.Expired {
			liveNames = append(liveNames, a.Name)
		}
		if name != "" && a.Name != name {
			continue
		}
		if a.Expired {
			expiredMatch++
			continue
		}
		matched++
		total += a.Size
	}
	partial := manifest.Total > len(manifest.Artifacts)

	if matched == 0 {
		// Expired is a DIFFERENT answer from absent, and the one an agent is
		// most likely to retry on if not told plainly. GitHub keeps the entry
		// after the retention period takes the bytes.
		//
		// Both spellings are needed: without a name, EVERY artifact matched and
		// every one of them expired, which is a whole-run answer rather than a
		// statement about an artifact the caller never named.
		if expiredMatch > 0 && name != "" {
			return faultf(http.StatusNotFound, "artifact_expired",
				"the artifact %q exists but has expired — GitHub deleted the bytes after its "+
					"retention period and kept the entry; retrying will not bring it back", name)
		}
		if expiredMatch > 0 {
			// On a partial page this must NOT claim the run. retention-days is
			// per upload step, so a run can hold short-retention artifacts
			// beside long ones — and "all of them expired, retrying will not
			// help" would stop an agent that could still take a live one from
			// the artifacts this read never saw.
			if partial {
				return faultf(http.StatusNotFound, "artifact_expired",
					"all %d of the first artifacts inspected have expired, and this run has %d in "+
						"total — the rest were not inspected, so some may still be live; name one "+
						"with `--name` to find out", expiredMatch, manifest.Total)
			}
			return faultf(http.StatusNotFound, "artifact_expired",
				"all %d of this run's artifacts have expired — GitHub deleted the bytes after "+
					"their retention period and kept the entries; retrying will not bring them back",
				expiredMatch)
		}
		if name != "" {
			hint := ""
			if partial {
				hint = fmt.Sprintf(" (this run has %d artifacts and only the first %d were "+
					"inspected, so the name may be among the rest)", manifest.Total, len(manifest.Artifacts))
			}
			return faultf(http.StatusNotFound, "no_artifact",
				"this run has no live artifact named %q; it has: %s%s",
				name, artifactNameList(liveNames), hint)
		}
		return faultf(http.StatusNotFound, "no_artifact",
			"this run has no live artifacts (they expire, and a run that failed early "+
				"may never have uploaded any)")
	}
	if name == "" && partial {
		return faultf(http.StatusRequestEntityTooLarge, "artifact_too_large",
			"this run has %d artifacts, more than the %d the proxy inspects at once, so it "+
				"cannot size a download of all of them; take one with `--name` (the first %d are: %s)",
			manifest.Total, ghArtifactsPerPage, len(manifest.Artifacts), artifactNameList(liveNames))
	}
	if total > maxGHArtifactBytes {
		return faultf(http.StatusRequestEntityTooLarge, "artifact_too_large",
			"that is %s of artifacts and the proxy will not download more than %s at once; "+
				"use `run artifacts` to see the sizes and `--name` to take one",
			humanBytes(total), humanBytes(maxGHArtifactBytes))
	}
	return nil
}

// artifactNameList renders the names a caller can choose from, bounded — a
// full page of 255-character names is a 25 KB error message, and every other
// payload in this file is bounded.
func artifactNameList(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	if len(sorted) > maxGHArtifactNamesReported {
		return strings.Join(sorted[:maxGHArtifactNamesReported], ", ") +
			fmt.Sprintf(", … and %d more", len(sorted)-maxGHArtifactNamesReported)
	}
	return strings.Join(sorted, ", ")
}

// discardArtifactDest removes a download that must not be kept. Best effort by
// design: the caller is already reporting a failure, and a cleanup that could
// not run is not a second one worth reporting on top of it.
//
// It is safe as a plain RemoveAll despite the os.Root care taken to CREATE the
// path, because dest is the value artifactDest returned — built from the
// validated work-tree root and a re-formatted integer — rather than anything
// resolved again here.
func discardArtifactDest(dest string) {
	if dest != "" {
		_ = os.RemoveAll(dest)
	}
}
