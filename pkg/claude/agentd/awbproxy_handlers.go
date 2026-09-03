package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// awbproxy_handlers.go is the HTTP surface of `tclaude proxy awb`.
//
// Every route is a POST, including the reads, for the same reason the Linear
// proxy's reads are: an AWB read SPENDS THE OPERATOR'S ACCOUNT against a
// private tracker, and the audit middleware records mutating methods only.
// Making the reads POSTs is what puts "this agent read the backlog as me" in
// the operator's audit trail beside "this agent closed an issue as me".
//
// The gates run in a fixed order, and the order matters:
//
//  1. permission slug — before the body is read, so an ungated caller cannot
//     probe for the existence of a workspace;
//  2. operator policy and grant scope, resolved together into this caller's
//     effective workspace set — the proxy is off unless SOME workspace is
//     reachable, and writes are off unless allow_write is set;
//  3. parameter validation — issue-reference shape, charset, length, and every
//     value AWB's fixed vocabulary bounds;
//  4. the workspace gate on the caller's issue reference or --workspace;
//  5. the call;
//  6. the workspace gate AGAIN, on the workspace AWB reported.
//
// Step 6 is not belt-and-braces. Step 4 checks a string the caller supplied;
// step 6 checks the thing actually reached — and an AWB reference may be a
// PREFIX, so the two are not the same statement.
//
// Steps 4 and 6 read the same effective set, and so do the listing verbs'
// filter and row-level drop, so no combination of operator list and grant scope
// can be enforced one way in one verb and another way in the next.

// ---------------------------------------------------------------------------
// Request shapes
// ---------------------------------------------------------------------------

// awbCompactRequest is the output mode every verb but `attach get` carries.
// Embedded rather than repeated so no verb can forget it, and so `--json` and
// `--compact` mean the same thing everywhere.
type awbCompactRequest struct {
	Compact bool `json:"compact,omitempty"`
}

// awbIssueRefRequest is the shape of every verb addressed by one issue.
type awbIssueRefRequest struct {
	awbCompactRequest
	ID string `json:"id"`
}

// awbFilterRequest is the filter set the four listing verbs share, mirroring
// awb's own FilterFlags. Which of them a given verb accepts is not uniform —
// `ready` fixes the status set and the assignee filter for itself, `blocked`
// fixes the status set — so the fields a verb rejects are refused rather than
// ignored. See awbFilterOptions.
type awbFilterRequest struct {
	awbCompactRequest
	Statuses       []string `json:"statuses,omitempty"`
	IncludeClosed  bool     `json:"include_closed,omitempty"`
	Types          []string `json:"types,omitempty"`
	Priorities     []int    `json:"priorities,omitempty"`
	PriorityMax    *int     `json:"priority_max,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	Assignees      []string `json:"assignees,omitempty"`
	Mine           bool     `json:"mine,omitempty"`
	Unassigned     bool     `json:"unassigned,omitempty"`
	Workspaces     []string `json:"workspaces,omitempty"`
	LegacyProjects []string `json:"projects,omitempty"`
	Parent         string   `json:"parent,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	Sort           string   `json:"sort,omitempty"`
	// Terms is search's own, and empty on the other three.
	Terms []string `json:"terms,omitempty"`
}

// awbFilterOptions says which filters one verb accepts, exactly as awb's own
// filterOptions does. A filter a verb rejects is a refusal rather than a
// silently dropped field: the caller asked for something narrower than it will
// get, and answering with the wider set would be the wrong answer given
// confidently.
type awbFilterOptions struct {
	// status is false for ready and blocked, which fix the status set.
	status bool
	// assignee is false for ready, which fixes the assignee filter to
	// unassigned: "what should nobody-in-particular pick up next" is the
	// question it exists to answer.
	assignee bool
	// relevance is true only for search, which has two more sort values.
	relevance bool
}

type awbCreateRequest struct {
	awbCompactRequest
	Workspace      string   `json:"workspace"`
	Title          string   `json:"title"`
	Description    *string  `json:"description,omitempty"`
	CommitHash     string   `json:"commit_hash,omitempty"`
	PullRequestURL string   `json:"pull_request_url,omitempty"`
	Type           string   `json:"type,omitempty"`
	Priority       *int     `json:"priority,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	Assignees      []string `json:"assignees,omitempty"`
	HasParent      string   `json:"has_parent,omitempty"`
	BlockedBy      []string `json:"blocked_by,omitempty"`
	DiscoveredFrom []string `json:"discovered_from,omitempty"`
	Related        []string `json:"related,omitempty"`
}

type awbUpdateRequest struct {
	awbIssueRefRequest
	// Every field is a pointer because omitting it means "leave it alone", and
	// awb update's whole contract is that it changes only what was named. An
	// empty description is a real state ("this issue has no body"), so absent
	// and "clear it" have to be told apart.
	Title          *string `json:"title,omitempty"`
	Description    *string `json:"description,omitempty"`
	CommitHash     *string `json:"commit_hash,omitempty"`
	PullRequestURL *string `json:"pull_request_url,omitempty"`
	Type           *string `json:"type,omitempty"`
	Priority       *int    `json:"priority,omitempty"`
}

type awbClaimRequest struct {
	awbIssueRefRequest
	// As is a removed-field tombstone. Keeping it only in the daemon's private
	// wire type lets an older client receive a clear refusal instead of being
	// told its requested identity was accepted while silently ignoring it.
	As    json.RawMessage `json:"as,omitempty"`
	Force bool            `json:"force,omitempty"`
}

type awbForceRequest struct {
	awbIssueRefRequest
	Force bool `json:"force,omitempty"`
}

type awbCloseRequest struct {
	awbIssueRefRequest
	// Reason is a pointer only to control whether the member is SENT. Since AWB
	// 0.6 there is no reason field to clear: a non-empty reason becomes a typed
	// comment on the closing transition, and empty and absent alike record no
	// comment. The pointer stays because "the caller typed --reason ''" is still
	// worth transmitting faithfully rather than silently dropping.
	Reason *string `json:"reason,omitempty"`
}

type awbLabelRequest struct {
	awbIssueRefRequest
	Label string `json:"label"`
}

type awbRelationRequest struct {
	awbIssueRefRequest
	Type  string `json:"type"`
	Other string `json:"other"`
	Force bool   `json:"force,omitempty"`
}

type awbCommentAddRequest struct {
	awbIssueRefRequest
	Body string `json:"body"`
}

type awbCommentListRequest struct {
	awbIssueRefRequest
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

// awbActivityListRequest is `activity`: the whole timeline, optionally narrowed
// to one kind. `comment list` is the same read with the kind fixed, which is
// why the two share a tail rather than a route — an audit row should say which
// question was asked.
type awbActivityListRequest struct {
	awbIssueRefRequest
	Kind   string `json:"kind,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

type awbAttachAddRequest struct {
	awbIssueRefRequest
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Content     []byte `json:"content"`
}

type awbAttachNameRequest struct {
	awbIssueRefRequest
	Name  string `json:"name"`
	Force bool   `json:"force,omitempty"`
}

// ---------------------------------------------------------------------------
// The shared prologue
// ---------------------------------------------------------------------------

// openAWBProxy is the shared prologue: method check, permission gate, bounded
// body decode, workspace-set resolution. Write verbs additionally clear
// allow_write here, so no handler can forget it.
//
// The permission gate goes through preflightProxyPermission, the same helper
// the git and Linear proxies use, for the same reason: an ungranted caller must
// still be refused cheaply, while a WORKSPACE-SCOPED grant cannot be decided
// against an empty ActionContext and would otherwise fall through to an
// ask-human popup on every single call. The scope is then resolved into the
// session's effective workspace set — the verbs here can span several workspaces at
// once, so a set is the only form the decision can take.
func openAWBProxy(w http.ResponseWriter, r *http.Request, perm string, body any) (*awbProxySession, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return nil, false
	}
	convID, workspaceScoped, ok := preflightProxyPermission(w, r, perm)
	if !ok {
		return nil, false
	}
	if body != nil && !decodeAWBProxyBody(w, r, body) {
		return nil, false
	}
	s, fault := newAWBProxySession(r, convID, perm, workspaceScoped)
	if fault != nil {
		writeProxyFault(w, fault)
		return nil, false
	}
	if workspaceScoped {
		// The scoped grant really did decide this request, so the audit row
		// must say so. It records the GRANT's workspaces, not the effective set,
		// so the field means what it means on a git or Linear row: the scope on
		// the grant that authorized the action, rather than that scope narrowed
		// by an operator setting the row does not otherwise mention. The
		// workspace actually acted on is already on the same row, in the verb's
		// own detail.
		//
		// grantWorkspaces is the EVALUATED set, which is what makes this honest:
		// the workspaces the scope merely names can be a superset of the ones it
		// admits, and recording those would claim authority the grant never
		// conferred.
		recordAuditPermissionScope(r, perm, permissionScopeDisplay(
			PermissionScope{ScopeDimAWBWorkspace: s.grantWorkspaces}))
	}
	if perm == PermAWBWrite {
		if fault := s.requireWrite(); fault != nil {
			writeProxyFault(w, fault)
			return nil, false
		}
	}
	return s, true
}

