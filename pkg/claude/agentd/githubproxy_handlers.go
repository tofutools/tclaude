package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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
// The field set each read returns is fixed here and nowhere else. It is the
// same vocabulary the proxy answered in when it shelled out to `gh --json`,
// because agents and the bundled `proxy-git` skill are written against it.

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

// openGHProxy is the shared prologue: method check, permission preflight,
// bounded body decode, every git-side gate plus repository derivation, then
// the final remote-scoped permission decision before a credential is spent.
func openGHProxy(w http.ResponseWriter, r *http.Request, perm string, body any, remoteOf func() string) (
	*ghProxySession, bool,
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
	g, fault := newGHProxySession(r.Context(), convID, remoteOf(), deferred)
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
// Pull requests
// ---------------------------------------------------------------------------

// ghRepoInfo is the repository metadata `pr create` needs when the caller did
// not name a base branch.
type ghRepoInfo struct {
	DefaultBranch string `json:"default_branch"`
}

// ghPRCreated is the part of a created pull request worth reporting. The URL is
// the whole answer for a human, and the number is what every follow-up verb
// takes.
type ghPRCreated struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
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
	// The head branch is a property of the AGENT's checkout, and this proxy has
	// no checkout of its own to read one from. The daemon supplies the agent's
	// real branch, read from the git session, which is the branch the caller
	// means anyway.
	head := strings.TrimSpace(body.Head)
	if head == "" {
		head = g.branch
	}
	if head == "" {
		writeError(w, http.StatusConflict, "detached_head",
			"this work tree is not on a branch (detached HEAD), so there is no head branch to "+
				"open a pull request from; check out a branch or pass --head explicitly")
		return
	}
	if fault := validateBranchName(head); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// ONE budget across both calls. Without a base this verb makes two, and a
	// per-call bound would let the daemon run to twice what the CLI is waiting
	// on — which on the one verb whose failure mode is "did the PR get created
	// or not?" is the worst place to leave that ambiguity.
	ctx, cancel := context.WithTimeout(r.Context(), ghProxyTimeout)
	defer cancel()

	base := strings.TrimSpace(body.Base)
	if base != "" {
		if fault := validateBranchName(base); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	} else {
		// REST requires an explicit base; there is no "the obvious one"
		// default the way there was on a command line. Resolving the
		// repository's own default branch is what keeps `--base` optional.
		resolved, failure, err := g.defaultBranch(ctx)
		if failure != nil || err != nil {
			g.respondOrFail(w, r, "pr.create", failure, err)
			return
		}
		base = resolved
	}

	payload := map[string]any{
		"title": strings.TrimSpace(body.Title),
		"head":  head,
		"base":  base,
		// Always sent, even when empty: an omitted body and an empty one are
		// the same pull request, and sending the field keeps the request shape
		// identical whether or not the caller supplied prose.
		"body": body.Body,
	}
	if body.Draft {
		payload["draft"] = true
	}
	raw, failure, err := g.rest(ctx, ghAPIRequest{
		Method: http.MethodPost,
		Path:   g.repoPath("pulls"),
		Body:   payload,
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pr.create", failure, err)
		return
	}
	var created ghPRCreated
	if err := json.Unmarshal(raw, &created); err != nil {
		// Reported, unlike the unreadable-response case in ghURLOf, and the
		// difference is deliberate: an edit or a comment has done its whole job
		// by the time the response arrives, while a pull request nobody can
		// name the URL of is a pull request the agent cannot go on to work
		// with. Saying "created, but I lost the address" beats saying nothing.
		g.respond(w, r, "pr.create", ProxyResult{},
			fmt.Errorf("the pull request was created but the response could not be read: %w", err))
		return
	}
	// The URL alone, as text rather than JSON. That is what this verb has
	// always printed, it is what an agent pastes into a report, and it is the
	// one output in this file a human reads more often than a machine does.
	g.respond(w, r, "pr.create", ProxyResult{Stdout: created.HTMLURL + "\n"}, nil)
}

// defaultBranch reads the repository's default branch.
func (g *ghProxySession) defaultBranch(ctx context.Context) (string, *ProxyResult, error) {
	raw, failure, err := g.rest(ctx, ghAPIRequest{Path: g.repoPath("")})
	if failure != nil || err != nil {
		return "", failure, err
	}
	var info ghRepoInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", nil, fmt.Errorf("could not read the repository's default branch: %w", err)
	}
	if info.DefaultBranch == "" {
		return "", nil, fmt.Errorf("the repository reported no default branch, so there is nothing to open a pull request against; pass --base")
	}
	return info.DefaultBranch, nil, nil
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
	limit, fault := validateGHLimitInt(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data struct {
		Repository struct {
			PullRequests struct {
				Nodes []ghGQLPullRequest `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	failure, err := g.graphql(r.Context(), ghPRListQuery, map[string]any{
		"owner": g.owner, "name": g.repo, "limit": limit, "states": ghPRStates(state),
	}, &data)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pr.list", failure, err)
		return
	}
	out := make([]ghPRListEntry, 0, len(data.Repository.PullRequests.Nodes))
	for _, pr := range data.Repository.PullRequests.Nodes {
		out = append(out, projectPRListEntry(pr))
	}
	g.respondJSON(w, r, "pr.list", out)
}

// ghPRStates maps the CLI's state vocabulary onto GraphQL's enum. "all" maps to
// nil, which GraphQL reads as no filter at all rather than as an empty set.
func ghPRStates(state string) []string {
	switch state {
	case "open":
		return []string{"OPEN"}
	case "closed":
		// CLOSED only, deliberately: a merged pull request is closed in the
		// colloquial sense but GitHub models MERGED as its own state, and
		// folding the two would make `--state closed` and `--state merged`
		// overlap where the caller expects them to partition.
		return []string{"CLOSED"}
	case "merged":
		return []string{"MERGED"}
	}
	return nil
}

// handleGHProxyPRView serves POST /v1/github/pr/view.
func handleGHProxyPRView(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumberInt(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	pr, failure, err := g.pullRequest(r.Context(), ghPRViewQuery, number)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pr.view", failure, err)
		return
	}
	g.respondJSON(w, r, "pr.view", projectPRView(*pr))
}

// pullRequest runs one of the pull-request documents and returns the node.
//
// A missing pull request arrives as `data.repository.pullRequest: null` with no
// GraphQL error at all, so the nil check here is the only thing between a typo
// and a response full of zero values that reads like a real pull request with
// an empty title.
func (g *ghProxySession) pullRequest(ctx context.Context, doc string, number int) (*ghGQLPullRequest, *ProxyResult, error) {
	var data struct {
		Repository *struct {
			PullRequest *ghGQLPullRequest `json:"pullRequest"`
		} `json:"repository"`
	}
	failure, err := g.graphql(ctx, doc, map[string]any{
		"owner": g.owner, "name": g.repo, "number": number,
	}, &data)
	if failure != nil || err != nil {
		return nil, failure, err
	}
	if data.Repository == nil || data.Repository.PullRequest == nil {
		notFound := ghResultFromError(fmt.Sprintf(
			"no pull request #%d in %s (it may be an issue number, or the repository may not be "+
				"visible to the token agentd is using)", number, g.ownerRepo))
		return nil, &notFound, nil
	}
	return data.Repository.PullRequest, nil, nil
}

// handleGHProxyPRChecks serves POST /v1/github/pr/checks — CI state for a PR.
//
// It reads the check rollup rather than asking "did the checks pass", because
// pending is a real answer and an agent watching a run needs to see it as one.
func handleGHProxyPRChecks(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumberInt(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	pr, failure, err := g.pullRequest(r.Context(), ghPRChecksQuery, number)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pr.checks", failure, err)
		return
	}
	out := ghPRChecksOut{Number: pr.Number, URL: pr.URL, Rollup: json.RawMessage("null")}
	if rollup := projectCheckRollup(*pr); rollup != nil {
		encoded, err := ghMarshal(rollup)
		if err != nil {
			g.respond(w, r, "pr.checks", ProxyResult{}, err)
			return
		}
		out.Rollup = encoded
	}
	g.respondJSON(w, r, "pr.checks", out)
}

// handleGHProxyPREdit serves POST /v1/github/pr/edit.
//
// Editing a description is a WRITE under the operator's GitHub identity, so it
// sits behind proxy.github.write beside `pr create` rather than beside the
// reads. It is deliberately narrow: title and body only. The API can also move
// the base branch, add reviewers and change labels, and none of that is
// something the proxy's semantic contract covers — an agent that wants it
// should ask a human, not get it as a side effect of fixing a typo.
func handleGHProxyPREdit(w http.ResponseWriter, r *http.Request) {
	var body ghProxyPREditRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumberInt(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	title := strings.TrimSpace(body.Title)
	hasBody := strings.TrimSpace(body.Body) != ""
	if title == "" && !hasBody {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"nothing to edit: pass a title, a body, or both")
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
		payload["body"] = body.Body
	}
	raw, failure, err := g.rest(r.Context(), ghAPIRequest{
		Method: http.MethodPatch,
		Path:   g.repoPath("pulls/%s", number),
		Body:   payload,
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pr.edit", failure, err)
		return
	}
	g.respond(w, r, "pr.edit", ProxyResult{Stdout: ghURLOf(raw)}, nil)
}

// ghProxyPREditRequest edits an existing pull request's title and/or body.
type ghProxyPREditRequest struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Remote string `json:"remote,omitempty"`
}

// ghURLOf pulls the html_url out of a write's response, so a verb whose effect
// is on GitHub answers with the address of what it did.
//
// An unreadable response degrades to empty rather than to an error: the write
// already happened, and its callers — `pr edit`, `pr comment`, `issue comment`
// — have nothing further for the agent to do with the address. `pr create` is
// the exception and reports instead, because there the agent's next step needs
// the pull request it just opened.
func ghURLOf(raw []byte) string {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if json.Unmarshal(raw, &out) != nil || out.HTMLURL == "" {
		return ""
	}
	return out.HTMLURL + "\n"
}

// handleGHProxyPRComment serves POST /v1/github/pr/comment.
//
// It posts to the ISSUE comment endpoint, which is not a mistake: GitHub models
// a pull request's conversation as the underlying issue's comments, and the
// pulls endpoint of the same name creates a line-level review comment instead —
// which needs a commit and a diff position this verb has neither of.
func handleGHProxyPRComment(w http.ResponseWriter, r *http.Request) {
	handleGHProxyComment(w, r, "pr.comment")
}

// handleGHProxyIssueComment serves POST /v1/github/issue/comment.
func handleGHProxyIssueComment(w http.ResponseWriter, r *http.Request) {
	handleGHProxyComment(w, r, "issue.comment")
}

// handleGHProxyComment is the shared body of both comment verbs. They differ
// only in the audit verb they record: the endpoint is the same one, because on
// GitHub the conversation on a pull request IS the issue's conversation.
func handleGHProxyComment(w http.ResponseWriter, r *http.Request, verb string) {
	var body ghProxyCommentRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumberInt(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateGHBody(body.Body, true); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	raw, failure, err := g.rest(r.Context(), ghAPIRequest{
		Method: http.MethodPost,
		Path:   g.repoPath("issues/%s/comments", number),
		Body:   map[string]any{"body": body.Body},
	})
	if failure != nil || err != nil {
		g.respondOrFail(w, r, verb, failure, err)
		return
	}
	g.respond(w, r, verb, ProxyResult{Stdout: ghURLOf(raw)}, nil)
}

// handleGHProxyPRReady serves POST /v1/github/pr/ready — mark a draft ready
// for review. It is the natural end of an agent's own workflow, which is why
// it is here and, say, `pr merge` is not: merging is the human's call.
//
// It is the one write with no REST route. REST will not clear a pull request's
// draft flag, so this resolves the node id and calls the GraphQL mutation that
// will — two calls where every other write is one.
func handleGHProxyPRReady(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumberInt(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// One budget, for the same reason `pr create` has one: this verb resolves a
	// node id and then mutates, and two independent 60s bounds would outlast
	// what the CLI waits on.
	ctx, cancel := context.WithTimeout(r.Context(), ghProxyTimeout)
	defer cancel()

	pr, failure, err := g.pullRequest(ctx, ghPRNodeIDQuery, number)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pr.ready", failure, err)
		return
	}
	if !pr.IsDraft {
		// Not an error. The caller asked for a state the pull request is
		// already in, and reporting that as a failure would have an agent
		// retrying or backing out a change that is fine.
		g.respond(w, r, "pr.ready",
			ProxyResult{Stdout: fmt.Sprintf("pull request #%d is already ready for review\n%s\n", pr.Number, pr.URL)}, nil)
		return
	}
	failure, err = g.graphql(ctx, ghPRReadyMutation, map[string]any{"id": pr.ID}, nil)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "pr.ready", failure, err)
		return
	}
	g.respond(w, r, "pr.ready", ProxyResult{Stdout: pr.URL + "\n"}, nil)
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

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
	limit, fault := validateGHLimitInt(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var states []string
	if state != "all" {
		states = []string{strings.ToUpper(state)}
	}
	var data struct {
		Repository struct {
			Issues struct {
				Nodes []ghGQLIssue `json:"nodes"`
			} `json:"issues"`
		} `json:"repository"`
	}
	failure, err := g.graphql(r.Context(), ghIssueListQuery, map[string]any{
		"owner": g.owner, "name": g.repo, "limit": limit, "states": states,
	}, &data)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "issue.list", failure, err)
		return
	}
	out := make([]ghIssueListEntry, 0, len(data.Repository.Issues.Nodes))
	for _, issue := range data.Repository.Issues.Nodes {
		out = append(out, projectIssueListEntry(issue))
	}
	g.respondJSON(w, r, "issue.list", out)
}

// handleGHProxyIssueView serves POST /v1/github/issue/view.
func handleGHProxyIssueView(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumberInt(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data struct {
		Repository *struct {
			Issue *ghGQLIssue `json:"issue"`
		} `json:"repository"`
	}
	failure, err := g.graphql(r.Context(), ghIssueViewQuery, map[string]any{
		"owner": g.owner, "name": g.repo, "number": number,
	}, &data)
	if failure != nil || err != nil {
		g.respondOrFail(w, r, "issue.view", failure, err)
		return
	}
	if data.Repository == nil || data.Repository.Issue == nil {
		g.respond(w, r, "issue.view", ghResultFromError(fmt.Sprintf(
			"no issue #%d in %s (it may be a pull-request number, or the repository may not be "+
				"visible to the token agentd is using)", number, g.ownerRepo)), nil)
		return
	}
	g.respondJSON(w, r, "issue.view", projectIssueView(*data.Repository.Issue))
}

// ---------------------------------------------------------------------------
// Pull-request comments
// ---------------------------------------------------------------------------

// ghIssueComment is one entry of the conversation: a plain comment on the pull
// request's underlying issue.
type ghIssueComment struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	AuthorAssociation string `json:"author_association"`
	Body              string `json:"body"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
	HTMLURL           string `json:"html_url"`
}

// ghReview is one review submission. Its BODY is part of the conversation; the
// line-level notes inside it are the other section entirely.
type ghReview struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	AuthorAssociation string `json:"author_association"`
	Body              string `json:"body"`
	State             string `json:"state"`
	SubmittedAt       string `json:"submitted_at"`
	HTMLURL           string `json:"html_url"`
}

// ghReviewComment is one line-level note inside a review's diff thread.
type ghReviewComment struct {
	Path string `json:"path"`
	// Line is null on a comment whose code has since changed; OriginalLine
	// still holds where it was written.
	Line         *int `json:"line"`
	OriginalLine *int `json:"original_line"`
	User         struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt   string `json:"created_at"`
	InReplyToID *int64 `json:"in_reply_to_id"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
}

const (
	ghConversationHeading = "=== conversation (issue comments and review bodies) ==="
	ghInlineHeading       = "=== inline review comments (line-level, where review bots file findings) ==="
)

// handleGHProxyPRComments serves POST /v1/github/pr/comments — everything said
// on a pull request, in two sections.
//
// It takes three reads, because GitHub keeps pull-request feedback in three
// places and there is no endpoint that returns them together:
//
//  1. issues/N/comments — plain comments on the conversation.
//  2. pulls/N/reviews — the BODY of each review submission, which is where a
//     review bot puts its summary.
//  3. pulls/N/comments — the line-level notes inside each review's diff
//     threads, which is where a review bot files its actual findings.
//
// (1) and (2) are interleaved chronologically into the conversation section,
// because that is the document a human reads and neither half makes sense
// alone. (3) is its own section: CodeRabbit's summary is a review body but
// every actionable item is an inline comment, so the conversation alone answers
// "was it reviewed?" and not "what did it say?".
//
// The inline section is a PROJECTION rather than the passthrough every other
// read in this file is, and that is on purpose: the raw review-comment payload
// carries a `diff_hunk` per entry, which repeats the surrounding diff and is
// routinely larger than the comments themselves. The consumer is an agent
// reading prose under a context budget, and handing it 200 KiB of duplicated
// diff to find 30 review notes in serves nobody.
func handleGHProxyPRComments(w http.ResponseWriter, r *http.Request) {
	var body ghProxyNumberRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumberInt(body.Number)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// One budget across every call, so the daemon's answer stays inside the
	// bound the CLI is waiting on however the work divides between them.
	ctx, cancel := context.WithTimeout(r.Context(), ghProxyCommentsTimeout)
	defer cancel()

	conversation, failure, err := g.conversation(ctx, number)
	if failure != nil || err != nil {
		// The pull request could not be read at all — no number, no access, no
		// network. The remaining reads would fail identically and say so twice.
		g.respondOrFail(w, r, "pr.comments", failure, err)
		return
	}
	inline, inlineFailure, inlineErr := g.inlineComments(ctx, number)

	g.respond(w, r, "pr.comments",
		joinGHCommentSections(conversation, inline, inlineFailure, inlineErr), nil)
}

// conversation reads the issue comments and review bodies and renders them as
// one chronological document.
func (g *ghProxySession) conversation(ctx context.Context, number int) (string, *ProxyResult, error) {
	type entry struct {
		when string
		text string
	}
	var entries []entry

	var comments []ghIssueComment
	truncatedComments, failure, err := g.restPaginated(ctx, ghProxyTimeout, ghAPIRequest{
		Path:  g.repoPath("issues/%s/comments", number),
		Query: url.Values{"per_page": []string{"100"}},
	}, func(page []byte) error {
		var batch []ghIssueComment
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("could not read the pull request's comments: %w", err)
		}
		comments = append(comments, batch...)
		return nil
	})
	if failure != nil || err != nil {
		return "", failure, err
	}
	for _, c := range comments {
		entries = append(entries, entry{when: c.CreatedAt, text: formatGHComment(
			c.User.Login, c.AuthorAssociation, "none", c.Body, c.CreatedAt != c.UpdatedAt)})
	}

	var reviews []ghReview
	truncatedReviews, failure, err := g.restPaginated(ctx, ghProxyTimeout, ghAPIRequest{
		Path:  g.repoPath("pulls/%s/reviews", number),
		Query: url.Values{"per_page": []string{"100"}},
	}, func(page []byte) error {
		var batch []ghReview
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("could not read the pull request's reviews: %w", err)
		}
		reviews = append(reviews, batch...)
		return nil
	})
	if failure != nil || err != nil {
		return "", failure, err
	}
	for _, rv := range reviews {
		// A review with no body is a bare approval or a container for inline
		// comments. Rendering it as an empty conversation entry would push the
		// entries that say something out of a bounded response.
		if strings.TrimSpace(rv.Body) == "" {
			continue
		}
		entries = append(entries, entry{when: rv.SubmittedAt, text: formatGHComment(
			rv.User.Login, rv.AuthorAssociation, ghReviewStatus(rv.State), rv.Body, false)})
	}

	// Chronological, because the two sources arrive as two lists and a reader
	// following an argument needs them interleaved. RFC 3339 with a fixed Z
	// offset, which is what GitHub emits, sorts correctly as a string.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].when < entries[j].when })

	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.text)
	}
	if truncatedComments || truncatedReviews {
		fmt.Fprintf(&b,
			"(the conversation has more than %d pages; the rest was not read)\n", maxGHPaginatedPages)
	}
	return b.String(), nil, nil
}

// ghReviewStatus renders a review state as the lower-case prose the section
// reads in. GitHub's own enum is SHOUTED and underscored.
func ghReviewStatus(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes requested"
	case "COMMENTED":
		return "commented"
	case "DISMISSED":
		return "dismissed"
	case "PENDING":
		return "pending"
	case "":
		return "none"
	}
	return strings.ToLower(state)
}

// formatGHComment renders one conversation entry: a small header of
// tab-separated keys, then the body between `--` rules.
//
// The shape is the one this verb has always emitted, and it is worth keeping
// for a reason beyond continuity: the `--` rules are what let a reader — human
// or agent — tell where a comment body ends, which matters precisely because a
// comment body is arbitrary third-party prose that may itself contain anything
// that looks like a header.
func formatGHComment(login, association, status, body string, edited bool) string {
	if login == "" {
		login = "(unknown)"
	}
	association = strings.ToLower(strings.TrimSpace(association))
	if association == "" {
		association = "none"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "author:\t%s\n", login)
	fmt.Fprintf(&b, "association:\t%s\n", association)
	fmt.Fprintf(&b, "edited:\t%t\n", edited)
	fmt.Fprintf(&b, "status:\t%s\n", status)
	b.WriteString("--\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n--\n")
	return b.String()
}

// inlineComments reads and renders the line-level review notes.
func (g *ghProxySession) inlineComments(ctx context.Context, number int) (string, *ProxyResult, error) {
	var comments []ghReviewComment
	truncated, failure, err := g.restPaginated(ctx, ghProxyTimeout, ghAPIRequest{
		Path:  g.repoPath("pulls/%s/comments", number),
		Query: url.Values{"per_page": []string{"100"}},
	}, func(page []byte) error {
		var batch []ghReviewComment
		if err := json.Unmarshal(page, &batch); err != nil {
			return fmt.Errorf("could not read the pull request's inline review comments: %w", err)
		}
		comments = append(comments, batch...)
		return nil
	})
	if failure != nil || err != nil {
		return "", failure, err
	}
	var b strings.Builder
	for _, c := range comments {
		line := "?"
		switch {
		case c.Line != nil:
			line = strconv.Itoa(*c.Line)
		case c.OriginalLine != nil:
			// Where it was written, for a comment whose code has since
			// changed. Reporting "?" rather than a position GitHub no longer
			// has is the honest answer when even that is absent.
			line = strconv.Itoa(*c.OriginalLine)
		}
		b.WriteString("--\n")
		fmt.Fprintf(&b, "file:\t%s:%s\n", c.Path, line)
		fmt.Fprintf(&b, "author:\t%s\n", c.User.Login)
		fmt.Fprintf(&b, "created:\t%s\n", c.CreatedAt)
		fmt.Fprintf(&b, "reply:\t%t\n", c.InReplyToID != nil)
		fmt.Fprintf(&b, "url:\t%s\n", c.HTMLURL)
		b.WriteString("--\n")
		b.WriteString(strings.TrimRight(c.Body, "\n"))
		b.WriteString("\n")
	}
	if truncated {
		fmt.Fprintf(&b, "(more than %d pages of inline comments; the rest was not read)\n", maxGHPaginatedPages)
	}
	return b.String(), nil, nil
}

// joinGHCommentSections assembles the reads into one text payload.
//
// Headings are always emitted, including over an empty section. "no inline
// review comments" and "the inline read failed" are different answers, and an
// agent that sees neither heading cannot tell either of them from "this PR was
// never reviewed" — which is the one conclusion that would make it stop
// looking.
//
// Each section is tailed SEPARATELY, so a long conversation cannot squeeze out
// the inline findings or the other way round.
func joinGHCommentSections(conversation, inline string, inlineFailure *ProxyResult, inlineErr error) ProxyResult {
	conv, convTruncated := ghTailText(conversation, maxGHProxyTextBytes)

	var b strings.Builder
	b.WriteString(ghConversationHeading + "\n\n")
	b.WriteString(ghSectionBody(conv, "(no issue comments or review bodies)"))
	b.WriteString("\n" + ghInlineHeading + "\n\n")

	out := ProxyResult{Truncated: convTruncated}
	switch {
	case inlineErr != nil:
		// A transport failure on the second read is folded into the outcome
		// rather than thrown, because throwing would discard a conversation
		// the daemon has already successfully read. The agent gets the half
		// that worked plus an honest account of the half that did not.
		b.WriteString("(the inline review comments could not be read; see stderr)\n")
		out.ExitCode = ghExitFailure
		out.Stderr = inlineErr.Error()
	case inlineFailure != nil:
		b.WriteString("(the inline review comments could not be read; see stderr)\n")
		out.ExitCode = inlineFailure.ExitCode
		out.Stderr = inlineFailure.Stderr
	default:
		text, truncated := ghTailText(inline, maxGHProxyTextBytes)
		b.WriteString(ghSectionBody(text, "(no inline review comments)"))
		out.Truncated = out.Truncated || truncated
	}
	out.Stdout = b.String()
	return out
}

func ghSectionBody(s, empty string) string {
	if strings.TrimSpace(s) == "" {
		return empty + "\n"
	}
	return strings.TrimRight(s, "\n") + "\n"
}
