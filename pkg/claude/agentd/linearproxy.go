package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// linearproxy.go is the daemon half of `tclaude proxy linear` — Linear issue
// operations performed with agentd's own Linear API key on behalf of an agent
// that holds no key of its own.
//
// It differs from the git and GitHub proxies in two ways that shape everything
// else in this file.
//
//  1. THERE IS NO TOOL. Linear ships no official CLI, so the daemon speaks
//     Linear's GraphQL API directly over HTTP rather than building an argv for
//     a subprocess. That removes a whole class of hazard — there is no argv to
//     inject into, no /proc/<pid>/cmdline to leak a body through, no repo-local
//     configuration that could re-aim a child process — and replaces it with
//     one rule, enforced by construction: EVERY GraphQL DOCUMENT IS A
//     COMPILE-TIME CONSTANT (see linearproxy_queries.go) and every caller value
//     travels in `variables`. A value in the variables map cannot change which
//     operation runs, the way a value in argv can.
//
//  2. THERE IS NO ANCHOR. The git proxy derives its repository from the agent's
//     daemon-recorded launch directory, so an agent can only reach the checkout
//     it was launched in. Linear has no filesystem artifact that corresponds to
//     that. The operator's team allow-list is therefore the ONLY scope gate,
//     which is why it is mandatory, fail-closed, and checked twice: once on the
//     identifier the caller supplied, and again on the team Linear reports on
//     the response — see enforceIssueTeam.

const (
	// linearEndpoint is Linear's only API endpoint. A constant, not config:
	// the point of the proxy is that the agent cannot choose where the
	// operator's credential is spent.
	linearEndpoint = "https://api.linear.app/graphql"

	// linearProxyTimeout bounds one GraphQL call. Linear is an API, not a
	// transport, so this is generous; a call that takes longer is Linear
	// being slow or rate-limiting.
	linearProxyTimeout = 45 * time.Second

	// maxLinearResponseBytes bounds what the daemon will read from Linear.
	// Every list verb is bounded by `first:` as well, so this is a backstop
	// against a pathological response rather than the working limit.
	maxLinearResponseBytes = 4 * 1024 * 1024

	// maxLinearBodyBytes bounds a description or comment body. Linear accepts
	// large markdown documents; this is well past anything an agent should be
	// writing into a ticket and is enforced before the request is built.
	maxLinearBodyBytes = 256 * 1024

	// maxLinearTitleLen bounds an issue title, in runes.
	maxLinearTitleLen = 256

	// maxLinearQueryLen bounds a search term.
	maxLinearQueryLen = 256

	// maxLinearLimit / defaultLinearLimit bound a list request. Linear's own
	// default page size is 50.
	maxLinearLimit     = 100
	defaultLinearLimit = 25

	// linearProxyDisabledCode / …Message are the fail-closed answer when the
	// operator has not opted in. Distinct from a permission denial: nothing
	// the agent or its operator grants can turn this into a success.
	linearProxyDisabledCode    = "linear_proxy_disabled"
	linearProxyDisabledMessage = "the Linear proxy is not configured: the operator has not set " +
		"agent.linear_proxy.allowed_teams in ~/.tclaude/data/config.json, and an empty allow-list means " +
		"no team is reachable"
)

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// linearRequest is the GraphQL wire request. Query is always one of the
// package-level constants in linearproxy_queries.go; Variables is the only
// place caller-supplied values ever appear.
type linearRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// linearError is one entry of GraphQL's error array. Linear puts the useful,
// human-readable text in extensions.userPresentableMessage — "The query is too
// complex", "You need to authenticate to access this operation" — and a
// terser one in message, so both are kept.
type linearError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code                   string `json:"code"`
		Type                   string `json:"type"`
		UserPresentableMessage string `json:"userPresentableMessage"`
	} `json:"extensions"`
}

// text renders the most useful form of one error.
func (e linearError) text() string {
	if m := strings.TrimSpace(e.Extensions.UserPresentableMessage); m != "" {
		return m
	}
	return strings.TrimSpace(e.Message)
}

