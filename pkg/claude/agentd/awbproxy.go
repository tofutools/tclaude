package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// awbproxy.go is the daemon half of `tclaude proxy awb` — Agent Work Board
// issue operations performed with agentd's own AWB account on behalf of an
// agent that holds no credentials of its own.
//
// It is built on the Linear proxy's shape, because the two answer the same
// question about two trackers, and it differs from it in the three ways AWB
// itself differs from Linear.
//
//  1. THERE IS NO TOOL, and no query language either. AWB publishes a REST API
//     whose document is its source of truth, so the daemon speaks HTTP directly
//     rather than building an argv for a subprocess. Every path is assembled
//     from compile-time constants plus a validated issue reference, and every
//     caller value travels either as a url.Values query parameter or as a field
//     of a JSON body this package marshals — never as a string spliced into a
//     path or a filter expression.
//
//  2. THERE IS NO ANCHOR. AWB has no filesystem artifact that ties a
//     conversation to an issue, so — exactly as in the Linear proxy — a WORKSPACE
//     SET is the whole scope gate: mandatory, fail-closed, and checked twice.
//     Once on the workspace key carried by the issue reference the caller
//     supplied, and again on the workspace AWB reports on the issue it returned
//     (see enforceIssueWorkspace). The set has two independent sources and a
//     request may act only within BOTH: the operator's
//     agent.awb_proxy.allowed_workspaces, and — when the caller's
//     proxy.awb.read / proxy.awb.write grant carries an `awb_workspace` scope —
//     the workspaces that grant names. See awbEffectiveWorkspaces.
//
//  3. THERE IS ONE CREDENTIAL. A Linear personal key reaches one workspace, so
//     that proxy routes teams to keys; an AWB account reaches every workspace it
//     is a member of on the one server the operator configured. There is
//     therefore no routing layer here at all — one URL, one username, one
//     password, and the multi-key machinery of linearproxy.go has no analogue.
//
// AWB applies its OWN authorization on top of ours: the daemon's account works
// in the workspaces it is a member of and a workspace it holds no access to is
// answered 404. That is a second, independent gate and not a substitute for
// this one — it bounds the operator, while this bounds the agent.

const (
	// awbProxyTimeout bounds ONE HTTP call to the AWB server.
	awbProxyTimeout = 45 * time.Second

	// awbProxyBudget bounds a whole REQUEST, however many calls the verb makes.
	// Several verbs make more than one — an identifier-shaped write reads the
	// issue first to confirm its workspace before mutating it, and an unfiltered
	// listing resolves the server's workspace list before it can build a filter.
	// A per-call bound alone would let the daemon run past the window the CLI
	// waits on, and the resulting ambiguity is the bad one: the agent cannot
	// tell whether the write landed, and a retry writes twice.
	awbProxyBudget = 60 * time.Second

	// awbMutationHeadroom is how much of that budget must remain for a write to
	// be attempted at all. Same reasoning as linearMutationHeadroom: refusing
	// before anything is sent is unambiguous, and a mutation cut off mid-flight
	// is not.
	awbMutationHeadroom = 5 * time.Second

	// maxAWBResponseBytes bounds what the daemon will read from AWB. Every
	// listing is bounded by `limit` as well, and a description is capped at
	// 64 KiB by AWB itself, so this is a backstop against a pathological
	// response rather than the working limit. `dep tree` is the verb that can
	// legitimately approach it.
	maxAWBResponseBytes = 16 * 1024 * 1024

	// maxAWBDescriptionBytes bounds an issue description. It is AWB's own
	// documented cap, applied here so an over-long description is refused with
	// the field named rather than as a 400 from the server.
	maxAWBDescriptionBytes = 64 * 1024

	// maxAWBAttachmentBytes bounds an attachment in either direction.
	//
	// Attachment content travels through the daemon as bytes in a JSON body,
	// base64-encoded by encoding/json. That is deliberate: the alternative —
	// handing the daemon a PATH and letting it read the agent's file — would
	// give the proxy filesystem reach, which is precisely what the git proxy's
	// "lends credentials, never filesystem reach" rule exists to deny. The cost
	// is that content is bounded rather than streamed, and this is the bound.
	maxAWBAttachmentBytes = 8 * 1024 * 1024

	// maxAWBRequestBytes bounds a request body before decoding. It is
	// maxAWBAttachmentBytes with room for base64's 4/3 expansion and the
	// scalars riding alongside, so `attach add` fails validation with the real
	// limit named rather than dying as "http: request body too large".
	maxAWBRequestBytes = maxAWBAttachmentBytes*4/3 + 64*1024

	// maxAWBTitleLen bounds an issue title, in runes. AWB's own cap.
	maxAWBTitleLen = 500

	// maxAWBWorkspaceKeyLen bounds a workspace key. AWB's own cap; applied both to
	// a caller's --workspace and to an `awb_workspace` permission-scope matcher.
	maxAWBWorkspaceKeyLen = 16

	// maxAWBLabelLen bounds a label or an assignee. AWB gives the two one
	// charset and one length, so they get one constant here too.
	maxAWBLabelLen = 64

	// maxAWBSearchTermLen bounds one search term. AWB's own cap.
	maxAWBSearchTermLen = 500

	// maxAWBSearchTerms bounds how many terms one search may carry. AWB ANDs
	// them, so a long list is a narrower query rather than a heavier one, but a
	// query string is not the place to discover that without a limit.
	maxAWBSearchTerms = 16

	// maxAWBAttachmentNameLen bounds an attachment name. AWB's own cap.
	maxAWBAttachmentNameLen = 255

	// maxAWBContentTypeLen bounds a declared content type. AWB's own cap.
	maxAWBContentTypeLen = 255

	// maxAWBCloseReasonLen bounds a close reason. AWB's own cap.
	//
	// A reason is no longer a FIELD on the issue: since AWB 0.6 a non-empty one
	// becomes a typed comment on the closing transition, in the same
	// transaction. The cap is still the server's, and is still shorter than a
	// comment's, because a reason is a line rather than a document — a longer
	// account belongs in `comment add`.
	maxAWBCloseReasonLen    = 500
	minAWBCommitHashLen     = 8
	maxAWBCommitHashLen     = 128
	maxAWBPullRequestURLLen = 1000

	// maxAWBCommentBytes bounds a comment body. It is AWB's own description cap
	// — a comment is Markdown prose held to the same bounds — applied here so
	// an over-long one is refused with the field named rather than as a 400
	// from the server.
	maxAWBCommentBytes = 64 * 1024

	// maxAWBOffset bounds how far into a timeline one request may skip.
	//
	// AWB itself sets no ceiling. The proxy does, for the same reason it
	// supplies a default limit: an offset is how a caller pages, and a page
	// beyond any real timeline is a request that spends the operator's account
	// to answer with nothing.
	maxAWBOffset = 100000

	// maxAWBLimit / defaultAWBLimit bound a listing.
	//
	// AWB itself has NO default limit — omitting it returns every row, which is
	// the right default for a human at a terminal and the wrong one for an
	// agent whose context the rows land in. The proxy therefore supplies one,
	// and says so in the CLI help: a listing here is bounded unless the caller
	// widens it, up to the cap.
	maxAWBLimit     = 500
	defaultAWBLimit = 50

	// awbProxyDisabledCode / …Message are the fail-closed answer when the
	// operator has not opted in AND the caller's grant carries no workspace scope
	// of its own. Distinct from a permission denial: nothing the agent can do
	// turns this into a success — only the operator, by writing one of the two
	// lists.
	awbProxyDisabledCode    = "awb_proxy_disabled"
	awbProxyDisabledMessage = "the AWB proxy has no workspace policy for this unscoped grant: the operator has not set " +
		"agent.awb_proxy.allowed_workspaces in ~/.tclaude/data/config.json, and an empty allow-list means " +
		"no workspace is reachable. Ask the operator to allow-list the workspace, or to scope the grant by " +
		"workspace (tclaude agent permissions grant <agent> proxy.awb.read --scope awb_workspace=awb)."

	// awbWorkspaceOutOfScopeCode is the refusal for a workspace the OPERATOR allows
	// but this caller's grant does not. Distinct from workspace_not_allowed so an
	// agent reading the code can tell its human which of the two lists to widen.
	awbWorkspaceOutOfScopeCode = "workspace_out_of_scope"

	// awbWorkspaceScopeEmptyCode is the refusal when the caller's workspace scope
	// and the operator's allow-list do not overlap at all, so the grant
	// authorizes nothing however the request is spelled. Reported once, up
	// front, rather than as a per-workspace refusal on every verb.
	awbWorkspaceScopeEmptyCode = "workspace_scope_empty"

	// awbMisconfiguredCode is the refusal for an operator policy the daemon
	// cannot act on: a URL that is not http(s), or one carrying userinfo.
	awbMisconfiguredCode = "awb_proxy_misconfigured"

	// awbNotConfiguredCode is the refusal when no server URL is configured at
	// all. Separate from awbProxyDisabledCode, which is about the workspace
	// policy: this one says there is nothing to call.
	awbNotConfiguredCode = "awb_not_configured"
)

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// AWBProxyRequest is one outbound call to the AWB server, as the transport
// seam sees it. Exported so the flow tests — which live in package agentd_test,
// to exercise the daemon from outside as a caller would — can assert on exactly
// what the daemon built.
//
// URL is fully assembled, query string included, which is the point: a test
// that reads it back proves a caller's string reached a query VALUE and never
// a path segment or a filter expression.
type AWBProxyRequest struct {
	Method      string
	URL         string
	Username    string
	Password    string
	ContentType string
	Body        []byte
}