// decodeAWBProxyBody reads a bounded JSON body.
//
// The bound is maxAWBRequestBytes rather than the git proxy's 16 KiB because
// this surface carries an attachment's bytes and an issue's markdown, not "a
// handful of short scalars". Behind a smaller reader a legitimate `attach add`
// would die with "http: request body too large" before validateAWBContent could
// name the real limit or the offending field.
func decodeAWBProxyBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAWBRequestBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return false
	}
	return true
}

// gateIssueRef is the pre-call half of the workspace gate for an
// identifier-shaped verb: it validates the reference's shape and checks the
// workspace it carries against the effective set.
//
// The workspace is checked BEFORE the call rather than after it, which is the
// whole reason validateAWBIssueRef refuses a bare hash: a reference that names
// no workspace could only be judged once the issue had already been fetched.
func (s *awbProxySession) gateIssueRef(raw string) (string, *proxyFault) {
	ref, fault := validateAWBIssueRef(raw)
	if fault != nil {
		return "", fault
	}
	if fault := s.requireAllowedWorkspace(workspaceKeyOf(ref)); fault != nil {
		return "", fault
	}
	return ref, nil
}

// identity is the AWB user the daemon authenticates as — the one identity an
// agent has here, and what `claim` records and `--mine` filters on.
//
// A server holding no users authenticates nothing, and then there is no
// identity to record. That is a legitimate AWB configuration and an illegitimate
// claim, so it is refused with the reason rather than with an empty assignee
// AWB would reject less clearly.
func (s *awbProxySession) identity(what string) (string, *proxyFault) {
	if s.policy.Username == "" {
		return "", faultf(http.StatusBadRequest, "awb_no_identity",
			"%s needs an identity, and the operator configured no agent.awb_proxy.username "+
				"(the AWB server it points at authenticates nobody)", what)
	}
	return s.policy.Username, nil
}

// ---------------------------------------------------------------------------
// Listing filters
// ---------------------------------------------------------------------------

// awbListingQuery turns a validated filter into the query string one listing
// call carries, and is the ONLY place a workspace constraint is constructed.
//
// Every list-shaped verb goes through it, so an unfiltered listing is not
// something a handler can produce by omission: the `workspace` parameter is
// always written, from a set the gate has already approved.
func (s *awbProxySession) awbListingQuery(
	ctx context.Context, f *awbFilterRequest, opts awbFilterOptions,
) (url.Values, []string, *proxyFault) {
	q := url.Values{}

	// Everything that can be decided from the request alone comes first, and
	// the workspace set — the only step that may have to ask the server — comes
	// last. A filter this verb does not accept is then a refusal made without
	// spending the operator's account, which is what "the cheap gate runs
	// before the network" has to mean here.
	if opts.status {
		for _, raw := range f.Statuses {
			status, fault := validateAWBStatus(raw)
			if fault != nil {
				return nil, nil, fault
			}
			if status != "" {
				q.Add("status", status)
			}
		}
		if f.IncludeClosed {
			q.Set("include-closed", "true")
		}
	} else if len(f.Statuses) > 0 || f.IncludeClosed {
		return nil, nil, faultf(http.StatusBadRequest, "invalid_arg",
			"this listing fixes the status set for itself, so it takes no status filter")
	}

	if fault := s.applyAssigneeFilter(q, f, opts); fault != nil {
		return nil, nil, fault
	}

	for _, raw := range f.Types {
		t, fault := validateAWBType(raw)
		if fault != nil {
			return nil, nil, fault
		}
		if t != "" {
			q.Add("type", t)
		}
	}
	for _, p := range f.Priorities {
		if fault := validateAWBPriority(p); fault != nil {
			return nil, nil, fault
		}
		q.Add("priority", strconv.Itoa(p))
	}
	if f.PriorityMax != nil {
		if fault := validateAWBPriority(*f.PriorityMax); fault != nil {
			return nil, nil, fault
		}
		q.Set("priority-max", strconv.Itoa(*f.PriorityMax))
	}
	for _, raw := range f.Labels {
		label, fault := validateAWBLabel(raw)
		if fault != nil {
			return nil, nil, fault
		}
		q.Add("label", label)
	}
	if strings.TrimSpace(f.Parent) != "" {
		// The parent is an issue, so it goes through the same gate every other
		// issue reference does: selecting the children of an issue in a workspace
		// this caller cannot reach would answer a question it may not ask.
		parent, fault := s.gateIssueRef(f.Parent)
		if fault != nil {
			return nil, nil, fault
		}
		q.Set("parent", parent)
	}
	sort, fault := validateAWBSort(f.Sort, opts.relevance)
	if fault != nil {
		return nil, nil, fault
	}
	if sort != "" {
		q.Set("sort", sort)
	}
	limit, fault := validateAWBLimit(f.Limit)
	if fault != nil {
		return nil, nil, fault
	}
	q.Set("limit", strconv.Itoa(limit))

	requested := append(append([]string{}, f.Workspaces...), f.LegacyProjects...)
	named := make([]string, 0, len(requested))
	for _, raw := range requested {
		key, fault := s.validateAWBWorkspace(raw)
		if fault != nil {
			return nil, nil, fault
		}
		named = appendWorkspaceKey(named, key)
	}
	workspaces, fault := s.listingWorkspaces(ctx, named)
	if fault != nil {
		return nil, nil, fault
	}
	for _, key := range workspaces {
		q.Add("workspace", key)
	}
	return q, workspaces, nil
}