// linearResponse is the GraphQL wire response. Note that a GraphQL error
// arrives with HTTP 200 as often as not, so Errors must be inspected on every
// call regardless of status — see linearExec.
type linearResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []linearError   `json:"errors"`
}

// linearHTTPResult is what the transport seam returns: the raw body plus the
// two things above it worth knowing. Headers is kept because Linear reports
// rate-limit and complexity budgets there, and turning a 429 into an
// actionable message needs them.
type linearHTTPResult struct {
	Status  int
	Body    []byte
	Headers http.Header
}

// linearDo is the outbound-HTTP boundary, mirroring proxyExec in gitproxy.go.
// Production performs the real request; flow tests swap in a recorder and
// assert on the exact document and variables the daemon built — which is the
// only way to regression-test that a caller value never reached the document
// body, since a document built from the wrong string produces no visible
// difference until it is the wrong operation.
var linearDo = doLinearRequest

// SetLinearTransportForTest swaps the outbound-HTTP boundary. Returns a
// restore func.
func SetLinearTransportForTest(fn func(ctx context.Context, key string, req linearRequest) (linearHTTPResult, error)) func() {
	prev := linearDo
	linearDo = fn
	return func() { linearDo = prev }
}

// SetLinearTransportForTestJSON is SetLinearTransportForTest spelled in
// exported types, so the flow tests — which live in package agentd_test, to
// exercise the daemon from outside as a caller would — can install a stub.
//
// The request reaches fn as `any` holding a linearRequest; a test marshals it,
// which is exactly what the real transport does with it, and can then assert
// on the document and the variables separately. That separation is the point:
// it is what lets a test prove a caller's string landed in `variables` and
// never in the document.
func SetLinearTransportForTestJSON(fn func(ctx context.Context, key string, req any) (int, []byte, error)) func() {
	return SetLinearTransportForTest(
		func(ctx context.Context, key string, req linearRequest) (linearHTTPResult, error) {
			status, body, err := fn(ctx, key, req)
			if err != nil {
				return linearHTTPResult{}, err
			}
			return linearHTTPResult{Status: status, Body: body, Headers: http.Header{}}, nil
		})
}

// linearHTTPClient is the daemon's client for Linear. Explicitly constructed
// rather than http.DefaultClient so the timeout is ours and a stray
// DefaultClient mutation elsewhere in the process cannot change it.
var linearHTTPClient = &http.Client{Timeout: linearProxyTimeout}