// awbHTTPResult is what the transport seam returns.
type awbHTTPResult struct {
	Status  int
	Body    []byte
	Headers http.Header
}

// awbDo is the outbound-HTTP boundary, mirroring linearDo. Production performs
// the real request; tests swap in a recorder.
var awbDo = doAWBRequest

// SetAWBTransportForTest swaps the outbound-HTTP boundary. Returns a restore
// func.
func SetAWBTransportForTest(
	fn func(ctx context.Context, req AWBProxyRequest) (int, []byte, http.Header, error),
) func() {
	prev := awbDo
	awbDo = func(ctx context.Context, req AWBProxyRequest) (awbHTTPResult, error) {
		status, body, headers, err := fn(ctx, req)
		if err != nil {
			return awbHTTPResult{}, err
		}
		if headers == nil {
			headers = http.Header{}
		}
		return awbHTTPResult{Status: status, Body: body, Headers: headers}, nil
	}
	return func() { awbDo = prev }
}

// awbHTTPClient is the daemon's client for AWB. Explicitly constructed rather
// than http.DefaultClient so the timeout is ours and a stray DefaultClient
// mutation elsewhere in the process cannot change it.
var awbHTTPClient = &http.Client{Timeout: awbProxyTimeout}

// doAWBRequest performs one HTTP call against the AWB server.
//
// The password rides in the Authorization header, straight from daemon memory.
// It never reaches argv and never enters a child process's environment, so
// unlike the GitHub half's GH_TOKEN there is no /proc window in which a
// same-uid process could read it.
func doAWBRequest(ctx context.Context, req AWBProxyRequest) (awbHTTPResult, error) {
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return awbHTTPResult{}, fmt.Errorf("build request: %w", err)
	}
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "tclaude-agentd")
	if req.Username != "" {
		httpReq.SetBasicAuth(req.Username, req.Password)
	}

	resp, err := awbHTTPClient.Do(httpReq)
	if err != nil {
		return awbHTTPResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte past the bound, so an over-long response is DETECTED rather
	// than silently truncated. A plain LimitReader returns the first N bytes
	// with no error, which for `attach get` would mean writing a corrupt file
	// and reporting success — the one failure mode a caller cannot notice.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAWBResponseBytes+1))
	if err != nil {
		return awbHTTPResult{}, fmt.Errorf("read response: %w", err)
	}
	if len(raw) > maxAWBResponseBytes {
		return awbHTTPResult{}, fmt.Errorf(
			"the AWB server's response is larger than the %d bytes this proxy will read",
			maxAWBResponseBytes)
	}
	return awbHTTPResult{Status: resp.StatusCode, Body: raw, Headers: resp.Header}, nil
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// awbProxySession is one AWB invocation context: the operator's policy, the
// caller's effective workspace set, and the credential that reaches the server.
type awbProxySession struct {
	policy config.AWBProxyConfig
	// base is the validated server URL, without a trailing slash.
	base string
	// deadline is the wall-clock end of this request's whole budget, shared by
	// every call the verb makes. Zero means "unbounded", which only happens in
	// a unit test that built a session directly.
	deadline time.Time

	// workspaces is the ONE workspace gate every check in this package consults:
	// the operator's allow-list narrowed by the caller's grant scope,
	// lower-cased. Never empty on a session that was returned — an empty
	// intersection is a fault, not a session that quietly authorizes nothing.
	workspaces []string

	// grantWorkspaces is the workspaces the caller's OWN grant admits, lower-cased,
	// before the operator's ceiling is applied — nil when the grant is
	// unscoped. It is evaluated, not merely enumerated, so it never names a
	// workspace the scope would refuse (see awbEffectiveWorkspaces). Reported,
	// never gated on: workspaces is the gate, and this exists so a refusal can
	// say which of the two lists excluded a key.
	grantWorkspaces []string

	// password, passwordLoaded and passwordFault memoize the credential read:
	// one file read per request however many calls the verb makes.
	password       string
	passwordLoaded bool
	passwordFault  *proxyFault

	// serverWorkspaces memoizes the intersection of the effective set with the
	// workspaces the server actually has and the daemon's account can see. It is
	// resolved lazily because only an unfiltered listing needs it.
	serverWorkspaces       []string
	serverWorkspacesLoaded bool
	serverWorkspacesFault  *proxyFault
}

// newAWBProxySession runs the operator-policy gates and resolves the caller's
// effective workspace set.
//
// Ordering matches the Linear proxy's: the fail-closed policy checks come
// before anything that could touch the network, so a caller holding
// proxy.awb.read against an unconfigured daemon gets "not configured" rather
// than a connection error.
//
// perm is the slug the calling verb gates on, and workspaceScoped says the
// permission preflight deferred its decision because that grant is
// workspace-scoped (see preflightProxyPermission). Both are needed here rather
// than at the gate, because the scope has to be resolved into a SET before any
// verb runs — see the file header.
func newAWBProxySession(
	r *http.Request, convID, perm string, workspaceScoped bool,
) (*awbProxySession, *proxyFault) {
	cfg, err := config.Load()
	if err != nil {
		return nil, faultf(http.StatusInternalServerError, "config",
			"could not read the daemon configuration: %v", err)
	}
	policy := cfg.ResolvedAWBProxy()
	base, fault := validateAWBBaseURL(policy.URL)
	if fault != nil {
		return nil, fault
	}
	workspaces, grantWorkspaces, fault := awbEffectiveWorkspaces(r, convID, perm, policy, workspaceScoped)
	if fault != nil {
		return nil, fault
	}
	return &awbProxySession{
		policy:          policy,
		base:            base,
		deadline:        time.Now().Add(awbProxyBudget),
		workspaces:      workspaces,
		grantWorkspaces: grantWorkspaces,
	}, nil
}

// validateAWBBaseURL bounds the operator's configured server URL.
//
// It refuses userinfo outright rather than stripping it: a URL carrying a
// password is a credential in a plaintext config file that also reaches log
// lines and the dashboard's Config tab, and silently ignoring it would leave
// the operator believing they had configured authentication.
func validateAWBBaseURL(raw string) (string, *proxyFault) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", faultf(http.StatusServiceUnavailable, awbNotConfiguredCode,
			"no AWB server is configured: set agent.awb_proxy.url in ~/.tclaude/data/config.json")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", faultf(http.StatusServiceUnavailable, awbMisconfiguredCode,
			"agent.awb_proxy.url is not a URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", faultf(http.StatusServiceUnavailable, awbMisconfiguredCode,
			"agent.awb_proxy.url must be an http:// or https:// URL, not %q", u.Scheme)
	}
	if u.Host == "" {
		return "", faultf(http.StatusServiceUnavailable, awbMisconfiguredCode,
			"agent.awb_proxy.url names no host")
	}
	if u.User != nil {
		return "", faultf(http.StatusServiceUnavailable, awbMisconfiguredCode,
			"agent.awb_proxy.url carries credentials in the URL; put them in username and "+
				"password_file instead, where they do not travel in log lines")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", faultf(http.StatusServiceUnavailable, awbMisconfiguredCode,
			"agent.awb_proxy.url must be a base URL, with no query string and no fragment")
	}
	return raw, nil
}

