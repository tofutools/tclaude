package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// gitlabapi.go is the GitLab half of the proxy's outbound edge: credential
// resolution and the two HTTP seams every `tclaude proxy gitlab` verb goes
// through. It is the sibling of githubapi.go and keeps the same shape on
// purpose — the same two seams, the same test hooks, the same bounds — so the
// flow tests for one family read like the flow tests for the other.
//
// Two things differ from GitHub, and both follow from GitLab being something
// people host themselves:
//
//  1. There is no fixed API host. The base URL comes from the operator's
//     agent.git_proxy.gitlab_hosts entry for the remote's host (or is
//     https://gitlab.com), is pinned on the session, and every request —
//     including a pagination link GitLab hands back — is refused unless it
//     points at that same scheme, host and port. A credential is only ever
//     sent to the instance the operator wrote down.
//  2. There is no CLI to ask for a token. `gh auth token` has a GitLab
//     counterpart in `glab`, but a self-hosted instance means per-host
//     accounts, and guessing which one glab would pick is exactly the kind of
//     quiet credential selection the GitHub side refuses to do. So the token
//     comes from a file the operator names, and from nowhere else.

const (
	// gitlabAPIPrefix is the REST root under an instance's base URL.
	gitlabAPIPrefix = "api/v4"

	// glUserAgent identifies the daemon in the instance's logs.
	glUserAgent = "tclaude-agentd"

	// maxGLAPIResponseBytes bounds an in-memory API response, the same figure
	// as the GitHub side and for the same reason: well above any single page
	// this proxy asks for, far below what would matter to the daemon.
	maxGLAPIResponseBytes = 16 << 20

	// maxGLPaginatedPages bounds a listing walk; see maxGHPaginatedPages.
	maxGLPaginatedPages = 20
)

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// gitlabToken resolves the credential a GitLab request will spend. One source,
// deliberately: the operator's configured token file. A file that cannot be
// read is an error rather than a reason to try something else, and with no
// file configured the proxy says exactly which key to set.
//
// Never cached across requests, so a rotated token file takes effect without a
// daemon restart.
func gitlabToken(policy config.GitProxyConfig) (string, *proxyFault) {
	configured := strings.TrimSpace(policy.GitLabTokenFile)
	if configured == "" {
		return "", faultf(http.StatusServiceUnavailable, "token_missing",
			"no GitLab token is available to the daemon: set agent.git_proxy.gitlab_token_file in "+
				"~/.tclaude/data/config.json to a file holding a personal or project access token "+
				"with the `api` scope")
	}
	raw, err := os.ReadFile(expandTilde(configured))
	if err != nil {
		return "", faultf(http.StatusServiceUnavailable, "token_unreadable",
			"the configured agent.git_proxy.gitlab_token_file could not be read: %v%s",
			err, shellVarHint(configured))
	}
	// Every ASCII whitespace character goes, anywhere, exactly as the git
	// credential helper's `tr -d '[:space:]'` does (gitLabTokenCredentialHelper),
	// so the API and git always send the same token from the same file.
	token := strings.Map(func(r rune) rune {
		if strings.ContainsRune(" \t\n\v\f\r", r) {
			return -1
		}
		return r
	}, string(raw))
	if token == "" {
		return "", faultf(http.StatusServiceUnavailable, "token_unreadable",
			"the configured agent.git_proxy.gitlab_token_file is empty")
	}
	// The same injection gate the GitHub token passes: a stray newline in the
	// file would be a header-splitting value.
	for _, r := range token {
		if r < 0x20 || r == 0x7f {
			return "", faultf(http.StatusServiceUnavailable, "token_unreadable",
				"the GitLab token from agent.git_proxy.gitlab_token_file contains a control character, "+
					"so it cannot be sent as a header; check for a stray newline or tab inside the value")
		}
	}
	return token, nil
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// glAPIRequest is one outbound GitLab call, fully resolved before it reaches
// the seam.
//
// Path is a REST path relative to <base>/api/v4 ("projects/123/merge_requests")
// or the absolute URL of a pagination link. As on the GitHub side, every
// caller-influenced scalar travels in Query or Body, never interpolated into
// Path — and a project path, which contains slashes, travels as ONE escaped
// path segment (see glProxySession.projectPath).
type glAPIRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   any
	// Accept overrides the default JSON media type. A job trace is plain text.
	Accept string
	// GraphQL sends the request to the instance's GraphQL endpoint,
	// <base>/api/graphql, instead of a REST path; Path and Query must then be
	// empty and Body carries {query, variables}. The query text is always a
	// daemon constant, with any value passed as a variable.
	GraphQL bool
}