// doLinearRequest performs one GraphQL POST.
//
// The key rides in the Authorization header, straight from daemon memory. It
// never reaches argv and never enters a child process's environment, so unlike
// the GitHub half's GH_TOKEN there is no /proc window in which a same-uid
// process could read it.
func doLinearRequest(ctx context.Context, key string, req linearRequest) (linearHTTPResult, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return linearHTTPResult{}, fmt.Errorf("encode GraphQL request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, linearEndpoint, bytes.NewReader(payload))
	if err != nil {
		return linearHTTPResult{}, fmt.Errorf("build request: %w", err)
	}
	// A personal API key is sent raw. Only OAuth access tokens take the
	// "Bearer " prefix, and v1 supports personal keys only.
	httpReq.Header.Set("Authorization", key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "tclaude-agentd")

	resp, err := linearHTTPClient.Do(httpReq)
	if err != nil {
		return linearHTTPResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLinearResponseBytes))
	if err != nil {
		return linearHTTPResult{}, fmt.Errorf("read response: %w", err)
	}
	return linearHTTPResult{Status: resp.StatusCode, Body: body, Headers: resp.Header}, nil
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// linearProxySession is one Linear invocation context: the operator's policy
// plus the resolved key. It holds no per-agent state, because there is none —
// the team allow-list is the whole scope gate.
type linearProxySession struct {
	policy config.LinearProxyConfig
	key    string
}

// newLinearProxySession runs the operator-policy gates and resolves the key.
//
// Ordering matches the git proxy's: the fail-closed policy check comes before
// anything that could touch the network, so a caller holding linear.read
// against an unconfigured daemon gets "not configured" rather than an
// authentication error from Linear.
func newLinearProxySession() (*linearProxySession, *proxyFault) {
	cfg, err := config.Load()
	if err != nil {
		return nil, faultf(http.StatusInternalServerError, "config",
			"could not read the daemon configuration: %v", err)
	}
	policy := cfg.ResolvedLinearProxy()
	if len(policy.AllowedTeams) == 0 {
		return nil, &proxyFault{
			Status: http.StatusServiceUnavailable,
			Code:   linearProxyDisabledCode,
			Msg:    linearProxyDisabledMessage,
		}
	}
	key, fault := linearAPIKey(policy)
	if fault != nil {
		return nil, fault
	}
	return &linearProxySession{policy: policy, key: key}, nil
}

// linearAPIKey resolves the operator's Linear personal API key: the configured
// file if there is one, else LINEAR_API_KEY from the daemon's own environment.
//
// Deliberately not stored in config.json — that file is plaintext, shows up in
// the dashboard's Config tab, and is the sort of thing an operator copies into
// a bug report.
func linearAPIKey(policy config.LinearProxyConfig) (string, *proxyFault) {
	if configured := strings.TrimSpace(policy.APIKeyFile); configured != "" {
		// "~/linear-key.txt" is how an operator naturally writes this, and the
		// same expandTilde every other human-typed path in the daemon goes
		// through applies here.
		raw, err := os.ReadFile(expandTilde(configured))
		if err != nil {
			return "", faultf(http.StatusServiceUnavailable, "key_unreadable",
				"the configured agent.linear_proxy.api_key_file could not be read: %v%s",
				err, shellVarHint(configured))
		}
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", faultf(http.StatusServiceUnavailable, "key_unreadable",
				"the configured agent.linear_proxy.api_key_file is empty")
		}
		return key, nil
	}
	if key := strings.TrimSpace(os.Getenv("LINEAR_API_KEY")); key != "" {
		return key, nil
	}
	return "", faultf(http.StatusServiceUnavailable, "key_missing",
		"no Linear API key is configured: set agent.linear_proxy.api_key_file, or put LINEAR_API_KEY "+
			"in the environment agentd runs under")
}

// requireWrite gates the mutating verbs on the operator's own ceiling. The
// linear.write slug says THIS AGENT may write; allow_write says the operator
// wants any agent to be able to. Both must hold.
func (s *linearProxySession) requireWrite() *proxyFault {
	if s.policy.AllowWrite {
		return nil
	}
	return faultf(http.StatusForbidden, "linear_write_disabled",
		"the operator has not enabled writes: set agent.linear_proxy.allow_write to true in "+
			"~/.tclaude/data/config.json")
}