// awbEffectiveWorkspaces resolves the workspace set one request may act within,
// from the operator's allow-list and the caller's grant scope.
//
// It is awbEffectiveWorkspaces and linearEffectiveTeams that make the two proxies
// behave alike, and the rules are deliberately identical:
//
//   - unscoped grant, operator list  → the operator's list;
//   - scoped grant, operator list    → the intersection, so an operator can
//     narrow one agent without touching the global list, and a grant can never
//     widen past it;
//   - scoped grant, no operator list → the grant's own workspaces;
//   - unscoped grant, no operator list → nothing is reachable: the fail-closed
//     awb_proxy_disabled answer.
//
// It resolves in TWO steps for the reason the Linear one does. First, which
// workspaces the SCOPE admits on its own merits: every workspace the scope names,
// put through the real evaluator, because permissionScopeEnumerate unions the
// matchers across the winning tier's rows and so NAMES more than it admits.
// Second, the operator's ceiling intersected over that. Separating the steps
// puts the two ways an empty result arises on two different paths, so each gets
// the refusal that names what the operator would actually have to change.
func awbEffectiveWorkspaces(
	r *http.Request, convID, perm string, policy config.AWBProxyConfig, workspaceScoped bool,
) (workspaces, grantWorkspaces []string, fault *proxyFault) {
	if !workspaceScoped {
		if len(policy.AllowedWorkspaces) == 0 {
			return nil, nil, &proxyFault{
				Status: http.StatusServiceUnavailable,
				Code:   awbProxyDisabledCode,
				Msg:    awbProxyDisabledMessage,
			}
		}
		return policy.AllowedWorkspaces, nil, nil
	}
	// A SECOND resolution, because the preflight's verdict does not travel
	// here. It re-reads the same request-scoped defaults, so in the ordinary
	// case this is the same verdict the preflight saw.
	v := resolvePermissionVerdictForRequest(r, convID, perm)
	if v.Resolution != permAllow {
		// The grant changed between the two reads. Name THAT rather than
		// blaming the shape of a scope that may no longer exist — and refuse,
		// rather than falling back on the operator's list.
		return nil, nil, faultf(http.StatusForbidden, "permission",
			"the %s grant was withdrawn while this request was being authorized", perm)
	}
	named, ok := permissionScopeEnumerate(v, ScopeDimAWBWorkspace)
	if !ok || len(named) == 0 {
		return nil, nil, faultf(http.StatusForbidden, awbWorkspaceScopeEmptyCode,
			"the %s grant is scoped, but names no AWB workspace this daemon can act on; "+
				"the scope must carry awb_workspace (e.g. --scope awb_workspace=awb)", perm)
	}
	for _, key := range named {
		if evalPermissionScope(v, convID, ActionContext{AWBWorkspace: key}).Satisfied {
			grantWorkspaces = appendWorkspaceKey(grantWorkspaces, key)
		}
	}
	if len(grantWorkspaces) == 0 {
		// The scope names workspaces but admits none of them, so something else
		// in the same scope refused. The operator's list is not the thing to
		// edit.
		return nil, nil, faultf(http.StatusForbidden, awbWorkspaceScopeEmptyCode,
			"the %s grant names workspace(s) %s, but its scope also constrains a dimension an AWB "+
				"request does not describe, so it authorizes nothing; the scope must constrain "+
				"awb_workspace alone", perm, strings.Join(lowerWorkspaceKeys(named), ", "))
	}
	if len(policy.AllowedWorkspaces) == 0 {
		// No operator list: the scope is the whole policy.
		return grantWorkspaces, grantWorkspaces, nil
	}
	for _, key := range grantWorkspaces {
		if policy.AWBWorkspaceAllowed(key) {
			workspaces = appendWorkspaceKey(workspaces, key)
		}
	}
	if len(workspaces) == 0 {
		return nil, grantWorkspaces, faultf(http.StatusForbidden, awbWorkspaceScopeEmptyCode,
			"the %s grant is scoped to workspace(s) %s, none of which is on the operator's "+
				"agent.awb_proxy.allowed_workspaces list (allowed: %s); the two must overlap",
			perm, strings.Join(grantWorkspaces, ", "), strings.Join(policy.AllowedWorkspaces, ", "))
	}
	return workspaces, grantWorkspaces, nil
}

// lowerWorkspaceKeys normalizes a workspace-key list the way the operator's
// allow-list is normalized, so the two are directly comparable and render
// alike.
func lowerWorkspaceKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = appendWorkspaceKey(out, key)
	}
	return out
}

// appendWorkspaceKey adds one normalized workspace key to a list, dropping blanks
// and duplicates. Order is preserved: these lists are rendered to humans in
// refusals and in `whoami`, so they keep the order the operator wrote.
func appendWorkspaceKey(out []string, key string) []string {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || slices.Contains(out, key) {
		return out
	}
	return append(out, key)
}

// credentials resolves the AWB account the daemon authenticates as.
//
// An empty username is a real configuration: AWB authenticates nothing when its
// database holds no user, and a server like that takes no credentials at all.
// A username WITHOUT a password is not — it is half a credential, and sending
// it would produce a 401 the operator would go looking for on the server.
//
// The password is deliberately not stored in config.json: that file is
// plaintext, shows up in the dashboard's Config tab, and is the sort of thing
// an operator copies into a bug report. It comes from a file, or from
// AWB_PASSWORD in the daemon's own environment.
func (s *awbProxySession) credentials() (username, password string, fault *proxyFault) {
	if s.policy.Username == "" {
		return "", "", nil
	}
	if s.passwordLoaded {
		return s.policy.Username, s.password, s.passwordFault
	}
	s.passwordLoaded = true
	s.password, s.passwordFault = resolveAWBPassword(s.policy)
	return s.policy.Username, s.password, s.passwordFault
}

