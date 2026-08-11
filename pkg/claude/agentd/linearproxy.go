package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
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
//     that. A TEAM SET is therefore the whole scope gate, which is why it is
//     mandatory, fail-closed, and checked twice: once on the identifier the
//     caller supplied, and again on the team Linear reports on the response —
//     see enforceIssueTeam.
//
//     That set has two independent sources, and a request may only act within
//     BOTH: the operator's agent.linear_proxy.allowed_teams (the ceiling for
//     every agent) and, when the caller's proxy.linear.read / proxy.linear.write grant
//     carries a `linear_team` scope, the teams that grant names. Resolving the
//     two into one effective set once, in newLinearProxySession, is what keeps
//     the identifier gate, the listing filter and the row-level drop from ever
//     disagreeing about which teams this caller may reach — see
//     linearEffectiveTeams.
//
//     Unlike the git proxy, the gate here is a SET rather than one value per
//     request, because two verbs (`issue ls` and `issue search` with no
//     --team) legitimately span every team the caller may reach and so need a
//     universe to build a filter from. The consequence is deliberate and worth
//     stating: a team outside the effective set is refused outright rather than
//     escalated to an ask-human popup the way an out-of-scope git remote is.
//     There is no single team such a popup could name for a cross-team listing,
//     and the operator's own allow-list has never had that escape either — so
//     within Linear the two halves of the gate behave the same way.

