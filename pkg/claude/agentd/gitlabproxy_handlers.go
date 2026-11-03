package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// gitlabproxy_handlers.go is the HTTP surface of `tclaude proxy gitlab` for
// merge requests and issues. Every route is a POST, including the reads, for
// the reason githubproxy_handlers.go gives: a read spends the operator's
// credential against a private project and belongs in the audit trail.

type glProxyMRCreateRequest struct {
	Remote string `json:"remote,omitempty"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	Base   string `json:"base,omitempty"`
	Head   string `json:"head,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
}

type glProxyListRequest struct {
	Remote string `json:"remote,omitempty"`
	State  string `json:"state,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type glProxyMRListRequest struct {
	glProxyListRequest
	// Head narrows the listing to merge requests opened FROM this branch.
	Head string `json:"head,omitempty"`
}

type glProxyNumberRequest struct {
	Remote string `json:"remote,omitempty"`
	Number int    `json:"number"`
}

type glProxyCommentRequest struct {
	Remote string `json:"remote,omitempty"`
	Number int    `json:"number"`
	Body   string `json:"body"`
}

type glProxyMREditRequest struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Remote string `json:"remote,omitempty"`
}

type glProxyMRMergeRequest struct {
	Remote  string `json:"remote,omitempty"`
	Number  int    `json:"number"`
	Method  string `json:"method,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
}

// openGLProxy is the shared prologue, the GitLab twin of openGHProxy.
func openGLProxy(w http.ResponseWriter, r *http.Request, perm string, body any, remoteOf func() string) (
	*glProxySession, bool,
) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return nil, false
	}
	convID, deferred, ok := preflightProxyPermission(w, r, perm)
	if !ok {
		return nil, false
	}
	if body != nil && !decodeGitProxyBody(w, r, body) {
		return nil, false
	}
	g, fault := newGLProxySession(r.Context(), convID, remoteOf(), deferred)
	if fault != nil {
		writeProxyFault(w, fault)
		return nil, false
	}
	if !finishProxyPermission(w, r, convID, perm, g.remoteKey, deferred) {
		return nil, false
	}
	return g, true
}

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// glAuthorOut renders a user in the GitHub proxy's author vocabulary, so a
// consumer written against `author.login` keeps working.
type glAuthorOut struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

func (u *glUser) project() *glAuthorOut {
	if u == nil || (u.Username == "" && u.ID == 0) {
		return nil
	}
	return &glAuthorOut{ID: u.ID, Login: u.Username, Name: u.Name}
}