// resolveAWBPassword is credentials() without the memoization.
func resolveAWBPassword(policy config.AWBProxyConfig) (string, *proxyFault) {
	if policy.PasswordFile != "" {
		// "~/awb-password.txt" is how an operator naturally writes this, and
		// the same expandTilde every other human-typed path in the daemon goes
		// through applies here.
		raw, err := os.ReadFile(expandTilde(policy.PasswordFile))
		if err != nil {
			return "", faultf(http.StatusServiceUnavailable, "password_unreadable",
				"agent.awb_proxy.password_file could not be read: %v%s",
				err, shellVarHint(policy.PasswordFile))
		}
		// Trailing whitespace is stripped because a file written by an editor
		// ends in a newline, and a password with one is a login that never
		// works for a reason nothing reports.
		password := strings.TrimSpace(string(raw))
		if password == "" {
			return "", faultf(http.StatusServiceUnavailable, "password_unreadable",
				"agent.awb_proxy.password_file is empty")
		}
		return password, nil
	}
	if password := strings.TrimSpace(os.Getenv("AWB_PASSWORD")); password != "" {
		return password, nil
	}
	return "", faultf(http.StatusServiceUnavailable, "password_missing",
		"agent.awb_proxy.username is set to %q but no password is configured: set "+
			"agent.awb_proxy.password_file, or put AWB_PASSWORD in the environment agentd runs under",
		policy.Username)
}

// requireMutationBudget refuses a write the request no longer has time to
// finish, BEFORE it is sent. See awbMutationHeadroom.
func (s *awbProxySession) requireMutationBudget() *proxyFault {
	if s.deadline.IsZero() || time.Until(s.deadline) >= awbMutationHeadroom {
		return nil
	}
	return faultf(http.StatusGatewayTimeout, "awb_budget_spent",
		"this request spent its %s budget reading AWB before it could write; nothing was "+
			"written, so it is safe to retry", awbProxyBudget)
}

// requireWrite gates the mutating verbs on the operator's own ceiling. The
// proxy.awb.write slug says THIS AGENT may write; allow_write says the operator
// wants any agent to be able to. Both must hold.
func (s *awbProxySession) requireWrite() *proxyFault {
	if s.policy.AllowWrite {
		return nil
	}
	return faultf(http.StatusForbidden, "awb_write_disabled",
		"the operator has not enabled writes: set agent.awb_proxy.allow_write to true in "+
			"~/.tclaude/data/config.json")
}

// callContext derives the context for one call: the per-call bound, clamped to
// whatever remains of the request's shared budget.
func (s *awbProxySession) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.deadline.IsZero() {
		return context.WithTimeout(ctx, awbProxyTimeout)
	}
	perCall := time.Now().Add(awbProxyTimeout)
	if s.deadline.Before(perCall) {
		return context.WithDeadline(ctx, s.deadline)
	}
	return context.WithDeadline(ctx, perCall)
}

// awbCall describes one operation against the AWB API.
//
// Path is assembled by the caller from compile-time constants and, at most, one
// validated issue reference or attachment name — each percent-encoded as a
// single path segment by awbSegment. Query carries every other caller value,
// and Body is marshalled by this package from a typed struct.
type awbCall struct {
	Method      string
	Path        string
	Query       url.Values
	Body        []byte
	ContentType string
}

// exec runs one call and unmarshals a JSON response into out.
//
// out may be nil for a call whose body is not wanted. The raw result is
// returned as well, because two verbs need what is beside the body: the
// listings read X-Total-Count, and `attach get` returns the bytes themselves.
func (s *awbProxySession) exec(ctx context.Context, call awbCall, out any) (*awbHTTPResult, *proxyFault) {
	username, password, fault := s.credentials()
	if fault != nil {
		return nil, fault
	}
	target := s.base + call.Path
	if len(call.Query) > 0 {
		target += "?" + call.Query.Encode()
	}

	// The tighter of the per-call bound and what is left of the whole request's
	// budget. The budget is what keeps a multi-call verb inside the window the
	// CLI is waiting on; the per-call bound is what stops any one hung call
	// consuming all of it.
	runCtx, cancel := s.callContext(ctx)
	defer cancel()

	res, err := awbDo(runCtx, AWBProxyRequest{
		Method:      call.Method,
		URL:         target,
		Username:    username,
		Password:    password,
		ContentType: call.ContentType,
		Body:        call.Body,
	})
	if err != nil {
		return nil, faultf(http.StatusBadGateway, "awb_unreachable",
			"could not reach the AWB server: %v", err)
	}
	if res.Status < 200 || res.Status > 299 {
		return nil, awbErrorFault(res)
	}
	if out == nil {
		return &res, nil
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return nil, faultf(http.StatusBadGateway, "awb_failed",
			"could not read the AWB response: %v", err)
	}
	return &res, nil
}

// awbErrorResponse is AWB's error body: one human-readable message and nothing
// else, by design — the vocabulary a caller must respect is published in the
// API document rather than reported per field.
type awbErrorResponse struct {
	Error string `json:"error"`
}

// awbErrorFault turns an AWB failure into one fault, preserving the
// classification AWB's own status codes carry.
//
// AWB documents each status as a category with an exit code behind it, so the
// mapping is a translation rather than a guess: 400 is a bad request, 404 is a
// missing entity, 409 is a constraint that depends on stored state, and 403 is
// a permission the DAEMON's account lacks. That last one is worth keeping
// distinct from this proxy's own refusals: the fix is an AWB membership, not a
// tclaude grant.
func awbErrorFault(res awbHTTPResult) *proxyFault {
	msg := ""
	var parsed awbErrorResponse
	if json.Unmarshal(res.Body, &parsed) == nil {
		msg = strings.TrimSpace(parsed.Error)
	}
	if msg == "" {
		msg = fmt.Sprintf("the AWB server returned HTTP %d", res.Status)
	}
	switch res.Status {
	case http.StatusBadRequest:
		return faultf(http.StatusBadRequest, "invalid_arg", "%s", msg)
	case http.StatusUnauthorized:
		return faultf(http.StatusServiceUnavailable, "awb_auth",
			"the AWB server rejected the operator's credentials: %s", msg)
	case http.StatusForbidden:
		return faultf(http.StatusForbidden, "awb_forbidden",
			"the AWB account the daemon authenticates as may not do this: %s", msg)
	case http.StatusNotFound:
		return faultf(http.StatusNotFound, "not_found", "%s", msg)
	case http.StatusConflict:
		return faultf(http.StatusConflict, "awb_conflict", "%s", msg)
	case http.StatusRequestEntityTooLarge:
		return faultf(http.StatusBadRequest, "invalid_arg",
			"the AWB server refused the request as too large: %s", msg)
	case http.StatusUnsupportedMediaType, http.StatusMethodNotAllowed:
		// The daemon chose the method and the content type, so neither is the
		// agent's doing. Named as a tclaude bug rather than a bad request.
		return faultf(http.StatusBadGateway, "awb_schema_drift",
			"tclaude's AWB request was refused by the server (this is a tclaude bug, not a bad "+
				"request): %s", msg)
	}
	return faultf(http.StatusBadGateway, "awb_failed", "%s", msg)
}

// ---------------------------------------------------------------------------
// The workspace gate
// ---------------------------------------------------------------------------

// workspaceAllowed reports whether key is in this caller's effective workspace set.
//
// What keeps the reference gate, the listing filter and the row-level drop from
// diverging is that all three read s.workspaces — this predicate for the
// per-issue checks, the listing filter reading the resolved set directly
// because it needs the whole of it.
func (s *awbProxySession) workspaceAllowed(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key != "" && slices.Contains(s.workspaces, key)
}