// exec runs one GraphQL operation and unmarshals `data` into out.
//
// doc must be one of the package-level document constants. vars carries every
// caller-supplied value. Nothing in this function inspects either — which is
// exactly why every caller builds vars from values that have passed a
// validateLinear* gate.
func (s *linearProxySession) exec(ctx context.Context, doc string, vars map[string]any, out any) *proxyFault {
	runCtx, cancel := context.WithTimeout(ctx, linearProxyTimeout)
	defer cancel()

	res, err := linearDo(runCtx, s.key, linearRequest{Query: doc, Variables: vars})
	if err != nil {
		return faultf(http.StatusBadGateway, "linear_unreachable",
			"could not reach the Linear API: %v", err)
	}
	if fault := linearRateLimitFault(res); fault != nil {
		return fault
	}

	var parsed linearResponse
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		// A body that is not JSON at all is Linear (or something in front of
		// it) reporting at the HTTP layer. Surface the status rather than a
		// decode error, which would say nothing useful.
		return faultf(http.StatusBadGateway, "linear_failed",
			"the Linear API returned an unreadable response (HTTP %d)", res.Status)
	}
	// GraphQL reports application errors with HTTP 200 as often as with 4xx,
	// so the error array is authoritative and is checked before the status.
	if len(parsed.Errors) > 0 {
		return linearGraphQLFault(res.Status, parsed.Errors)
	}
	if res.Status < 200 || res.Status > 299 {
		return faultf(http.StatusBadGateway, "linear_failed",
			"the Linear API returned HTTP %d", res.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(parsed.Data, out); err != nil {
		return faultf(http.StatusBadGateway, "linear_failed",
			"could not read the Linear response: %v", err)
	}
	return nil
}

// linearGraphQLFault turns Linear's error array into one fault. The first
// error's text leads, because Linear puts the actionable one first, and the
// authentication and complexity cases get their own codes so an operator
// reading the audit log can tell a misconfigured key from a bad query.
func linearGraphQLFault(status int, errs []linearError) *proxyFault {
	first := errs[0]
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		if t := e.text(); t != "" {
			msgs = append(msgs, t)
		}
	}
	joined := strings.Join(msgs, "; ")
	if joined == "" {
		joined = fmt.Sprintf("the Linear API reported an error (HTTP %d)", status)
	}
	switch first.Extensions.Code {
	case "AUTHENTICATION_ERROR":
		return faultf(http.StatusServiceUnavailable, "linear_auth",
			"Linear rejected the operator's API key: %s", joined)
	case "GRAPHQL_VALIDATION_FAILED":
		// Our own document no longer matches Linear's schema. Not the agent's
		// fault and not fixable by retrying, so it is named as a daemon bug.
		return faultf(http.StatusBadGateway, "linear_schema_drift",
			"tclaude's Linear query was rejected by the schema (this is a tclaude bug, not a bad "+
				"request): %s", joined)
	case "RATELIMITED":
		return faultf(http.StatusTooManyRequests, "linear_rate_limited", "%s", joined)
	}
	return faultf(http.StatusBadGateway, "linear_failed", "%s", joined)
}

// linearRateLimitFault converts an HTTP 429 into a fault carrying the reset
// time, which is the one thing a caller can act on. Linear reports the reset
// as epoch milliseconds.
func linearRateLimitFault(res linearHTTPResult) *proxyFault {
	if res.Status != http.StatusTooManyRequests {
		return nil
	}
	msg := "the Linear API rate-limited this request"
	if reset := strings.TrimSpace(res.Headers.Get("X-RateLimit-Requests-Reset")); reset != "" {
		if ms, err := strconv.ParseInt(reset, 10, 64); err == nil && ms > 0 {
			msg += fmt.Sprintf("; the budget resets at %s",
				time.UnixMilli(ms).UTC().Format(time.RFC3339))
		}
	}
	return faultf(http.StatusTooManyRequests, "linear_rate_limited", "%s", msg)
}

// ---------------------------------------------------------------------------
// The team gate
// ---------------------------------------------------------------------------

// teamKeyOf extracts the team key from an issue identifier: "TCL-568" → "TCL".
// Returns "" when the string is not in identifier form.
func teamKeyOf(identifier string) string {
	key, num, ok := strings.Cut(strings.TrimSpace(identifier), "-")
	if !ok || key == "" || num == "" {
		return ""
	}
	return key
}

// requireAllowedTeam refuses a team key the operator did not allow-list. The
// message names the allow-list so an agent can tell the operator exactly what
// to add rather than guessing from a refusal.
func (s *linearProxySession) requireAllowedTeam(key string) *proxyFault {
	if s.policy.LinearTeamAllowed(key) {
		return nil
	}
	return faultf(http.StatusForbidden, "team_not_allowed",
		"team %q is not on the operator's agent.linear_proxy.allowed_teams list (allowed: %s)",
		key, strings.Join(s.policy.AllowedTeams, ", "))
}