const (
	// linearEndpoint is Linear's only API endpoint. A constant, not config:
	// the point of the proxy is that the agent cannot choose where the
	// operator's credential is spent.
	linearEndpoint = "https://api.linear.app/graphql"

	// linearProxyTimeout bounds ONE GraphQL call. Linear is an API, not a
	// transport, so this is generous; a call that takes longer is Linear
	// being slow or rate-limiting.
	linearProxyTimeout = 45 * time.Second

	// linearProxyBudget bounds a whole REQUEST, however many calls the verb
	// makes. Several verbs make more than one, and a fully-specified `issue
	// update` makes seven: confirm the issue, resolve the team's states, then
	// one lookup each for the project, milestone and assignee names and one for
	// the whole label set, then the mutation. A per-call bound alone would let
	// the daemon run to 7x45s while the CLI gave up at 75s. That failure mode is
	// the bad one: the agent cannot tell whether the mutation landed, and a
	// retry double-posts under the operator's name.
	//
	// Every one of those calls is an indexed lookup returning a handful of rows,
	// so the budget is not tight in practice — but it is what makes a degraded
	// Linear surface as the daemon's own answer rather than as a client hang-up.

	// linearMutationHeadroom is how much of that budget must remain for a write
	// to be attempted at all.
	//
	// The reads a write verb makes first — confirming the issue, resolving names
	// — can eat the budget between them when Linear is degraded, and the
	// mutation would then be sent with a sliver of deadline and cut off
	// mid-flight. That is the one outcome the budget exists to prevent: the
	// agent cannot tell whether the write landed, and a retry writes twice under
	// the operator's name. Refusing before sending anything is unambiguous, and
	// it is what "not attempted" has to mean.
	linearMutationHeadroom = 5 * time.Second
	//
	// Same reasoning as ghProxyCommentsTimeout, which is one budget across
	// both of `pr comments`' gh calls. The budget stays inside the bound the
	// CLI waits on however the work divides between the calls.
	linearProxyBudget = 60 * time.Second

	// maxLinearResponseBytes bounds what the daemon will read from Linear.
	// Every list verb is bounded by `first:` as well, so this is a backstop
	// against a pathological response rather than the working limit.
	maxLinearResponseBytes = 4 * 1024 * 1024

	// maxLinearBodyBytes bounds a description or comment body. Linear accepts
	// large markdown documents; this is well past anything an agent should be
	// writing into a ticket and is enforced before the request is built.
	maxLinearBodyBytes = 256 * 1024

	// maxLinearRequestBytes bounds a request body before decoding. It is
	// deliberately NOT the git proxy's 16 KiB: that bound is documented as
	// "a handful of short scalars", which is true there and false here — this
	// proxy advertises --body-file for exactly the multi-kilobyte markdown a
	// progress report is made of. A reader below maxLinearBodyBytes would
	// reject such a body with a raw transport error before validateLinearBody
	// could name the real limit or the offending field.
	//
	// The headroom over maxLinearBodyBytes covers JSON escaping and the other
	// scalars riding alongside the body.
	maxLinearRequestBytes = maxLinearBodyBytes + 64*1024

	// maxLinearTitleLen bounds an issue title, in runes.
	maxLinearTitleLen = 256

	// maxLinearTeamKeyLen bounds a team key. Linear's own keys are short
	// alphanumeric labels ("TCL", "JOH"); this is far past any real one. Applied
	// both to a caller's --team and to a `linear_team` permission-scope matcher.
	maxLinearTeamKeyLen = 16

	// maxLinearQueryLen bounds a search term.
	maxLinearQueryLen = 256

	// maxLinearStateNameLen bounds a workflow-state name. Linear's own state
	// names are short labels ("In Review", "Ready to Deploy"); this is far
	// past any real one.
	maxLinearStateNameLen = 128

	// maxLinearLimit / defaultLinearLimit bound a list request. Linear's own
	// default page size is 50.
	maxLinearLimit     = 100
	defaultLinearLimit = 25

	// linearProxyDisabledCode / …Message are the fail-closed answer when the
	// operator has not opted in AND the caller's grant carries no team scope of
	// its own. Distinct from a permission denial: nothing the agent can do turns
	// this into a success — only the operator, by writing one of the two lists.
	linearProxyDisabledCode    = "linear_proxy_disabled"
	linearProxyDisabledMessage = "the Linear proxy has no team policy for this unscoped grant: the operator has not set " +
		"agent.linear_proxy.allowed_teams in ~/.tclaude/data/config.json, and an empty allow-list means " +
		"no team is reachable. Ask the operator to allow-list the team, or to scope the grant by team " +
		"(tclaude agent permissions grant <agent> proxy.linear.read --scope linear_team=TCL)."

	// linearTeamOutOfScopeCode is the refusal for a team the OPERATOR allows but
	// this caller's grant does not. Distinct from team_not_allowed so an agent
	// reading the code can tell its human which of the two lists to widen.
	linearTeamOutOfScopeCode = "team_out_of_scope"

	// linearTeamScopeEmptyCode is the refusal when the caller's team scope and
	// the operator's allow-list do not overlap at all, so the grant authorizes
	// nothing however the request is spelled. Reported once, up front, rather
	// than as a per-team refusal on every verb.
	linearTeamScopeEmptyCode = "team_scope_empty"

	// linearMisconfiguredCode is the refusal for an operator policy the daemon
	// cannot act on at all: a workspace entry naming no key or no team, or two
	// entries claiming the same team. Fail-closed rather than best-effort —
	// picking a key for an ambiguously-routed team would answer with the wrong
	// workspace's credential, and Linear reports that as "no such issue" rather
	// than as an error, sending the operator to look at their tracker instead of
	// at their config.
	linearMisconfiguredCode = "linear_proxy_misconfigured"

	// maxLinearFanout bounds how many credentials one TEAM-SPANNING verb may
	// spend. Verbs that name a team, or an issue, spend one whatever it is.
	//
	// A verb that spans teams (`issue ls` with no --team, `whoami`) makes one
	// call per workspace those teams live in, sequentially, inside the single
	// linearProxyBudget every request shares. Past a handful of workspaces that
	// budget is what would fail rather than the policy, and a context-deadline
	// error says nothing about the cause — so the cause is named in
	// fanoutRoutes instead.
	maxLinearFanout = 8

	// defaultLinearRouteName labels the credential every team no
	// agent.linear_proxy.workspaces entry claims is reached with. Reserved: a
	// workspace entry may not take it, since `whoami` reports routes by name.
	defaultLinearRouteName = "default"
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

// linearProxySession is one Linear invocation context: the operator's policy,
// the caller's effective team set, and the credentials that reach it.
type linearProxySession struct {
	policy config.LinearProxyConfig
	// deadline is the wall-clock end of this request's whole budget, shared
	// by every call the verb makes. Zero means "unbounded", which only
	// happens in a unit test that built a session directly.
	deadline time.Time

	// teams is the ONE team gate every check in this package consults: the
	// operator's allow-list narrowed by the caller's grant scope, lower-cased.
	// Never empty on a session that was returned — an empty intersection is a
	// fault, not a session that quietly authorizes nothing.
	teams []string

	// grantTeams is the teams the caller's OWN grant admits, lower-cased, before
	// the operator's ceiling is applied — nil when the grant is unscoped. It is
	// evaluated, not merely enumerated, so it never names a team the scope would
	// refuse (see linearEffectiveTeams). Reported, never gated on: teams is the
	// gate, and this exists so a refusal can say which of the two lists
	// excluded a key.
	grantTeams []string

	// routes is the credentials this caller's effective team set is spread
	// across — one per Linear workspace involved, in the order teams first
	// appear in `teams`. Ordinarily there is exactly one.
	//
	// A route carries the teams it reaches rather than a key, because the key
	// is read lazily: a broken key file for a workspace this request never
	// touches must not fail the request. See routeKey.
	routes []*linearRoute
	// routeByTeam maps every team in `teams` to the route that reaches it, so
	// a per-team verb picks its credential without scanning.
	routeByTeam map[string]*linearRoute
}

// linearRoute is one credential and the part of the caller's effective team
// set it reaches.
type linearRoute struct {
	// name labels the route in diagnostics: the operator's workspace name, or
	// "default" for the key every unclaimed team uses.
	name string
	// keyFile is the file holding this route's key. Empty only on the default
	// route, which then falls back to LINEAR_API_KEY.
	keyFile string
	// isDefault marks the route every team no workspaces entry claims uses. It
	// is what makes the LINEAR_API_KEY fallback positional rather than a
	// property of "any route with no file": one environment variable names one
	// workspace's key, and a second route borrowing it would answer its teams
	// with a credential that cannot see them.
	isDefault bool
	// teams is this route's share of the session's effective team set,
	// lower-cased, in the session's own order.
	teams []string

	// key, loaded and fault memoize routeKey: one read per route per request,
	// whichever verb asks and however many calls it makes.
	key    string
	loaded bool
	fault  *proxyFault
}

// newLinearProxySession runs the operator-policy gates, resolves the caller's
// effective team set, and resolves the key.
//
// Ordering matches the git proxy's: the fail-closed policy check comes before
// anything that could touch the network, so a caller holding proxy.linear.read
// against an unconfigured daemon gets "not configured" rather than an
// authentication error from Linear.
//
// perm is the slug the calling verb gates on, and teamScoped says the
// permission preflight deferred its decision because that grant is team-scoped
// (see preflightProxyPermission). Both are needed here rather than at the gate,
// because the scope has to be resolved into a SET before any verb runs — see the
// file header.
func newLinearProxySession(
	r *http.Request, convID, perm string, teamScoped bool,
) (*linearProxySession, *proxyFault) {
	cfg, err := config.Load()
	if err != nil {
		return nil, faultf(http.StatusInternalServerError, "config",
			"could not read the daemon configuration: %v", err)
	}
	policy := cfg.ResolvedLinearProxy()
	teams, grantTeams, fault := linearEffectiveTeams(r, convID, perm, policy, teamScoped)
	if fault != nil {
		return nil, fault
	}
	routes, byTeam, fault := linearRoutes(policy, teams)
	if fault != nil {
		return nil, fault
	}
	return &linearProxySession{
		policy:      policy,
		deadline:    time.Now().Add(linearProxyBudget),
		teams:       teams,
		grantTeams:  grantTeams,
		routes:      routes,
		routeByTeam: byTeam,
	}, nil
}

// linearEffectiveTeams resolves the team set one request may act within, from
// the operator's allow-list and the caller's grant scope.
//
// The two lists are enforced TOGETHER when both exist, and either alone is
// enough to make the proxy usable:
//
//   - unscoped grant, operator list  → the operator's list (the historical case,
//     unchanged);
//   - scoped grant, operator list    → the intersection, so an operator can
//     narrow one agent without touching the global list, and a grant can never
//     widen past it;
//   - scoped grant, no operator list → the grant's own teams, mirroring the git
//     proxy's remote-scoped grant with an empty allowed_remotes;
//   - unscoped grant, no operator list → nothing is reachable: the fail-closed
//     linear_proxy_disabled answer.
//
// It resolves in TWO steps, and the split is what makes both answers precise.
//
// First, which teams the SCOPE admits, on its own merits: every team the scope
// names, put through the real evaluator. That is not the same as the names
// themselves. permissionScopeEnumerate unions the matchers across the winning
// tier's rows, so a tier holding {linear_team: TCL} beside {linear_team: JOH,
// group: dev} NAMES both while admitting only TCL — and reporting JOH as
// something the grant covers would over-claim in a refusal message and in the
// audit row. Only evalPermissionScope decides, which is also what keeps a scope
// carrying a second dimension ANDing rather than collapsing to its team term.
//
// Second, the operator's ceiling intersected over that — or, with no operator
// list at all, the scope's own answer standing as the whole policy.
//
// Separating the steps also puts the two ways an empty result arises on two
// different paths, so each gets the refusal that names what the operator would
// actually have to change.
func linearEffectiveTeams(
	r *http.Request, convID, perm string, policy config.LinearProxyConfig, teamScoped bool,
) (teams, grantTeams []string, fault *proxyFault) {
	if !teamScoped {
		if len(policy.AllowedTeams) == 0 {
			return nil, nil, &proxyFault{
				Status: http.StatusServiceUnavailable,
				Code:   linearProxyDisabledCode,
				Msg:    linearProxyDisabledMessage,
			}
		}
		return policy.AllowedTeams, nil, nil
	}
	// A SECOND resolution, because the preflight's verdict does not travel here.
	// It re-reads the same request-scoped defaults, so in the ordinary case this
	// is the same verdict the preflight saw.
	v := resolvePermissionVerdictForRequest(r, convID, perm)
	if v.Resolution != permAllow {
		// The grant changed between the two reads. Name THAT rather than blaming
		// the shape of a scope that may no longer exist — and refuse, rather than
		// falling back on the operator's list.
		return nil, nil, faultf(http.StatusForbidden, "permission",
			"the %s grant was withdrawn while this request was being authorized", perm)
	}
	named, ok := permissionScopeEnumerate(v, ScopeDimLinearTeam)
	if !ok || len(named) == 0 {
		// The preflight established a scoped allow, so reaching here means this
		// build cannot decode a scope row, or the scope constrains only
		// dimensions a Linear request does not describe. Either way the grant
		// does not speak to any team.
		return nil, nil, faultf(http.StatusForbidden, linearTeamScopeEmptyCode,
			"the %s grant is scoped, but names no Linear team this daemon can act on; "+
				"the scope must carry linear_team (e.g. --scope linear_team=TCL)", perm)
	}
	for _, key := range named {
		if evalPermissionScope(v, convID, ActionContext{LinearTeam: key}).Satisfied {
			grantTeams = appendTeamKey(grantTeams, key)
		}
	}
	if len(grantTeams) == 0 {
		// The scope names teams but admits none of them, so something else in the
		// same scope refused. The operator's list is not the thing to edit.
		return nil, nil, faultf(http.StatusForbidden, linearTeamScopeEmptyCode,
			"the %s grant names team(s) %s, but its scope also constrains a dimension a Linear "+
				"request does not describe, so it authorizes nothing; the scope must constrain "+
				"linear_team alone", perm, strings.Join(lowerTeamKeys(named), ", "))
	}
	if len(policy.AllowedTeams) == 0 {
		// No operator list: the scope is the whole policy, exactly as a
		// remote-scoped git grant is against an empty allowed_remotes.
		return grantTeams, grantTeams, nil
	}
	for _, key := range grantTeams {
		if policy.LinearTeamAllowed(key) {
			teams = appendTeamKey(teams, key)
		}
	}
	if len(teams) == 0 {
		return nil, grantTeams, faultf(http.StatusForbidden, linearTeamScopeEmptyCode,
			"the %s grant is scoped to team(s) %s, none of which is on the operator's "+
				"agent.linear_proxy.allowed_teams list (allowed: %s); the two must overlap",
			perm, strings.Join(grantTeams, ", "), strings.Join(policy.AllowedTeams, ", "))
	}
	return teams, grantTeams, nil
}

// lowerTeamKeys normalizes a team-key list the way the operator's allow-list is
// normalized, so the two are directly comparable and render alike.
func lowerTeamKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = appendTeamKey(out, key)
	}
	return out
}