// requireAllowedWorkspace refuses a workspace key outside the caller's effective
// set.
//
// The message names the list that ACTUALLY excluded the key — the operator's
// allow-list or the caller's own grant scope — so an agent can tell its human
// exactly which one to widen rather than guessing from a refusal. The codes are
// distinct for the same reason.
func (s *awbProxySession) requireAllowedWorkspace(key string) *proxyFault {
	if s.workspaceAllowed(key) {
		return nil
	}
	if len(s.policy.AllowedWorkspaces) > 0 && !s.policy.AWBWorkspaceAllowed(key) {
		return faultf(http.StatusForbidden, "workspace_not_allowed",
			"workspace %q is not on the operator's agent.awb_proxy.allowed_workspaces list (allowed: %s)",
			key, strings.Join(s.policy.AllowedWorkspaces, ", "))
	}
	return faultf(http.StatusForbidden, awbWorkspaceOutOfScopeCode,
		"workspace %q is outside this caller's AWB workspace scope (this grant covers: %s)",
		key, strings.Join(s.grantWorkspaces, ", "))
}

// enforceIssueWorkspace is the SECOND half of the workspace gate, and the
// load-bearing one. The reference check tests a string the caller supplied;
// this tests the workspace AWB actually reports on the issue it returned.
//
// The two can disagree. An AWB issue reference may be an unambiguous PREFIX, so
// "awb-a3" resolves to whatever issue that prefix names — and the prefix a
// caller wrote is not the same thing as the issue reached. Refusing here, after
// the read but before the response is rendered, means the data never leaves the
// daemon.
func (s *awbProxySession) enforceIssueWorkspace(issue *awbIssue) *proxyFault {
	if issue == nil {
		return faultf(http.StatusNotFound, "not_found", "no such issue")
	}
	if key := strings.TrimSpace(issue.Workspace); key != "" {
		return s.requireAllowedWorkspace(key)
	}
	// No workspace on the response means AWB returned something this package
	// cannot gate. Refuse rather than let an unchecked issue through.
	return faultf(http.StatusInternalServerError, "workspace_unresolved",
		"the AWB response carried no workspace; refusing to return an issue the workspace gate could "+
			"not be checked against")
}

// enforceIssueList applies the workspace gate to every row of a listing.
//
// The filter should already have made this a no-op. It runs anyway: the filter
// is a request AWB honours, while this is a check the daemon makes, and only
// the second one is a gate. A row from outside the effective set is dropped
// rather than refused — one unexpected row must not deny the agent the rest of
// a legitimate listing — and dropping is safe because the rows are data, not an
// operation.
func (s *awbProxySession) enforceIssueList(issues []awbIssue) []awbIssue {
	kept := make([]awbIssue, 0, len(issues))
	for i := range issues {
		if s.workspaceAllowed(issues[i].Workspace) {
			kept = append(kept, issues[i])
		}
	}
	return kept
}

// pruneTree applies the workspace gate to a decomposition tree.
//
// `dep tree` follows children ACROSS workspace boundaries, which is right for AWB
// — a decomposition is not confined to one workspace — and wrong for a gate whose
// whole job is to bound what leaves the daemon: a child in an unreachable
// workspace would arrive as a complete issue, description and all.
//
// So an out-of-scope node is dropped together with its subtree, and the count
// of what went is reported beside the tree rather than left to be inferred from
// a gap. Dropping rather than refusing follows enforceIssueList: the caller
// asked about an issue it may see, and the answer is the part of the
// decomposition it may see.
func (s *awbProxySession) pruneTree(node *awbIssueTree) (kept *awbIssueTree, pruned int) {
	if node == nil {
		return nil, 0
	}
	if !s.workspaceAllowed(node.Workspace) {
		return nil, countTreeNodes(node)
	}
	children := make([]awbIssueTree, 0, len(node.Children))
	for i := range node.Children {
		child, dropped := s.pruneTree(&node.Children[i])
		pruned += dropped
		if child != nil {
			children = append(children, *child)
		}
	}
	node.Children = children
	return node, pruned
}

// countTreeNodes is how many issues one subtree holds, so a pruned answer can
// say how much of the decomposition it is not showing.
func countTreeNodes(node *awbIssueTree) int {
	if node == nil {
		return 0
	}
	n := 1
	for i := range node.Children {
		n += countTreeNodes(&node.Children[i])
	}
	return n
}

// listingWorkspaces is the workspace set ONE listing may ask about.
//
// A named workspace is that workspace, already gated. An unnamed one is the whole
// effective set — intersected with the workspaces the server actually holds and
// the daemon's account can see, because AWB answers a `workspace` filter naming
// no workspace with a 404 rather than with an empty listing. Without that
// intersection a single stale entry in the operator's allow-list would break
// every unfiltered listing, and the 404 would name the workspace rather than the
// reason.
func (s *awbProxySession) listingWorkspaces(ctx context.Context, named []string) ([]string, *proxyFault) {
	if len(named) > 0 {
		return named, nil
	}
	if s.serverWorkspacesLoaded {
		return s.serverWorkspaces, s.serverWorkspacesFault
	}
	s.serverWorkspacesLoaded = true
	s.serverWorkspaces, s.serverWorkspacesFault = s.resolveServerWorkspaces(ctx)
	return s.serverWorkspaces, s.serverWorkspacesFault
}

// resolveServerWorkspaces is listingWorkspaces' one call, without the memoization.
func (s *awbProxySession) resolveServerWorkspaces(ctx context.Context) ([]string, *proxyFault) {
	var workspaces []awbWorkspace
	if _, fault := s.exec(ctx, awbCall{Method: http.MethodGet, Path: "/api/workspaces"}, &workspaces); fault != nil {
		return nil, fault
	}
	present := make(map[string]bool, len(workspaces))
	for _, p := range workspaces {
		present[strings.ToLower(strings.TrimSpace(p.Key))] = true
	}
	kept := make([]string, 0, len(s.workspaces))
	for _, key := range s.workspaces {
		if present[key] {
			kept = append(kept, key)
		}
	}
	if len(kept) == 0 {
		return nil, faultf(http.StatusNotFound, "not_found",
			"none of the workspace(s) this caller may reach (%s) exists on %s, or the AWB account "+
				"the daemon authenticates as is not a member of any of them",
			strings.Join(s.workspaces, ", "), s.base)
	}
	return kept, nil
}

// ---------------------------------------------------------------------------
// Parameter validation
// ---------------------------------------------------------------------------

// validateAWBIssueRef bounds an issue reference to a form that CARRIES A
// WORKSPACE: "<workspace>-<hash-prefix>".
//
// AWB itself also accepts a bare hash, matched across the whole database. That
// is deliberately refused here for the reason the Linear proxy refuses a raw
// UUID: the workspace allow-list is checked before the call is made, and a bare
// hash names no workspace, so accepting one would mean every read had to be made
// first and judged afterwards. Requiring the workspace prefix keeps the cheap
// gate ahead of the network — and it is how an agent refers to an issue anyway,
// every listing having printed the full id.
//
// A PREFIX of the hash is still fine: it carries the workspace, which is all the
// pre-call gate needs, and enforceIssueWorkspace checks what it actually resolved
// to afterwards.
func validateAWBIssueRef(raw string) (string, *proxyFault) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if id == "" {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"an issue id is required, in <workspace>-<hash> form")
	}
	if len(id) > maxAWBWorkspaceKeyLen+1+64 {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "%q is not an issue id: it is too long", raw)
	}
	// Split on the LAST hyphen: a workspace key may itself contain hyphens, which
	// is why AWB's own SplitID does the same.
	i := strings.LastIndex(id, "-")
	if i <= 0 || i == len(id)-1 {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not an issue id; the form is <workspace>-<hash>, e.g. awb-a3f9c1. A bare hash is "+
				"not accepted here: it names no workspace, and the workspace is what the gate is checked "+
				"against", raw)
	}
	workspace, hash := id[:i], id[i+1:]
	if fault := validateAWBWorkspaceKeyShape(workspace); fault != nil {
		return "", fault
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"%q is not an issue id (the form is <workspace>-<hash>); the part after the workspace "+
					"key must be lowercase hexadecimal", raw)
		}
	}
	return id, nil
}