// enforceIssueTeam is the SECOND half of the team gate, and the load-bearing
// one. requireAllowedTeam checks the identifier the caller supplied; this
// checks the team Linear actually reports on the issue it returned, and is
// what makes the gate hold rather than merely look like it does.
//
// The two can disagree. Linear's `issue(id:)` accepts a UUID as well as an
// identifier, and it resolves an identifier that has been MOVED between teams
// to the issue's current team — so a check on the caller's string alone would
// be a check on a label rather than on the thing reached. Refusing here, after
// the read but before the response is rendered, means the data never leaves
// the daemon.
func (s *linearProxySession) enforceIssueTeam(issue *linearIssue) *proxyFault {
	if issue == nil {
		return faultf(http.StatusNotFound, "not_found", "no such issue")
	}
	if key := strings.TrimSpace(issue.Team.Key); key != "" {
		return s.requireAllowedTeam(key)
	}
	// No team on the response means the query did not ask for one, which is a
	// programming error in this package. Refuse rather than let an unchecked
	// issue through.
	return faultf(http.StatusInternalServerError, "team_unresolved",
		"the Linear response carried no team; refusing to return an issue the allow-list could not "+
			"be checked against")
}

// ---------------------------------------------------------------------------
// Parameter validation
// ---------------------------------------------------------------------------

// validateLinearIdentifier bounds an issue reference to the human identifier
// form, "TEAM-123".
//
// RAW UUIDs ARE DELIBERATELY REFUSED even though Linear accepts them. The team
// allow-list is checked on the identifier before the call is made, and a UUID
// carries no team, so accepting one would mean every read had to be made first
// and judged afterwards. Requiring the identifier keeps the cheap gate ahead
// of the network, and the identifier is how an agent refers to an issue anyway.
func validateLinearIdentifier(id string) (string, *proxyFault) {
	id = strings.ToUpper(strings.TrimSpace(id))
	if id == "" {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"an issue identifier is required, in TEAM-123 form")
	}
	key, num, ok := strings.Cut(id, "-")
	if !ok {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not an issue identifier; the form is TEAM-123", id)
	}
	if fault := validateLinearTeamKeyShape(key); fault != nil {
		return "", fault
	}
	if num == "" || len(num) > 9 {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not an issue identifier (the form is TEAM-123); the number after the team key is "+
				"missing or too long", id)
	}
	for _, r := range num {
		if r < '0' || r > '9' {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"%q is not an issue identifier (the form is TEAM-123); the part after the team key "+
					"must be a number", id)
		}
	}
	return key + "-" + num, nil
}

// validateLinearTeamKeyShape bounds a team key's charset. Linear team keys are
// short alphanumerics; anything else is refused before it can reach a filter.
func validateLinearTeamKeyShape(key string) *proxyFault {
	key = strings.TrimSpace(key)
	if key == "" {
		return faultf(http.StatusBadRequest, "invalid_arg", "a team key is required")
	}
	if len(key) > 16 {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"team key %q is longer than 16 characters", key)
	}
	for _, r := range key {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"team key %q contains a character that is not a letter or a digit", key)
		}
	}
	return nil
}

// validateLinearTeam normalises and allow-list-checks a team key supplied as a
// parameter (`--team`), returning the upper-cased key.
func (s *linearProxySession) validateLinearTeam(key string) (string, *proxyFault) {
	if fault := validateLinearTeamKeyShape(key); fault != nil {
		return "", fault
	}
	key = strings.ToUpper(strings.TrimSpace(key))
	if fault := s.requireAllowedTeam(key); fault != nil {
		return "", fault
	}
	return key, nil
}

// validateLinearBody bounds free text. Like the GitHub half's body, it is
// deliberately unrestricted in charset: it is prose that will be published.
// Unlike that one it needs no file staging, because it travels in a JSON
// request body rather than in argv.
func validateLinearBody(body string, required bool) *proxyFault {
	if strings.TrimSpace(body) == "" {
		if required {
			return faultf(http.StatusBadRequest, "invalid_arg", "a body is required")
		}
		return nil
	}
	if len(body) > maxLinearBodyBytes {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"body is %d bytes; the maximum is %d", len(body), maxLinearBodyBytes)
	}
	return nil
}