// applyAssigneeFilter writes the mutually exclusive assignee filters.
//
// The three are exclusive in awb and stay exclusive here, because they are
// three different questions and a request that asks two of them has not decided
// which answer it wants.
func (s *awbProxySession) applyAssigneeFilter(
	q url.Values, f *awbFilterRequest, opts awbFilterOptions,
) *proxyFault {
	if !opts.assignee {
		if len(f.Assignees) > 0 || f.Mine || f.Unassigned {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"this listing answers \"what should nobody-in-particular pick up next\", so it "+
					"fixes the assignee filter to unassigned and takes none of its own")
		}
		return nil
	}
	given := 0
	if len(f.Assignees) > 0 {
		given++
	}
	if f.Mine {
		given++
	}
	if f.Unassigned {
		given++
	}
	if given > 1 {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"mine, assignee and unassigned are mutually exclusive")
	}
	switch {
	case f.Mine:
		// The OPERATOR's AWB user: the daemon holds the account, and agents
		// have no AWB identity of their own.
		identity, fault := s.identity("--mine")
		if fault != nil {
			return fault
		}
		q.Add("assignee", identity)
	case f.Unassigned:
		q.Set("unassigned", "true")
	default:
		for _, raw := range f.Assignees {
			assignee, fault := validateAWBAssignee(raw)
			if fault != nil {
				return fault
			}
			q.Add("assignee", assignee)
		}
	}
	return nil
}

// runAWBListing is the shared tail of list, ready, blocked and search: build
// the query, make the call, apply the row-level gate, render.
func (s *awbProxySession) runAWBListing(
	w http.ResponseWriter, r *http.Request, verb, path string,
	f *awbFilterRequest, opts awbFilterOptions, withBlockers bool,
) {
	// The terms are validated BEFORE the query is built, because building it can
	// resolve the server's workspace list — one call on the operator's account.
	// A malformed term must not spend that call to reach its refusal, which is
	// the same ordering rule the filter checks in awbListingQuery follow.
	var terms []string
	if opts.relevance {
		var fault *proxyFault
		if terms, fault = validateAWBSearchTerms(f.Terms); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	} else if len(trimmedNonEmpty(f.Terms)) > 0 {
		// Refused rather than dropped, for the reason the rejected status and
		// assignee filters are: a caller that asked to NARROW a listing and got
		// the wide one back has been answered confidently and wrongly. Only
		// `search` matches text, and the other three have no way to.
		writeProxyFault(w, faultf(http.StatusBadRequest, "invalid_arg",
			"this listing does not match text; `search` is the verb that takes terms"))
		return
	}
	q, workspaces, fault := s.awbListingQuery(r.Context(), f, opts)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	for _, term := range terms {
		q.Add("q", term)
	}
	// Non-nil even when nothing matched: an empty listing has to render as `[]`
	// rather than as `null`, or a consumer's "did I get rows" test would depend
	// on which of two empties it received.
	issues := []awbIssue{}
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet, Path: path, Query: q,
	}, &issues); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	issues = s.enforceIssueList(issues)
	s.respond(w, r, verb, f.Compact, issues, awbCompactLines(issues, withBlockers),
		fmt.Sprintf("workspaces=%s rows=%d", strings.Join(workspaces, ","), len(issues)))
}

// ---------------------------------------------------------------------------
// whoami
// ---------------------------------------------------------------------------

// awbWhoamiResponse is the discovery verb's answer: who the daemon's account
// is, what this caller may reach, and what the server actually holds.
type awbWhoamiResponse struct {
	URL string `json:"url"`
	// Username is the account the daemon authenticates as, and Identity is who
	// the SERVER says that is. They ordinarily agree; a server with no users
	// reports neither, which is how an agent can tell "unauthenticated server"
	// from "wrong password" without reading a 401.
	Username string `json:"username,omitempty"`
	Identity string `json:"identity,omitempty"`
	// OperatorWorkspaces is agent.awb_proxy.allowed_workspaces, absent when the
	// operator configured none. GrantWorkspaces is the awb_workspace scope on the
	// caller's own grant, absent when it is unscoped. AllowedWorkspaces is what
	// the ones that ARE present leave this caller — the set every other verb
	// echoes.
	OperatorWorkspaces []string `json:"operator_workspaces,omitempty"`
	GrantWorkspaces    []string `json:"grant_workspaces,omitempty"`
	AllowedWorkspaces  []string `json:"allowed_workspaces"`
	// AllowWrite is the operator's own ceiling: false means every mutating verb
	// is refused however the caller's grants are spelled.
	AllowWrite bool `json:"allow_write"`
	// Workspaces is every workspace the daemon's account can see, each marked with
	// whether THIS caller may reach it. A workspace the caller may reach and the
	// account cannot see does not appear — which is itself the answer, and the
	// reason the two lists are reported side by side.
	//
	// An UNREACHABLE entry carries its key and nothing else. The key has to be
	// there: "this workspace exists on the server, ask the operator to add it" is
	// the diagnostic this verb exists for, and every refusal already names the
	// operator's list anyway. Its name, its issue count and the account's access
	// in it are a different thing — they describe the workspace rather than
	// establish that the key exists — and the workspace gate is exactly the rule
	// that says this caller may not read them. See awbWhoamiWorkspace.
	Workspaces []awbWhoamiWorkspace `json:"workspaces"`
	// Note carries a best-effort read that failed. The membership read is the
	// one call here whose failure must not fail the verb: a server with no
	// users has no account to read, and `whoami` is exactly the command an
	// agent runs to find that out.
	Note string `json:"note,omitempty"`
}

// awbWhoamiWorkspace is one workspace the daemon's account can see.
//
// Everything but Key and Reachable is omitempty and filled in only for a
// workspace THIS caller may reach, so an out-of-scope row says that the key
// exists and stops there. See awbWhoamiResponse.Workspaces.
type awbWhoamiWorkspace struct {
	Key       string `json:"key"`
	Reachable bool   `json:"reachable"`
	// Name and ActiveIssues describe the workspace, so they are reported only
	// where the caller could read the same facts from a listing anyway.
	Name         string `json:"name,omitempty"`
	ActiveIssues int    `json:"active_issues,omitempty"`
	// Access is the daemon account's access level in this workspace — "regular"
	// or "admin" — and empty on a server that authenticates nobody.
	Access string `json:"access,omitempty"`
}