// appendTeamKey adds one normalized team key to a list, dropping blanks and
// duplicates. Order is preserved: these lists are rendered to humans in
// refusals and in `whoami`, so they keep the order the operator wrote.
func appendTeamKey(out []string, key string) []string {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || slices.Contains(out, key) {
		return out
	}
	return append(out, key)
}

// linearRoutes spreads the caller's effective team set across the credentials
// that reach it, and is the whole of the multi-key mechanism.
//
// It exists because a Linear personal API key is scoped to ONE workspace — the
// one its creator was logged into. Within that workspace the key reaches every
// team the account can see, whoever created them, so the ordinary operator
// needs exactly one key and gets exactly one route here. Teams in a separate
// workspace are invisible to that key, and no permission on it can change
// that, so covering them takes a second key and a
// agent.linear_proxy.workspaces entry saying which teams it is for.
//
// Two properties matter more than the mapping itself:
//
//   - Routing is not authorization. Every team routed here has already passed
//     linearEffectiveTeams; a workspace entry can only decide WHICH KEY reaches
//     a team the caller could already reach, never whether it may. That is why
//     this runs after the team set is resolved rather than alongside it.
//
//   - The mapping is total and unambiguous or the request does not run. A team
//     no entry claims goes to the default key, which is the pre-workspaces
//     behaviour; a team TWO entries claim is a refusal, because guessing
//     between them produces a "no such issue" from the wrong workspace rather
//     than an error.
//
// The whole policy is validated on every request, including entries this
// caller's teams never touch. A misconfigured entry is then reported by the
// first call anyone makes rather than lying in wait for the one agent whose
// team it routes.
func linearRoutes(
	policy config.LinearProxyConfig, teams []string,
) ([]*linearRoute, map[string]*linearRoute, *proxyFault) {
	claimed := make(map[string]*linearRoute)
	for i, ws := range policy.Workspaces {
		name := ws.Name
		if name == "" {
			name = fmt.Sprintf("workspace %d", i+1)
		}
		if ws.APIKeyFile == "" {
			return nil, nil, faultf(http.StatusServiceUnavailable, linearMisconfiguredCode,
				"agent.linear_proxy.workspaces[%d] (%s) has no api_key_file; an extra workspace is only "+
					"reachable with a key created inside it, and there is no environment fallback for one",
				i, name)
		}
		if len(ws.Teams) == 0 {
			return nil, nil, faultf(http.StatusServiceUnavailable, linearMisconfiguredCode,
				"agent.linear_proxy.workspaces[%d] (%s) lists no teams, so it names a key nothing would "+
					"ever use", i, name)
		}
		if strings.EqualFold(name, defaultLinearRouteName) {
			// Reserved, because `whoami` reports routes by name and the key every
			// unclaimed team uses is already called this. Two rows called
			// "default" in the verb an operator runs to work out which key
			// answered is worse than making them pick another label.
			return nil, nil, faultf(http.StatusServiceUnavailable, linearMisconfiguredCode,
				"agent.linear_proxy.workspaces[%d] is named %q, which names the key every team no entry "+
					"claims already uses; give this workspace a different name", i, name)
		}
		rt := &linearRoute{name: name, keyFile: ws.APIKeyFile}
		for _, key := range ws.Teams {
			if prev, dup := claimed[key]; dup {
				return nil, nil, faultf(http.StatusServiceUnavailable, linearMisconfiguredCode,
					"team %q is claimed by two agent.linear_proxy.workspaces entries (%s and %s); one team "+
						"key can be routed to one workspace, so exactly one entry may name it",
					key, prev.name, name)
			}
			claimed[key] = rt
		}
	}

	var (
		ordered  []*linearRoute
		byTeam   = make(map[string]*linearRoute, len(teams))
		fallback *linearRoute
	)
	for _, key := range teams {
		rt, ok := claimed[key]
		if !ok {
			if fallback == nil {
				fallback = &linearRoute{
					name: defaultLinearRouteName, keyFile: policy.APIKeyFile, isDefault: true,
				}
			}
			rt = fallback
		}
		if len(rt.teams) == 0 {
			ordered = append(ordered, rt)
		}
		rt.teams = append(rt.teams, key)
		byTeam[key] = rt
	}
	return ordered, byTeam, nil
}

