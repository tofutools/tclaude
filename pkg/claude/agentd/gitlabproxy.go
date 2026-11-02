package agentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// gitlabproxy.go is the daemon half of `tclaude proxy gitlab` — merge-request,
// issue and CI operations performed with agentd's own GitLab token on behalf
// of an agent that holds no credential of its own.
//
// It is the GitHub proxy's sibling and reuses its gates wholesale: the
// repository still comes from the agent's daemon-recorded launch directory, and
// the GitLab PROJECT it acts on is derived from that repository's validated,
// allow-listed remote. There is no --project parameter and no passthrough. The
// one gate this family adds is the instance itself: a remote is only a GitLab
// project if its host is one the operator wrote into
// agent.git_proxy.gitlab_hosts (or is gitlab.com), because a self-hosted
// instance cannot be recognised by name, and the base URL that entry carries is
// the only place the token is ever sent.
//
// Where GitHub needs GraphQL for some reads, GitLab's REST API answers every
// question this proxy asks, so there is one transport and no documents. The
// rendered field names follow the GitHub proxy's (number, headRefName, url,
// isDraft) so the bundled skill and an agent's habits carry over; the VALUES
// are GitLab's own (an iid, "opened", a pipeline rather than a check rollup).

// The per-verb budgets are the GitHub proxy's. Same kinds of call, same
// reasons; see the constants in githubproxy.go.
const (
	glProxyTimeout         = ghProxyTimeout
	glProxyLogTimeout      = ghProxyLogTimeout
	glProxyCommentsTimeout = ghProxyCommentsTimeout
	glProxyDownloadTimeout = ghProxyDownloadTimeout
)

// glProxySession is one GitLab invocation context: the project the agent's
// own remote resolved to, the instance it lives on, and the credential.
type glProxySession struct {
	convID string
	// host is the remote's host as the allow-list spells it; base is the
	// instance's web/API root, which may carry a port the SSH remote does not.
	host string
	base string
	// project is the full namespace path, "group/subgroup/project". GitLab
	// identifies a project by exactly that, and nested groups are ordinary, so
	// unlike the GitHub slug it is never re-derived from first and last
	// segments: it IS the path the allow-list admitted.
	project string
	// projectID is project escaped as ONE path segment (group%2Fsub%2Fproject),
	// which is how GitLab's REST API takes a project path in a URL.
	projectID string
	remoteKey string
	// token is the operator's GitLab credential, held for one request and sent
	// only as an `Authorization: Bearer` header. Never logged, never rendered.
	token string
	// branch is the agent's current branch, read while the git session is
	// open; `mr create` needs it as the source branch.
	branch string
	// repoRoot is the agent's own work-tree root; only `job download` uses it,
	// as the root it may not write outside of.
	repoRoot   string
	auditExtra string
}

// newGLProxySession runs every gate and resolves the project from the agent's
// own remote. The git-side gates run first and in full, so a caller with
// proxy.gitlab.read still cannot reach a project whose remote is not on the
// operator's allow-list.
func newGLProxySession(ctx context.Context, convID, requestedRemote string, remoteScoped bool) (*glProxySession, *proxyFault) {
	s, resolved, fault := openProxyRemote(ctx, convID, requestedRemote, remoteScoped)
	if fault != nil {
		return nil, fault
	}
	host := resolved.FetchRef.Host
	base := s.policy.GitLabBase(host)
	if base == "" {
		if host == "github.com" {
			return nil, faultf(http.StatusConflict, "not_gitlab",
				"remote %q points at github.com; use `tclaude proxy github` for it", resolved.Name)
		}
		return nil, faultf(http.StatusConflict, "not_gitlab",
			"remote %q points at %s, which is not a configured GitLab instance; the operator adds it to "+
				"agent.git_proxy.gitlab_hosts as {\"%s\": \"https://…\"} (gitlab.com needs no entry)",
			resolved.Name, host, host)
	}
	for _, seg := range resolved.FetchRef.Path {
		if !isGitLabPathSegment(seg) {
			return nil, faultf(http.StatusConflict, "not_gitlab",
				"remote %q does not resolve to a valid GitLab project path (segment %q)", resolved.Name, seg)
		}
	}
	project := strings.Join(resolved.FetchRef.Path, "/")
	token, fault := gitlabToken(s.policy)
	if fault != nil {
		return nil, fault
	}
	slog.Debug("gitlab proxy resolved a project", "project", project, "base", base)
	return &glProxySession{
		convID:    convID,
		host:      host,
		base:      base,
		project:   project,
		projectID: url.PathEscape(project),
		remoteKey: resolved.FetchRef.Key(),
		token:     token,
		branch:    s.currentBranch(ctx),
		repoRoot:  s.repoRoot,
	}, nil
}