// gitlabGraphQLPath is the instance-relative GraphQL endpoint.
const gitlabGraphQLPath = "api/graphql"

// glAPIResult is a bounded response.
type glAPIResult struct {
	Status int
	Body   []byte
	Header http.Header
}

// glStreamResult is what a bulk transfer reports back; see ghStreamResult.
type glStreamResult struct {
	Status int
	Bytes  int64
	Body   []byte
}

// glHTTPClient is the daemon's client for GitLab instances. No client-level
// timeout, for the reason ghHTTPClient has none: every call runs under a
// per-verb context deadline.
var glHTTPClient = &http.Client{CheckRedirect: glCheckRedirect}

// glMaxRedirects bounds a redirect chain, as Go's default policy does.
const glMaxRedirects = 10

// glCheckRedirect keeps the token on its instance across redirects. Go's own
// rule (net/http shouldCopyHeaderOnRedirect) keeps Authorization on any
// redirect to the same HOSTNAME, whatever its scheme or port, and on its
// subdomains. That is not the instance: object storage on another port of the
// same box, a plain-http endpoint, or a storage subdomain would all receive the
// token. So the token rides only while a hop stays on the original request's
// exact origin (scheme, host and port, with :443 folded). A redirect elsewhere,
// such as an artifact download handed to object storage, is followed without
// it. A redirect to anything but https is refused outright, so neither the
// token nor the response travels in clear text. Go copies the headers onto req
// before calling this, so deleting the header here takes effect.
func glCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= glMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", glMaxRedirects)
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("refusing a redirect to a non-https URL (%s)", req.URL.Redacted())
	}
	if len(via) > 0 && !sameHTTPSOrigin(req.URL, via[0].URL) {
		req.Header.Del("Authorization")
	}
	return nil
}

// sameHTTPSOrigin compares scheme, hostname and port, folding https's
// implicit 443.
func sameHTTPSOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		httpsPortKey(a.Port()) == httpsPortKey(b.Port())
}

// glDo is the bounded-response boundary. base is the instance the session
// pinned; production performs the real request, flow tests swap in a recorder.
var glDo = doGitLabRequest

// SetGitLabTransportForTest swaps the bounded-response boundary. Returns a
// restore func.
func SetGitLabTransportForTest(fn func(ctx context.Context, base, token string, req glAPIRequest) (glAPIResult, error)) func() {
	prev := glDo
	glDo = fn
	return func() { glDo = prev }
}

// GitLabRequestForTest is a recorded request in exported terms, for flow tests
// in package agentd_test. Base is the instance URL the daemon resolved the
// request against, which is the one thing a GitLab test asserts on that a
// GitHub test cannot: that a self-hosted remote reached its configured base and
// not gitlab.com.
type GitLabRequestForTest struct {
	Base    string
	Method  string
	Path    string
	Query   url.Values
	Body    []byte
	Accept  string
	GraphQL bool
}

// SetGitLabTransportForTestJSON is SetGitLabTransportForTest in exported types.
func SetGitLabTransportForTestJSON(
	fn func(ctx context.Context, token string, req GitLabRequestForTest) (int, []byte, http.Header, error),
) func() {
	return SetGitLabTransportForTest(
		func(ctx context.Context, base, token string, req glAPIRequest) (glAPIResult, error) {
			var body []byte
			if req.Body != nil {
				var err error
				if body, err = json.Marshal(req.Body); err != nil {
					return glAPIResult{}, err
				}
			}
			status, out, header, err := fn(ctx, token, GitLabRequestForTest{
				Base: base, Method: req.Method, Path: req.Path, Query: req.Query, Body: body, Accept: req.Accept,
				GraphQL: req.GraphQL,
			})
			if err != nil {
				return glAPIResult{}, err
			}
			if header == nil {
				header = http.Header{}
			}
			return glAPIResult{Status: status, Body: out, Header: header}, nil
		})
}