// fanoutRoutes is every credential a team-spanning verb has to spend, bounded.
//
// The bound lives here rather than on the session because it is a bound on the
// FAN-OUT, and most verbs do not fan out: `issue view TCL-1` spends one
// credential whether the operator configured two workspaces or twenty, and
// refusing it for the shape of a listing it is not performing would be a
// restriction with no cause behind it.
func (s *linearProxySession) fanoutRoutes() ([]*linearRoute, *proxyFault) {
	if len(s.routes) > maxLinearFanout {
		return nil, faultf(http.StatusServiceUnavailable, linearMisconfiguredCode,
			"this caller's teams are spread across %d Linear workspaces, and a verb that spans teams "+
				"spends one credential per workspace; %d is the most one request may spend — name a team, "+
				"or narrow the caller's teams with a linear_team grant scope",
			len(s.routes), maxLinearFanout)
	}
	return s.routes, nil
}

// routeFor returns the credential that reaches one team. The team has already
// passed the gate, so a miss is a programming error rather than a refusal —
// reported as one, so a verb that forgets to gate first cannot silently borrow
// the default key.
func (s *linearProxySession) routeFor(teamKey string) (*linearRoute, *proxyFault) {
	if rt, ok := s.routeByTeam[strings.ToLower(strings.TrimSpace(teamKey))]; ok {
		return rt, nil
	}
	return nil, faultf(http.StatusInternalServerError, "team_unresolved",
		"no Linear credential is routed to team %q; refusing to guess one", teamKey)
}

