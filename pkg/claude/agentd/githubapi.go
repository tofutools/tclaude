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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

// githubapi.go is the GitHub half of the proxy's outbound edge: credential
// resolution and the two HTTP seams every verb goes through.
//
// It replaces what used to be a `gh` subprocess. The reason is the same one
// that shaped the Linear proxy: a token in a child process's environment is
// readable through /proc/<pid>/environ by any same-uid process for the life of
// the call, while a token in an Authorization header never leaves daemon
// memory. Dropping the subprocess also drops a dependency on one CLI's output
// formatting staying stable across releases, and lets `run download` bound an
// unpack while it is happening rather than after.
//
// Two seams rather than one, because the payloads are different in kind:
//
//   - ghDo performs a request whose response is a bounded document (JSON, or a
//     job log read into memory). Every verb but the two bulk ones uses it.
//   - ghStream performs a request whose response is bulk bytes written straight
//     to disk (an artifact zip, a run's log archive). Buffering those in memory
//     is precisely what the size caps exist to prevent.
//
// Both are package-level vars so flow tests can assert on the exact request the
// daemon built — the direct equivalent of the argv assertions the subprocess
// seam allowed, and the only way to regression-test that a caller-supplied
// value landed in a query parameter or a JSON body rather than in a path.

const (
	// githubAPIBase is the REST root. A constant, never derived from the
	// remote: the proxy only speaks to github.com (newGHProxySession refuses
	// any other host), and deriving a base URL from repository-supplied data
	// is how a credential ends up pointed at someone else's endpoint.
	githubAPIBase = "https://api.github.com"

	// githubAPIVersion pins the REST schema. GitHub's own recommendation, and
	// it is what keeps a future breaking change from arriving unannounced.
	githubAPIVersion = "2022-11-28"

	// ghUserAgent identifies the daemon in GitHub's logs. GitHub rejects a
	// request with no User-Agent outright.
	ghUserAgent = "tclaude-agentd"

	// maxGHAPIResponseBytes bounds an in-memory API response. Well above any
	// single page of the documents this proxy asks for, and far below what
	// would matter to the daemon; the bulk paths do not come through here.
	maxGHAPIResponseBytes = 16 << 20

	// maxGHPaginatedPages bounds a --paginate-equivalent walk. Only the inline
	// review comments paginate, and a pull request with more than this many
	// pages of them is one no agent is going to read to the end of anyway.
	maxGHPaginatedPages = 20
)

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// ghTokenSource names where a token came from. It is reported in the daemon
// log and in the "no token" diagnosis, never to the agent: which credential
// store the operator uses is not the agent's business, and the token itself is
// never rendered anywhere.
type ghTokenSource string

const (
	ghTokenFromFile    ghTokenSource = "agent.git_proxy.github_token_file"
	ghTokenFromEnv     ghTokenSource = "GH_TOKEN/GITHUB_TOKEN in agentd's environment"
	ghTokenFromHosts   ghTokenSource = "the gh CLI's hosts.yml"
	ghTokenFromKeyring ghTokenSource = "the OS keyring (gh's secure storage)"
	ghTokenFromGHCLI   ghTokenSource = "gh auth token"
)

// ghKeyringGet is the OS-keyring seam. Production reads the real store; tests
// swap it, because a unit test must not depend on a session D-Bus being up.
var ghKeyringGet = keyring.Get

// SetGHKeyringForTest swaps the keyring reader. Returns a restore func.
func SetGHKeyringForTest(fn func(service, user string) (string, error)) func() {
	prev := ghKeyringGet
	ghKeyringGet = fn
	return func() { ghKeyringGet = prev }
}

// ghTokenCommand is the last-resort `gh auth token` seam, kept separate from
// the keyring one so a test can exercise either without the other.
var ghTokenCommand = runGHAuthToken

// SetGHTokenCommandForTest swaps the `gh auth token` fallback. Returns a
// restore func.
func SetGHTokenCommandForTest(fn func(ctx context.Context) (string, error)) func() {
	prev := ghTokenCommand
	ghTokenCommand = fn
	return func() { ghTokenCommand = prev }
}

