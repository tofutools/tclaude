package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// githubproxy_calls.go is the session-level bridge between the raw transport in
// githubapi.go and the verbs in githubproxy_handlers.go.
//
// Every verb still hands `respond` a ProxyResult, exactly as it did when a gh
// subprocess produced one. That is not inertia: ProxyResult is the shape the
// wire contract, the audit row and the CLI's renderer are all written against,
// and an agent that has learned "exit_code 0 means it worked, stderr says why
// it did not" should not have to learn something else because the daemon
// stopped forking. So a GitHub failure becomes a non-zero ProxyResult carrying
// GitHub's own words, and only a failure to reach GitHub at all becomes an
// error — which `respond` turns into a 502, the same as before.

// ghExitFailure is the exit code a GitHub-reported failure is rendered with.
// One, because that is what gh used and what "the command failed" conventionally
// means; the distinction the caller acts on is zero versus non-zero, and the
// actionable detail is in stderr.
const ghExitFailure = 1

// ghResultFromError renders a refusal GitHub made, as opposed to one the daemon
// made. It is deliberately not a proxyFault: a 404 on a pull-request number the
// agent typed wrong is an ANSWER, and reporting it as an HTTP-level daemon
// failure would tell the agent to retry something that will never work.
func ghResultFromError(text string) ProxyResult {
	return ProxyResult{ExitCode: ghExitFailure, Stderr: strings.TrimRight(text, "\n")}
}

// callContext derives the context for one call, bounded by the verb's own
// budget where it has one.
func ghCallContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		if perCall := time.Now().Add(timeout); deadline.Before(perCall) {
			return context.WithCancel(ctx)
		}
	}
	return context.WithTimeout(ctx, timeout)
}

// rest performs one REST call and returns the decoded body, or a ProxyResult
// describing GitHub's refusal.
//
// The three return values are the three distinct outcomes, and collapsing any
// two of them loses something a caller acts on:
//
//	body != nil            GitHub answered and the answer is the payload
//	failure != nil         GitHub refused; report it and stop
//	err != nil             GitHub was not reached; this is a daemon-level 502
func (g *ghProxySession) rest(ctx context.Context, req ghAPIRequest) (body []byte, failure *ProxyResult, err error) {
	return g.restBounded(ctx, ghProxyTimeout, req)
}

func (g *ghProxySession) restBounded(ctx context.Context, timeout time.Duration, req ghAPIRequest) ([]byte, *ProxyResult, error) {
	runCtx, cancel := ghCallContext(ctx, timeout)
	defer cancel()

	res, err := ghDo(runCtx, g.token, req)
	if err != nil {
		return nil, nil, fmt.Errorf("could not reach the GitHub API: %w", err)
	}
	if res.Status < 200 || res.Status > 299 {
		return nil, ghFailureFor(res), nil
	}
	return res.Body, nil, nil
}

// ghFailureFor renders a non-2xx response. Rate limiting gets its own sentence
// ahead of GitHub's message because "API rate limit exceeded" alone does not
// say when to come back, and an agent told only that will retry immediately.
func ghFailureFor(res ghAPIResult) *ProxyResult {
	text := ghErrorText(res)
	if limit := ghRateLimitText(res); limit != "" {
		text = limit + ": " + text
	}
	failure := ghResultFromError(text)
	return &failure
}

// graphql performs one GraphQL operation and unmarshals `data` into out.
//
// doc must be one of the package-level document constants in
// githubproxy_queries.go. vars carries every caller-supplied value. Nothing
// here inspects either — which is exactly why every caller builds vars from
// values that have passed a validateGH* gate, and why no caller builds a
// document by concatenation.
func (g *ghProxySession) graphql(ctx context.Context, doc string, vars map[string]any, out any) (*ProxyResult, error) {
	runCtx, cancel := ghCallContext(ctx, ghProxyTimeout)
	defer cancel()

	res, err := ghDo(runCtx, g.token, ghAPIRequest{
		Method: http.MethodPost,
		Path:   "graphql",
		Body:   ghGraphQLRequest{Query: doc, Variables: vars},
	})
	if err != nil {
		return nil, fmt.Errorf("could not reach the GitHub API: %w", err)
	}
	var parsed ghGraphQLResponse
	// The body is parsed before the status is judged, because GraphQL reports
	// application errors with HTTP 200 as often as with 4xx and the error array
	// is the authoritative one either way.
	decodeErr := json.Unmarshal(res.Body, &parsed)
	if decodeErr != nil {
		if res.Status < 200 || res.Status > 299 {
			return ghFailureFor(res), nil
		}
		return nil, fmt.Errorf("the GitHub GraphQL API returned an unreadable response (HTTP %d)", res.Status)
	}
	if len(parsed.Errors) > 0 {
		text := parsed.errorText()
		if limit := ghRateLimitText(res); limit != "" {
			text = limit + ": " + text
		}
		failure := ghResultFromError(text)
		return &failure, nil
	}
	if res.Status < 200 || res.Status > 299 {
		return ghFailureFor(res), nil
	}
	if out == nil {
		return nil, nil
	}
	if err := json.Unmarshal(parsed.Data, out); err != nil {
		return nil, fmt.Errorf("could not read the GitHub GraphQL response: %w", err)
	}
	return nil, nil
}