// handleAWBProxyWhoami serves POST /v1/awb/whoami — the discovery verb.
//
// It is the command to point an agent at when something is refused: it reports
// both halves of the gate beside the workspaces the account can actually see, so
// the agent can tell the operator exactly which list to widen rather than
// guessing from a 403.
func handleAWBProxyWhoami(w http.ResponseWriter, r *http.Request) {
	var body awbCompactRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	resp := awbWhoamiResponse{
		URL:                s.base,
		Username:           s.policy.Username,
		OperatorWorkspaces: s.policy.AllowedWorkspaces,
		GrantWorkspaces:    s.grantWorkspaces,
		AllowedWorkspaces:  s.workspaces,
		AllowWrite:         s.policy.AllowWrite,
		Workspaces:         []awbWhoamiWorkspace{},
	}
	var identity awbIdentityResponse
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet, Path: "/api/identity",
	}, &identity); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	resp.Identity = identity.Identity

	var workspaces []awbWorkspace
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet, Path: "/api/workspaces",
	}, &workspaces); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	access := s.whoamiAccess(r.Context(), &resp)
	for _, p := range workspaces {
		key := strings.ToLower(strings.TrimSpace(p.Key))
		row := awbWhoamiWorkspace{Key: p.Key, Reachable: s.workspaceAllowed(key)}
		if row.Reachable {
			row.Name = p.Name
			row.ActiveIssues = p.ActiveIssues
			row.Access = access[key]
		}
		resp.Workspaces = append(resp.Workspaces, row)
	}
	s.respond(w, r, "whoami", body.Compact, &resp, awbWhoamiText(&resp),
		fmt.Sprintf("workspaces=%d", len(resp.Workspaces)))
}

// whoamiAccess reads the daemon account's per-workspace access level,
// best-effort.
//
// Best-effort because this is the one call in `whoami` whose failure is
// information rather than an error: an unauthenticated server has no account to
// read, and an account without user_admin may still read ITSELF — AWB
// guarantees that much — so a failure here says something about the server that
// the rest of the answer should still be able to report.
func (s *awbProxySession) whoamiAccess(ctx context.Context, resp *awbWhoamiResponse) map[string]string {
	if s.policy.Username == "" {
		return nil
	}
	var user awbUser
	if _, fault := s.exec(ctx, awbCall{
		Method: http.MethodGet, Path: "/api/users/" + awbSegment(s.policy.Username),
	}, &user); fault != nil {
		resp.Note = "could not read the daemon account's memberships: " + fault.Msg
		return nil
	}
	access := make(map[string]string, len(user.Workspaces))
	for _, m := range user.Workspaces {
		access[strings.ToLower(strings.TrimSpace(m.Workspace))] = m.Access
	}
	return access
}

// awbWhoamiText is `whoami --compact`: one line per workspace, prefixed with the
// two lines that answer "who am I" and "what may I do". awb has no compact form
// for this verb — it is the proxy's own — so the shape is chosen to match the
// rest: fields that cannot contain a space, positional, one line each.
func awbWhoamiText(resp *awbWhoamiResponse) string {
	var b strings.Builder
	b.WriteString("url " + resp.URL + "\n")
	identity := resp.Identity
	if identity == "" {
		identity = "-"
	}
	write := "read-only"
	if resp.AllowWrite {
		write = "read-write"
	}
	b.WriteString("identity " + identity + " " + write + "\n")
	for _, p := range resp.Workspaces {
		if !p.Reachable {
			// The key alone, for the reason awbWhoamiResponse.Workspaces gives.
			b.WriteString(p.Key + " -\n")
			continue
		}
		access := p.Access
		if access == "" {
			access = "-"
		}
		b.WriteString(p.Key + " reachable " + access + " " +
			strconv.Itoa(p.ActiveIssues) + " " + awbJSONString(p.Name) + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// handleAWBProxyIssueShow serves POST /v1/awb/issue/show.
func handleAWBProxyIssueShow(w http.ResponseWriter, r *http.Request) {
	var body awbIssueRefRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var issue awbIssue
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet, Path: "/api/issues/" + awbSegment(ref),
	}, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueWorkspace(&issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, "issue.show", body.Compact, &issue, awbCompactLine(&issue, false)+"\n",
		"issue="+issue.ID)
}

// handleAWBProxyIssueList serves POST /v1/awb/issue/list.
func handleAWBProxyIssueList(w http.ResponseWriter, r *http.Request) {
	var body awbFilterRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	s.runAWBListing(w, r, "issue.list", "/api/issues", &body,
		awbFilterOptions{status: true, assignee: true}, false)
}

// handleAWBProxyIssueReady serves POST /v1/awb/issue/ready — the primary agent
// entry point, and the one listing that answers a question rather than
// describing a filter.
func handleAWBProxyIssueReady(w http.ResponseWriter, r *http.Request) {
	var body awbFilterRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	s.runAWBListing(w, r, "issue.ready", "/api/ready", &body, awbFilterOptions{}, false)
}

// handleAWBProxyIssueBlocked serves POST /v1/awb/issue/blocked. It is the one
// listing rendered WITH blockers, because they are its point.
func handleAWBProxyIssueBlocked(w http.ResponseWriter, r *http.Request) {
	var body awbFilterRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	s.runAWBListing(w, r, "issue.blocked", "/api/blocked", &body,
		awbFilterOptions{assignee: true}, true)
}

// handleAWBProxyIssueSearch serves POST /v1/awb/issue/search.
func handleAWBProxyIssueSearch(w http.ResponseWriter, r *http.Request) {
	var body awbFilterRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	s.runAWBListing(w, r, "issue.search", "/api/search", &body,
		awbFilterOptions{status: true, assignee: true, relevance: true}, false)
}

// handleAWBProxyDepTree serves POST /v1/awb/dep/tree.
//
// This is the one read whose answer can reach outside the workspace gate on its
// own: AWB follows children across workspace boundaries by design. See pruneTree
// for what happens to a child the caller may not see.
func handleAWBProxyDepTree(w http.ResponseWriter, r *http.Request) {
	var body awbIssueRefRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var tree awbIssueTree
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet, Path: "/api/issues/" + awbSegment(ref) + "/tree",
	}, &tree); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	kept, pruned := s.pruneTree(&tree)
	if kept == nil {
		// The ROOT is out of scope, which the pre-call gate should already have
		// refused — a prefix resolves within its own workspace. Refusing rather
		// than answering with nothing keeps the two halves of the gate saying
		// the same thing.
		writeProxyFault(w, s.enforceIssueWorkspace(&tree.awbIssue))
		return
	}
	var text strings.Builder
	awbCompactTree(kept, 0, &text)
	s.respond(w, r, "dep.tree", body.Compact, kept, text.String(),
		fmt.Sprintf("issue=%s pruned=%d", kept.ID, pruned))
}