// workspaceKeyOf extracts the workspace key from a VALIDATED issue reference.
// Returns "" for anything that did not come through validateAWBIssueRef.
func workspaceKeyOf(ref string) string {
	i := strings.LastIndex(ref, "-")
	if i <= 0 {
		return ""
	}
	return ref[:i]
}

// validateAWBWorkspaceKeyShape bounds a workspace key's charset.
func validateAWBWorkspaceKeyShape(key string) *proxyFault {
	if err := awbWorkspaceKeyShapeErr(key); err != nil {
		return faultf(http.StatusBadRequest, "invalid_arg", "%s", err.Error())
	}
	return nil
}

// awbWorkspaceKeyShapeErr is the shape rule itself, as a plain error.
//
// It exists apart from validateAWBWorkspaceKeyShape so the permission-scope
// parser can apply the SAME rule to an `awb_workspace` matcher without depending
// on the proxy's fault type. One rule, two callers: a matcher an operator can
// write must be a workspace key this proxy could also accept as a parameter, or a
// scope could name something no request can ever match — which reads as a
// narrow grant and silently authorizes nothing.
//
// The rule is AWB's own: lowercase ASCII letters, digits and hyphens, starting
// with a letter.
func awbWorkspaceKeyShapeErr(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("a workspace key is required")
	}
	if len(key) > maxAWBWorkspaceKeyLen {
		return fmt.Errorf("workspace key %q is longer than %d characters", key, maxAWBWorkspaceKeyLen)
	}
	if c := key[0]; c < 'a' || c > 'z' {
		return fmt.Errorf("workspace key %q must start with a lowercase letter", key)
	}
	for _, r := range key {
		isLower := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if !isLower && !isDigit && r != '-' {
			return fmt.Errorf(
				"workspace key %q contains a character that is not a lowercase letter, a digit or a hyphen",
				key)
		}
	}
	return nil
}

// validateAWBWorkspace normalises and allow-list-checks a workspace key supplied as
// a parameter (`--workspace`).
func (s *awbProxySession) validateAWBWorkspace(key string) (string, *proxyFault) {
	key = strings.ToLower(strings.TrimSpace(key))
	if fault := validateAWBWorkspaceKeyShape(key); fault != nil {
		return "", fault
	}
	if fault := s.requireAllowedWorkspace(key); fault != nil {
		return "", fault
	}
	return key, nil
}

// validateAWBLabel bounds a label. AWB's charset, rejected rather than
// normalised, exactly as AWB rejects rather than normalises it.
func validateAWBLabel(raw string) (string, *proxyFault) {
	return validateAWBNameToken(raw, "label")
}

// validateAWBAssignee bounds an assignee or username. AWB gives assignees and
// labels one charset on purpose — a username IS an assignee — so they share one
// validator here too.
func validateAWBAssignee(raw string) (string, *proxyFault) {
	return validateAWBNameToken(raw, "assignee")
}

func validateAWBCommitHash(raw string) (string, *proxyFault) {
	if raw == "" {
		return "", nil
	}
	if len(raw) < minAWBCommitHashLen || len(raw) > maxAWBCommitHashLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"commit hash must be between %d and %d hexadecimal characters",
			minAWBCommitHashLen, maxAWBCommitHashLen)
	}
	for _, r := range raw {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"commit hash must contain only hexadecimal characters")
		}
	}
	return raw, nil
}

func validateAWBPullRequestURL(raw string) (string, *proxyFault) {
	if raw == "" {
		return "", nil
	}
	if !utf8.ValidString(raw) {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "pull request URL is not valid UTF-8")
	}
	if utf8.RuneCountInString(raw) > maxAWBPullRequestURLLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"pull request URL is too long: maximum %d characters", maxAWBPullRequestURLLen)
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "pull request URL must not contain whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"pull request URL must be an absolute http or https URL")
	}
	return raw, nil
}

// validateAWBNameToken is the shared rule: lowercase ASCII letters, digits,
// hyphens, underscores, dots and slashes.
func validateAWBNameToken(raw, what string) (string, *proxyFault) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "an empty %s is not one", what)
	}
	if len(v) > maxAWBLabelLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%s %q is longer than %d characters", what, v, maxAWBLabelLen)
	}
	for _, r := range v {
		isLower := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if !isLower && !isDigit && r != '-' && r != '_' && r != '.' && r != '/' {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"%s %q contains %q; AWB allows lowercase letters, digits, hyphens, underscores, "+
					"dots and slashes, and rejects anything else rather than normalising it",
				what, v, string(r))
		}
	}
	return v, nil
}

// validateAWBTitle bounds an issue title.
//
// A title reaches no argv here, so the leading-"-" rule the GitHub half needs
// does not apply. The control-character and Unicode-format-character rules do:
// this title is published in the operator's tracker under their account, where
// a reader has no reason to suspect the displayed text is not the stored text,
// and U+202E and friends reorder how it renders without changing what it says.
func validateAWBTitle(title string) (string, *proxyFault) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "a title is required")
	}
	if utf8.RuneCountInString(title) > maxAWBTitleLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"title is longer than %d characters", maxAWBTitleLen)
	}
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the title contains a control character (did you mean to put this in the description?)")
		}
		if unicode.Is(unicode.Cf, r) {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the title contains a Unicode format character (U+%04X); those can reorder how the "+
					"title renders without changing what it says", r)
		}
	}
	return title, nil
}

// validateAWBDescription bounds a description. Like the Linear proxy's body it
// is deliberately unrestricted in charset: it is Markdown prose that will be
// published, and AWB round-trips it byte for byte.
func validateAWBDescription(body string) *proxyFault {
	if len(body) > maxAWBDescriptionBytes {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"the description is %d bytes; AWB's maximum is %d", len(body), maxAWBDescriptionBytes)
	}
	return nil
}

// validateAWBComment bounds a comment body.
//
// A comment is prose that will be published under the operator's account, so
// the charset is deliberately as permissive as AWB's own: any text, with the
// whitespace controls that Markdown needs, and nothing else from the control
// range. Unlike a description it may not be blank — an empty comment is an
// entry in the timeline that says nothing, and AWB refuses one too.
func validateAWBComment(body string) *proxyFault {
	if strings.TrimSpace(body) == "" {
		return faultf(http.StatusBadRequest, "invalid_arg", "a comment body is required")
	}
	if len(body) > maxAWBCommentBytes {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"the comment is %d bytes; AWB's maximum is %d", len(body), maxAWBCommentBytes)
	}
	if !utf8.ValidString(body) {
		return faultf(http.StatusBadRequest, "invalid_arg", "the comment is not valid UTF-8")
	}
	for _, r := range body {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if unicode.Is(unicode.Cc, r) {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"the comment contains a control character (U+%04X)", r)
		}
	}
	return nil
}

// validateAWBOffset bounds how far into a timeline a request may skip.
func validateAWBOffset(offset int) (int, *proxyFault) {
	if offset < 0 || offset > maxAWBOffset {
		return 0, faultf(http.StatusBadRequest, "invalid_arg",
			"offset must be between 0 and %d", maxAWBOffset)
	}
	return offset, nil
}