// glStream is the bulk-transfer boundary: a job's artifact archive or trace
// written straight to the caller's writer.
var glStream = streamGitLabResponse

// SetGitLabStreamForTest swaps the bulk-transfer boundary. Returns a restore
// func.
func SetGitLabStreamForTest(
	fn func(ctx context.Context, base, token string, req glAPIRequest, dst io.Writer, maxBytes int64) (glStreamResult, error),
) func() {
	prev := glStream
	glStream = fn
	return func() { glStream = prev }
}

// SetGitLabStreamForTestBytes is SetGitLabStreamForTest in exported types, with
// the size cap enforced here so a fixture cannot prove a bound the production
// seam does not have.
func SetGitLabStreamForTestBytes(
	fn func(ctx context.Context, token string, req GitLabRequestForTest, write func([]byte) error) (int, error),
) func() {
	return SetGitLabStreamForTest(
		func(ctx context.Context, base, token string, req glAPIRequest, dst io.Writer, maxBytes int64) (glStreamResult, error) {
			var written int64
			status, err := fn(ctx, token, GitLabRequestForTest{
				Base: base, Method: req.Method, Path: req.Path, Query: req.Query, Accept: req.Accept,
			}, func(b []byte) error {
				if written+int64(len(b)) > maxBytes {
					return fmt.Errorf("the download exceeded the %s the proxy will transfer",
						humanBytes(maxBytes))
				}
				n, writeErr := dst.Write(b)
				written += int64(n)
				return writeErr
			})
			return glStreamResult{Status: status, Bytes: written}, err
		})
}