// handleAWBProxyCommentList serves POST /v1/awb/comment/list.
//
// It is the activity listing narrowed to `kind=comment`, which is how awb
// spells it too: comments and change records are one append-only timeline, and
// `comment list` is a view of it rather than a separate store. A close reason
// therefore arrives here, as a comment carrying action "closed".
//
// NOTE that this is one of the two reads on this surface that carry
// THIRD-PARTY PROSE into an agent's context — anyone with access to the tracker
// can write a comment. The CLI help says so; there is nothing the daemon can do
// about it beyond bounding the size, which is why it is said rather than
// enforced.
func handleAWBProxyCommentList(w http.ResponseWriter, r *http.Request) {
	var body awbCommentListRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	s.runAWBActivityListing(w, r, "comment.list", awbActivityQuery{
		id: body.ID, kind: awbActivityKindComment, limit: body.Limit, offset: body.Offset,
		compact: body.Compact,
	})
}

// handleAWBProxyActivityList serves POST /v1/awb/activity/list — the whole
// timeline, comments and change records together, newest first.
//
// The change records are what `comment list` leaves out: who claimed the issue,
// when it was closed and what moved. Reading them is how an agent picks up work
// somebody else touched without having to ask.
//
// It carries the same third-party prose `comment list` does, since comments are
// part of what it returns.
func handleAWBProxyActivityList(w http.ResponseWriter, r *http.Request) {
	var body awbActivityListRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	kind, fault := validateAWBActivityKind(body.Kind)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.runAWBActivityListing(w, r, "activity.list", awbActivityQuery{
		id: body.ID, kind: kind, limit: body.Limit, offset: body.Offset, compact: body.Compact,
	})
}

// awbActivityQuery is one timeline read, however it was asked for.
type awbActivityQuery struct {
	id string
	// kind narrows the timeline; empty means every entry.
	kind    string
	limit   int
	offset  int
	compact bool
}

// runAWBActivityListing is the tail `comment list` and `activity` share.
//
// One implementation rather than two, because the gate, the bounds and the
// rendering must not be able to differ between "the comments" and "the
// timeline" — they are the same rows read through the same endpoint.
func (s *awbProxySession) runAWBActivityListing(
	w http.ResponseWriter, r *http.Request, verb string, q awbActivityQuery,
) {
	ref, fault := s.gateIssueRef(q.id)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateAWBLimit(q.limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	offset, fault := validateAWBOffset(q.offset)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	query := url.Values{"limit": []string{strconv.Itoa(limit)}}
	if q.kind != "" {
		query.Set("kind", q.kind)
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	// Non-nil so an issue with no entries renders as `[]` rather than `null`,
	// for the reason every other listing here does.
	entries := []awbActivity{}
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet, Path: "/api/issues/" + awbSegment(ref) + "/activity", Query: query,
	}, &entries); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, verb, q.compact, entries, awbCompactActivityLines(entries),
		fmt.Sprintf("issue=%s kind=%s rows=%d", ref, awbKindOrAll(q.kind), len(entries)))
}

// awbKindOrAll labels an unfiltered timeline read in the audit row, so a row
// says which question was asked rather than leaving the field blank.
func awbKindOrAll(kind string) string {
	if kind == "" {
		return "all"
	}
	return kind
}

// handleAWBProxyAttachList serves POST /v1/awb/attach/list.
func handleAWBProxyAttachList(w http.ResponseWriter, r *http.Request) {
	var body awbIssueRefRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	attachments := []awbAttachment{}
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet, Path: "/api/issues/" + awbSegment(ref) + "/attachments",
	}, &attachments); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, "attach.list", body.Compact, attachments,
		awbCompactAttachmentLines(attachments), fmt.Sprintf("issue=%s rows=%d", ref, len(attachments)))
}

// handleAWBProxyAttachShow serves POST /v1/awb/attach/show — one attachment's
// metadata. Its content is what `attach get` writes.
func handleAWBProxyAttachShow(w http.ResponseWriter, r *http.Request) {
	var body awbAttachNameRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	ref, name, fault := s.gateAttachment(&body)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var attachment awbAttachment
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet,
		Path:   "/api/issues/" + awbSegment(ref) + "/attachments/" + awbSegment(name),
	}, &attachment); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, "attach.show", body.Compact, &attachment,
		awbCompactAttachmentLine(&attachment)+"\n", fmt.Sprintf("issue=%s name=%q", ref, name))
}

// handleAWBProxyAttachGet serves POST /v1/awb/attach/get — the bytes exactly as
// they were uploaded.
//
// It is the one verb with no output mode: --json and --compact do not apply to
// content, exactly as they do not apply to awb's own `attach get`. The bytes
// travel back base64-encoded inside the ordinary JSON outcome, and the CLI
// writes them to a file or to stdout.
func handleAWBProxyAttachGet(w http.ResponseWriter, r *http.Request) {
	var body awbAttachNameRequest
	s, ok := openAWBProxy(w, r, PermAWBRead, &body)
	if !ok {
		return
	}
	ref, name, fault := s.gateAttachment(&body)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// The metadata read comes first, and it is not merely informative: it is
	// what bounds the download. Reading an unbounded stream and discovering it
	// was too large afterwards would have spent the bandwidth already, and the
	// size AWB records is the one it serves.
	var attachment awbAttachment
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet,
		Path:   "/api/issues/" + awbSegment(ref) + "/attachments/" + awbSegment(name),
	}, &attachment); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if attachment.Size > maxAWBAttachmentBytes {
		writeProxyFault(w, faultf(http.StatusBadRequest, "invalid_arg",
			"%q on %s is %d bytes; the proxy's maximum is %d, because content travels through the "+
				"daemon in a response body rather than being streamed to a path it would have to write",
			name, ref, attachment.Size, maxAWBAttachmentBytes))
		return
	}
	res, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodGet,
		Path:   "/api/issues/" + awbSegment(ref) + "/attachments/" + awbSegment(name) + "/content",
	}, nil)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// The bytes must be the ones the metadata describes. AWB records a size and
	// serves that response uncompressed precisely so a short transfer is
	// detectable, and this is the one verb where accepting a short one would be
	// invisible: the caller writes a file and reads it as the attachment.
	// Refusing beats handing over something that looks like evidence and is not.
	if int64(len(res.Body)) != attachment.Size {
		writeProxyFault(w, faultf(http.StatusBadGateway, "awb_failed",
			"%q on %s is recorded as %d bytes but %d arrived; the transfer was truncated or the "+
				"stored file no longer matches its metadata",
			name, ref, attachment.Size, len(res.Body)))
		return
	}
	s.respondContent(w, r, "attach.get", res.Body,
		fmt.Sprintf("issue=%s name=%q bytes=%d", ref, name, len(res.Body)))
}