// routeKey resolves one route's Linear personal API key: the configured file,
// or — on the default route only — LINEAR_API_KEY from the daemon's own
// environment.
//
// Keys are deliberately not stored in config.json — that file is plaintext,
// shows up in the dashboard's Config tab, and is the sort of thing an operator
// copies into a bug report.
//
// The result is memoized on the route, so a verb making several calls reads
// each file once, and a route the verb never uses is never read at all.
func (s *linearProxySession) routeKey(rt *linearRoute) (string, *proxyFault) {
	if rt.loaded {
		return rt.key, rt.fault
	}
	rt.loaded = true
	rt.key, rt.fault = resolveLinearRouteKey(rt)
	return rt.key, rt.fault
}

// resolveLinearRouteKey is routeKey without the memoization.
func resolveLinearRouteKey(rt *linearRoute) (string, *proxyFault) {
	// Which setting to name in a refusal, so an operator is sent to the line
	// they actually have to fix.
	field := "agent.linear_proxy.api_key_file"
	if !rt.isDefault {
		field = fmt.Sprintf("the api_key_file of agent.linear_proxy.workspaces %q", rt.name)
	}
	if rt.keyFile != "" {
		// "~/linear-key.txt" is how an operator naturally writes this, and the
		// same expandTilde every other human-typed path in the daemon goes
		// through applies here.
		raw, err := os.ReadFile(expandTilde(rt.keyFile))
		if err != nil {
			return "", faultf(http.StatusServiceUnavailable, "key_unreadable",
				"%s could not be read: %v%s", field, err, shellVarHint(rt.keyFile))
		}
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", faultf(http.StatusServiceUnavailable, "key_unreadable", "%s is empty", field)
		}
		return key, nil
	}
	// The environment fallback belongs to the default route alone: one
	// LINEAR_API_KEY names one workspace's key, so letting a second route
	// borrow it would answer that route's teams with the wrong workspace's
	// credential — and Linear reports that as a missing issue rather than as an
	// error. linearRoutes already refuses a workspace entry with no key file;
	// this is the same rule where the key is actually chosen, so the two would
	// have to be wrong together for the environment key to reach a workspace it
	// does not belong to.
	if !rt.isDefault {
		return "", faultf(http.StatusServiceUnavailable, linearMisconfiguredCode,
			"the agent.linear_proxy.workspaces entry %q names no api_key_file, and LINEAR_API_KEY "+
				"belongs to the default workspace rather than to this one", rt.name)
	}
	if key := strings.TrimSpace(os.Getenv("LINEAR_API_KEY")); key != "" {
		return key, nil
	}
	return "", faultf(http.StatusServiceUnavailable, "key_missing",
		"no Linear API key is configured for team(s) %s: set agent.linear_proxy.api_key_file, or put "+
			"LINEAR_API_KEY in the environment agentd runs under",
		strings.Join(rt.teams, ", "))
}