// validateAWBCloseReason bounds a close reason.
func validateAWBCloseReason(reason string) *proxyFault {
	if utf8.RuneCountInString(reason) > maxAWBCloseReasonLen {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"the close reason is longer than %d characters", maxAWBCloseReasonLen)
	}
	return nil
}

// validateAWBType bounds an issue type against AWB's fixed five.
func validateAWBType(t string) (string, *proxyFault) {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return "", nil
	}
	if !slices.Contains(awbTypes, t) {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not an issue type; AWB has exactly: %s", t, strings.Join(awbTypes, ", "))
	}
	return t, nil
}

// validateAWBStatus bounds a status against AWB's fixed three.
func validateAWBStatus(s string) (string, *proxyFault) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !slices.Contains(awbStatuses, s) {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not a status; AWB has exactly: %s", s, strings.Join(awbStatuses, ", "))
	}
	return s, nil
}

// validateAWBRelationType bounds a relation against AWB's fixed four.
func validateAWBRelationType(t string) (string, *proxyFault) {
	t = strings.ToLower(strings.TrimSpace(t))
	if !slices.Contains(awbRelationTypes, t) {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not a relation type; AWB has exactly: %s", t, strings.Join(awbRelationTypes, ", "))
	}
	return t, nil
}

// validateAWBPriority bounds a priority. AWB's scale is fixed: 0 is the
// highest and 4 the lowest.
func validateAWBPriority(p int) *proxyFault {
	if p < 0 || p > 4 {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"priority must be between 0 (highest) and 4 (lowest)")
	}
	return nil
}

// validateAWBSort bounds an ordering. The vocabulary is AWB's, and `relevance`
// is accepted only by search — a sort AWB would refuse is refused here with the
// list rather than sent for the server to reject.
func validateAWBSort(sort string, relevance bool) (string, *proxyFault) {
	sort = strings.TrimSpace(sort)
	if sort == "" {
		return "", nil
	}
	allowed := awbSorts
	if relevance {
		allowed = awbSearchSorts
	}
	if !slices.Contains(allowed, sort) {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not a sort; this verb accepts: %s", sort, strings.Join(allowed, ", "))
	}
	return sort, nil
}

// validateAWBLimit bounds a listing. See maxAWBLimit for why the proxy supplies
// a default AWB itself does not have.
func validateAWBLimit(limit int) (int, *proxyFault) {
	if limit == 0 {
		return defaultAWBLimit, nil
	}
	if limit < 1 || limit > maxAWBLimit {
		return 0, faultf(http.StatusBadRequest, "invalid_arg",
			"limit must be between 1 and %d", maxAWBLimit)
	}
	return limit, nil
}

// validateAWBSearchTerms bounds a search. Each term is one literal string AWB
// matches whole; no operator, wildcard or column prefix passes through, so the
// only limits that matter are count, length and control characters.
func validateAWBSearchTerms(terms []string) ([]string, *proxyFault) {
	out := make([]string, 0, len(terms))
	for _, raw := range terms {
		term := strings.TrimSpace(raw)
		if term == "" {
			continue
		}
		if utf8.RuneCountInString(term) > maxAWBSearchTermLen {
			return nil, faultf(http.StatusBadRequest, "invalid_arg",
				"a search term longer than %d characters is not one", maxAWBSearchTermLen)
		}
		for _, r := range term {
			if r < 0x20 || r == 0x7f {
				return nil, faultf(http.StatusBadRequest, "invalid_arg",
					"a search term contains a control character")
			}
		}
		out = append(out, term)
	}
	if len(out) == 0 {
		return nil, faultf(http.StatusBadRequest, "invalid_arg", "a search term is required")
	}
	if len(out) > maxAWBSearchTerms {
		return nil, faultf(http.StatusBadRequest, "invalid_arg",
			"at most %d search terms; AWB ANDs them, so this is already a very narrow query",
			maxAWBSearchTerms)
	}
	return out, nil
}

// validateAWBAttachmentName bounds an attachment name.
//
// It is a NAME and not a path, and the rules say so: a slash, a backslash, a
// control character and the two names that mean a directory are refused rather
// than stripped. AWB applies the same rule at its end; applying it here too
// means an agent gets the reason rather than a 400, and means nothing
// path-shaped ever reaches the URL this proxy builds.
func validateAWBAttachmentName(raw string) (string, *proxyFault) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", faultf(http.StatusBadRequest, "invalid_arg", "an attachment name is required")
	}
	if len(name) > maxAWBAttachmentNameLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"an attachment name longer than %d bytes is not one", maxAWBAttachmentNameLen)
	}
	if name == "." || name == ".." {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q means a directory rather than a file, so it is not an attachment name", name)
	}
	for _, r := range name {
		if r == '/' || r == '\\' {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"an attachment name may not contain %q: it is a name, never somewhere to write",
				string(r))
		}
		if r < 0x20 || r == 0x7f {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the attachment name contains a control character")
		}
	}
	return name, nil
}

// validateAWBContentType bounds a declared media type. Empty is fine — AWB
// sniffs one from the content's first bytes, which is the better answer anyway
// because it does not depend on an extension table on somebody's machine.
func validateAWBContentType(raw string) (string, *proxyFault) {
	ct := strings.TrimSpace(raw)
	if ct == "" {
		return "", nil
	}
	if len(ct) > maxAWBContentTypeLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"a content type longer than %d bytes is not one", maxAWBContentTypeLen)
	}
	for _, r := range ct {
		if r < 0x20 || r == 0x7f {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the content type contains a control character")
		}
	}
	return ct, nil
}

// validateAWBContent bounds attachment bytes. See maxAWBAttachmentBytes for why
// content travels as bytes at all.
func validateAWBContent(content []byte) *proxyFault {
	if len(content) == 0 {
		return faultf(http.StatusBadRequest, "invalid_arg", "an attachment needs content")
	}
	if len(content) > maxAWBAttachmentBytes {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"the attachment is %d bytes; the proxy's maximum is %d, because content travels through "+
				"the daemon in a request body rather than as a path it would have to read",
			len(content), maxAWBAttachmentBytes)
	}
	return nil
}

// awbSegment percent-encodes one caller value as a single path segment.
//
// Every value that reaches it has already been through a validator that admits
// no slash, so this is the second of two independent reasons a caller value
// cannot become a path of its own — and the one that does not depend on a
// validator elsewhere staying correct.
func awbSegment(v string) string { return url.PathEscape(v) }

// ---------------------------------------------------------------------------
// AWB's vocabulary
// ---------------------------------------------------------------------------

// AWB's vocabulary is fixed and small by design — five types, three statuses,
// five priorities, four relation types — which is what makes it teachable to an
// agent in a few lines. Repeating it here is what lets the proxy refuse a bad
// value with the list rather than forwarding it for a 400.
// The timeline's two kinds. A comment is prose somebody wrote; a change is the
// compact record a successful mutation leaves behind. `comment list` is the
// listing narrowed to the first; `activity` takes either, or neither for the
// whole timeline.
const (
	awbActivityKindComment = "comment"
	awbActivityKindChange  = "change"
)

var awbActivityKinds = []string{awbActivityKindComment, awbActivityKindChange}

// validateAWBActivityKind bounds the timeline filter. Empty is the whole
// timeline, which is what `activity` answers with when no kind is named.
func validateAWBActivityKind(kind string) (string, *proxyFault) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return "", nil
	}
	if !slices.Contains(awbActivityKinds, kind) {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"%q is not an activity kind; AWB has exactly: %s",
			kind, strings.Join(awbActivityKinds, ", "))
	}
	return kind, nil
}