// gateAttachment validates and gates the (issue, name) pair that addresses one
// attachment. An attachment has no id of its own — the pair IS the reference —
// so the two are validated together.
func (s *awbProxySession) gateAttachment(body *awbAttachNameRequest) (ref, name string, fault *proxyFault) {
	if ref, fault = s.gateIssueRef(body.ID); fault != nil {
		return "", "", fault
	}
	if name, fault = validateAWBAttachmentName(body.Name); fault != nil {
		return "", "", fault
	}
	return ref, name, nil
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// awbIssueCreateBody is the create payload, matching AWB's IssueCreate schema.
// AWB rejects an unrecognised field rather than ignoring it, so this is spelled
// out rather than assembled from a map.
type awbIssueCreateBody struct {
	Workspace      string               `json:"workspace"`
	Title          string               `json:"title"`
	Description    *string              `json:"description,omitempty"`
	CommitHash     string               `json:"commit_hash,omitempty"`
	PullRequestURL string               `json:"pull_request_url,omitempty"`
	Type           string               `json:"type,omitempty"`
	Priority       *int                 `json:"priority,omitempty"`
	Assignees      []string             `json:"assignees,omitempty"`
	Labels         []string             `json:"labels,omitempty"`
	Relations      []awbNewRelationBody `json:"relations,omitempty"`
}

type awbNewRelationBody struct {
	Type  string `json:"type"`
	Other string `json:"other"`
}

// handleAWBProxyIssueCreate serves POST /v1/awb/issue/create.
func handleAWBProxyIssueCreate(w http.ResponseWriter, r *http.Request) {
	var body awbCreateRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	// Validate every caller-controlled field before spending the operator's
	// account on workspace discovery. The workspace is populated only after
	// the otherwise complete payload has passed its cheap local gates.
	payload, fault := s.buildAWBCreateBody("", &body)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	workspace, fault := s.resolveCreateWorkspace(r.Context(), body.Workspace)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	payload.Workspace = workspace
	encoded, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not encode the AWB request")
		return
	}
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var issue awbIssue
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodPost, Path: "/api/issues",
		Body: encoded, ContentType: "application/json",
	}, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueWorkspace(&issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// awb create is the exception to "a mutating command prints nothing":
	// minting an id is the point, so the compact form is that id.
	s.respond(w, r, "issue.create", body.Compact, &issue, issue.ID+"\n", "issue="+issue.ID)
}

// resolveCreateWorkspace accepts an explicit workspace or infers the only
// workspace both visible to the daemon account and admitted by this caller's
// effective proxy gate. Ambiguity is refused before the mutation.
func (s *awbProxySession) resolveCreateWorkspace(ctx context.Context, raw string) (string, *proxyFault) {
	if strings.TrimSpace(raw) != "" {
		return s.validateAWBWorkspace(raw)
	}
	workspaces, fault := s.listingWorkspaces(ctx, nil)
	if fault != nil {
		return "", fault
	}
	if len(workspaces) > 1 {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"--workspace is required because several visible workspaces are within this caller's proxy gate: %s; "+
				"run `tclaude proxy awb whoami` to inspect workspace access",
			strings.Join(workspaces, ", "))
	}
	return workspaces[0], nil
}

// buildAWBCreateBody validates every field of a create and assembles AWB's
// request body.
//
// The relation targets go through the same gate as any other issue reference.
// Relating a new issue to one in a workspace this caller cannot reach would write
// into that issue's graph — the relation shows up at both ends — so it is
// refused rather than allowed on the grounds that only the subject is "the"
// issue being created.
func (s *awbProxySession) buildAWBCreateBody(
	workspace string, body *awbCreateRequest,
) (*awbIssueCreateBody, *proxyFault) {
	title, fault := validateAWBTitle(body.Title)
	if fault != nil {
		return nil, fault
	}
	out := &awbIssueCreateBody{Workspace: workspace, Title: title}
	if body.Description != nil {
		if fault := validateAWBDescription(*body.Description); fault != nil {
			return nil, fault
		}
		out.Description = body.Description
	}
	if out.CommitHash, fault = validateAWBCommitHash(body.CommitHash); fault != nil {
		return nil, fault
	}
	if out.PullRequestURL, fault = validateAWBPullRequestURL(body.PullRequestURL); fault != nil {
		return nil, fault
	}
	if out.Type, fault = validateAWBType(body.Type); fault != nil {
		return nil, fault
	}
	if body.Priority != nil {
		if fault := validateAWBPriority(*body.Priority); fault != nil {
			return nil, fault
		}
		out.Priority = body.Priority
	}
	for _, raw := range body.Assignees {
		assignee, assignFault := validateAWBAssignee(raw)
		if assignFault != nil {
			return nil, assignFault
		}
		out.Assignees = append(out.Assignees, assignee)
	}
	for _, raw := range body.Labels {
		label, fault := validateAWBLabel(raw)
		if fault != nil {
			return nil, fault
		}
		out.Labels = append(out.Labels, label)
	}
	// Read "the new issue — relation — the named issue", the single convention
	// of the whole tool, which is why the flags are named after the relation
	// rather than after the direction.
	for _, rel := range []struct {
		typ    string
		others []string
	}{
		{"has-parent", nonEmptyRefs(body.HasParent)},
		{"blocked-by", body.BlockedBy},
		{"discovered-from", body.DiscoveredFrom},
		{"related", body.Related},
	} {
		for _, raw := range rel.others {
			other, fault := s.gateIssueRef(raw)
			if fault != nil {
				return nil, fault
			}
			out.Relations = append(out.Relations, awbNewRelationBody{Type: rel.typ, Other: other})
		}
	}
	return out, nil
}

// nonEmptyRefs turns an optional single reference into the zero-or-one list the
// relation loop above iterates.
func nonEmptyRefs(ref string) []string {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	return []string{ref}
}

// awbIssuePatchBody is the update payload. Only fields awb update can
// change appear: AWB rejects an unrecognised field, and accepts but refuses to
// CHANGE status, assignee and labels — those move through their own verbs, so
// that in_progress and an assignee cannot drift apart and a claim cannot be
// taken silently.
type awbIssuePatchBody struct {
	Title          *string `json:"title,omitempty"`
	Description    *string `json:"description,omitempty"`
	CommitHash     *string `json:"commit_hash,omitempty"`
	PullRequestURL *string `json:"pull_request_url,omitempty"`
	Type           *string `json:"type,omitempty"`
	Priority       *int    `json:"priority,omitempty"`
}

// handleAWBProxyIssueUpdate serves POST /v1/awb/issue/update.
func handleAWBProxyIssueUpdate(w http.ResponseWriter, r *http.Request) {
	var body awbUpdateRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var patch awbIssuePatchBody
	if body.Title != nil {
		title, fault := validateAWBTitle(*body.Title)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		patch.Title = &title
	}
	if body.Description != nil {
		if fault := validateAWBDescription(*body.Description); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		patch.Description = body.Description
	}
	if body.CommitHash != nil {
		value, fault := validateAWBCommitHash(*body.CommitHash)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		patch.CommitHash = &value
	}
	if body.PullRequestURL != nil {
		value, fault := validateAWBPullRequestURL(*body.PullRequestURL)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		patch.PullRequestURL = &value
	}
	if body.Type != nil {
		t, fault := validateAWBType(*body.Type)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		patch.Type = &t
	}
	if body.Priority != nil {
		if fault := validateAWBPriority(*body.Priority); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		patch.Priority = body.Priority
	}
	s.mutateIssue(w, r, "issue.update", ref, body.Compact, awbCall{
		Method: http.MethodPatch, Path: "/api/issues/" + awbSegment(ref),
	}, patch, "")
}