type glPipeline struct {
	ID        int64  `json:"id"`
	SHA       string `json:"sha"`
	Ref       string `json:"ref"`
	Status    string `json:"status"`
	Source    string `json:"source"`
	WebURL    string `json:"web_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type glPipelineOut struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	SHA       string `json:"sha"`
	Ref       string `json:"ref"`
	Source    string `json:"source"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func (p *glPipeline) project() *glPipelineOut {
	if p == nil || p.ID == 0 {
		return nil
	}
	return &glPipelineOut{ID: p.ID, Status: p.Status, SHA: p.SHA, Ref: p.Ref, Source: p.Source,
		URL: p.WebURL, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

// glMR is every merge-request field any verb reads. One struct, as on the
// GitHub side, so the projections cannot drift apart.
type glMR struct {
	IID                 int         `json:"iid"`
	Title               string      `json:"title"`
	Description         string      `json:"description"`
	State               string      `json:"state"`
	Draft               bool        `json:"draft"`
	SourceBranch        string      `json:"source_branch"`
	TargetBranch        string      `json:"target_branch"`
	WebURL              string      `json:"web_url"`
	CreatedAt           string      `json:"created_at"`
	UpdatedAt           string      `json:"updated_at"`
	SHA                 string      `json:"sha"`
	MergeCommitSHA      string      `json:"merge_commit_sha"`
	SquashCommitSHA     string      `json:"squash_commit_sha"`
	HasConflicts        bool        `json:"has_conflicts"`
	DetailedMergeStatus string      `json:"detailed_merge_status"`
	SourceProjectID     int64       `json:"source_project_id"`
	TargetProjectID     int64       `json:"target_project_id"`
	Author              *glUser     `json:"author"`
	HeadPipeline        *glPipeline `json:"head_pipeline"`
}

type glMRListEntry struct {
	Number      int          `json:"number"`
	Title       string       `json:"title"`
	State       string       `json:"state"`
	IsDraft     bool         `json:"isDraft"`
	HeadRefName string       `json:"headRefName"`
	BaseRefName string       `json:"baseRefName"`
	URL         string       `json:"url"`
	UpdatedAt   string       `json:"updatedAt"`
	Author      *glAuthorOut `json:"author"`
}

func projectGLMRListEntry(mr glMR) glMRListEntry {
	return glMRListEntry{
		Number: mr.IID, Title: mr.Title, State: mr.State, IsDraft: mr.Draft,
		HeadRefName: mr.SourceBranch, BaseRefName: mr.TargetBranch, URL: mr.WebURL,
		UpdatedAt: mr.UpdatedAt, Author: mr.Author.project(),
	}
}

type glMRViewOut struct {
	Number      int          `json:"number"`
	Title       string       `json:"title"`
	State       string       `json:"state"`
	IsDraft     bool         `json:"isDraft"`
	HeadRefName string       `json:"headRefName"`
	BaseRefName string       `json:"baseRefName"`
	URL         string       `json:"url"`
	Body        string       `json:"body"`
	CreatedAt   string       `json:"createdAt"`
	UpdatedAt   string       `json:"updatedAt"`
	Author      *glAuthorOut `json:"author"`
	// Mergeable is GitLab's detailed_merge_status ("mergeable",
	// "ci_still_running", "conflict", "draft_status", …), which is the
	// closest thing GitLab has to GitHub's mergeable plus reviewDecision.
	Mergeable    bool           `json:"mergeable"`
	MergeStatus  string         `json:"mergeStatus"`
	HasConflicts bool           `json:"hasConflicts"`
	HeadSHA      string         `json:"headSha"`
	HeadPipeline *glPipelineOut `json:"headPipeline"`
}

func projectGLMRView(mr glMR) glMRViewOut {
	return glMRViewOut{
		Number: mr.IID, Title: mr.Title, State: mr.State, IsDraft: mr.Draft,
		HeadRefName: mr.SourceBranch, BaseRefName: mr.TargetBranch, URL: mr.WebURL,
		Body: mr.Description, CreatedAt: mr.CreatedAt, UpdatedAt: mr.UpdatedAt,
		Author: mr.Author.project(), Mergeable: mr.DetailedMergeStatus == "mergeable",
		MergeStatus: mr.DetailedMergeStatus, HasConflicts: mr.HasConflicts, HeadSHA: mr.SHA,
		HeadPipeline: mr.HeadPipeline.project(),
	}
}

type glIssue struct {
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	WebURL      string   `json:"web_url"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Author      *glUser  `json:"author"`
	Labels      []string `json:"labels"`
	Assignees   []glUser `json:"assignees"`
}

type glIssueListEntry struct {
	Number    int          `json:"number"`
	Title     string       `json:"title"`
	State     string       `json:"state"`
	URL       string       `json:"url"`
	UpdatedAt string       `json:"updatedAt"`
	Author    *glAuthorOut `json:"author"`
	Labels    []string     `json:"labels"`
}

func projectGLIssueListEntry(issue glIssue) glIssueListEntry {
	labels := issue.Labels
	if labels == nil {
		labels = []string{}
	}
	return glIssueListEntry{
		Number: issue.IID, Title: issue.Title, State: issue.State, URL: issue.WebURL,
		UpdatedAt: issue.UpdatedAt, Author: issue.Author.project(), Labels: labels,
	}
}

type glIssueViewOut struct {
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	State     string        `json:"state"`
	URL       string        `json:"url"`
	Body      string        `json:"body"`
	CreatedAt string        `json:"createdAt"`
	UpdatedAt string        `json:"updatedAt"`
	Author    *glAuthorOut  `json:"author"`
	Labels    []string      `json:"labels"`
	Assignees []glAuthorOut `json:"assignees"`
}

func projectGLIssueView(issue glIssue) glIssueViewOut {
	out := glIssueViewOut{
		Number: issue.IID, Title: issue.Title, State: issue.State, URL: issue.WebURL,
		Body: issue.Description, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
		Author: issue.Author.project(), Labels: issue.Labels, Assignees: []glAuthorOut{},
	}
	if out.Labels == nil {
		out.Labels = []string{}
	}
	for i := range issue.Assignees {
		if a := issue.Assignees[i].project(); a != nil {
			out.Assignees = append(out.Assignees, *a)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Merge requests
// ---------------------------------------------------------------------------

type glProjectInfo struct {
	DefaultBranch string `json:"default_branch"`
}

// defaultBranch reads the project's default branch.
func (g *glProxySession) defaultBranch(ctx context.Context) (string, *ProxyResult, error) {
	raw, failure, err := g.rest(ctx, glAPIRequest{Path: g.projectPath("")})
	if failure != nil || err != nil {
		return "", failure, err
	}
	var info glProjectInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", nil, fmt.Errorf("could not read the project's default branch: %w", err)
	}
	if info.DefaultBranch == "" {
		return "", nil, fmt.Errorf("the project reported no default branch, so there is nothing to open a merge request against; pass --base")
	}
	return info.DefaultBranch, nil, nil
}

// mergeRequest reads one merge request. A 404 is GitLab's answer for "no such
// iid" and for "not visible", and it is rendered as the refusal it is.
func (g *glProxySession) mergeRequest(ctx context.Context, iid int) (*glMR, *ProxyResult, error) {
	raw, failure, err := g.rest(ctx, glAPIRequest{Path: g.projectPath("merge_requests/%s", iid)})
	if failure != nil || err != nil {
		return nil, failure, err
	}
	var mr glMR
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, nil, fmt.Errorf("could not read the merge request: %w", err)
	}
	if mr.IID == 0 {
		notFound := ghResultFromError(fmt.Sprintf("no merge request !%d in %s", iid, g.project))
		return nil, &notFound, nil
	}
	return &mr, nil, nil
}

// handleGLProxyMRCreate serves POST /v1/gitlab/mr/create.
func handleGLProxyMRCreate(w http.ResponseWriter, r *http.Request) {
	var body glProxyMRCreateRequest
	g, ok := openGLProxy(w, r, PermGitLabWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	if fault := validateGHTitle(body.Title); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateGHBody(body.Body, false); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	head := strings.TrimSpace(body.Head)
	if head == "" {
		head = g.branch
	}
	if head == "" {
		writeError(w, http.StatusConflict, "detached_head",
			"this work tree is not on a branch (detached HEAD), so there is no source branch to "+
				"open a merge request from; check out a branch or pass --head explicitly")
		return
	}
	if fault := validateBranchName(head); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glProxyTimeout)
	defer cancel()

	base := strings.TrimSpace(body.Base)
	if base != "" {
		if fault := validateBranchName(base); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	} else {
		resolved, failure, err := g.defaultBranch(ctx)
		if failure != nil || err != nil {
			g.respondOrFail(w, r, "mr.create", failure, err)
			return
		}
		base = resolved
	}

	title := strings.TrimSpace(body.Title)
	if body.Draft {
		// GitLab's draft state IS the title marker; see glDraftPrefixes.
		title = glDraftTitle(title)
	}
	payload := map[string]any{
		"source_branch": head,
		"target_branch": base,
		"title":         title,
		"description":   body.Body,
	}
	raw, failure, err := g.rest(ctx, glAPIRequest{
		Method: http.MethodPost,
		Path:   g.projectPath("merge_requests"),
		Body:   payload,
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.create", failure, err)
		return
	}
	var created glMR
	if err := json.Unmarshal(raw, &created); err != nil || created.WebURL == "" {
		g.respond(w, r, "mr.create", ProxyResult{},
			fmt.Errorf("the merge request was created but the response could not be read: %v", err))
		return
	}
	// The same pr.opened edge the GitHub proxy and `present-pr` record, so the
	// dashboard shows the merge request and the triggers see it.
	var recordFailure error
	if agentID, lookupErr := db.AgentIDForConv(g.convID); lookupErr != nil {
		slog.Warn("gitlab proxy: resolve MR author for trigger", "conv", g.convID, "error", lookupErr)
		recordFailure = lookupErr
	} else if agentID != "" {
		state := "open"
		if created.Draft || body.Draft {
			state = "draft"
		}
		if _, persistErr := recordCreatedPR(agentID, created.WebURL,
			glStripDraftPrefix(created.Title), state, head, state == "draft", g.repoRoot); persistErr != nil {
			slog.Warn("gitlab proxy: persist created MR for trigger reconciliation",
				"conv", g.convID, "mr", created.WebURL, "error", persistErr)
			recordFailure = persistErr
		}
	}
	out := ProxyResult{Stdout: created.WebURL + "\n"}
	if recordFailure != nil {
		// The merge request exists, so this is still a success; only the
		// dashboard's record of it failed, and the agent can repair that.
		out.Stderr = fmt.Sprintf("the merge request was created, but the dashboard could not record it (%v); "+
			"run `tclaude agent present-pr %s` to show it there\n", recordFailure, created.WebURL)
	}
	g.respond(w, r, "mr.create", out, nil)
}

// handleGLProxyMRList serves POST /v1/gitlab/mr/list.
func handleGLProxyMRList(w http.ResponseWriter, r *http.Request) {
	var body glProxyMRListRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	state, fault := validateGHEnum("state", body.State, glMRStates...)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateGHLimitInt(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	query := url.Values{
		"state":    []string{glMRStateParam(state)},
		"per_page": []string{strconv.Itoa(limit)},
		// Newest-created first, the set `pr ls` has always returned.
		"order_by": []string{"created_at"},
		"sort":     []string{"desc"},
	}
	if h := strings.TrimSpace(body.Head); h != "" {
		if fault := validateBranchName(h); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		query.Set("source_branch", h)
	}
	mrs, failure, err := g.listMergeRequests(r.Context(), query, strings.TrimSpace(body.Head) != "", limit)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.list", failure, err)
		return
	}
	out := make([]glMRListEntry, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, projectGLMRListEntry(mr))
	}
	g.respondJSON(w, r, "mr.list", out)
}

// errGLListFull stops a paged read once it holds enough rows.
var errGLListFull = errors.New("enough rows")

// listMergeRequests reads one page of the project's merge requests, or, for
// --head, as many pages as it takes to find `limit` of the project's own.
// --head means THIS project's branch, and a fork's merge request into the
// project can carry the same branch name; dropping those from one page could
// leave nothing although the project's own is on the next.
func (g *glProxySession) listMergeRequests(ctx context.Context, query url.Values, ownOnly bool, limit int) ([]glMR, *ProxyResult, error) {
	req := glAPIRequest{Path: g.projectPath("merge_requests"), Query: query}
	if !ownOnly {
		raw, failure, err := g.rest(ctx, req)
		if failure != nil || err != nil {
			return nil, failure, err
		}
		var mrs []glMR
		if err := json.Unmarshal(raw, &mrs); err != nil {
			return nil, nil, fmt.Errorf("could not read the merge-request listing: %w", err)
		}
		return mrs, nil, nil
	}
	query.Set("per_page", "100")
	var mrs []glMR
	_, failure, err := g.restPaginated(ctx, glProxyTimeout, req, func(page []byte) error {
		var batch []glMR
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("could not read the merge-request listing: %w", err)
		}
		for _, mr := range batch {
			if glMRFromOwnProject(mr) {
				mrs = append(mrs, mr)
			}
		}
		if len(mrs) >= limit {
			return errGLListFull
		}
		return nil
	})
	if errors.Is(err, errGLListFull) {
		err = nil
	}
	if failure != nil || err != nil {
		return nil, failure, err
	}
	if len(mrs) > limit {
		mrs = mrs[:limit]
	}
	return mrs, nil, nil
}

// handleGLProxyMRView serves POST /v1/gitlab/mr/view.
func handleGLProxyMRView(w http.ResponseWriter, r *http.Request) {
	var body glProxyNumberRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	mr, failure, err := g.mergeRequest(r.Context(), iid)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.view", failure, err)
		return
	}
	g.respondJSON(w, r, "mr.view", projectGLMRView(*mr))
}

// glJob is a CI job as the jobs endpoints report it.
type glJob struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Stage         string `json:"stage"`
	Status        string `json:"status"`
	AllowFailure  bool   `json:"allow_failure"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	WebURL        string `json:"web_url"`
	ArtifactsFile *struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"artifacts_file"`
	Artifacts []struct {
		FileType string `json:"file_type"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"artifacts"`
	ArtifactsExpireAt string `json:"artifacts_expire_at"`
	Pipeline          struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"pipeline"`
}

type glJobOut struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allowFailure"`
	StartedAt    string `json:"startedAt"`
	FinishedAt   string `json:"finishedAt"`
	URL          string `json:"url"`
}

func projectGLJob(j glJob) glJobOut {
	return glJobOut{ID: j.ID, Name: j.Name, Stage: j.Stage, Status: j.Status, AllowFailure: j.AllowFailure,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, URL: j.WebURL}
}

// glMRChecksOut is what `mr checks` returns: the head pipeline and its jobs.
// GitLab has no check rollup; the head pipeline's jobs are the equivalent, and
// a merge request with no pipeline yet answers with null for both.
type glMRChecksOut struct {
	Number   int             `json:"number"`
	URL      string          `json:"url"`
	Pipeline *glPipelineOut  `json:"pipeline"`
	Jobs     json.RawMessage `json:"jobs"`
	// JobsTruncated says the pipeline has more jobs than the proxy reads
	// (maxGLPaginatedPages pages); jobs is then not the whole pipeline, and
	// the pipeline's own status is the verdict.
	JobsTruncated bool `json:"jobs_truncated,omitempty"`
}

// handleGLProxyMRChecks serves POST /v1/gitlab/mr/checks.
func handleGLProxyMRChecks(w http.ResponseWriter, r *http.Request) {
	var body glProxyNumberRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glProxyTimeout)
	defer cancel()

	mr, failure, err := g.mergeRequest(ctx, iid)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.checks", failure, err)
		return
	}
	out := glMRChecksOut{Number: mr.IID, URL: mr.WebURL, Pipeline: mr.HeadPipeline.project(), Jobs: json.RawMessage("null")}
	if out.Pipeline != nil {
		jobs, truncated, failure, err := g.pipelineJobs(ctx, out.Pipeline.ID)
		if failure != nil || err != nil {
			g.respondOrFail(w, r, "mr.checks", failure, err)
			return
		}
		out.JobsTruncated = truncated
		projected := make([]glJobOut, 0, len(jobs))
		for _, j := range jobs {
			projected = append(projected, projectGLJob(j))
		}
		encoded, err := ghMarshal(projected)
		if err != nil {
			g.respond(w, r, "mr.checks", ProxyResult{}, err)
			return
		}
		out.Jobs = encoded
	}
	g.respondJSON(w, r, "mr.checks", out)
}