// requireMutationBudget refuses a write the request no longer has time to
// finish, BEFORE it is sent.
//
// It is called at the point a verb has done all its reading and is about to
// mutate. See linearMutationHeadroom for why a clean refusal beats a write cut
// off mid-flight.
func (s *linearProxySession) requireMutationBudget() *proxyFault {
	if s.deadline.IsZero() || time.Until(s.deadline) >= linearMutationHeadroom {
		return nil
	}
	return faultf(http.StatusGatewayTimeout, "linear_budget_spent",
		"this request spent its %s budget reading Linear before it could write; nothing was "+
			"written, so it is safe to retry", linearProxyBudget)
}

// requireWrite gates the mutating verbs on the operator's own ceiling. The
// proxy.linear.write slug says THIS AGENT may write; allow_write says the operator
// wants any agent to be able to. Both must hold.
func (s *linearProxySession) requireWrite() *proxyFault {
	if s.policy.AllowWrite {
		return nil
	}
	return faultf(http.StatusForbidden, "linear_write_disabled",
		"the operator has not enabled writes: set agent.linear_proxy.allow_write to true in "+
			"~/.tclaude/data/config.json")
}

// exec runs one GraphQL operation against one route's credential and
// unmarshals `data` into out.
//
// doc must be one of the package-level document constants. vars carries every
// caller-supplied value. Nothing in this function inspects either — which is
// exactly why every caller builds vars from values that have passed a
// validateLinear* gate.
//
// The route is a parameter rather than session state because it is the second
// half of the team gate's answer: having decided that a team may be reached,
// the caller must also spend the credential that can reach IT and no other. A
// verb that picked the wrong route would query a workspace where the team does
// not exist, and Linear reports that as an empty result rather than an error.
func (s *linearProxySession) exec(
	ctx context.Context, rt *linearRoute, doc string, vars map[string]any, out any,
) *proxyFault {
	key, fault := s.routeKey(rt)
	if fault != nil {
		return fault
	}
	// The tighter of the per-call bound and what is left of the whole
	// request's budget. The budget is what keeps a multi-call verb inside the
	// window the CLI is waiting on; the per-call bound is what stops any one
	// hung call consuming all of it.
	runCtx, cancel := s.callContext(ctx)
	defer cancel()

	res, err := linearDo(runCtx, key, linearRequest{Query: doc, Variables: vars})
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

// callContext derives the context for one call: the per-call bound, clamped to
// whatever remains of the request's shared budget.
func (s *linearProxySession) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.deadline.IsZero() {
		return context.WithTimeout(ctx, linearProxyTimeout)
	}
	perCall := time.Now().Add(linearProxyTimeout)
	if s.deadline.Before(perCall) {
		return context.WithDeadline(ctx, s.deadline)
	}
	return context.WithDeadline(ctx, perCall)
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
	// A missing issue must not read as a daemon failure. Query.issue is
	// declared `Issue!` in Linear's schema, so a bad identifier cannot come
	// back as `data.issue: null` — it comes back as an ERROR, which would
	// otherwise be classified as linear_failed. That matters because 502 is
	// documented as "a tclaude bug, do not retry" or "could not reach Linear",
	// and neither is true of a typo'd issue number.
	//
	// Matched on the message text because Linear does not give this case a
	// distinct extensions.code. A heuristic, deliberately: if Linear rewords
	// it the classification degrades to the generic fault below, which is
	// where it is today — no worse than before, and right in the common case.
	if isLinearEntityNotFound(joined) {
		return faultf(http.StatusNotFound, "not_found", "%s", joined)
	}
	return faultf(http.StatusBadGateway, "linear_failed", "%s", joined)
}

