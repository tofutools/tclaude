package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// linearproxy_handlers.go is the HTTP surface of `tclaude proxy linear`.
//
// Every route is a POST, including the reads, for the same reason the GitHub
// proxy's reads are: a Linear read SPENDS THE OPERATOR'S CREDENTIAL against a
// private workspace, and the audit middleware records mutating methods only.
// Making the reads POSTs is what puts "this agent read the backlog as me" in
// the operator's audit trail beside "this agent commented as me".
//
// The gates run in a fixed order, and the order matters:
//
//  1. permission slug — before the body is read, so an ungated caller cannot
//     probe for the existence of a team;
//  2. operator policy and grant scope, resolved together into this caller's
//     effective team set — the proxy is off unless SOME team is reachable, and
//     writes are off unless allow_write is set;
//  3. parameter validation — identifier shape, charset, length;
//  4. the team gate on the caller's identifier;
//  5. the call;
//  6. the team gate AGAIN, on the team Linear reported — see enforceIssueTeam.
//
// Step 6 is not belt-and-braces. Step 4 checks a string the caller supplied;
// step 6 checks the thing actually reached.
//
// Steps 4 and 6 read the same effective set, and so do the listing verbs' filter
// and row-level drop, so no combination of operator list and grant scope can be
// enforced one way in one verb and another way in the next.

// maxLinearCommentsTextBytes is the tail kept from `issue comments`, whose
// output IS the payload rather than a diagnosis. Same reasoning and same size
// as the GitHub half's maxGHProxyTextBytes: enough for a real discussion, and
// the tail is the useful end because comments render oldest-first.
const maxLinearCommentsTextBytes = 256 * 1024

// whoamiTeamPageSize bounds the team listing in `whoami`. Past it the response
// says so explicitly (TeamsTruncated) rather than presenting a partial list as
// the whole workspace.
const whoamiTeamPageSize = 100

type linearIssueRequest struct {
	Identifier string `json:"identifier"`
}

type linearListRequest struct {
	Team       string `json:"team,omitempty"`
	State      string `json:"state,omitempty"`
	AssignedMe bool   `json:"assigned_me,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type linearSearchRequest struct {
	Term  string `json:"term"`
	Team  string `json:"team,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type linearCommentsRequest struct {
	Identifier string `json:"identifier"`
	Limit      int    `json:"limit,omitempty"`
}

type linearCommentRequest struct {
	Identifier string `json:"identifier"`
	Body       string `json:"body"`
}

type linearCreateRequest struct {
	Team        string `json:"team"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Priority    int    `json:"priority,omitempty"`
	State       string `json:"state,omitempty"`
}

type linearUpdateRequest struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state,omitempty"`
	// Priority is a pointer because 0 is a meaningful value ("no priority"),
	// so absent and "set it to none" have to be distinguishable.
	Priority *int `json:"priority,omitempty"`
}

type linearLinkRequest struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
}

// openLinearProxy is the shared prologue: method check, permission gate,
// bounded body decode, team-set resolution. Write verbs additionally clear
// allow_write here, so no handler can forget it.
//
// The permission gate goes through preflightProxyPermission, the same helper the
// git proxy uses, for the same reason: an ungranted caller must still be refused
// cheaply, while a TEAM-SCOPED grant cannot be decided against an empty
// ActionContext and would otherwise fall through to an ask-human popup on every
// single call. Where git then finishes the decision against one resolved remote,
// the Linear proxy resolves the scope into the session's effective team set (see
// newLinearProxySession) — its verbs can span several teams at once, so a set is
// the only form the decision can take.
func openLinearProxy(w http.ResponseWriter, r *http.Request, perm string, body any) (*linearProxySession, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return nil, false
	}
	convID, teamScoped, ok := preflightProxyPermission(w, r, perm)
	if !ok {
		return nil, false
	}
	if body != nil && !decodeLinearProxyBody(w, r, body) {
		return nil, false
	}
	s, fault := newLinearProxySession(r, convID, perm, teamScoped)
	if fault != nil {
		writeProxyFault(w, fault)
		return nil, false
	}
	if teamScoped {
		// The scoped grant really did decide this request, so the audit row must
		// say so. In the git proxy this happens inside finishProxyPermission's
		// requirePermission call; here the decision is the set resolution above,
		// so the record is written explicitly.
		//
		// It records the GRANT's teams, not the effective set, so the field means
		// the same thing it does on a git row: the scope on the grant that
		// authorized the action, rather than that scope narrowed by an operator
		// setting the row does not otherwise mention. The team actually acted on is
		// already on the same audit row, in the verb's own detail.
		//
		// grantTeams is the EVALUATED set, which is what makes this honest: the
		// teams the scope merely names can be a superset of the teams it admits,
		// and recording those would claim authority the grant never conferred.
		recordAuditPermissionScope(r, perm, permissionScopeDisplay(
			PermissionScope{ScopeDimLinearTeam: s.grantTeams}))
	}
	if perm == PermLinearWrite {
		if fault := s.requireWrite(); fault != nil {
			writeProxyFault(w, fault)
			return nil, false
		}
	}
	return s, true
}