// githubToken resolves the credential this request will spend, in the order
// that keeps every posture that worked when the proxy shelled out to `gh`.
//
// The order matters and is not arbitrary:
//
//  1. The operator's explicit configuration wins. If they named a token file,
//     that is the identity they chose for agents, and an ambient `gh auth
//     login` must not quietly override it.
//  2. GH_TOKEN / GITHUB_TOKEN from agentd's own environment, which is what gh
//     itself would have honoured had the daemon forwarded them. It did not —
//     ghProxyEnv built the child's environment from an allow-list that omitted
//     them — so this is the one place the direct client is deliberately MORE
//     permissive than the subprocess was. It is the documented way to run gh
//     unattended, and an operator who exports it into the daemon's environment
//     has already chosen it as the daemon's identity.
//  3. gh's own configuration, read directly: hosts.yml first, then the OS
//     keyring gh stores tokens in when secure storage is available. This is
//     what makes `gh auth login` keep working with no gh process involved.
//  4. `gh auth token`, if gh happens to be installed. A fallback, not a
//     dependency: it is reached only when every direct read failed, which in
//     practice means a keyring gh can open and the daemon cannot.
//
// A resolved token is never cached across requests. The operator can rotate a
// token file or re-run `gh auth login` without restarting the daemon, and the
// read is a file or a keyring lookup rather than anything worth memoising.
func githubToken(ctx context.Context, policy config.GitProxyConfig) (string, ghTokenSource, *proxyFault) {
	if configured := strings.TrimSpace(policy.GitHubTokenFile); configured != "" {
		// "~/github-token.txt" is how an operator naturally writes this in a
		// JSON config file, and the same expandTilde every other human-typed
		// path in the daemon goes through applies here.
		raw, err := os.ReadFile(expandTilde(configured))
		if err != nil {
			return "", "", faultf(http.StatusServiceUnavailable, "token_unreadable",
				"the configured agent.git_proxy.github_token_file could not be read: %v%s",
				err, shellVarHint(configured))
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", "", faultf(http.StatusServiceUnavailable, "token_unreadable",
				"the configured agent.git_proxy.github_token_file is empty")
		}
		// A configured file is an explicit choice, so a bad value in it is an
		// error rather than a reason to fall through to something else.
		if fault := validateGHTokenShape(token, string(ghTokenFromFile)); fault != nil {
			return "", "", fault
		}
		return token, ghTokenFromFile, nil
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			if fault := validateGHTokenShape(token, name); fault != nil {
				return "", "", fault
			}
			return token, ghTokenFromEnv, nil
		}
	}
	hostsUser, hostsToken := readGHHostsToken()
	if hostsToken != "" {
		if fault := validateGHTokenShape(hostsToken, string(ghTokenFromHosts)); fault != nil {
			return "", "", fault
		}
		return hostsToken, ghTokenFromHosts, nil
	}
	if token := readGHKeyringToken(hostsUser); token != "" {
		if fault := validateGHTokenShape(token, string(ghTokenFromKeyring)); fault != nil {
			return "", "", fault
		}
		return token, ghTokenFromKeyring, nil
	}
	if token, err := ghTokenCommand(ctx); err == nil {
		if token = strings.TrimSpace(token); token != "" {
			if fault := validateGHTokenShape(token, string(ghTokenFromGHCLI)); fault != nil {
				return "", "", fault
			}
			return token, ghTokenFromGHCLI, nil
		}
	}
	return "", "", faultf(http.StatusServiceUnavailable, "token_missing",
		"no GitHub token is available to the daemon: set agent.git_proxy.github_token_file in "+
			"~/.tclaude/data/config.json, put GH_TOKEN in the environment agentd runs under, or "+
			"authenticate the account agentd runs as with `gh auth login`")
}

// validateGHTokenShape refuses a token that cannot go in a header.
//
// This is not a strength check — the proxy has no business judging the
// operator's credential — it is an injection gate. A stray newline in a token
// file (an editor adding one mid-value, a copy-paste through a terminal) would
// otherwise be a header-splitting value handed to net/http, which rejects it
// with an error naming the header rather than the cause. Saying so here turns
// a baffling failure into a one-line fix.
func validateGHTokenShape(token, source string) *proxyFault {
	for _, r := range token {
		if r < 0x20 || r == 0x7f {
			return faultf(http.StatusServiceUnavailable, "token_unreadable",
				"the GitHub token from %s contains a control character, so it cannot be sent as a "+
					"header; check for a stray newline or tab inside the value", source)
		}
	}
	return nil
}

