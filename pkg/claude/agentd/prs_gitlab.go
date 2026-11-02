package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// prs_gitlab.go teaches the presented-PR machinery to read a GitLab merge
// request where it would otherwise call `gh`. The dashboard's pills, the CI
// hover and the triggers all key on a URL, a number and a state; this file
// supplies those three for a merge request through the GitLab API, under
// the same policy gates the GitHub path applies.
//
// A merge-request URL is recognised by its grammar alone —
// `/<group>/…/<project>/-/merge_requests/<n>` is GitLab's and nobody else's —
// so a URL can be classified without a config read on the snapshot hot path.
// Whether the daemon may SPEND a credential on it is decided separately, by
// the same policy check every presented PR passes: the host must be a
// configured GitLab instance and the project must be on the allow-list.

// gitlabMRRef is a parsed merge-request URL.
type gitlabMRRef struct {
	// base is scheme://host[:port] as the URL spelled it; it must match the
	// instance's configured base before any request is made against it.
	base string
	// host is the DNS host without the port, which is how the allow-list and
	// gitlab_hosts spell it.
	host string
	// project is the full namespace path.
	project string
	number  int
}

// key identifies the merge request across every cache: the instance's whole
// origin (scheme, host and port), not just its host, so two instances on one
// host never share state.
func (r gitlabMRRef) key() string {
	return strings.ToLower(r.base+"/"+r.project) + "!" + strconv.Itoa(r.number)
}

// canonical renders the one spelling the strict validator accepts.
func (r gitlabMRRef) canonical() string {
	return r.base + "/" + r.project + "/-/merge_requests/" + strconv.Itoa(r.number)
}

// remote is the project as a remote reference, for the allow-list.
func (r gitlabMRRef) remote() remoteRef {
	return remoteRef{Scheme: "https", Host: r.host, Path: strings.Split(r.project, "/")}
}

// gitlabMRRefFromURL parses a merge-request URL. It accepts only https, a
// plain DNS host, and at least two path segments before the `/-/` marker,
// each a valid GitLab path segment.
func gitlabMRRefFromURL(rawURL string) (gitlabMRRef, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return gitlabMRRef{}, false
	}
	host := strings.ToLower(u.Hostname())
	if !validRemoteHost(host) {
		return gitlabMRRef{}, false
	}
	segs := pathSegments(u.Path)
	// group/project/-/merge_requests/<n> is the shortest form.
	if len(segs) < 5 || segs[len(segs)-3] != "-" || segs[len(segs)-2] != "merge_requests" {
		// Allow a trailing sub-path (/diffs, /pipelines) by scanning for the
		// marker rather than anchoring it at the end.
		idx := -1
		for i := 0; i+2 < len(segs); i++ {
			if segs[i] == "-" && segs[i+1] == "merge_requests" {
				idx = i
				break
			}
		}
		if idx < 2 {
			return gitlabMRRef{}, false
		}
		segs = segs[:idx+3]
	}
	n, err := strconv.Atoi(segs[len(segs)-1])
	if err != nil || n <= 0 || !isAllDigits(segs[len(segs)-1]) {
		return gitlabMRRef{}, false
	}
	project := segs[:len(segs)-3]
	for _, seg := range project {
		if !isGitLabPathSegment(seg) {
			return gitlabMRRef{}, false
		}
	}
	return gitlabMRRef{
		base:    strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host),
		host:    host,
		project: strings.Join(project, "/"),
		number:  n,
	}, true
}

// isGitLabPresentedPRURL reports whether a URL is a merge request.
func isGitLabPresentedPRURL(rawURL string) bool {
	_, ok := gitlabMRRefFromURL(rawURL)
	return ok
}

// isForgePresentedPRURL reports whether a URL names a pull or merge request
// the daemon knows how to refresh.
func isForgePresentedPRURL(rawURL string) bool {
	return isGitHubPresentedPRURL(rawURL) || isGitLabPresentedPRURL(rawURL)
}

// deriveForgePRNumber is deriveGitHubPRNumber for both forges: the number the
// dashboard pill shows.
func deriveForgePRNumber(rawURL string) int {
	if n := deriveGitHubPRNumber(rawURL); n > 0 {
		return n
	}
	if ref, ok := gitlabMRRefFromURL(rawURL); ok {
		return ref.number
	}
	return 0
}