// decodeLinearProxyBody reads a bounded JSON body.
//
// It exists rather than reusing decodeGitProxyBody because that one's 16 KiB
// bound is documented as "a handful of short scalars" — true of a git request,
// false of a Linear one, which carries the markdown of a comment or an issue
// description. Behind the smaller reader a 20 KB progress report died with
// "http: request body too large" before validateLinearBody could say what the
// real limit was, or which field had exceeded it.
func decodeLinearProxyBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxLinearRequestBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return false
	}
	return true
}

// linearTeamClause builds the team half of an issue filter.
//
// It is the ONLY place a team constraint is constructed, and every list-shaped
// verb goes through it, so an unfiltered listing is not something a handler can
// produce by omission. When the caller named a team it has already been gated;
// when it did not, the clause is the caller's whole effective team set — which
// is exactly why that set has to be resolved before any verb runs, rather than
// one team at a time as a request arrives.
//
// eqIgnoreCase rather than `in`: Linear's StringComparator has no
// case-insensitive list form, and the effective set is stored lower-cased while
// Linear's team keys are upper-case.
func (s *linearProxySession) linearTeamClause(team string) map[string]any {
	if team != "" {
		return map[string]any{"team": map[string]any{"key": map[string]any{"eqIgnoreCase": team}}}
	}
	alternatives := make([]any, 0, len(s.teams))
	for _, key := range s.teams {
		alternatives = append(alternatives,
			map[string]any{"team": map[string]any{"key": map[string]any{"eqIgnoreCase": key}}})
	}
	return map[string]any{"or": alternatives}
}

// linearIssueFilter assembles a complete IssueFilter. Clauses are ANDed
// explicitly through `and` rather than relying on Linear's implicit
// same-level conjunction, so the team constraint's relationship to the others
// is stated in the request instead of inferred from filter semantics.
func (s *linearProxySession) linearIssueFilter(team, state string, assignedMe bool) map[string]any {
	clauses := []any{s.linearTeamClause(team)}
	if state != "" {
		clauses = append(clauses,
			map[string]any{"state": map[string]any{"name": map[string]any{"eqIgnoreCase": state}}})
	}
	if assignedMe {
		clauses = append(clauses,
			map[string]any{"assignee": map[string]any{"isMe": map[string]any{"eq": true}}})
	}
	return map[string]any{"and": clauses}
}

// enforceIssueList applies the team gate to every row of a listing.
//
// The filter above should already have made this a no-op. It runs anyway: the
// filter is a request Linear honours, while this is a check the daemon makes,
// and only the second one is a gate. A row from outside the effective set is
// dropped rather than refused — one unexpected row must not deny the agent the
// rest of a legitimate listing — and dropping is safe because the rows are
// data, not an operation.
func (s *linearProxySession) enforceIssueList(issues []linearIssue) []linearIssue {
	kept := make([]linearIssue, 0, len(issues))
	for _, issue := range issues {
		if s.teamAllowed(issue.Team.Key) {
			kept = append(kept, issue)
		}
	}
	return kept
}

// resolveTeamMeta looks up a team's UUID and workflow states. Callers have
// already allow-list-checked the key.
func (s *linearProxySession) resolveTeamMeta(ctx context.Context, key string) (*linearTeamMeta, *proxyFault) {
	var data linearTeamMetaData
	if fault := s.exec(ctx, linearQueryTeamMeta, map[string]any{"key": key}, &data); fault != nil {
		return nil, fault
	}
	if len(data.Teams.Nodes) == 0 {
		return nil, faultf(http.StatusNotFound, "not_found",
			"team %q does not exist, or the operator's Linear key cannot see it", key)
	}
	meta := data.Teams.Nodes[0]
	// The allow-list is checked against what Linear returned, not against what
	// was asked for, for the same reason enforceIssueTeam exists.
	if fault := s.requireAllowedTeam(meta.Key); fault != nil {
		return nil, fault
	}
	return &meta, nil
}