type awbClaimBody struct {
	Assignee string `json:"assignee,omitempty"`
	Force    bool   `json:"force,omitempty"`
}

// handleAWBProxyIssueClaim serves POST /v1/awb/issue/claim.
func handleAWBProxyIssueClaim(w http.ResponseWriter, r *http.Request) {
	var body awbClaimRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if body.As != nil {
		writeProxyFault(w, faultf(http.StatusBadRequest, "invalid_arg",
			"claim no longer accepts as; claims always use the operator's AWB user"))
		return
	}
	// A claim always belongs to the AWB user whose account the daemon uses.
	// AWB assignees are users, not free-form agent labels.
	assignee, fault := s.identity("claim")
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if assignee, fault = validateAWBAssignee(assignee); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.mutateIssue(w, r, "issue.claim", ref, body.Compact, awbCall{
		Method: http.MethodPost, Path: "/api/issues/" + awbSegment(ref) + "/claim",
	}, awbClaimBody{Assignee: assignee, Force: body.Force}, "")
}

type awbReleaseBody struct {
	Assignee string `json:"assignee,omitempty"`
	Force    bool   `json:"force,omitempty"`
}

// handleAWBProxyIssueRelease serves POST /v1/awb/issue/release.
func handleAWBProxyIssueRelease(w http.ResponseWriter, r *http.Request) {
	var body awbForceRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// The identity serves only the "assigned to somebody else" refusal, so it
	// is REQUIRED only when that refusal applies. A forced release on a server
	// that authenticates nobody is a legitimate request and must not fail for
	// want of a name nothing would compare.
	out := awbReleaseBody{Force: body.Force}
	if body.Force {
		out.Assignee = s.policy.Username
	} else if out.Assignee, fault = s.identity("release"); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.mutateIssue(w, r, "issue.release", ref, body.Compact, awbCall{
		Method: http.MethodPost, Path: "/api/issues/" + awbSegment(ref) + "/release",
	}, out, "")
}

type awbCloseBody struct {
	Reason *string `json:"reason,omitempty"`
}

// handleAWBProxyIssueClose serves POST /v1/awb/issue/close.
func handleAWBProxyIssueClose(w http.ResponseWriter, r *http.Request) {
	var body awbCloseRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if body.Reason != nil {
		if fault := validateAWBCloseReason(*body.Reason); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	}
	s.mutateIssue(w, r, "issue.close", ref, body.Compact, awbCall{
		Method: http.MethodPost, Path: "/api/issues/" + awbSegment(ref) + "/close",
	}, awbCloseBody{Reason: body.Reason}, "")
}

// handleAWBProxyIssueReopen serves POST /v1/awb/issue/reopen.
func handleAWBProxyIssueReopen(w http.ResponseWriter, r *http.Request) {
	var body awbIssueRefRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.mutateIssue(w, r, "issue.reopen", ref, body.Compact, awbCall{
		Method: http.MethodPost, Path: "/api/issues/" + awbSegment(ref) + "/reopen",
	}, nil, "")
}

// handleAWBProxyIssueDelete serves POST /v1/awb/issue/delete.
//
// This is a HARD delete AWB cannot undo, and it never refuses on account of
// dependents: it orphans any children and drops every relation. The `force`
// flag is therefore required — a confirmation the caller has to type, exactly
// as `awb delete` requires it — and the answer says how many relations went,
// because removing a blocker silently makes other issues ready.
func handleAWBProxyIssueDelete(w http.ResponseWriter, r *http.Request) {
	var body awbForceRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !body.Force {
		writeProxyFault(w, faultf(http.StatusBadRequest, "invalid_arg",
			"delete needs force: it is a hard delete AWB cannot undo, and it orphans any children "+
				"and drops every relation"))
		return
	}
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var issue awbIssue
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodDelete, Path: "/api/issues/" + awbSegment(ref),
	}, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueWorkspace(&issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// The relation count is derived rather than reported: AWB answers a delete
	// with the issue as it was immediately before, relations included, which is
	// exactly the count and is what --json prints.
	s.respond(w, r, "issue.delete", body.Compact, &issue,
		fmt.Sprintf("Deleted %s and %d relation(s).\n", issue.ID, len(issue.Relations)),
		fmt.Sprintf("issue=%s relations=%d", issue.ID, len(issue.Relations)))
}

type awbLabelBody struct {
	Label string `json:"label"`
}

// handleAWBProxyLabelAdd serves POST /v1/awb/label/add.
func handleAWBProxyLabelAdd(w http.ResponseWriter, r *http.Request) {
	s, ref, label, body, ok := openAWBLabelVerb(w, r)
	if !ok {
		return
	}
	s.mutateIssue(w, r, "label.add", ref, body.Compact, awbCall{
		Method: http.MethodPost, Path: "/api/issues/" + awbSegment(ref) + "/labels",
	}, awbLabelBody{Label: label}, "label="+label)
}

// handleAWBProxyLabelRemove serves POST /v1/awb/label/rm.
//
// The label travels as a query parameter rather than a path segment because a
// label may contain a slash — AWB's own charset allows one — and a slash in a
// path segment is a different path.
func handleAWBProxyLabelRemove(w http.ResponseWriter, r *http.Request) {
	s, ref, label, body, ok := openAWBLabelVerb(w, r)
	if !ok {
		return
	}
	s.mutateIssue(w, r, "label.rm", ref, body.Compact, awbCall{
		Method: http.MethodDelete,
		Path:   "/api/issues/" + awbSegment(ref) + "/labels",
		Query:  url.Values{"label": []string{label}},
	}, nil, "label="+label)
}

// openAWBLabelVerb is the prologue the two label verbs share: they take the
// same arguments and differ only in the call they make.
func openAWBLabelVerb(
	w http.ResponseWriter, r *http.Request,
) (s *awbProxySession, ref, label string, body awbLabelRequest, ok bool) {
	s, ok = openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return nil, "", "", body, false
	}
	var fault *proxyFault
	if ref, fault = s.gateIssueRef(body.ID); fault != nil {
		writeProxyFault(w, fault)
		return nil, "", "", body, false
	}
	if label, fault = validateAWBLabel(body.Label); fault != nil {
		writeProxyFault(w, fault)
		return nil, "", "", body, false
	}
	return s, ref, label, body, true
}

type awbRelationBody struct {
	Type  string `json:"type"`
	Other string `json:"other"`
	Force bool   `json:"force,omitempty"`
}

// handleAWBProxyDepAdd serves POST /v1/awb/dep/add.
func handleAWBProxyDepAdd(w http.ResponseWriter, r *http.Request) {
	s, ref, relType, other, body, ok := openAWBDepVerb(w, r)
	if !ok {
		return
	}
	s.mutateIssue(w, r, "dep.add", ref, body.Compact, awbCall{
		Method: http.MethodPost, Path: "/api/issues/" + awbSegment(ref) + "/relations",
	}, awbRelationBody{Type: relType, Other: other, Force: body.Force},
		fmt.Sprintf("rel=%s other=%s", relType, other))
}