// gitlabMRPolicyBase answers whether a merge-request URL may be refreshed
// with the operator's credential, and against which base. The URL's own
// scheme, host and port must be the configured instance's, so a URL that
// merely looks like a merge request on an allow-listed host cannot steer a
// request elsewhere, and the project must pass the allow-list as a remote.
func gitlabMRPolicyBase(ref gitlabMRRef, policy config.GitProxyConfig) (string, error) {
	base := policy.GitLabBase(ref.host)
	if base == "" {
		return "", fmt.Errorf("%s is not a configured GitLab instance (agent.git_proxy.gitlab_hosts)", ref.host)
	}
	if !strings.EqualFold(strings.TrimRight(base, "/"), ref.base) {
		return "", fmt.Errorf("merge request URL %s does not match the configured instance base %s", ref.base, base)
	}
	if len(policy.AllowedRemotes) == 0 || !remoteAllowed(ref.remote(), policy.AllowedRemotes) {
		return "", fmt.Errorf("project %s/%s is not on the operator's agent.git_proxy.allowed_remotes list", ref.host, ref.project)
	}
	return base, nil
}

// ---------------------------------------------------------------------------
// Reading a merge request for the dashboard
// ---------------------------------------------------------------------------

// gitlabPresentedPRTimeout bounds one dashboard-driven read: the merge
// request, and then its head pipeline's jobs.
const gitlabPresentedPRTimeout = 20 * time.Second

