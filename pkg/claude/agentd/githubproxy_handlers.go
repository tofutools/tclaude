package agentd

import (
	"net/http"
	"strings"
)

// githubproxy_handlers.go is the HTTP surface of `tclaude proxy github`.
//
// Every route is a POST, including the reads. That is deliberate: a GitHub
// read SPENDS THE OPERATOR'S CREDENTIAL against a private repository, and the
// audit middleware records mutating methods only. Making the reads POSTs is
// what puts "this agent read the issue list as me" in the operator's audit
// trail alongside "this agent opened a PR as me".
//
// The daemon models none of gh's response schemas — `--json` output is passed
// through verbatim (see ghProxyOutcome.JSON). Modelling them would mean a
// daemon release every time GitHub adds a field.

// The field sets gh is asked for. Fixed constants, never caller input.
const (
	ghPRListFields  = "number,title,state,isDraft,headRefName,baseRefName,url,updatedAt,author"
	ghPRViewFields  = "number,title,state,isDraft,headRefName,baseRefName,url,body,createdAt,updatedAt,author,mergeable,reviewDecision"
	ghPRChecksField = "number,url,statusCheckRollup"
	ghIssueListFlds = "number,title,state,url,updatedAt,author,labels"
	ghIssueViewFlds = "number,title,state,url,body,createdAt,updatedAt,author,labels,assignees"
)

type ghProxyPRCreateRequest struct {
	Remote string `json:"remote,omitempty"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	Base   string `json:"base,omitempty"`
	Head   string `json:"head,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
}

type ghProxyListRequest struct {
	Remote string `json:"remote,omitempty"`
	State  string `json:"state,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type ghProxyNumberRequest struct {
	Remote string `json:"remote,omitempty"`
	Number int    `json:"number"`
}

type ghProxyCommentRequest struct {
	Remote string `json:"remote,omitempty"`
	Number int    `json:"number"`
	Body   string `json:"body"`
}

// openGHProxy is the shared prologue: method check, permission gate, bounded
// body decode, then every git-side gate plus the GitHub repo derivation.
func openGHProxy(w http.ResponseWriter, r *http.Request, perm string, body any, remoteOf func() string) (
	*ghProxySession, bool,
) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return nil, false
	}
	convID, ok := requirePermission(w, r, perm)
	if !ok {
		return nil, false
	}
	if body != nil && !decodeGitProxyBody(w, r, body) {
		return nil, false
	}
	g, fault := newGHProxySession(r.Context(), convID, remoteOf())
	if fault != nil {
		writeProxyFault(w, fault)
		return nil, false
	}
	return g, true
}

// handleGHProxyPRCreate serves POST /v1/github/pr/create — the operation this
// whole feature exists for.
func handleGHProxyPRCreate(w http.ResponseWriter, r *http.Request) {
	var body ghProxyPRCreateRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
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
	args := []string{"pr", "create", "--repo", g.ownerRepo, "--title", strings.TrimSpace(body.Title)}
	if body.Draft {
		args = append(args, "--draft")
	}
	for _, ref := range []struct {
		flag  string
		value string
	}{{"--base", body.Base}, {"--head", body.Head}} {
		branch := strings.TrimSpace(ref.value)
		if branch == "" {
			continue
		}
		if fault := validateBranchName(branch); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		args = append(args, ref.flag, branch)
	}
	// The body always travels by file, even when empty: a PR body is prose
	// that may contain anything, and argv is world-readable.
	path, cleanup, fault := g.bodyFile(body.Body)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	defer cleanup()
	args = append(args, "--body-file", path)

	res, err := g.gh(r.Context(), args...)
	g.respond(w, r, "pr.create", res, err)
}

// handleGHProxyPRList serves POST /v1/github/pr/list.
func handleGHProxyPRList(w http.ResponseWriter, r *http.Request) {
	var body ghProxyListRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	state, fault := validateGHState(body.State, "open", "closed", "merged", "all")
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateGHLimit(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	res, err := g.gh(r.Context(), "pr", "list", "--repo", g.ownerRepo,
		"--state", state, "--limit", limit, "--json", ghPRListFields)
	g.respond(w, r, "pr.list", res, err)
}

// handleGHProxyPRView serves POST /v1/github/pr/view.
func handleGHProxyPRView(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumber(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	res, err := g.gh(r.Context(), "pr", "view", number, "--repo", g.ownerRepo, "--json", ghPRViewFields)
	g.respond(w, r, "pr.view", res, err)
}

// handleGHProxyPRChecks serves POST /v1/github/pr/checks — CI state for a PR.
// It uses `pr view --json statusCheckRollup` rather than `gh pr checks`
// because the latter exits non-zero when checks are merely pending, which
// would read as a daemon failure rather than as an answer.
func handleGHProxyPRChecks(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumber(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	res, err := g.gh(r.Context(), "pr", "view", number, "--repo", g.ownerRepo, "--json", ghPRChecksField)
	g.respond(w, r, "pr.checks", res, err)
}

// handleGHProxyPRComment serves POST /v1/github/pr/comment.
func handleGHProxyPRComment(w http.ResponseWriter, r *http.Request) {
	var body ghProxyCommentRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumber(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateGHBody(body.Body, true); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	path, cleanup, fault := g.bodyFile(body.Body)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	defer cleanup()
	res, err := g.gh(r.Context(), "pr", "comment", number, "--repo", g.ownerRepo, "--body-file", path)
	g.respond(w, r, "pr.comment", res, err)
}

// handleGHProxyPRReady serves POST /v1/github/pr/ready — mark a draft ready
// for review. It is the natural end of an agent's own workflow, which is why
// it is here and, say, `pr merge` is not: merging is the human's call.
func handleGHProxyPRReady(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumber(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	res, err := g.gh(r.Context(), "pr", "ready", number, "--repo", g.ownerRepo)
	g.respond(w, r, "pr.ready", res, err)
}

// handleGHProxyIssueList serves POST /v1/github/issue/list.
func handleGHProxyIssueList(w http.ResponseWriter, r *http.Request) {
	var body ghProxyListRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	state, fault := validateGHState(body.State, "open", "closed", "all")
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateGHLimit(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	res, err := g.gh(r.Context(), "issue", "list", "--repo", g.ownerRepo,
		"--state", state, "--limit", limit, "--json", ghIssueListFlds)
	g.respond(w, r, "issue.list", res, err)
}

// handleGHProxyIssueView serves POST /v1/github/issue/view.
func handleGHProxyIssueView(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumber(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	res, err := g.gh(r.Context(), "issue", "view", number, "--repo", g.ownerRepo, "--json", ghIssueViewFlds)
	g.respond(w, r, "issue.view", res, err)
}

// handleGHProxyIssueComment serves POST /v1/github/issue/comment.
func handleGHProxyIssueComment(w http.ResponseWriter, r *http.Request) {
	var body ghProxyCommentRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumber(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateGHBody(body.Body, true); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	path, cleanup, fault := g.bodyFile(body.Body)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	defer cleanup()
	res, err := g.gh(r.Context(), "issue", "comment", number, "--repo", g.ownerRepo, "--body-file", path)
	g.respond(w, r, "issue.comment", res, err)
}