// handleGLProxyMREdit serves POST /v1/gitlab/mr/edit — title and description
// only, for the reason `pr edit` is narrow.
func handleGLProxyMREdit(w http.ResponseWriter, r *http.Request) {
	var body glProxyMREditRequest
	g, ok := openGLProxy(w, r, PermGitLabWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	title := strings.TrimSpace(body.Title)
	hasBody := strings.TrimSpace(body.Body) != ""
	if title == "" && !hasBody {
		writeError(w, http.StatusBadRequest, "invalid_arg", "nothing to edit: pass a title, a body, or both")
		return
	}
	payload := map[string]any{}
	if title != "" {
		if fault := validateGHTitle(title); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		payload["title"] = title
	}
	if hasBody {
		if fault := validateGHBody(body.Body, true); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		payload["description"] = body.Body
	}
	raw, failure, err := g.rest(r.Context(), glAPIRequest{
		Method: http.MethodPut,
		Path:   g.projectPath("merge_requests/%s", iid),
		Body:   payload,
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.edit", failure, err)
		return
	}
	g.respond(w, r, "mr.edit", ProxyResult{Stdout: glWebURLOf(raw)}, nil)
}

// glWebURLOf pulls web_url out of a write's response; empty when unreadable,
// on the same reasoning as ghURLOf.
func glWebURLOf(raw []byte) string {
	var out struct {
		WebURL string `json:"web_url"`
	}
	if json.Unmarshal(raw, &out) != nil || out.WebURL == "" {
		return ""
	}
	return out.WebURL + "\n"
}

// handleGLProxyMRComment serves POST /v1/gitlab/mr/comment.
func handleGLProxyMRComment(w http.ResponseWriter, r *http.Request) {
	handleGLProxyNote(w, r, "mr.comment", "merge_requests")
}

// handleGLProxyIssueComment serves POST /v1/gitlab/issue/comment.
func handleGLProxyIssueComment(w http.ResponseWriter, r *http.Request) {
	handleGLProxyNote(w, r, "issue.comment", "issues")
}

// handleGLProxyNote posts a note. GitLab calls a comment a note, and the
// endpoint differs only in the noun between a merge request and an issue.
func handleGLProxyNote(w http.ResponseWriter, r *http.Request, verb, noun string) {
	var body glProxyCommentRequest
	g, ok := openGLProxy(w, r, PermGitLabWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateGHBody(body.Body, true); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	raw, failure, err := g.rest(r.Context(), glAPIRequest{
		Method: http.MethodPost,
		Path:   g.projectPath(noun+"/%s/notes", iid),
		Body:   map[string]any{"body": body.Body},
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, verb, failure, err)
		return
	}
	// A note carries no web_url of its own; the anchor GitLab's UI uses is the
	// parent's URL plus `#note_<id>`, which is what a reader needs to find it.
	var note struct {
		ID int64 `json:"id"`
	}
	stdout := ""
	if json.Unmarshal(raw, &note) == nil && note.ID > 0 {
		stdout = fmt.Sprintf("%s/%s/-/%s/%d#note_%d\n", g.base, g.project, noun, iid, note.ID)
	}
	g.respond(w, r, verb, ProxyResult{Stdout: stdout}, nil)
}

// handleGLProxyMRReady serves POST /v1/gitlab/mr/ready. GitLab's draft state
// is the title marker, so this is a title edit that removes it.
func handleGLProxyMRReady(w http.ResponseWriter, r *http.Request) {
	var body glProxyNumberRequest
	g, ok := openGLProxy(w, r, PermGitLabWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glProxyTimeout)
	defer cancel()

	mr, failure, err := g.mergeRequest(ctx, iid)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.ready", failure, err)
		return
	}
	if !mr.Draft {
		g.respond(w, r, "mr.ready",
			ProxyResult{Stdout: fmt.Sprintf("merge request !%d is already ready for review\n%s\n", mr.IID, mr.WebURL)}, nil)
		return
	}
	title := glStripDraftPrefix(mr.Title)
	if title == "" {
		g.respond(w, r, "mr.ready", ghResultFromError(fmt.Sprintf(
			"merge request !%d has no title besides its draft marker; give it one with `mr edit --title` first", mr.IID)), nil)
		return
	}
	updated, failure, err := g.rest(ctx, glAPIRequest{
		Method: http.MethodPut,
		Path:   g.projectPath("merge_requests/%s", iid),
		Body:   map[string]any{"title": title},
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.ready", failure, err)
		return
	}
	state := "open"
	var live glMR
	if json.Unmarshal(updated, &live) == nil {
		if s := prStateFromGitLab(live.State, live.Draft); s != "" {
			state = s
		}
	}
	recordReadyState(g.convID, mr.WebURL, state)
	g.respond(w, r, "mr.ready", ProxyResult{Stdout: mr.WebURL + "\n"}, nil)
}

// recordReadyState writes a pull or merge request's state as the forge now
// reports it onto the caller's dashboard row, so the row stops reading draft
// at once rather than at the next refresh. Quietly: leaving draft is not a
// lifecycle edge a trigger listens for. A failure here costs only that wait.
func recordReadyState(convID, prURL, state string) {
	if state == "" {
		state = "open"
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return
	}
	if _, err := db.UpdateAgentPRStateQuiet(agentID, prURL, state); err != nil {
		slog.Warn("forge proxy: record a ready request's state", "conv", convID, "pr", prURL, "error", err)
	}
}

// handleGLProxyMRMerge serves POST /v1/gitlab/mr/merge, behind
// proxy.gitlab.merge for the reason `pr merge` has its own slug.
//
// GitLab answers "already merged", "closed", "draft" and "conflicts" with 405s
// and 406s that read alike, so the state is read first and each case is named.
func handleGLProxyMRMerge(w http.ResponseWriter, r *http.Request) {
	var body glProxyMRMergeRequest
	g, ok := openGLProxy(w, r, PermGitLabMerge, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	method, fault := validateGLMergeMethod(body.Method)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	subject := strings.TrimSpace(body.Subject)
	if subject != "" {
		if fault := validateGHHeadline("subject", subject); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	}
	if fault := validateGHBody(body.Body, false); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	g.auditExtra = fmt.Sprintf("mr=%d", iid)
	ctx, cancel := context.WithTimeout(r.Context(), glProxyTimeout)
	defer cancel()

	mr, failure, err := g.mergeRequest(ctx, iid)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.merge", failure, err)
		return
	}
	if answer := glMergePreflight(mr); answer != nil {
		g.respond(w, r, "mr.merge", *answer, nil)
		return
	}

	// Never ask GitLab to delete the source branch. Sent explicitly, so this
	// request opts into nothing; a merge request whose own "delete source
	// branch" option is set is still GitLab's to honour.
	payload := map[string]any{"should_remove_source_branch": false}
	message := subject
	if strings.TrimSpace(body.Body) != "" {
		if message == "" {
			message = body.Body
		} else {
			message = subject + "\n\n" + body.Body
		}
	}
	if method == "squash" {
		payload["squash"] = true
		if message != "" {
			payload["squash_commit_message"] = message
		}
	} else if message != "" {
		payload["merge_commit_message"] = message
	}
	raw, failure, err := g.rest(ctx, glAPIRequest{
		Method: http.MethodPut,
		Path:   g.projectPath("merge_requests/%s/merge", iid),
		Body:   payload,
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.merge", failure, err)
		return
	}
	var merged glMR
	if err := json.Unmarshal(raw, &merged); err != nil {
		g.respond(w, r, "mr.merge", ProxyResult{},
			fmt.Errorf("the merge was accepted but the response could not be read: %w", err))
		return
	}
	if merged.State != "merged" {
		// GitLab accepted the request without merging: merge_when_pipeline_succeeds
		// is off here, so this is the auto-merge the project enabled on its own,
		// and the honest answer is the state it is in.
		g.respond(w, r, "mr.merge", ghResultFromError(fmt.Sprintf(
			"merge request !%d was not merged; GitLab reports it as %q (status %q)",
			mr.IID, merged.State, merged.DetailedMergeStatus)), nil)
		return
	}
	sha := merged.MergeCommitSHA
	if sha == "" {
		sha = merged.SquashCommitSHA
	}
	g.auditExtra = fmt.Sprintf("mr=%d sha=%s", mr.IID, sha)
	g.respond(w, r, "mr.merge", ProxyResult{Stdout: fmt.Sprintf(
		"merged !%d into %s as %s\n%s\n", mr.IID, mr.TargetBranch, sha, mr.WebURL)}, nil)
}

// glMergePreflight turns the merge request's own state into the answer for the
// cases GitLab would report as one "not mergeable". A nil return means
// nothing known stops it; approvals and protected-branch rules are GitLab's to
// judge at merge time.
func glMergePreflight(mr *glMR) *ProxyResult {
	switch {
	case mr.State == "merged":
		return &ProxyResult{Stdout: fmt.Sprintf("merge request !%d is already merged\n%s\n", mr.IID, mr.WebURL)}
	case mr.State == "closed":
		failure := ghResultFromError(fmt.Sprintf(
			"merge request !%d is closed, so there is nothing to merge; reopen it first", mr.IID))
		return &failure
	case mr.Draft:
		failure := ghResultFromError(fmt.Sprintf(
			"merge request !%d is a draft; mark it ready for review first (tclaude proxy gitlab mr ready %d)",
			mr.IID, mr.IID))
		return &failure
	case mr.HasConflicts || mr.DetailedMergeStatus == "conflict":
		failure := ghResultFromError(fmt.Sprintf(
			"merge request !%d conflicts with %s; update the branch and let GitLab recompute before merging",
			mr.IID, mr.TargetBranch))
		return &failure
	}
	return nil
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

// handleGLProxyIssueList serves POST /v1/gitlab/issue/list. GitLab keeps
// issues and merge requests apart, so unlike GitHub's REST the listing is
// issues only.
func handleGLProxyIssueList(w http.ResponseWriter, r *http.Request) {
	var body glProxyListRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	state, fault := validateGHEnum("state", body.State, glIssueStates...)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateGHLimitInt(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	query := url.Values{
		"per_page": []string{strconv.Itoa(limit)},
		"order_by": []string{"created_at"},
		"sort":     []string{"desc"},
	}
	if state != "all" {
		query.Set("state", glMRStateParam(state))
	}
	raw, failure, err := g.rest(r.Context(), glAPIRequest{Path: g.projectPath("issues"), Query: query})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "issue.list", failure, err)
		return
	}
	var issues []glIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		g.respond(w, r, "issue.list", ProxyResult{}, fmt.Errorf("could not read the issue listing: %w", err))
		return
	}
	out := make([]glIssueListEntry, 0, len(issues))
	for _, issue := range issues {
		out = append(out, projectGLIssueListEntry(issue))
	}
	g.respondJSON(w, r, "issue.list", out)
}

// handleGLProxyIssueView serves POST /v1/gitlab/issue/view.
func handleGLProxyIssueView(w http.ResponseWriter, r *http.Request) {
	var body glProxyNumberRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	raw, failure, err := g.rest(r.Context(), glAPIRequest{Path: g.projectPath("issues/%s", iid)})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "issue.view", failure, err)
		return
	}
	var issue glIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		// An answer that does not decode is an upstream read failure, not a
		// missing issue.
		g.respond(w, r, "issue.view", ProxyResult{}, fmt.Errorf("could not read GitLab's answer for issue #%d: %w", iid, err))
		return
	}
	if issue.IID == 0 {
		g.respond(w, r, "issue.view", ghResultFromError(fmt.Sprintf("no issue #%d in %s", iid, g.project)), nil)
		return
	}
	g.respondJSON(w, r, "issue.view", projectGLIssueView(issue))
}