// gitlabPresentedPRInfo reads a merge request's title, state and head
// pipeline for the presented-PR refresh. It runs only when the git proxy is
// configured: the token and the instance both come from that policy, and
// without it there is no credential to spend.
func gitlabPresentedPRInfo(rawURL string) (presentedPRInfo, bool) {
	ref, ok := gitlabMRRefFromURL(rawURL)
	if !ok {
		return presentedPRInfo{}, false
	}
	cfg, err := config.Load()
	if err != nil || !cfg.GitProxyEnabled() {
		return presentedPRInfo{}, false
	}
	policy := cfg.ResolvedGitProxy()
	base, err := gitlabMRPolicyBase(ref, policy)
	if err != nil {
		return presentedPRInfo{}, false
	}
	token, fault := gitlabToken(policy)
	if fault != nil {
		return presentedPRInfo{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitlabPresentedPRTimeout)
	defer cancel()

	mr, ok := gitlabReadMR(ctx, base, token, ref)
	if !ok {
		return presentedPRInfo{}, false
	}
	checks := gitlabPipelineChecks(ctx, base, token, ref, mr.HeadPipeline, time.Now())
	return presentedPRInfo{
		Title:  truncateAuthoredPRTitle(glStripDraftPrefix(mr.Title)),
		Number: mr.IID,
		URL:    mr.WebURL,
		State:  prStateFromGitLab(mr.State, mr.Draft),
		Checks: &checks,
	}, true
}

// gitlabPRChecks is the hover-driven resolver's GitLab half: the head
// pipeline's jobs as a check list, plus the merge request's state so a merged
// one stops being polled.
func gitlabPRChecks(rawURL string) (prChecksInfo, bool) {
	ref, ok := gitlabMRRefFromURL(rawURL)
	if !ok {
		return prChecksInfo{}, false
	}
	cfg, err := config.Load()
	if err != nil || !cfg.GitProxyEnabled() {
		return prChecksInfo{}, false
	}
	policy := cfg.ResolvedGitProxy()
	base, err := gitlabMRPolicyBase(ref, policy)
	if err != nil {
		return prChecksInfo{}, false
	}
	token, fault := gitlabToken(policy)
	if fault != nil {
		return prChecksInfo{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitlabPresentedPRTimeout)
	defer cancel()

	mr, ok := gitlabReadMR(ctx, base, token, ref)
	if !ok {
		return prChecksInfo{}, false
	}
	info := gitlabPipelineChecks(ctx, base, token, ref, mr.HeadPipeline, time.Now())
	info.PRState = prStateFromGitLab(mr.State, mr.Draft)
	return info, true
}

// gitlabReadMR performs the merge-request read through the proxy's transport
// seam, so flow tests script it the way they script the proxy.
func gitlabReadMR(ctx context.Context, base, token string, ref gitlabMRRef) (*glMR, bool) {
	res, err := glDo(ctx, base, token, glAPIRequest{
		Path: "projects/" + url.PathEscape(ref.project) + "/merge_requests/" + strconv.Itoa(ref.number),
	})
	if err != nil || res.Status < 200 || res.Status > 299 {
		return nil, false
	}
	var mr glMR
	if json.Unmarshal(res.Body, &mr) != nil || mr.IID == 0 {
		return nil, false
	}
	return &mr, true
}

// gitlabPipelineChecks turns a head pipeline's jobs into the check list the
// dashboard renders. No pipeline parses to "resolved, zero checks", the same
// answer a GitHub PR with no rollup gives.
//
// Every page of jobs is read (through the proxy's own pipelineJobs, which
// follows GitLab's pagination up to its cap), and the summary counts all of
// them. Only the displayed list stops at prChecksMaxRuns. When the read is
// incomplete, because the page cap was reached or a page failed, the jobs in
// hand are a subset, and a subset of green jobs must not read as a green
// pipeline: GitLab's own verdict for the pipeline sets the state instead.
func gitlabPipelineChecks(ctx context.Context, base, token string, ref gitlabMRRef, pipeline *glPipeline, now time.Time) prChecksInfo {
	checks := []prCheckRun{}
	complete := true
	if pipeline != nil && pipeline.ID > 0 {
		session := &glProxySession{base: base, token: token, project: ref.project, projectID: url.PathEscape(ref.project)}
		jobs, truncated, failure, err := session.pipelineJobs(ctx, pipeline.ID)
		complete = !truncated && failure == nil && err == nil
		for _, job := range jobs {
			if run, ok := gitlabJobCheckRun(job, pipeline.WebURL); ok {
				checks = append(checks, run)
			}
		}
	}
	summary := summarizePRChecks(checks, now)
	if pipeline != nil {
		verdict := glPipelineChecks(&struct {
			Status string `json:"status"`
		}{Status: pipeline.Status}, "")
		switch {
		case !complete:
			summary.State = "pending"
			if verdict != nil {
				summary.State = verdict.State
			}
		case verdict != nil && checkStateRank(verdict.State) > checkStateRank(summary.State):
			// Even a complete read is not the whole pipeline: the Jobs API
			// leaves out trigger (bridge) jobs, so a downstream pipeline's
			// failure, or one still running, shows only in the pipeline's own
			// status. That status can make the summary worse, never better.
			summary.State = verdict.State
		}
	}
	if len(checks) > prChecksMaxRuns {
		checks = checks[:prChecksMaxRuns]
	}
	return prChecksInfo{Summary: summary, Checks: checks, FetchedAt: now}
}

// checkStateRank orders summary states from good to bad, for letting the
// pipeline's own verdict override a summary only towards worse.
func checkStateRank(state string) int {
	switch state {
	case "failing":
		return 3
	case "pending":
		return 2
	case "passing":
		return 1
	}
	return 0
}

// gitlabJobCheckRun maps one CI job onto a check row. Buckets follow the
// GitHub mapping's judgement: a canceled or skipped job is noise rather than
// a failure, a manual job is waiting on a human and so pending, and a job
// that is allowed to fail does not paint the badge red.
func gitlabJobCheckRun(job glJob, pipelineURL string) (prCheckRun, bool) {
	run := prCheckRun{
		Name:        clipPRCheckText(job.Name),
		Source:      clipPRCheckText(job.Stage),
		URL:         safePRCheckURL(job.WebURL),
		RunURL:      safePRCheckURL(pipelineURL),
		StartedAt:   normalizePRCheckTimestamp(job.StartedAt),
		CompletedAt: normalizePRCheckTimestamp(job.FinishedAt),
	}
	status := strings.ToLower(strings.TrimSpace(job.Status))
	switch status {
	case "success":
		run.Bucket, run.Conclusion = "pass", "success"
	case "failed":
		if job.AllowFailure {
			run.Bucket, run.Conclusion = "skipped", "failed (allowed)"
		} else {
			run.Bucket, run.Conclusion = "fail", "failed"
		}
	case "canceled", "cancelled":
		// GitLab spells it "canceled"; the summary counts a cancelled run
		// under the one spelling, so a pipeline cancelled wholesale is not
		// read as green.
		run.Bucket, run.Conclusion = "skipped", "cancelled"
	case "skipped":
		run.Bucket, run.Conclusion = "skipped", status
	case "":
		run.Bucket, run.Conclusion = "pending", "pending"
	case "running":
		// The dashboard's running clock keys on GitHub's "in progress".
		run.Bucket, run.Conclusion = "pending", "in progress"
	default:
		// created, pending, running, manual, scheduled, waiting_for_resource,
		// preparing: all still to come.
		run.Bucket, run.Conclusion = "pending", strings.ReplaceAll(status, "_", " ")
	}
	return run, run.Name != ""
}

// prStateFromGitLab folds GitLab's state and draft flag into the dashboard's
// single state string, as prStateFromGH does for GitHub. "locked" is the
// moment of a merge and counts as open.
func prStateFromGitLab(state string, draft bool) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "opened", "locked", "open":
		if draft {
			return "draft"
		}
		return "open"
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	}
	return ""
}