// validateLinearTitle bounds an issue title.
//
// A title reaches no argv here, so the leading-"-" rule the GitHub half needs
// does not apply. The control-character and Unicode-format-character rules do:
// this title is published under the operator's Linear account, where a reader
// has no reason to suspect the displayed text is not the stored text, and
// U+202E and friends reorder how it renders without changing what it says.
func validateLinearTitle(title string) *proxyFault {
	title = strings.TrimSpace(title)
	if title == "" {
		return faultf(http.StatusBadRequest, "invalid_arg", "a title is required")
	}
	if utf8.RuneCountInString(title) > maxLinearTitleLen {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"title is longer than %d characters", maxLinearTitleLen)
	}
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"the title contains a control character (did you mean to put this in the description?)")
		}
		if unicode.Is(unicode.Cf, r) {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"the title contains a Unicode format character (U+%04X); those can reorder how the "+
					"title renders without changing what it says", r)
		}
	}
	return nil
}

// validateLinearSearchTerm bounds a search string. It goes into `variables` as
// an opaque term, so the only limits that matter are length and control
// characters.
func validateLinearSearchTerm(term string) (string, *proxyFault) {
	term = strings.TrimSpace(term)
	if term == "" {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "a search term is required")
	}
	if utf8.RuneCountInString(term) > maxLinearQueryLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"the search term is longer than %d characters", maxLinearQueryLen)
	}
	for _, r := range term {
		if r < 0x20 || r == 0x7f {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the search term contains a control character")
		}
	}
	return term, nil
}

// validateLinearLimit bounds a list request.
func validateLinearLimit(limit int) (int, *proxyFault) {
	if limit == 0 {
		return defaultLinearLimit, nil
	}
	if limit < 1 || limit > maxLinearLimit {
		return 0, faultf(http.StatusBadRequest, "invalid_arg",
			"limit must be between 1 and %d", maxLinearLimit)
	}
	return limit, nil
}

// validateLinearPriority bounds an issue priority. Linear's scale is fixed:
// 0 none, 1 urgent, 2 high, 3 normal, 4 low.
func validateLinearPriority(p int) *proxyFault {
	if p < 0 || p > 4 {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"priority must be 0 (none), 1 (urgent), 2 (high), 3 (normal) or 4 (low)")
	}
	return nil
}

// validateLinearAttachmentURL bounds a link destination. http(s) only: an
// attachment is rendered as a clickable link in the operator's workspace, and
// a javascript: or data: URL there is a trap for whoever clicks it.
func validateLinearAttachmentURL(raw string) (string, *proxyFault) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "a URL is required")
	}
	if len(raw) > 2048 {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "the URL is longer than 2048 characters")
	}
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"only http:// and https:// URLs can be attached")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the URL contains a space or control character")
		}
	}
	return raw, nil
}

// ---------------------------------------------------------------------------
// Response shape
// ---------------------------------------------------------------------------

// linearProxyOutcome is what the CLI renders.
//
// Unlike the git and GitHub proxies there is no ExitCode: there is no
// subprocess whose verdict could differ from the daemon's. A 2xx means the
// operation happened; anything else is a fault with a code and a message, and
// the CLI exits non-zero on it.
type linearProxyOutcome struct {
	// Teams is the operator's allow-list, echoed on every response. It is the
	// single most common thing an agent needs when a call is refused, and
	// carrying it means the agent does not have to run `whoami` to find out.
	Teams []string `json:"teams,omitempty"`
	// JSON is the payload, already shaped by this package rather than passed
	// through: a GraphQL response is only ever the fields we asked for, so
	// there is no "whatever the API added this week" to preserve.
	JSON json.RawMessage `json:"json,omitempty"`
	// Text carries the verbs whose output IS prose — `issue comments`.
	Text string `json:"text,omitempty"`
}

// respond renders a successful Linear result.
func (s *linearProxySession) respond(w http.ResponseWriter, r *http.Request, verb string, payload any, detail string) {
	out := linearProxyOutcome{Teams: s.policy.AllowedTeams}
	switch v := payload.(type) {
	case nil:
	case string:
		out.Text = v
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"could not encode the Linear response")
			return
		}
		out.JSON = encoded
	}
	setAuditDetail(r, fmt.Sprintf("op=%s %s", verb, detail))
	writeJSON(w, http.StatusOK, out)
}