// ---------------------------------------------------------------------------
// Merge-request notes
// ---------------------------------------------------------------------------

// glNote is one note on a merge request. A DiffNote carries a position and is
// the line-level kind; everything else is conversation. System notes ("added
// 3 commits", "changed title") are GitLab's own bookkeeping and are left out.
type glNote struct {
	ID         int64  `json:"id"`
	Body       string `json:"body"`
	Author     glUser `json:"author"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	System     bool   `json:"system"`
	Type       string `json:"type"`
	Resolvable bool   `json:"resolvable"`
	Resolved   bool   `json:"resolved"`
	Position   *struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
		NewLine *int   `json:"new_line"`
		OldLine *int   `json:"old_line"`
	} `json:"position"`
}

const (
	glConversationHeading = "=== conversation (merge-request notes) ==="
	glInlineHeading       = "=== inline review comments (line-level diff notes) ==="
)

// handleGLProxyMRComments serves POST /v1/gitlab/mr/comments — everything said
// on a merge request, in the two sections the GitHub verb uses. One paginated
// read, because GitLab keeps both kinds in the same notes list.
func handleGLProxyMRComments(w http.ResponseWriter, r *http.Request) {
	var body glProxyNumberRequest
	g, ok := openGLProxy(w, r, PermGitLabRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	iid, fault := validateGLIID(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glProxyCommentsTimeout)
	defer cancel()

	var notes []glNote
	truncated, failure, err := g.restPaginated(ctx, glProxyTimeout, glAPIRequest{
		Path: g.projectPath("merge_requests/%s/notes", iid),
		Query: url.Values{
			"per_page": []string{"100"},
			"order_by": []string{"created_at"},
			// Newest first, so a read stopped by the page cap keeps the
			// newest notes; reversed below into reading order.
			"sort": []string{"desc"},
		},
	}, func(page []byte) error {
		var batch []glNote
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("could not read the merge request's notes: %w", err)
		}
		notes = append(notes, batch...)
		return nil
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "mr.comments", failure, err)
		return
	}
	slices.Reverse(notes)

	var conversation, inline strings.Builder
	for _, n := range notes {
		if n.System || strings.TrimSpace(n.Body) == "" {
			continue
		}
		if n.Position != nil {
			file := n.Position.NewPath
			if file == "" {
				file = n.Position.OldPath
			}
			line := "?"
			switch {
			case n.Position.NewLine != nil:
				line = strconv.Itoa(*n.Position.NewLine)
			case n.Position.OldLine != nil:
				line = strconv.Itoa(*n.Position.OldLine)
			}
			inline.WriteString("--\n")
			fmt.Fprintf(&inline, "file:\t%s:%s\n", file, line)
			fmt.Fprintf(&inline, "author:\t%s\n", glLogin(n.Author))
			fmt.Fprintf(&inline, "created:\t%s\n", n.CreatedAt)
			fmt.Fprintf(&inline, "resolved:\t%t\n", n.Resolvable && n.Resolved)
			fmt.Fprintf(&inline, "url:\t%s/%s/-/merge_requests/%d#note_%d\n", g.base, g.project, iid, n.ID)
			inline.WriteString("--\n")
			inline.WriteString(strings.TrimRight(n.Body, "\n"))
			inline.WriteString("\n")
			continue
		}
		status := "none"
		if n.Resolvable {
			status = "unresolved"
			if n.Resolved {
				status = "resolved"
			}
		}
		conversation.WriteString(formatGHComment(glLogin(n.Author), "none", status, n.Body, n.CreatedAt != n.UpdatedAt))
	}
	conv, convTruncated := ghTailText(conversation.String(), maxGHProxyTextBytes)
	inl, inlTruncated := ghTailText(inline.String(), maxGHProxyTextBytes)

	var b strings.Builder
	b.WriteString(glConversationHeading + "\n\n")
	b.WriteString(ghSectionBody(conv, "(no notes)"))
	b.WriteString("\n" + glInlineHeading + "\n\n")
	b.WriteString(ghSectionBody(inl, "(no inline diff notes)"))
	if truncated {
		fmt.Fprintf(&b, "(the merge request has more than %d pages of notes; the rest was not read)\n", maxGLPaginatedPages)
	}
	g.respond(w, r, "mr.comments", ProxyResult{Stdout: b.String(), Truncated: convTruncated || inlTruncated}, nil)
}

func glLogin(u glUser) string {
	if u.Username != "" {
		return u.Username
	}
	return "(unknown)"
}