// glRequestURL renders one request's absolute URL against the session's base.
//
// A Path carrying a scheme is a pagination link GitLab handed back, and it is
// GitLab-supplied data: it is used as given only when its scheme, host and port
// are the base's own. Go strips Authorization across a cross-host redirect, but
// a URL handed straight to Do gets the header with no such check.
func glRequestURL(base string, req glAPIRequest) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" {
		return "", fmt.Errorf("the GitLab base URL %q is not an https URL", base)
	}
	if req.GraphQL {
		if req.Path != "" || len(req.Query) > 0 {
			return "", fmt.Errorf("a GitLab GraphQL request carries its query in the body, not a path or query string")
		}
		return strings.TrimRight(base, "/") + "/" + gitlabGraphQLPath, nil
	}
	raw := req.Path
	if !strings.Contains(raw, "://") {
		raw = strings.TrimRight(base, "/") + "/" + gitlabAPIPrefix + "/" + strings.TrimPrefix(raw, "/")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("build GitLab URL: %w", err)
	}
	if u.Scheme != "https" || !sameHTTPSOrigin(u, baseURL) {
		return "", fmt.Errorf("refusing a GitLab API URL outside %s: %s", baseURL.Host, u.Redacted())
	}
	if len(req.Query) > 0 {
		q := u.Query()
		for k, vs := range req.Query {
			q.Del(k)
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// newGitLabHTTPRequest builds the *http.Request both seams send.
func newGitLabHTTPRequest(ctx context.Context, base, token string, req glAPIRequest) (*http.Request, error) {
	target, err := glRequestURL(base, req)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	var encoded []byte
	if req.Body != nil {
		if encoded, err = json.Marshal(req.Body); err != nil {
			return nil, fmt.Errorf("encode GitLab request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// The OAuth-compliant bearer header rather than PRIVATE-TOKEN. GitLab
	// documents both for personal, project and group access tokens, but they
	// do not travel alike:
	//   - a reverse proxy in front of the instance may drop a non-standard
	//     header (one in the field forwarded Authorization and silently lost
	//     PRIVATE-TOKEN, so every call read as anonymous and came back 401);
	//   - a custom header follows a redirect wherever it points, while
	//     glCheckRedirect can hold Authorization to the instance's exact
	//     origin (Go's own rule alone keeps it for any port, scheme or
	//     subdomain of the same host; see glCheckRedirect).
	httpReq.Header.Set("Authorization", "Bearer "+token)
	accept := req.Accept
	if accept == "" {
		accept = "application/json"
	}
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", glUserAgent)
	if encoded != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return httpReq, nil
}

// doGitLabRequest performs one bounded call.
func doGitLabRequest(ctx context.Context, base, token string, req glAPIRequest) (glAPIResult, error) {
	httpReq, err := newGitLabHTTPRequest(ctx, base, token, req)
	if err != nil {
		return glAPIResult{}, err
	}
	resp, err := glHTTPClient.Do(httpReq)
	if err != nil {
		return glAPIResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGLAPIResponseBytes))
	if err != nil {
		return glAPIResult{}, fmt.Errorf("read response: %w", err)
	}
	return glAPIResult{Status: resp.StatusCode, Body: body, Header: resp.Header}, nil
}

// streamGitLabResponse performs one bulk transfer, writing at most maxBytes to
// dst; see streamGitHubResponse for why the cap is on bytes written rather
// than on a declared length.
func streamGitLabResponse(ctx context.Context, base, token string, req glAPIRequest, dst io.Writer, maxBytes int64) (glStreamResult, error) {
	httpReq, err := newGitLabHTTPRequest(ctx, base, token, req)
	if err != nil {
		return glStreamResult{}, err
	}
	resp, err := glHTTPClient.Do(httpReq)
	if err != nil {
		return glStreamResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxGLAPIResponseBytes))
		return glStreamResult{Status: resp.StatusCode, Body: body}, nil
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return glStreamResult{Status: resp.StatusCode, Bytes: n}, err
	}
	if n > maxBytes {
		return glStreamResult{Status: resp.StatusCode, Bytes: n},
			fmt.Errorf("the download exceeded the %s the proxy will transfer", humanBytes(maxBytes))
	}
	return glStreamResult{Status: resp.StatusCode, Bytes: n}, nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// glAPIError is GitLab's error document. `message` is usually a string, but on
// a validation failure it is an object of field name to a list of problems,
// and a few endpoints answer with `error` instead; all three are rendered.
type glAPIError struct {
	Message json.RawMessage `json:"message"`
	Error   string          `json:"error"`
}

func (e glAPIError) text() string {
	if msg := glMessageText(e.Message); msg != "" {
		return msg
	}
	return strings.TrimSpace(e.Error)
}

// glMessageText flattens the shapes GitLab puts in `message`.
func glMessageText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return strings.TrimSpace(s)
	}
	var list []string
	if json.Unmarshal(trimmed, &list) == nil {
		return strings.Join(list, "; ")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) == nil {
		parts := make([]string, 0, len(fields))
		for field, problems := range fields {
			if p := glMessageText(problems); p != "" {
				parts = append(parts, field+": "+p)
			}
		}
		// Sorted so the same failure renders the same way twice.
		sort.Strings(parts)
		return strings.Join(parts, "; ")
	}
	return strings.TrimSpace(string(trimmed))
}

// glErrorText turns any non-2xx response into one line of prose.
func glErrorText(res glAPIResult) string {
	trimmed := bytes.TrimSpace(res.Body)
	if len(trimmed) > 0 {
		var apiErr glAPIError
		if json.Unmarshal(trimmed, &apiErr) == nil {
			if t := apiErr.text(); t != "" {
				return t
			}
		}
	}
	if res.Status == http.StatusNotFound {
		return "404 Not Found (the project, or the number within it, is not visible to the token agentd is using)"
	}
	return fmt.Sprintf("GitLab returned HTTP %d", res.Status)
}

// glRateLimitText adds the one detail that makes a 429 actionable. GitLab
// sends Retry-After on every throttled response and RateLimit-* headers on
// the per-user limits; either is enough to say how long to wait.
func glRateLimitText(res glAPIResult) string {
	if s := res.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs > 0 {
			return fmt.Sprintf("GitLab applied a rate limit; retry in %s",
				(time.Duration(secs) * time.Second).String())
		}
	}
	if res.Header.Get("RateLimit-Remaining") == "0" {
		if s := res.Header.Get("RateLimit-Reset"); s != "" {
			if epoch, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				if d := time.Until(time.Unix(epoch, 0)); d > 0 {
					return fmt.Sprintf("the token's GitLab API rate limit is exhausted; it resets in %s",
						d.Round(time.Second).String())
				}
			}
		}
		return "the token's GitLab API rate limit is exhausted"
	}
	return ""
}