// handleAWBProxyDepRemove serves POST /v1/awb/dep/rm. It takes the same
// relation and the same two ids in the same order as `dep add`, so removing a
// relation is literally the add request with rm substituted.
func handleAWBProxyDepRemove(w http.ResponseWriter, r *http.Request) {
	s, ref, relType, other, body, ok := openAWBDepVerb(w, r)
	if !ok {
		return
	}
	s.mutateIssue(w, r, "dep.rm", ref, body.Compact, awbCall{
		Method: http.MethodDelete,
		Path: "/api/issues/" + awbSegment(ref) + "/relations/" +
			awbSegment(relType) + "/" + awbSegment(other),
	}, nil, fmt.Sprintf("rel=%s other=%s", relType, other))
}

// openAWBDepVerb is the prologue the two relation verbs share.
//
// BOTH ends go through the workspace gate. A relation is read from either end —
// it appears in `relations` on the other issue too — so writing one into a
// workspace this caller cannot reach would be a write outside the gate wearing
// the subject's clothes.
func openAWBDepVerb(
	w http.ResponseWriter, r *http.Request,
) (s *awbProxySession, ref, relType, other string, body awbRelationRequest, ok bool) {
	s, ok = openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return nil, "", "", "", body, false
	}
	var fault *proxyFault
	if ref, fault = s.gateIssueRef(body.ID); fault != nil {
		writeProxyFault(w, fault)
		return nil, "", "", "", body, false
	}
	if relType, fault = validateAWBRelationType(body.Type); fault != nil {
		writeProxyFault(w, fault)
		return nil, "", "", "", body, false
	}
	if other, fault = s.gateIssueRef(body.Other); fault != nil {
		writeProxyFault(w, fault)
		return nil, "", "", "", body, false
	}
	return s, ref, relType, other, body, true
}

// handleAWBProxyCommentAdd serves POST /v1/awb/comment/add — the write an agent
// reports progress with.
//
// Unlike every other write here it answers with the new TIMELINE ENTRY rather
// than with the issue, because that is what AWB returns and what the caller
// asked for; a comment does not move any field of the issue, so echoing the
// issue would show nothing that changed.
func handleAWBProxyCommentAdd(w http.ResponseWriter, r *http.Request) {
	var body awbCommentAddRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateAWBComment(body.Body); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	encoded, err := json.Marshal(awbCommentBody{Body: body.Body})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not encode the AWB request")
		return
	}
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var entry awbActivity
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodPost, Path: "/api/issues/" + awbSegment(ref) + "/comments",
		Body: encoded, ContentType: "application/json",
	}, &entry); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// The entry names the issue it landed on, so the gate gets its second look
	// here as it does everywhere else — this time on a reference rather than on
	// an issue body.
	if fault := s.requireAllowedWorkspace(workspaceKeyOf(strings.ToLower(entry.Issue))); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// Nothing on success in compact mode, exactly as awb prints nothing: the
	// caller already knows what it wrote.
	s.respond(w, r, "comment.add", body.Compact, &entry, "",
		fmt.Sprintf("issue=%s entry=%d bytes=%d", entry.Issue, entry.ID, len(body.Body)))
}

// awbCommentBody is AWB's CommentCreate. Spelled out rather than a map because
// AWB rejects an unrecognised field rather than ignoring it.
type awbCommentBody struct {
	Body string `json:"body"`
}

// handleAWBProxyAttachAdd serves POST /v1/awb/attach/add.
//
// The content arrives as bytes in the request body and leaves as bytes in an
// octet-stream body, never as a path the daemon reads. See
// maxAWBAttachmentBytes for why that is worth a size bound.
func handleAWBProxyAttachAdd(w http.ResponseWriter, r *http.Request) {
	var body awbAttachAddRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, fault := s.gateIssueRef(body.ID)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	name, fault := validateAWBAttachmentName(body.Name)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	contentType, fault := validateAWBContentType(body.ContentType)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateAWBContent(body.Content); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// Everything about the file that is not its content travels as a query
	// parameter, which is what keeps Content-Type free to be what it is
	// everywhere else — a statement about the body on the wire.
	q := url.Values{"name": []string{name}}
	if contentType != "" {
		q.Set("content-type", contentType)
	}
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var attachment awbAttachment
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodPost,
		Path:   "/api/issues/" + awbSegment(ref) + "/attachments",
		Query:  q, Body: body.Content, ContentType: "application/octet-stream",
	}, &attachment); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// Nothing on success in compact mode, exactly as awb prints nothing: there
	// is no id to print, and the caller already knows what it attached and
	// under what name.
	s.respond(w, r, "attach.add", body.Compact, &attachment, "",
		fmt.Sprintf("issue=%s name=%q bytes=%d", ref, name, len(body.Content)))
}

// handleAWBProxyAttachDelete serves POST /v1/awb/attach/delete.
func handleAWBProxyAttachDelete(w http.ResponseWriter, r *http.Request) {
	var body awbAttachNameRequest
	s, ok := openAWBProxy(w, r, PermAWBWrite, &body)
	if !ok {
		return
	}
	ref, name, fault := s.gateAttachment(&body)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !body.Force {
		writeProxyFault(w, faultf(http.StatusBadRequest, "invalid_arg",
			"attach delete needs force: it is not recoverable"))
		return
	}
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var attachment awbAttachment
	if _, fault := s.exec(r.Context(), awbCall{
		Method: http.MethodDelete,
		Path:   "/api/issues/" + awbSegment(ref) + "/attachments/" + awbSegment(name),
	}, &attachment); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, "attach.delete", body.Compact, &attachment,
		fmt.Sprintf("Deleted attachment %s from %s.\n", awbJSONString(attachment.Name), attachment.Issue),
		fmt.Sprintf("issue=%s name=%q", ref, name))
}

// mutateIssue is the shared tail of every mutating verb that answers with the
// issue: budget check, call, the second half of the workspace gate, render.
//
// payload is marshalled to a JSON body when non-nil; a verb whose endpoint
// takes no body passes nil. detail is appended to the audit row's issue= field.
//
// The compact rendering is EMPTY on purpose. awb prints nothing on a successful
// mutation — the exceptions, create and delete, render their own line and do
// not come through here — and a proxy that invented output would make the two
// disagree about what a silent success looks like.
func (s *awbProxySession) mutateIssue(
	w http.ResponseWriter, r *http.Request, verb, ref string, compact bool,
	call awbCall, payload any, detail string,
) {
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "could not encode the AWB request")
			return
		}
		call.Body = encoded
		call.ContentType = "application/json"
	}
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var issue awbIssue
	if _, fault := s.exec(r.Context(), call, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueWorkspace(&issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if detail != "" {
		detail = " " + detail
	}
	s.respond(w, r, verb, compact, &issue, "", "issue="+issue.ID+detail)
}