var (
	awbTypes         = []string{"epic", "feature", "bug", "task", "chore"}
	awbStatuses      = []string{"open", "in_progress", "closed"}
	awbRelationTypes = []string{"blocked-by", "has-parent", "discovered-from", "related"}
	awbSorts         = []string{
		"order", "-order", "workspace", "-workspace", "status", "-status", "assignee", "-assignee", "blockers", "-blockers", "priority", "-priority", "created", "-created", "updated", "-updated", "id", "-id",
	}
	awbSearchSorts = append(append([]string{}, awbSorts...), "relevance", "-relevance")
)

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

// awbIssue mirrors AWB's Issue schema. It is spelled out rather than passed
// through as raw JSON for two reasons: the workspace gate has to READ the workspace
// off every issue that comes back, and --compact renders from these fields. A
// field AWB adds later is dropped rather than forwarded, which is the same
// trade the Linear proxy makes.
//
// There is deliberately no close_reason. AWB 0.6 removed it from the issue
// entirely: a close reason is now a typed comment on the closing transition,
// reachable through `comment list`. Keeping the field would have reported
// `"close_reason": ""` on every issue — a value that reads as "this issue has
// no reason recorded" for a concept the tracker no longer has.
type awbIssue struct {
	ID             string          `json:"id"`
	Workspace      string          `json:"workspace"`
	Title          string          `json:"title"`
	Description    string          `json:"description"`
	CommitHash     string          `json:"commit_hash"`
	PullRequestURL string          `json:"pull_request_url"`
	Type           string          `json:"type"`
	Status         string          `json:"status"`
	Priority       int             `json:"priority"`
	Order          int             `json:"order"`
	BoardHidden    bool            `json:"board_hidden"`
	Labels         []string        `json:"labels"`
	Assignees      []string        `json:"assignees"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	ClosedAt       string          `json:"closed_at"`
	Blocked        bool            `json:"blocked"`
	Blockers       []string        `json:"blockers"`
	Relations      []awbRelation   `json:"relations"`
	Links          []awbLink       `json:"links"`
	Attachments    []awbAttachment `json:"attachments"`
}

// awbIssueTree is one issue extended with its children, recursively.
type awbIssueTree struct {
	awbIssue
	Children []awbIssueTree `json:"children"`
}

type awbRelation struct {
	Type      string `json:"type"`
	Other     string `json:"other"`
	Direction string `json:"direction"`
}

type awbLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type awbAttachment struct {
	Issue       string `json:"issue"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	CreatedAt   string `json:"created_at"`
}

// awbActivity is one entry of an issue's append-only timeline: a comment
// somebody wrote, or a compact record of a change.
//
// The two are one shape on purpose, and `comment list` is the activity listing
// narrowed to `kind=comment` rather than a separate endpoint — which is also
// why a close reason arrives here, as a comment carrying Action "closed".
type awbActivity struct {
	ID    int64  `json:"id"`
	Issue string `json:"issue"`
	Kind  string `json:"kind"`
	// Actor is empty when the server authenticates nobody, or on a migrated
	// entry whose author is not reliably known.
	Actor string `json:"actor"`
	// Body is Markdown for a comment and empty for a change.
	Body string `json:"body"`
	// Action is empty for an ordinary comment and "closed" for a close-reason
	// one; for a change it names the change.
	Action    string              `json:"action"`
	Changes   []awbActivityChange `json:"changes"`
	CreatedAt string              `json:"created_at"`
}

// awbActivityChange is one field-level difference. From and To are raw JSON so
// a scalar and a collection share one wire shape without losing their types —
// passing them through untouched is what keeps that promise across the proxy.
type awbActivityChange struct {
	Field string          `json:"field"`
	From  json.RawMessage `json:"from"`
	To    json.RawMessage `json:"to"`
}

type awbWorkspace struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	ActiveIssues int    `json:"active_issues"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type awbMembership struct {
	Workspace string `json:"workspace"`
	User      string `json:"user"`
	Access    string `json:"access"`
}

type awbUser struct {
	Name           string          `json:"name"`
	WorkspaceAdmin bool            `json:"workspace_admin"`
	UserAdmin      bool            `json:"user_admin"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	Workspaces     []awbMembership `json:"workspaces"`
}

type awbIdentityResponse struct {
	Identity string `json:"identity"`
}

// ---------------------------------------------------------------------------
// Response shape
// ---------------------------------------------------------------------------

// awbProxyOutcome is what the CLI renders.
//
// Like the Linear proxy's there is no ExitCode: no subprocess runs, so there is
// no second verdict to report. A 2xx means the operation happened; anything
// else is a fault with a code and a message, and the CLI exits non-zero on it.
type awbProxyOutcome struct {
	// Workspaces is the caller's EFFECTIVE workspace set — the operator's
	// allow-list narrowed by this caller's grant scope — echoed on every
	// response. It is the single most common thing an agent needs when a call
	// is refused, and carrying it means the agent does not have to run `whoami`
	// to find out.
	Workspaces []string `json:"workspaces,omitempty"`
	// LegacyProjects keeps separately installed older clients fail-safe during
	// the project-to-workspace transition. Remove after one compatibility cycle.
	LegacyProjects []string `json:"projects,omitempty"`
	// JSON is the payload in --json mode, already shaped by this package.
	JSON json.RawMessage `json:"json,omitempty"`
	// Text is the payload in --compact mode, and the one line a delete or a
	// create prints when --json is not in force.
	Text string `json:"text,omitempty"`
	// Content is an attachment's bytes, for `attach get` alone. It is base64 on
	// the wire, which encoding/json does for a []byte without being asked.
	Content []byte `json:"content,omitempty"`
	// HasContent distinguishes an attachment that is genuinely empty from a
	// verb that returns no content at all. Content is omitempty (a large field
	// should not be spelled out as `null` on every other verb), so without this
	// the CLI could not tell "zero bytes" from "not this verb".
	HasContent bool `json:"has_content,omitempty"`
}

// respond renders a successful AWB result in whichever of the two output modes
// the caller asked for.
//
// The MODE decides which field is filled, not whether the value is empty, and
// that distinction is load-bearing: --compact on a listing that matched nothing
// renders no lines, exactly as awb renders none, and a fall-through to the JSON
// payload on an empty string would print the rows instead. So compact is a
// parameter rather than something inferred from text.
//
// What --compact MEANS is the verb's decision, because it differs by verb in
// awb and the proxy follows it: one line per issue for a listing, the new id
// for a create, one summary line for a delete, and nothing at all for every
// other mutation.
func (s *awbProxySession) respond(
	w http.ResponseWriter, r *http.Request, verb string, compact bool, payload any, text, detail string,
) {
	out := awbProxyOutcome{Workspaces: s.workspaces, LegacyProjects: s.workspaces}
	if compact {
		out.Text = text
	} else if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "could not encode the AWB response")
			return
		}
		out.JSON = encoded
	}
	setAuditDetail(r, fmt.Sprintf("op=%s %s", verb, detail))
	writeJSON(w, http.StatusOK, out)
}

// respondContent renders `attach get`: the bytes, and nothing else. --json and
// --compact do not apply to it, exactly as they do not apply to awb's own
// `attach get`, because its output is not text and not a mode.
func (s *awbProxySession) respondContent(
	w http.ResponseWriter, r *http.Request, verb string, content []byte, detail string,
) {
	setAuditDetail(r, fmt.Sprintf("op=%s %s", verb, detail))
	writeJSON(w, http.StatusOK, awbProxyOutcome{
		Workspaces: s.workspaces, LegacyProjects: s.workspaces, Content: content, HasContent: true,
	})
}