// isLinearEntityNotFound reports whether Linear's error text is its
// "no such record" answer. See the caller for why this is matched on text.
func isLinearEntityNotFound(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "entity not found") ||
		strings.Contains(lower, "could not find referenced")
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

// teamAllowed reports whether key is in this caller's effective team set.
//
// What keeps the identifier gate, the listing filter and the row-level drop from
// diverging is that all three read s.teams — this predicate for the two
// per-team checks, linearTeamClause reading the field directly because it needs
// the whole set. One field, resolved once, is the invariant; a single reader is
// not.
func (s *linearProxySession) teamAllowed(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key != "" && slices.Contains(s.teams, key)
}

// requireAllowedTeam refuses a team key outside the caller's effective set.
//
// The message names the list that ACTUALLY excluded the key — the operator's
// allow-list or the caller's own grant scope — so an agent can tell its human
// exactly which one to widen rather than guessing from a refusal. The codes are
// distinct for the same reason.
func (s *linearProxySession) requireAllowedTeam(key string) *proxyFault {
	if s.teamAllowed(key) {
		return nil
	}
	if len(s.policy.AllowedTeams) > 0 && !s.policy.LinearTeamAllowed(key) {
		return faultf(http.StatusForbidden, "team_not_allowed",
			"team %q is not on the operator's agent.linear_proxy.allowed_teams list (allowed: %s)",
			key, strings.Join(s.policy.AllowedTeams, ", "))
	}
	return faultf(http.StatusForbidden, linearTeamOutOfScopeCode,
		"team %q is outside this caller's Linear team scope (this grant covers: %s)",
		key, strings.Join(s.grantTeams, ", "))
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
		"the Linear response carried no team; refusing to return an issue the team gate could not "+
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
	if err := linearTeamKeyShapeErr(key); err != nil {
		return faultf(http.StatusBadRequest, "invalid_arg", "%s", err.Error())
	}
	return nil
}