// ---------------------------------------------------------------------------
// A branch's merge request, for the dashboard's Branch column
// ---------------------------------------------------------------------------

// gitlabMRForBranch is ghPRForBranch for a GitLab project: the merge request
// opened FROM branch, preferring an open one, with its head pipeline's jobs
// as the checks. The branch has passed validateBranchName on its way here and
// travels as a query value, never in a path.
func gitlabMRForBranch(policy config.GitProxyConfig, repo branchLinkRepo, branch string) (int, string, string, string, *prChecksInfo) {
	if !safeBranchForGH("", branch) || repo.Base == "" || repo.OwnerRepo == "" {
		return 0, "", "", "", nil
	}
	token, fault := gitlabToken(policy)
	if fault != nil {
		return 0, "", "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitlabPresentedPRTimeout)
	defer cancel()

	projectID := url.PathEscape(repo.OwnerRepo)
	list := func(state string) []glMR {
		res, err := glDo(ctx, repo.Base, token, glAPIRequest{
			Path: "projects/" + projectID + "/merge_requests",
			Query: url.Values{
				"source_branch": []string{branch},
				"state":         []string{state},
				"per_page":      []string{"10"},
				"order_by":      []string{"created_at"},
				"sort":          []string{"desc"},
			},
		})
		if err != nil || res.Status < 200 || res.Status > 299 {
			return nil
		}
		var mrs []glMR
		if json.Unmarshal(res.Body, &mrs) != nil {
			return nil
		}
		return mrs
	}
	// The project's own branch only: a merge request INTO this project from a
	// fork can carry the same branch name (the GitHub path skips a fork's
	// head for the same reason).
	ownBranch := func(mr glMR) bool {
		return mr.SourceProjectID == 0 || mr.TargetProjectID == 0 || mr.SourceProjectID == mr.TargetProjectID
	}
	// Open first, so an open merge request is never hidden behind newer
	// merged or closed ones; otherwise the newest in any state.
	pick := -1
	mrs := list("opened")
	for i := range mrs {
		if mrs[i].State == "opened" && ownBranch(mrs[i]) {
			pick = i
			break
		}
	}
	if pick < 0 {
		mrs = list("all")
		for i := range mrs {
			if ownBranch(mrs[i]) {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		return 0, "", "", "", nil
	}
	mr := mrs[pick]
	// The listing carries no head pipeline; one more read fetches it, and the
	// jobs follow. A merge request with no pipeline parses to zero checks.
	ref := gitlabMRRef{base: repo.Base, host: strings.ToLower(strings.TrimSpace(hostOf(repo.Base))), project: repo.OwnerRepo, number: mr.IID}
	full, ok := gitlabReadMR(ctx, repo.Base, token, ref)
	if !ok {
		// No checks rather than zero checks: a failed read must leave the
		// cached CI summary alone instead of overwriting it with nothing.
		return mr.IID, mr.WebURL, prStateFromGitLab(mr.State, mr.Draft), truncateAuthoredPRTitle(mr.Title), nil
	}
	checks := gitlabPipelineChecks(ctx, repo.Base, token, ref, full.HeadPipeline, time.Now())
	return mr.IID, mr.WebURL, prStateFromGitLab(mr.State, mr.Draft), truncateAuthoredPRTitle(mr.Title), &checks
}

// hostOf returns the host (without port) of a base URL, or "" when it does
// not parse.
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