// resolveStateID maps a workflow-state NAME to its UUID within one team.
//
// Exact, case-insensitive, and never fuzzy. A near-match here would silently
// move a ticket to the wrong column, which is worse than a refusal — so a miss
// lists the team's real states instead of guessing.
func resolveStateID(meta *linearTeamMeta, name string) (string, *proxyFault) {
	want := strings.ToLower(strings.TrimSpace(name))
	available := make([]string, 0, len(meta.States.Nodes))
	for _, st := range meta.States.Nodes {
		available = append(available, st.Name)
		if strings.ToLower(strings.TrimSpace(st.Name)) == want {
			return st.ID, nil
		}
	}
	return "", faultf(http.StatusBadRequest, "unknown_state",
		"team %s has no workflow state named %q; it has: %s",
		meta.Key, name, strings.Join(available, ", "))
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// handleLinearProxyWhoami serves POST /v1/linear/whoami — the discovery verb.
// It is the command to point an agent at when something is refused: it reports
// the allow-list beside the teams the key can actually see, so the agent can
// tell the operator exactly what to add.
func handleLinearProxyWhoami(w http.ResponseWriter, r *http.Request) {
	s, ok := openLinearProxy(w, r, PermLinearRead, nil)
	if !ok {
		return
	}
	var data linearViewerData
	if fault := s.exec(r.Context(), linearQueryViewer,
		map[string]any{"first": whoamiTeamPageSize}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	type teamView struct {
		Key     string `json:"key"`
		Name    string `json:"name,omitempty"`
		Allowed bool   `json:"allowed"`
	}
	teams := make([]teamView, 0, len(data.Teams.Nodes))
	for _, t := range data.Teams.Nodes {
		teams = append(teams, teamView{Key: t.Key, Name: t.Name, Allowed: s.teamAllowed(t.Key)})
	}
	s.respond(w, r, "whoami", struct {
		Viewer linearUserRef `json:"viewer"`
		Teams  []teamView    `json:"teams"`
		// AllowedTeams is what THIS caller may reach: the operator's list
		// narrowed by its own grant scope.
		AllowedTeams []string `json:"allowed_teams"`
		// OperatorTeams and GrantTeams break that down when the two differ, so
		// the agent can tell its human which of the two lists to widen rather
		// than reporting a refusal it cannot explain. GrantTeams is absent for
		// an unscoped grant, and OperatorTeams is absent when the operator has
		// configured no list at all (a scope-only posture).
		OperatorTeams []string `json:"operator_teams,omitempty"`
		GrantTeams    []string `json:"grant_teams,omitempty"`
		WriteAllowed  bool     `json:"write_allowed"`
		// TeamsTruncated says the listing hit the page size, so a team the
		// caller is looking for may exist and not be shown. A silent cap here
		// would be the worst place for one: this is the verb an agent runs to
		// find out which team to ask the operator to allow-list.
		TeamsTruncated bool `json:"teams_truncated,omitempty"`
	}{
		Viewer:         data.Viewer,
		Teams:          teams,
		AllowedTeams:   s.teams,
		OperatorTeams:  s.policy.AllowedTeams,
		GrantTeams:     s.grantTeams,
		WriteAllowed:   s.policy.AllowWrite,
		TeamsTruncated: len(data.Teams.Nodes) >= whoamiTeamPageSize,
	}, "")
}

// handleLinearProxyIssueView serves POST /v1/linear/issue/view.
func handleLinearProxyIssueView(w http.ResponseWriter, r *http.Request) {
	var body linearIssueRequest
	s, ok := openLinearProxy(w, r, PermLinearRead, &body)
	if !ok {
		return
	}
	id, fault := validateLinearIdentifier(body.Identifier)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.requireAllowedTeam(teamKeyOf(id)); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearIssueData
	if fault := s.exec(r.Context(), linearQueryIssue, map[string]any{"id": id}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueTeam(data.Issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, "issue.view", data.Issue, "issue="+data.Issue.Identifier)
}

// handleLinearProxyIssueList serves POST /v1/linear/issue/list.
func handleLinearProxyIssueList(w http.ResponseWriter, r *http.Request) {
	var body linearListRequest
	s, ok := openLinearProxy(w, r, PermLinearRead, &body)
	if !ok {
		return
	}
	team, fault := optionalTeam(s, body.Team)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateLinearLimit(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	state, fault := validateLinearStateName(body.State)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearIssuesData
	if fault := s.exec(r.Context(), linearQueryIssues, map[string]any{
		"filter": s.linearIssueFilter(team, state, body.AssignedMe),
		"first":  limit,
	}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	issues := s.enforceIssueList(data.Issues.Nodes)
	s.respond(w, r, "issue.list", issues, fmt.Sprintf("team=%s rows=%d", teamOrAll(team), len(issues)))
}

// handleLinearProxyIssueSearch serves POST /v1/linear/issue/search.
func handleLinearProxyIssueSearch(w http.ResponseWriter, r *http.Request) {
	var body linearSearchRequest
	s, ok := openLinearProxy(w, r, PermLinearRead, &body)
	if !ok {
		return
	}
	term, fault := validateLinearSearchTerm(body.Term)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	team, fault := optionalTeam(s, body.Team)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateLinearLimit(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearSearchData
	if fault := s.exec(r.Context(), linearQuerySearch, map[string]any{
		"term":   term,
		"filter": s.linearIssueFilter(team, "", false),
		"first":  limit,
	}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	issues := s.enforceIssueList(data.SearchIssues.Nodes)
	s.respond(w, r, "issue.search", issues, fmt.Sprintf("team=%s rows=%d", teamOrAll(team), len(issues)))
}

// handleLinearProxyIssueComments serves POST /v1/linear/issue/comments.
//
// This verb returns TEXT rather than JSON, like the GitHub half's `pr
// comments`, because its output is the payload rather than a diagnosis. It is
// also the one read that carries third-party prose into an agent's context: a
// Linear comment can be written by anyone with access to the workspace.
func handleLinearProxyIssueComments(w http.ResponseWriter, r *http.Request) {
	var body linearCommentsRequest
	s, ok := openLinearProxy(w, r, PermLinearRead, &body)
	if !ok {
		return
	}
	id, fault := validateLinearIdentifier(body.Identifier)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.requireAllowedTeam(teamKeyOf(id)); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateLinearLimit(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearCommentsData
	if fault := s.exec(r.Context(), linearQueryIssueComments, map[string]any{
		"id": id, "first": limit,
	}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if data.Issue == nil {
		writeProxyFault(w, faultf(http.StatusNotFound, "not_found", "no such issue"))
		return
	}
	// The gate runs BEFORE any comment body is rendered: prose from an issue
	// the agent may not read must not reach it even in an error path.
	if fault := s.requireAllowedTeam(data.Issue.Team.Key); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	text := renderLinearComments(data.Issue.Identifier, data.Issue.Title, data.Issue.URL,
		data.Issue.Comments.Nodes)
	// A full page means there may be older comments this read did not fetch.
	// Say so rather than let a bounded answer read as a complete one — the
	// page holds the NEWEST comments, so what is missing is the start of the
	// discussion.
	if len(data.Issue.Comments.Nodes) >= limit {
		text += fmt.Sprintf("\n(showing the %d most recent comments; there may be older ones — "+
			"raise --limit to see further back)\n", limit)
	}
	s.respond(w, r, "issue.comments", text,
		fmt.Sprintf("issue=%s rows=%d", data.Issue.Identifier, len(data.Issue.Comments.Nodes)))
}

// renderLinearComments formats a comment thread, oldest first, bounded at
// maxLinearCommentsTextBytes. When it overflows the TAIL is kept — the newest
// comments, which are the ones a caller is usually here for.
//
// It SORTS rather than trusting the order Linear returned. Linear's
// connections expose no direction control — `orderBy` picks the field
// (createdAt / updatedAt), not the direction — and the API returns the newest
// first, which is the right SET for `first: N` (the N most recent comments)
// and the wrong ORDER to read a discussion in. Sorting here makes the
// rendering independent of that, so both promises this function makes —
// "oldest first" and "the tail is the newest" — hold whatever Linear does,
// rather than only while its default direction stays what it is today.
func renderLinearComments(identifier, title, url string, comments []linearComment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n%s\n\n", identifier, title, url)
	if len(comments) == 0 {
		b.WriteString("(no comments)\n")
		return b.String()
	}
	// RFC 3339 timestamps sort correctly as strings, and SliceStable keeps
	// Linear's own relative order for any two that compare equal.
	ordered := append([]linearComment(nil), comments...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].CreatedAt < ordered[j].CreatedAt
	})
	for _, c := range ordered {
		author := "(unknown)"
		if c.User != nil && strings.TrimSpace(c.User.DisplayName) != "" {
			author = c.User.DisplayName
		}
		fmt.Fprintf(&b, "--- %s at %s\n%s\n\n", author, c.CreatedAt, strings.TrimRight(c.Body, "\n"))
	}
	out := b.String()
	if len(out) > maxLinearCommentsTextBytes {
		out = "(earlier comments truncated)\n" + out[len(out)-maxLinearCommentsTextBytes:]
	}
	return out
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// handleLinearProxyIssueComment serves POST /v1/linear/issue/comment — the
// write this half of the feature exists for: an agent reporting progress on
// its own ticket.
func handleLinearProxyIssueComment(w http.ResponseWriter, r *http.Request) {
	var body linearCommentRequest
	s, ok := openLinearProxy(w, r, PermLinearWrite, &body)
	if !ok {
		return
	}
	id, fault := validateLinearIdentifier(body.Identifier)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.requireAllowedTeam(teamKeyOf(id)); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateLinearBody(body.Body, true); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// Read the issue first. commentCreate takes an issue reference and would
	// happily accept one outside the allow-list, so the team is confirmed
	// against Linear's own answer before anything is written.
	var issue linearIssueData
	if fault := s.exec(r.Context(), linearQueryIssue, map[string]any{"id": id}, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueTeam(issue.Issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearCommentCreateData
	if fault := s.exec(r.Context(), linearMutationCommentCreate, map[string]any{
		"input": map[string]any{"issueId": id, "body": body.Body},
	}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !data.CommentCreate.Success {
		writeProxyFault(w, faultf(http.StatusBadGateway, "linear_failed",
			"Linear reported the comment was not created"))
		return
	}
	s.respond(w, r, "issue.comment", data.CommentCreate, "issue="+id)
}

// handleLinearProxyIssueCreate serves POST /v1/linear/issue/create.
func handleLinearProxyIssueCreate(w http.ResponseWriter, r *http.Request) {
	var body linearCreateRequest
	s, ok := openLinearProxy(w, r, PermLinearWrite, &body)
	if !ok {
		return
	}
	team, fault := s.validateLinearTeam(body.Team)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateLinearTitle(body.Title); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateLinearBody(body.Description, false); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := validateLinearPriority(body.Priority); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	meta, fault := s.resolveTeamMeta(r.Context(), team)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	input := map[string]any{"teamId": meta.ID, "title": strings.TrimSpace(body.Title)}
	if d := body.Description; strings.TrimSpace(d) != "" {
		input["description"] = d
	}
	if body.Priority > 0 {
		input["priority"] = body.Priority
	}
	if st := strings.TrimSpace(body.State); st != "" {
		stateID, fault := resolveStateID(meta, st)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		input["stateId"] = stateID
	}
	var data linearIssueCreateData
	if fault := s.exec(r.Context(), linearMutationIssueCreate, map[string]any{"input": input}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !data.IssueCreate.Success || data.IssueCreate.Issue == nil {
		writeProxyFault(w, faultf(http.StatusBadGateway, "linear_failed",
			"Linear reported the issue was not created"))
		return
	}
	if fault := s.enforceIssueTeam(data.IssueCreate.Issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, "issue.create", data.IssueCreate.Issue, "issue="+data.IssueCreate.Issue.Identifier)
}

// handleLinearProxyIssueUpdate serves POST /v1/linear/issue/update.
//
// Only title, state and priority can be changed — the same deliberate
// narrowness as the GitHub half's `pr edit`. Reassigning a team would move an
// issue out of the allow-list, and reassigning an owner is a workspace
// decision rather than a coding one.
func handleLinearProxyIssueUpdate(w http.ResponseWriter, r *http.Request) {
	var body linearUpdateRequest
	s, ok := openLinearProxy(w, r, PermLinearWrite, &body)
	if !ok {
		return
	}
	id, fault := validateLinearIdentifier(body.Identifier)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.requireAllowedTeam(teamKeyOf(id)); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	title := strings.TrimSpace(body.Title)
	state := strings.TrimSpace(body.State)
	if title == "" && state == "" && body.Priority == nil {
		writeProxyFault(w, faultf(http.StatusBadRequest, "invalid_arg",
			"nothing to update — pass a title, a state, or a priority"))
		return
	}
	if title != "" {
		if fault := validateLinearTitle(title); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	}
	if body.Priority != nil {
		if fault := validateLinearPriority(*body.Priority); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	}
	// Confirm the issue's real team before writing, and pick up the team key
	// the state name has to be resolved within.
	var issue linearIssueData
	if fault := s.exec(r.Context(), linearQueryIssue, map[string]any{"id": id}, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueTeam(issue.Issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	input := map[string]any{}
	if title != "" {
		input["title"] = title
	}
	if body.Priority != nil {
		input["priority"] = *body.Priority
	}
	if state != "" {
		meta, fault := s.resolveTeamMeta(r.Context(), issue.Issue.Team.Key)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		stateID, fault := resolveStateID(meta, state)
		if fault != nil {
			writeProxyFault(w, fault)
			return
		}
		input["stateId"] = stateID
	}
	// Mutate by the UUID the confirming read returned, not by the identifier.
	// Linear documents commentCreate and attachmentLinkURL as accepting an
	// identifier; issueUpdate carries no such promise, and the read that has
	// already happened makes relying on one unnecessary.
	target := strings.TrimSpace(issue.Issue.ID)
	if target == "" {
		writeProxyFault(w, faultf(http.StatusBadGateway, "linear_failed",
			"the Linear response carried no issue id; refusing to guess one for a write"))
		return
	}
	var data linearIssueUpdateData
	if fault := s.exec(r.Context(), linearMutationIssueUpdate, map[string]any{
		"id": target, "input": input,
	}, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !data.IssueUpdate.Success || data.IssueUpdate.Issue == nil {
		writeProxyFault(w, faultf(http.StatusBadGateway, "linear_failed",
			"Linear reported the issue was not updated"))
		return
	}
	// Check the mutation's own answer too, as `issue create` does. The
	// pre-write read already established the team, so this is belt and
	// braces — but an asymmetry here is the kind that survives a refactor of
	// the read and quietly stops being covered.
	if fault := s.enforceIssueTeam(data.IssueUpdate.Issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	s.respond(w, r, "issue.update", data.IssueUpdate.Issue, "issue="+id)
}

// handleLinearProxyIssueLink serves POST /v1/linear/issue/link — attaching a
// URL (in practice a pull request) to the ticket it implements. This is the
// step that closes the loop after `tclaude proxy github pr create`.
func handleLinearProxyIssueLink(w http.ResponseWriter, r *http.Request) {
	var body linearLinkRequest
	s, ok := openLinearProxy(w, r, PermLinearWrite, &body)
	if !ok {
		return
	}
	id, fault := validateLinearIdentifier(body.Identifier)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.requireAllowedTeam(teamKeyOf(id)); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	url, fault := validateLinearAttachmentURL(body.URL)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	title := strings.TrimSpace(body.Title)
	if title != "" {
		if fault := validateLinearTitle(title); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	}
	var issue linearIssueData
	if fault := s.exec(r.Context(), linearQueryIssue, map[string]any{"id": id}, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueTeam(issue.Issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	vars := map[string]any{"issueId": id, "url": url}
	if title != "" {
		vars["title"] = title
	}
	var data linearAttachmentLinkData
	if fault := s.exec(r.Context(), linearMutationAttachmentLink, vars, &data); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !data.AttachmentLinkURL.Success {
		writeProxyFault(w, faultf(http.StatusBadGateway, "linear_failed",
			"Linear reported the link was not attached"))
		return
	}
	s.respond(w, r, "issue.link", data.AttachmentLinkURL, "issue="+id)
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

// optionalTeam validates and allow-list-checks a `--team` value, treating
// empty as "every allow-listed team".
func optionalTeam(s *linearProxySession, raw string) (string, *proxyFault) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return s.validateLinearTeam(raw)
}

// teamOrAll renders a team for the audit detail.
func teamOrAll(team string) string {
	if team == "" {
		return "*"
	}
	return team
}