// ghPathf builds a REST path from the session's own slug and values that have
// already passed a gate.
//
// Every argument is percent-escaped for a path segment even though the gates
// upstream already refuse anything that would need it. Two reasons: the gate
// and the use are far apart, so a future validator loosened for a good reason
// must not silently become a path-traversal bug here; and an owner or repo
// name is derived from a remote URL rather than from a constant, which is one
// derivation more than "it cannot contain a slash" wants to rest on.
func ghPathf(format string, args ...any) string {
	escaped := make([]any, len(args))
	for i, a := range args {
		escaped[i] = url.PathEscape(fmt.Sprint(a))
	}
	return fmt.Sprintf(format, escaped...)
}

// repoPath renders a path under this session's repository.
//
// Every verb in the format must be %s, including the ones standing in for
// numbers: ghPathf escapes each argument into a string before formatting, so a
// %d would render as a format error rather than as the id. The trailing slash
// an empty format leaves is trimmed, because "repos/o/r/" and "repos/o/r" are
// not the same resource to GitHub.
func (g *ghProxySession) repoPath(format string, args ...any) string {
	return strings.TrimSuffix(
		ghPathf("repos/%s/%s/"+format, append([]any{g.owner, g.repo}, args...)...), "/")
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// ghNextLinkPattern extracts the `next` URL from a Link header.
//
// GitHub emits the header as `<url>; rel="next", <url>; rel="last"`. Parsing it
// rather than incrementing a page counter is what makes a walk terminate on
// GitHub's own terms: a filtered listing can end before the declared last page,
// and a synthesised `?page=N+1` would keep asking for empty pages until the
// page cap stopped it.
var ghNextLinkPattern = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

func ghNextLink(header http.Header) string {
	for _, value := range header.Values("Link") {
		for _, part := range strings.Split(value, ",") {
			if m := ghNextLinkPattern.FindStringSubmatch(part); m != nil {
				return strings.TrimSpace(m[1])
			}
		}
	}
	return ""
}

// restPaginated walks a listing endpoint, calling visit with each page's raw
// body until GitHub stops offering a next link.
//
// It is the equivalent of `gh api --paginate`, and it carries the same bound
// the rest of this proxy does: maxGHPaginatedPages, so a pathological thread
// cannot hold a request open indefinitely. The bound is reported through the
// truncated return rather than silently, because a caller that renders a
// partial list as a complete one is the failure mode this whole file avoids.
func (g *ghProxySession) restPaginated(
	ctx context.Context, timeout time.Duration, req ghAPIRequest, visit func(page []byte) error,
) (truncated bool, failure *ProxyResult, err error) {
	next := req
	for page := 0; page < maxGHPaginatedPages; page++ {
		runCtx, cancel := ghCallContext(ctx, timeout)
		res, doErr := ghDo(runCtx, g.token, next)
		cancel()
		if doErr != nil {
			return false, nil, fmt.Errorf("could not reach the GitHub API: %w", doErr)
		}
		if res.Status < 200 || res.Status > 299 {
			return false, ghFailureFor(res), nil
		}
		if err := visit(res.Body); err != nil {
			return false, nil, err
		}
		link := ghNextLink(res.Header)
		if link == "" {
			return false, nil, nil
		}
		// The link already carries its own query, so passing Query again would
		// overwrite the cursor GitHub just handed back.
		next = ghAPIRequest{Method: req.Method, Path: link, Accept: req.Accept}
	}
	return true, nil, nil
}

// ---------------------------------------------------------------------------
// Shared response assembly
// ---------------------------------------------------------------------------

// ghMarshal renders a document the CLI will pretty-print.
//
// json.Marshal, not an encoder with SetEscapeHTML(false): the CLI's renderer
// runs json.Indent over these bytes and GitHub's own responses are escaped the
// same way, so this keeps one escaping convention across every verb.
func ghMarshal(v any) ([]byte, error) {
	doc, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("could not render the GitHub response: %w", err)
	}
	return doc, nil
}

// ghTailText bounds a text payload the way the subprocess seam's tail did:
// keep the END, because that is where a failing step's error and a comment
// thread's most recent entry are.
func ghTailText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	tail := s[len(s)-max:]
	// Start at a line boundary rather than mid-line, so the first line of a
	// truncated log is not a fragment that reads like corrupted output.
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:]
	}
	// The slice is by BYTES, so it can land inside a multi-byte rune, and the
	// line-boundary trim above only repairs that when the tail happens to hold
	// a newline. A comment thread in a non-Latin script, or a log line longer
	// than the bound, would otherwise begin with an invalid sequence. This is
	// the same repair proxyTail.String applies to the same kind of payload.
	return strings.ToValidUTF8(tail, "?"), true
}