// linearTeamKeyShapeErr is the shape rule itself, as a plain error.
//
// It exists apart from validateLinearTeamKeyShape so the permission-scope parser
// can apply the SAME rule to a `linear_team` matcher without depending on the
// proxy's fault type. One rule, two callers: a matcher an operator can write must
// be a team key this proxy could also accept as a parameter, or a scope could
// name something no request can ever match — which reads as a narrow grant and
// silently authorizes nothing.
func linearTeamKeyShapeErr(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("a team key is required")
	}
	if len(key) > maxLinearTeamKeyLen {
		return fmt.Errorf("team key %q is longer than %d characters", key, maxLinearTeamKeyLen)
	}
	for _, r := range key {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return fmt.Errorf("team key %q contains a character that is not a letter or a digit", key)
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

// validateLinearStateName bounds a workflow-state name used as a FILTER.
//
// Distinct from validateLinearBody, which the list handler used to borrow: a
// state name is a short label, not prose, and giving it the body validator's
// 256 KiB-and-any-character latitude was a slip rather than a decision. It
// only ever reaches a GraphQL variable, so nothing was exploitable — but a
// bound that does not describe what it is bounding invites the next caller to
// reuse it somewhere that is.
func validateLinearStateName(name string) (string, *proxyFault) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if utf8.RuneCountInString(name) > maxLinearStateNameLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"a workflow-state name longer than %d characters is not one", maxLinearStateNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the workflow-state name contains a control character")
		}
	}
	return name, nil
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
	// Teams is the caller's EFFECTIVE team set — the operator's allow-list
	// narrowed by this caller's grant scope — echoed on every response. It is
	// the single most common thing an agent needs when a call is refused, and
	// carrying it means the agent does not have to run `whoami` to find out.
	// Deliberately the effective set rather than the operator's raw list: an
	// agent that cannot reach a team is not helped by being told someone else
	// could.
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
	out := linearProxyOutcome{Teams: s.teams}
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
