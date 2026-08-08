package agentd

import (
	"context"
	"fmt"
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
	ghRunListFields = "databaseId,headSha,attempt,conclusion,status,workflowName,displayTitle,headBranch,event,createdAt,url"
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

// openGHProxy is the shared prologue: method check, permission preflight,
// bounded body decode, every git-side gate plus repository derivation, then
// the final remote-scoped permission decision before gh can run.
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
	// --head is NOT optional here, whatever gh's own default says. gh derives
	// the head branch from the local repository, and this proxy runs gh in a
	// neutral directory on purpose — so without an explicit value gh fails with
	// "could not determine the current branch: ... not a git repository"
	// (verified on gh 2.97). The daemon supplies the agent's real branch, read
	// from the git session, which is the branch the caller means anyway.
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

	args := []string{"pr", "create", "--repo", g.ownerRepo, "--title", strings.TrimSpace(body.Title)}
	if body.Draft {
		args = append(args, "--draft")
	}
	for _, ref := range []struct {
		flag  string
		value string
	}{{"--base", body.Base}, {"--head", head}} {
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

// handleGHProxyPRComments serves POST /v1/github/pr/comments — everything
// said on a pull request, in two sections.
//
// It takes TWO gh calls, because GitHub keeps PR feedback in two places and gh
// has no single command that returns both:
//
//  1. `pr view --comments` — issue comments and the BODY of each review
//     submission, interleaved chronologically. This is the conversation.
//  2. `api …/pulls/N/comments` — the line-level comments inside each review's
//     diff threads. This is where a review bot files its actual findings;
//     CodeRabbit's summary is a review body, but every actionable item is an
//     inline comment, so section 1 alone answers "was it reviewed?" and not
//     "what did it say?".
//
// Neither is `--json`, and the second is deliberately PROJECTED through a
// fixed jq program rather than passed through unmodelled the way every other
// read in this file is. That is a real exception to the file's rule and it is
// made on purpose: the raw review-comment payload carries `diff_hunk` for
// every entry, which repeats the surrounding diff and is routinely larger than
// the comments themselves. The consumer here is an agent reading prose under a
// context budget, and handing it 200 KiB of duplicated diff to find 30 review
// notes in serves nobody. The projection lives in one constant below, so the
// cost of a new GitHub field is editing that string.
func handleGHProxyPRComments(w http.ResponseWriter, r *http.Request) {
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
	// One budget across both calls, so the daemon's answer stays inside the
	// bound the CLI is waiting on however the work divides between them.
	ctx, cancel := context.WithTimeout(r.Context(), ghProxyCommentsTimeout)
	defer cancel()

	// Bulk on both: a long-running PR's thread runs to tens of kilobytes, and
	// the diagnosis-sized default tail would cut it off mid-review. Each call
	// is tailed separately, so a huge conversation cannot squeeze out the
	// inline findings or the other way round.
	conv, err := g.ghBulk(ctx, ghProxyTimeout,
		"pr", "view", number, "--repo", g.ownerRepo, "--comments")
	if err != nil || conv.ExitCode != 0 {
		// gh could not read the pull request at all — no number, no access, no
		// network. The second call would fail identically and say it twice.
		g.respond(w, r, "pr.comments", conv, err)
		return
	}
	// The literal derived slug, NEVER gh's `{owner}/{repo}` placeholders: gh
	// expands those from the repository it discovers in the working directory,
	// which is the agent-writable .git/config this proxy runs in a neutral
	// directory to escape. Both halves are slug-validated in newGHProxySession
	// (no '/', no '?'), and number is a re-formatted integer, so the path is
	// fully determined by values that have already passed a gate.
	//
	// The query string carries per_page because `gh api -f`/`-F` would flip the
	// request to POST — writing with a credential from a read verb.
	inlinePath := fmt.Sprintf("repos/%s/pulls/%s/comments?per_page=100", g.ownerRepo, number)
	inline, inlineErr := g.ghBulk(ctx, ghProxyTimeout,
		"api", inlinePath, "--paginate", "--jq", ghReviewCommentProjection)

	// nil, deliberately, whatever the inline call did. respond() turns a
	// non-nil error into a bare 502 and drops the result — which would throw
	// away a conversation the daemon has already successfully read. A transport
	// failure on the second call is folded into the outcome instead, so the
	// agent gets the half that worked plus an honest account of the half that
	// did not.
	g.respond(w, r, "pr.comments", joinGHCommentSections(conv, inline, inlineErr), nil)
}

// ghReviewCommentProjection renders one inline review comment per entry, in
// the same `key:\tvalue` + `--` + body shape gh's own `--comments` output
// uses, so the two sections read as one document.
//
// `line` is null on a comment whose code has since changed; `original_line`
// still holds where it was written, and reporting "?" rather than "null" says
// so without pretending to a position GitHub no longer has.
const ghReviewCommentProjection = `.[] | "--\nfile:\t\(.path):\(.line // .original_line // "?")` +
	`\nauthor:\t\(.user.login)\ncreated:\t\(.created_at)` +
	`\nreply:\t\(.in_reply_to_id != null)\nurl:\t\(.html_url)\n--\n\(.body)\n"`

const (
	ghConversationHeading = "=== conversation (issue comments and review bodies) ==="
	ghInlineHeading       = "=== inline review comments (line-level, where review bots file findings) ==="
)

// joinGHCommentSections assembles the two reads into one text payload.
//
// Headings are always emitted, including over an empty section. "no inline
// review comments" and "the inline read failed" are different answers, and an
// agent that sees neither heading cannot tell either of them from "this PR was
// never reviewed" — which is the one conclusion that would make it stop
// looking.
func joinGHCommentSections(conv, inline ProxyResult, inlineErr error) ProxyResult {
	// A transport failure becomes an ordinary non-zero outcome carrying gh's
	// reason, so from here down there is one failure shape rather than two.
	if inlineErr != nil {
		inline = ProxyResult{ExitCode: -1, Stderr: inlineErr.Error()}
	}
	var b strings.Builder
	b.WriteString(ghConversationHeading + "\n\n")
	b.WriteString(ghSectionBody(conv.Stdout, "(no issue comments or review bodies)"))
	b.WriteString("\n" + ghInlineHeading + "\n\n")
	if inline.ExitCode != 0 {
		b.WriteString("(the inline review comments could not be read; see stderr)\n")
	} else {
		b.WriteString(ghSectionBody(inline.Stdout, "(no inline review comments)"))
	}
	return ProxyResult{
		Stdout: b.String(),
		// The conversation succeeded to get here, so its own stderr is at most
		// a warning; the failure worth reporting is the inline read's.
		Stderr:    inline.Stderr,
		ExitCode:  inline.ExitCode,
		Truncated: conv.Truncated || inline.Truncated,
		TimedOut:  conv.TimedOut || inline.TimedOut,
	}
}

func ghSectionBody(s, empty string) string {
	if strings.TrimSpace(s) == "" {
		return empty + "\n"
	}
	return strings.TrimRight(s, "\n") + "\n"
}

// ghProxyRunRequest names one GitHub Actions workflow run.
type ghProxyRunRequest struct {
	Remote string `json:"remote,omitempty"`
	RunID  int64  `json:"run_id"`
}

type ghProxyRunListRequest struct {
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// handleGHProxyRunList serves POST /v1/github/run/list — the run ids
// `run log-failed` needs.
//
// Without it the only route to a run id is regexing one out of the detailsUrl
// buried in a `pr checks` rollup, which works and reads like a workaround.
//
// It also reaches runs `pr checks` cannot show, though not the ones you might
// expect. A statusCheckRollup is scoped to the pull request's HEAD COMMIT, so
// a force-push or an amend takes every run against the superseded commit out
// of `pr checks` entirely, while `run ls --branch` still lists them. Re-runs
// are NOT such a case: re-running does not create a new run, it adds an
// attempt to the same run id, and both `pr checks` and `run list` then report
// that latest attempt's conclusion.
//
// `databaseId` is the field that matters — it is the id every other run verb
// takes — so it leads the field list rather than sitting alphabetically among
// the rest. `headSha` and `attempt` follow because they are what distinguish a
// superseded run from a current one, and a re-run from a first try.
func handleGHProxyRunList(w http.ResponseWriter, r *http.Request) {
	var body ghProxyRunListRequest
	g, ok := openGHProxy(w, r, PermGitHubRead, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	limit, fault := validateGHLimit(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	args := []string{"run", "list", "--repo", g.ownerRepo, "--limit", limit, "--json", ghRunListFields}
	if branch := strings.TrimSpace(body.Branch); branch != "" {
		// The same gate every other ref parameter passes. A branch reaches
		// argv, so "--exec=id" must not survive being called a branch name.
		if fault := validateBranchName(branch); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		args = append(args, "--branch", branch)
	}
	status, fault := validateGHRunStatus(body.Status)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if status != "" {
		args = append(args, "--status", status)
	}
	// Not bulk: a bounded list of run metadata, like every other --json read.
	res, err := g.gh(r.Context(), args...)
	g.respond(w, r, "run.list", res, err)
}

// handleGHProxyRunLogFailed serves POST /v1/github/run/log-failed — the log of
// whatever steps failed in a workflow run.
//
// It is the other half of `pr checks`: checks says WHICH job went red, this
// says why. Only the failed steps, never `--log`: the full log of a green
// matrix build is megabytes of noise that would blow the response bound and
// tell the agent nothing it did not already know from the check rollup.
//
// The run id is not derived from anything — an agent takes it from the
// `detailsUrl` in a `pr checks` rollup — but it is still bounded by the same
// repository, because gh is given the derived --repo and a run id belonging to
// another repository simply 404s.
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
	res, err := g.ghBulk(r.Context(), ghProxyLogTimeout,
		"run", "view", runID, "--repo", g.ownerRepo, "--log-failed")
	g.respond(w, r, "run.log-failed", res, err)
}

// ghProxyPREditRequest edits an existing pull request's title and/or body.
type ghProxyPREditRequest struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	Remote string `json:"remote,omitempty"`
}

// handleGHProxyPREdit serves POST /v1/github/pr/edit.
//
// Editing a description is a WRITE under the operator's GitHub identity, so it
// sits behind github.write beside `pr create` rather than beside the reads. It
// is deliberately narrow: title and body only. `gh pr edit` can also move the
// base branch, add reviewers and change labels, and none of that is something
// the proxy's semantic contract covers — an agent that wants it should ask a
// human, not get it as a side effect of fixing a typo.
func handleGHProxyPREdit(w http.ResponseWriter, r *http.Request) {
	var body ghProxyPREditRequest
	g, ok := openGHProxy(w, r, PermGitHubWrite, &body, func() string { return body.Remote })
	if !ok {
		return
	}
	number, fault := validateGHNumber(body.Number)
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
	args := []string{"pr", "edit", number, "--repo", g.ownerRepo}
	if title != "" {
		if fault := validateGHTitle(title); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		args = append(args, "--title", title)
	}
	if hasBody {
		if fault := validateGHBody(body.Body, true); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		// By file, like every other free text: a PR body is prose that may
		// contain anything, and argv is world-readable through /proc.
		path, cleanup, fault := g.bodyFile(body.Body)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		defer cleanup()
		args = append(args, "--body-file", path)
	}
	res, err := g.gh(r.Context(), args...)
	g.respond(w, r, "pr.edit", res, err)
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