// isGitLabPathSegment bounds one namespace or project path segment to what
// GitLab itself allows: letters, digits, underscores, dots and hyphens,
// starting with a letter, digit or underscore. The remote parser has already
// refused ".", ".." and empty segments; this refuses what would need escaping
// in a URL, so projectID is a straight PathEscape of a value known to be
// plain.
func isGitLabPathSegment(seg string) bool {
	if seg == "" || len(seg) > 255 {
		return false
	}
	for i, r := range seg {
		switch {
		case unicode.IsLetter(r) && r < 0x80, r >= '0' && r <= '9', r == '_':
		case r == '.' || r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------

// rest performs one REST call against the session's instance; the three
// outcomes mean what they mean on the GitHub side (body, refusal, unreachable).
func (g *glProxySession) rest(ctx context.Context, req glAPIRequest) ([]byte, *ProxyResult, error) {
	return g.restBounded(ctx, glProxyTimeout, req)
}

func (g *glProxySession) restBounded(ctx context.Context, timeout time.Duration, req glAPIRequest) ([]byte, *ProxyResult, error) {
	runCtx, cancel := ghCallContext(ctx, timeout)
	defer cancel()

	res, err := glDo(runCtx, g.base, g.token, req)
	if err != nil {
		return nil, nil, fmt.Errorf("could not reach the GitLab API at %s: %w", g.base, err)
	}
	if res.Status < 200 || res.Status > 299 {
		return nil, glFailureFor(res), nil
	}
	return res.Body, nil, nil
}

// glFailureFor renders a non-2xx response, rate limiting first.
func glFailureFor(res glAPIResult) *ProxyResult {
	text := glErrorText(res)
	if limit := glRateLimitText(res); limit != "" {
		text = limit + ": " + text
	}
	failure := ghResultFromError(text)
	return &failure
}

// projectPath renders a path under this session's project. Every verb in the
// format is %s (ghPathf escapes each argument into a string first), and the
// project id is prepended as the already-escaped single segment it is.
func (g *glProxySession) projectPath(format string, args ...any) string {
	return strings.TrimSuffix("projects/"+g.projectID+"/"+ghPathf(format, args...), "/")
}

// restPaginated walks a listing, following GitLab's Link header until it stops
// offering a next page or the page cap is reached. GitLab emits the same
// `<url>; rel="next"` form GitHub does, so the parser is shared.
func (g *glProxySession) restPaginated(
	ctx context.Context, timeout time.Duration, req glAPIRequest, visit func(page []byte) error,
) (truncated bool, failure *ProxyResult, err error) {
	next := req
	for page := 0; page < maxGLPaginatedPages; page++ {
		runCtx, cancel := ghCallContext(ctx, timeout)
		res, doErr := glDo(runCtx, g.base, g.token, next)
		cancel()
		if doErr != nil {
			return false, nil, fmt.Errorf("could not reach the GitLab API at %s: %w", g.base, doErr)
		}
		if res.Status < 200 || res.Status > 299 {
			return false, glFailureFor(res), nil
		}
		if err := visit(res.Body); err != nil {
			return false, nil, err
		}
		link := ghNextLink(res.Header)
		if link == "" {
			return false, nil, nil
		}
		next = glAPIRequest{Method: req.Method, Path: link, Accept: req.Accept}
	}
	return true, nil, nil
}

// ---------------------------------------------------------------------------
// Response shape
// ---------------------------------------------------------------------------

// The wire shape is the GitHub proxy's ghProxyOutcome, unchanged: the CLI
// renders both families with one renderer, and an agent that has learned
// "exit_code 0 means it worked, stderr says why it did not" learns it once.

func (g *glProxySession) respond(w http.ResponseWriter, r *http.Request, verb string, res ProxyResult, err error) {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			g.writeOutcome(w, r, verb, ghProxyOutcome{
				Repo:     g.project,
				ExitCode: ghExitFailure,
				TimedOut: true,
				Stderr: "the GitLab API did not answer within the time the daemon allows: " +
					err.Error(),
			})
			return
		}
		writeError(w, http.StatusBadGateway, "gitlab_failed", err.Error())
		return
	}
	g.writeOutcome(w, r, verb, ghProxyOutcome{
		Repo:      g.project,
		ExitCode:  res.ExitCode,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Truncated: res.Truncated,
		TimedOut:  res.TimedOut,
	})
}

func (g *glProxySession) respondJSON(w http.ResponseWriter, r *http.Request, verb string, payload any) {
	doc, err := ghMarshal(payload)
	if err != nil {
		writeError(w, http.StatusBadGateway, "gitlab_failed", err.Error())
		return
	}
	if len(doc) > maxGHProxyDocumentBytes {
		writeProxyFault(w, faultf(http.StatusRequestEntityTooLarge, "response_too_large",
			"GitLab's answer to this read is %s, over the %s the proxy will return; ask for fewer "+
				"items with `--limit`", humanBytes(int64(len(doc))), humanBytes(maxGHProxyDocumentBytes)))
		return
	}
	g.writeOutcome(w, r, verb, ghProxyOutcome{Repo: g.project, JSON: doc})
}

func (g *glProxySession) respondOrFail(w http.ResponseWriter, r *http.Request, verb string, failure *ProxyResult, err error) {
	if err != nil {
		g.respond(w, r, verb, ProxyResult{}, err)
		return
	}
	if failure != nil {
		g.respond(w, r, verb, *failure, nil)
		return
	}
	writeError(w, http.StatusInternalServerError, "gitlab_failed",
		"the daemon produced no result for this request; this is a tclaude bug")
}

func (g *glProxySession) writeOutcome(w http.ResponseWriter, r *http.Request, verb string, out ghProxyOutcome) {
	detail := fmt.Sprintf("repo=%s/%s op=%s exit=%d", g.host, g.project, verb, out.ExitCode)
	if g.auditExtra != "" {
		detail += " " + g.auditExtra
	}
	setAuditDetail(r, detail)
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Vocabularies
// ---------------------------------------------------------------------------

// glMRStates is the `mr ls --state` vocabulary, in the CLI's words. GitLab
// spells the first one "opened"; glMRStateParam translates. "locked" is left
// out on purpose: it is a transient state during a merge, not something an
// agent lists by.
var glMRStates = []string{"open", "closed", "merged", "all"}

func glMRStateParam(state string) string {
	if state == "open" {
		return "opened"
	}
	return state
}

// glIssueStates is the `issue ls --state` vocabulary.
var glIssueStates = []string{"open", "closed", "all"}

// glMergeMethods is the `mr merge --method` vocabulary. GitLab's merge
// endpoint takes a squash flag and nothing else; a rebase is a separate
// operation on the merge request that this proxy does not perform, so it is
// not offered rather than being silently mapped to a merge commit.
//
// This is the AUTHORITY; the CLI keeps a copy for completion and a test pins
// the two together.
var glMergeMethods = []string{"merge", "squash"}

// GLMergeMethodsForTest exposes the gate's vocabulary.
func GLMergeMethodsForTest() []string { return append([]string(nil), glMergeMethods...) }

func validateGLMergeMethod(method string) (string, *proxyFault) {
	return validateGHEnum("merge method", method, glMergeMethods...)
}

// glPipelineStatuses is the `pipeline ls --status` vocabulary: GitLab's own
// pipeline statuses, which its listing endpoint filters by.
var glPipelineStatuses = []string{
	"created", "waiting_for_resource", "preparing", "pending", "running",
	"success", "failed", "canceled", "skipped", "manual", "scheduled",
}

// GLPipelineStatusesForTest exposes the gate's vocabulary.
func GLPipelineStatusesForTest() []string { return append([]string(nil), glPipelineStatuses...) }

// validateGLPipelineStatus bounds the pipeline-list filter; empty means no
// filter, as on the GitHub run listing.
func validateGLPipelineStatus(status string) (string, *proxyFault) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return "", nil
	}
	for _, a := range glPipelineStatuses {
		if status == a {
			return a, nil
		}
	}
	return "", faultf(http.StatusBadRequest, "invalid_arg",
		"status %q is not one of: %s", status, strings.Join(glPipelineStatuses, ", "))
}