// ghHostsFile locates gh's hosts.yml the way gh does: GH_CONFIG_DIR wins,
// then XDG_CONFIG_HOME/gh, then ~/.config/gh.
//
// It reads gh's configuration; it does not run gh. The distinction is the whole
// point of this file — an operator who authenticated once with `gh auth login`
// keeps working, and a host with no gh installed at all still works as long as
// the token reaches the daemon some other way.
func ghHostsFile() string {
	if dir := strings.TrimSpace(os.Getenv("GH_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "hosts.yml")
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "gh", "hosts.yml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh", "hosts.yml")
}

// ghHostsEntry is the part of gh's hosts.yml this daemon reads. `user` is kept
// even when there is no token beside it, because it is the keyring lookup's
// second key.
type ghHostsEntry struct {
	User       string `yaml:"user"`
	OAuthToken string `yaml:"oauth_token"`
}

// readGHHostsToken returns gh's configured github.com user and, if it is
// stored in the clear, its token.
//
// Every failure is silent and yields empty strings: an absent, unreadable or
// malformed hosts.yml means "gh is not the credential source here", which is
// an ordinary situation and not something to refuse the request over. The
// caller falls through to the next source, and the final "no token" fault names
// every option rather than blaming this one.
func readGHHostsToken() (user, token string) {
	path := ghHostsFile()
	if path == "" {
		return "", ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var hosts map[string]ghHostsEntry
	if err := yaml.Unmarshal(raw, &hosts); err != nil {
		return "", ""
	}
	entry := hosts["github.com"]
	return strings.TrimSpace(entry.User), strings.TrimSpace(entry.OAuthToken)
}

// readGHKeyringToken reads the token gh stores in the OS secret store when
// secure storage is available — which, since gh 2.28, is the default for
// `gh auth login` on any host with a working keyring.
//
// gh keys these entries as service "gh:<hostname>", storing the token twice:
// once under the empty user, which is the active one, and once under the
// account's login so `gh auth switch` can find it. Reading the empty user first
// therefore gets the identity gh itself would use.
//
// tclaude and gh 2.97 depend on the same go-keyring release, so this reads the
// same entries with the same code rather than reimplementing a store format.
func readGHKeyringToken(user string) string {
	const service = "gh:github.com"
	keys := []string{""}
	if user != "" {
		keys = append(keys, user)
	}
	for _, key := range keys {
		token, err := ghKeyringGet(service, key)
		if err != nil {
			continue
		}
		if token = strings.TrimSpace(token); token != "" {
			return token
		}
	}
	return ""
}

// runGHAuthToken is the production `gh auth token` fallback.
//
// It is the one place this package still executes gh, and it is deliberately
// the LAST thing tried: it exists so that a host where the daemon cannot open
// the keyring gh can open keeps working, which was the posture before this file
// existed. gh is looked up at call time and its absence is an ordinary error,
// so a host without gh is not a host with a broken proxy.
func runGHAuthToken(ctx context.Context) (string, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, "auth", "token", "--hostname", "github.com")
	// A constructed environment for the same reason ghProxyEnv built one: an
	// allow-list cannot drift where a deny-list can. gh needs HOME to find its
	// config and PATH to find its own helpers, and nothing else.
	cmd.Env = []string{"LC_ALL=C", "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"}
	for _, name := range []string{"PATH", "HOME", "XDG_CONFIG_HOME", "GH_CONFIG_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// ghAPIRequest is one outbound GitHub call, fully resolved before it reaches
// the seam. Nothing downstream adds to it.
//
// Path is a REST path relative to githubAPIBase ("repos/o/r/pulls/1"), or the
// absolute URL of a pagination link. Query carries every caller-influenced
// scalar, which is the direct analogue of the old rule that no caller value
// reached argv unquoted: a value in Query is escaped by net/url, while a value
// interpolated into Path would not be.
type ghAPIRequest struct {
	Method string
	Path   string
	Query  url.Values
	// Body is JSON-marshalled when non-nil. Free text — a PR body, a comment —
	// travels here rather than in a file, because there is no longer a child
	// process for a file to exist for: the bytes go from daemon memory into a
	// TLS connection without touching disk or argv.
	Body any
	// Accept overrides the default JSON media type. `run view --log-failed`'s
	// per-job fallback uses it to ask for plain text.
	Accept string
}

// ghAPIResult is a bounded response.
type ghAPIResult struct {
	Status int
	Body   []byte
	Header http.Header
}

// ghStreamResult is what a bulk transfer reports back. Bytes is what actually
// landed, which the caller needs to enforce a cap the response headers may not
// have declared.
type ghStreamResult struct {
	Status int
	Bytes  int64
	// Body carries the response only when the status says the transfer did not
	// happen, so an error can be reported with GitHub's own words. A successful
	// transfer went to the writer, not here.
	Body []byte
}

// ghHTTPClient is the daemon's client for GitHub. Explicitly constructed
// rather than http.DefaultClient so the timeout is ours and a stray
// DefaultClient mutation elsewhere in the process cannot change it.
//
// There is no client-level timeout: every call already runs under a context
// deadline chosen per verb (60s for an API read, 300s for a download), and a
// second, coarser bound here would only ever cut a legitimate transfer short.
var ghHTTPClient = &http.Client{}

// ghDo is the bounded-response HTTP boundary, mirroring linearDo. Production
// performs the real request; flow tests swap in a recorder and assert on the
// exact method, path, query and body the daemon built.
var ghDo = doGitHubRequest

// SetGitHubTransportForTest swaps the bounded-response boundary. Returns a
// restore func.
func SetGitHubTransportForTest(fn func(ctx context.Context, token string, req ghAPIRequest) (ghAPIResult, error)) func() {
	prev := ghDo
	ghDo = fn
	return func() { ghDo = prev }
}

// GitHubRequestForTest is SetGitHubTransportForTest's request spelled in
// exported terms, so the flow tests — which live in package agentd_test, to
// exercise the daemon from outside as a caller would — can assert on it.
//
// Body arrives already marshalled, which is what the real transport does with
// it, so a test asserts on the same bytes GitHub would receive.
type GitHubRequestForTest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
	Accept string
}

// SetGitHubTransportForTestJSON is SetGitHubTransportForTest in exported types.
func SetGitHubTransportForTestJSON(
	fn func(ctx context.Context, token string, req GitHubRequestForTest) (int, []byte, http.Header, error),
) func() {
	return SetGitHubTransportForTest(
		func(ctx context.Context, token string, req ghAPIRequest) (ghAPIResult, error) {
			var body []byte
			if req.Body != nil {
				var err error
				if body, err = json.Marshal(req.Body); err != nil {
					return ghAPIResult{}, err
				}
			}
			status, out, header, err := fn(ctx, token, GitHubRequestForTest{
				Method: req.Method, Path: req.Path, Query: req.Query, Body: body, Accept: req.Accept,
			})
			if err != nil {
				return ghAPIResult{}, err
			}
			if header == nil {
				header = http.Header{}
			}
			return ghAPIResult{Status: status, Body: out, Header: header}, nil
		})
}

// ghStream is the bulk-transfer boundary: a zip archive written straight to
// the caller's writer rather than into memory.
var ghStream = streamGitHubResponse

// SetGitHubStreamForTest swaps the bulk-transfer boundary. Returns a restore
// func.
func SetGitHubStreamForTest(
	fn func(ctx context.Context, token string, req ghAPIRequest, dst io.Writer, maxBytes int64) (ghStreamResult, error),
) func() {
	prev := ghStream
	ghStream = fn
	return func() { ghStream = prev }
}

// SetGitHubStreamForTestBytes is SetGitHubStreamForTest in exported types, with
// the writer presented as a callback so a test hands over a whole archive
// rather than implementing io.Writer plumbing.
//
// The size cap is enforced here rather than inside the test's callback, so a
// fixture cannot accidentally prove a bound the production seam does not have.
func SetGitHubStreamForTestBytes(
	fn func(ctx context.Context, token string, req GitHubRequestForTest, write func([]byte) error) (int, error),
) func() {
	return SetGitHubStreamForTest(
		func(ctx context.Context, token string, req ghAPIRequest, dst io.Writer, maxBytes int64) (ghStreamResult, error) {
			var written int64
			status, err := fn(ctx, token, GitHubRequestForTest{
				Method: req.Method, Path: req.Path, Query: req.Query, Accept: req.Accept,
			}, func(b []byte) error {
				if written+int64(len(b)) > maxBytes {
					return fmt.Errorf("the download exceeded the %s the proxy will transfer",
						humanBytes(maxBytes))
				}
				n, writeErr := dst.Write(b)
				written += int64(n)
				return writeErr
			})
			return ghStreamResult{Status: status, Bytes: written}, err
		})
}

// ghRequestURL renders one request's absolute URL. A Path that is already
// absolute — a pagination link GitHub handed back — is used as given, with its
// own query preserved.
func ghRequestURL(req ghAPIRequest) (string, error) {
	raw := req.Path
	// A path carrying ANY scheme is treated as absolute and validated as such.
	// Testing only for "https://" would quietly fold `http://api.github.com/…`
	// into a nonsense path under the API root instead of refusing it, which is
	// a confusing failure rather than an honest one.
	if !strings.Contains(raw, "://") {
		raw = githubAPIBase + "/" + strings.TrimPrefix(raw, "/")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("build GitHub URL: %w", err)
	}
	// api.github.com and nothing else. A pagination link is GitHub-supplied
	// data, and following one off-host would send the operator's token
	// somewhere it was never meant to go. Go strips Authorization across a
	// redirect to another host; it does not strip it from a URL handed
	// straight to Do, which is what a Link header is.
	if u.Scheme != "https" || u.Host != "api.github.com" {
		return "", fmt.Errorf("refusing a GitHub API URL outside api.github.com: %s", u.Redacted())
	}
	if len(req.Query) > 0 {
		q := u.Query()
		for k, vs := range req.Query {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// newGitHubHTTPRequest builds the *http.Request both seams send.
func newGitHubHTTPRequest(ctx context.Context, token string, req ghAPIRequest) (*http.Request, error) {
	target, err := ghRequestURL(req)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	var encoded []byte
	if req.Body != nil {
		if encoded, err = json.Marshal(req.Body); err != nil {
			return nil, fmt.Errorf("encode GitHub request: %w", err)
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
	// The token rides in a header, straight from daemon memory. It never
	// reaches argv and never enters a child process's environment, so unlike
	// the GH_TOKEN this proxy used to hand `gh` there is no /proc window in
	// which a same-uid process could read it.
	httpReq.Header.Set("Authorization", "Bearer "+token)
	accept := req.Accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	httpReq.Header.Set("User-Agent", ghUserAgent)
	if encoded != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return httpReq, nil
}

// doGitHubRequest performs one bounded call.
func doGitHubRequest(ctx context.Context, token string, req ghAPIRequest) (ghAPIResult, error) {
	httpReq, err := newGitHubHTTPRequest(ctx, token, req)
	if err != nil {
		return ghAPIResult{}, err
	}
	resp, err := ghHTTPClient.Do(httpReq)
	if err != nil {
		return ghAPIResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGHAPIResponseBytes))
	if err != nil {
		return ghAPIResult{}, fmt.Errorf("read response: %w", err)
	}
	return ghAPIResult{Status: resp.StatusCode, Body: body, Header: resp.Header}, nil
}

// streamGitHubResponse performs one bulk transfer, writing at most maxBytes to
// dst.
//
// The cap is enforced against what is actually written rather than against the
// Content-Length GitHub declares, because the transfer this serves — an
// artifact zip fetched from a redirect target — is the one place a declared
// size is not the daemon's to trust. Exceeding it is an error and the partial
// write is the caller's to discard; there is no truncated-but-usable zip.
func streamGitHubResponse(ctx context.Context, token string, req ghAPIRequest, dst io.Writer, maxBytes int64) (ghStreamResult, error) {
	httpReq, err := newGitHubHTTPRequest(ctx, token, req)
	if err != nil {
		return ghStreamResult{}, err
	}
	resp, err := ghHTTPClient.Do(httpReq)
	if err != nil {
		return ghStreamResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body of a failure is small and is GitHub's own explanation, so
		// it is read rather than discarded; the caller renders it.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxGHAPIResponseBytes))
		return ghStreamResult{Status: resp.StatusCode, Body: body}, nil
	}
	// maxBytes+1, so a transfer that lands exactly on the cap is accepted and
	// one byte past it is detected rather than silently truncated.
	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return ghStreamResult{Status: resp.StatusCode, Bytes: n}, err
	}
	if n > maxBytes {
		return ghStreamResult{Status: resp.StatusCode, Bytes: n},
			fmt.Errorf("the download exceeded the %s the proxy will transfer", humanBytes(maxBytes))
	}
	return ghStreamResult{Status: resp.StatusCode, Bytes: n}, nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ghAPIError is GitHub's REST error document. `message` is the actionable part
// — "Not Found", "Validation Failed", "A pull request already exists for
// owner:branch" — and `errors` carries the per-field detail that turns the
// second of those from a shrug into a fix.
type ghAPIError struct {
	Message string `json:"message"`
	Errors  []struct {
		Resource string `json:"resource"`
		Field    string `json:"field"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// text renders a REST error the way an agent needs to read it: GitHub's own
// message, then whatever field-level detail came with it.
func (e ghAPIError) text() string {
	msg := strings.TrimSpace(e.Message)
	var detail []string
	for _, d := range e.Errors {
		switch {
		case strings.TrimSpace(d.Message) != "":
			detail = append(detail, strings.TrimSpace(d.Message))
		case d.Field != "":
			detail = append(detail, fmt.Sprintf("%s.%s: %s", d.Resource, d.Field, d.Code))
		case d.Code != "":
			detail = append(detail, d.Code)
		}
	}
	if len(detail) == 0 {
		return msg
	}
	if msg == "" {
		return strings.Join(detail, "; ")
	}
	return msg + " (" + strings.Join(detail, "; ") + ")"
}

// ghErrorText turns any non-2xx response into one line of prose, falling back
// through the shapes GitHub actually returns: a REST error document, a GraphQL
// error array, a bare string, and finally the status alone.
func ghErrorText(res ghAPIResult) string {
	trimmed := bytes.TrimSpace(res.Body)
	if len(trimmed) > 0 {
		// A REST error document is recognised by its top-level `message`. That
		// test has to come first AND be this specific: a GraphQL error array
		// also decodes cleanly into ghAPIError.Errors, so accepting any
		// successful decode would classify every GraphQL failure as a REST one
		// and drop the error type with it.
		var apiErr ghAPIError
		restShaped := json.Unmarshal(trimmed, &apiErr) == nil && strings.TrimSpace(apiErr.Message) != ""
		if restShaped {
			return apiErr.text()
		}
		var gql ghGraphQLResponse
		if json.Unmarshal(trimmed, &gql) == nil && len(gql.Errors) > 0 {
			return gql.errorText()
		}
		if t := apiErr.text(); t != "" {
			return t
		}
	}
	// A status line with no body at all: 404 on a private repository the token
	// cannot see is the common one, and it is worth naming rather than
	// reporting as an empty failure.
	if res.Status == http.StatusNotFound {
		return "Not Found (the repository, or the number within it, is not visible to the token agentd is using)"
	}
	return fmt.Sprintf("GitHub returned HTTP %d", res.Status)
}

// ghRateLimitText adds the one detail that makes a 403 or 429 actionable:
// whether it is the ordinary hourly budget (which has a reset time) or a
// secondary limit (which has a retry-after), and how long either has to go.
func ghRateLimitText(res ghAPIResult) string {
	if s := res.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs > 0 {
			return fmt.Sprintf("GitHub applied a secondary rate limit; retry in %s",
				(time.Duration(secs) * time.Second).String())
		}
	}
	if res.Header.Get("X-RateLimit-Remaining") == "0" {
		if s := res.Header.Get("X-RateLimit-Reset"); s != "" {
			if epoch, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				if d := time.Until(time.Unix(epoch, 0)); d > 0 {
					return fmt.Sprintf("the token's GitHub API rate limit is exhausted; it resets in %s",
						d.Round(time.Second).String())
				}
			}
			return "the token's GitHub API rate limit is exhausted"
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// GraphQL
// ---------------------------------------------------------------------------

// ghGraphQLRequest is the GraphQL wire request. Query is always one of the
// package-level document constants in githubproxy_queries.go; Variables is the
// only place a caller-supplied value ever appears.
type ghGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// ghGraphQLError is one entry of GraphQL's error array.
type ghGraphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

// ghGraphQLResponse is the GraphQL wire response. A GraphQL error arrives with
// HTTP 200 as often as not, so Errors must be inspected on every call
// regardless of status.
type ghGraphQLResponse struct {
	Data   json.RawMessage  `json:"data"`
	Errors []ghGraphQLError `json:"errors"`
}

func (r ghGraphQLResponse) errorText() string {
	msgs := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		if m := strings.TrimSpace(e.Message); m != "" {
			if e.Type != "" {
				m = e.Type + ": " + m
			}
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return "the GitHub GraphQL API reported an error with no message"
	}
	return strings.Join(msgs, "; ")
}