// validateGLIID bounds a merge-request or issue iid. Same range as a GitHub
// number: per-project and small.
func validateGLIID(n int) (int, *proxyFault) {
	if n <= 0 || n > 100_000_000 {
		return 0, faultf(http.StatusBadRequest, "invalid_arg",
			"a positive merge-request/issue number (iid) is required")
	}
	return n, nil
}

// validateGLID bounds a pipeline or job id, which like a GitHub run id is a
// global database id. 2^53 for the same reason validateGHRunID uses it.
func validateGLID(what string, id int64) (int64, *proxyFault) {
	if id <= 0 || id > 1<<53 {
		return 0, faultf(http.StatusBadRequest, "invalid_arg", "a positive %s id is required", what)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Draft titles
// ---------------------------------------------------------------------------

// GitLab has no draft flag an API caller sets: a merge request is a draft when
// its title starts with one of these markers, and it is marked ready by
// removing them. The API reports the result in the read-only `draft` field.
var glDraftPrefixes = []string{"draft:", "[draft]", "(draft)", "draft -", "wip:", "[wip]"}

// glStripDraftPrefix removes every leading draft marker from a title.
func glStripDraftPrefix(title string) string {
	for {
		trimmed := strings.TrimSpace(title)
		lower := strings.ToLower(trimmed)
		stripped := false
		for _, p := range glDraftPrefixes {
			if strings.HasPrefix(lower, p) {
				title = trimmed[len(p):]
				stripped = true
				break
			}
		}
		if !stripped {
			return trimmed
		}
	}
}

// glDraftTitle marks a title as a draft the way GitLab's own UI does.
func glDraftTitle(title string) string {
	return "Draft: " + glStripDraftPrefix(title)
}
