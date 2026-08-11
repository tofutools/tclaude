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
	// The project, milestone, assignee and label names, shared with `issue
	// update` so the two verbs cannot drift apart on what they accept. A create
	// has nothing to leave alone, so its handler folds an empty value into an
	// absent one — see normalizedForCreate.
	linearIssueNameFields
}

type linearUpdateRequest struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state,omitempty"`
	// Priority is a pointer because 0 is a meaningful value ("no priority"),
	// so absent and "set it to none" have to be distinguishable.
	Priority *int `json:"priority,omitempty"`
	// Description is a pointer for the same reason the fields below are: an
	// empty description is a real state ("this ticket has no body"), so absent
	// and "clear it" have to be told apart. Title and State stay plain strings
	// because neither can be cleared — a title is required and a state is one
	// of the team's own.
	Description *string `json:"description,omitempty"`
	linearIssueNameFields
}

type linearLinkRequest struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
}

// normalizedForCreate drops the empty values a create has no meaning for.
//
// On `issue update` an empty value is the CLEAR — "this issue should have no
// project" — and it reaches Linear as an explicit null. A new issue has nothing
// to clear, so the same spelling there would send a null that says exactly what
// omitting the field says. Folding it here keeps that difference in one place
// rather than in every resolver.
func (b linearCreateRequest) normalizedForCreate() linearIssueNameFields {
	f := b.linearIssueNameFields
	if f.Project != nil && strings.TrimSpace(*f.Project) == "" {
		f.Project = nil
	}
	if f.Milestone != nil && strings.TrimSpace(*f.Milestone) == "" {
		f.Milestone = nil
	}
	if f.Assignee != nil && strings.TrimSpace(*f.Assignee) == "" {
		f.Assignee = nil
	}
	if f.Labels != nil && len(trimmedNonEmpty(*f.Labels)) == 0 {
		f.Labels = nil
	}
	return f
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

// linearTeamClause builds the team half of an issue filter, over exactly the
// teams ONE call may see.
//
// It is the ONLY place a team constraint is constructed, and every list-shaped
// verb goes through it, so an unfiltered listing is not something a handler can
// produce by omission. Its input is a team list rather than the session's whole
// set because a team-spanning read is now one call per workspace: each call may
// only ask about the teams the credential it spends actually reaches — see
// scanTargets.
//
// eqIgnoreCase rather than `in`: Linear's StringComparator has no
// case-insensitive list form, and the effective set is stored lower-cased while
// Linear's team keys are upper-case.
func linearTeamClause(teams []string) map[string]any {
	if len(teams) == 0 {
		// Unreachable: every scan target carries at least one team. Spelled out
		// anyway because the alternative shapes — an empty `or`, or an omitted
		// clause — are "match everything" in a filter whose whole job is to
		// match something narrower. A team key is never empty, so this matches
		// nothing.
		return map[string]any{"team": map[string]any{"key": map[string]any{"eq": ""}}}
	}
	if len(teams) == 1 {
		return map[string]any{"team": map[string]any{"key": map[string]any{"eqIgnoreCase": teams[0]}}}
	}
	alternatives := make([]any, 0, len(teams))
	for _, key := range teams {
		alternatives = append(alternatives,
			map[string]any{"team": map[string]any{"key": map[string]any{"eqIgnoreCase": key}}})
	}
	return map[string]any{"or": alternatives}
}

// linearIssueFilter assembles a complete IssueFilter. Clauses are ANDed
// explicitly through `and` rather than relying on Linear's implicit
// same-level conjunction, so the team constraint's relationship to the others
// is stated in the request instead of inferred from filter semantics.
func linearIssueFilter(teams []string, state string, assignedMe bool) map[string]any {
	clauses := []any{linearTeamClause(teams)}
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

// linearScan is one call of a team-spanning read: the credential to spend and
// the teams to ask it about.
type linearScan struct {
	route *linearRoute
	teams []string
}

// scanTargets turns a verb's optional --team into the calls it has to make.
//
// A named team is one call against the one credential that reaches it. An
// unnamed team is one call PER WORKSPACE the caller's effective set spans —
// ordinarily still one, because ordinarily every allowed team lives in the
// workspace the operator's single key belongs to.
//
// The fan-out is sequential and shares the request's whole budget, which is
// what maxLinearFanout bounds. Concurrency would spend several of the
// operator's credentials at once against a rate limit they share; a handful of
// serial calls stays inside the window the CLI waits on.
func (s *linearProxySession) scanTargets(team string) ([]linearScan, *proxyFault) {
	if team != "" {
		rt, fault := s.routeFor(team)
		if fault != nil {
			return nil, fault
		}
		return []linearScan{{route: rt, teams: []string{team}}}, nil
	}
	routes, fault := s.fanoutRoutes()
	if fault != nil {
		return nil, fault
	}
	targets := make([]linearScan, 0, len(routes))
	for _, rt := range routes {
		targets = append(targets, linearScan{route: rt, teams: rt.teams})
	}
	return targets, nil
}

// mergeByUpdated merges the per-workspace results of `issue ls` and bounds them
// to what the caller asked for.
//
// Each call asked for `first: limit` rows within its own workspace, so N
// workspaces can return N*limit rows between them and the caller must still get
// the limit rows it asked for — the MOST RECENT ones, which is what the query's
// `orderBy: updatedAt` promises within one workspace and what this restores
// across several. Timestamps are RFC 3339 and so sort correctly as strings.
//
// A single workspace keeps Linear's own order untouched: re-sorting a result
// that is already ordered could only introduce differences, never fix any.
func mergeByUpdated(groups [][]linearIssue, limit int) []linearIssue {
	if len(groups) == 1 {
		return truncateIssues(groups[0], limit)
	}
	// Non-nil even when every group is empty. enforceIssueList never returns
	// nil, so a single-workspace listing with no matches has always rendered as
	// `[]`; a nil here would render the same empty answer as `null` on a
	// multi-workspace daemon, making the response's JSON type depend on
	// operator configuration the agent cannot see.
	merged := make([]linearIssue, 0, limit)
	for _, g := range groups {
		merged = append(merged, g...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].UpdatedAt > merged[j].UpdatedAt
	})
	return truncateIssues(merged, limit)
}

// mergeByRelevance merges the per-workspace results of `issue search`.
//
// Unlike a listing there is no field to re-sort on: relevance is Linear's own
// ranking within ONE response, and two responses' ranks are not comparable. So
// the merge takes turns instead — each workspace's best result, then each
// workspace's second, and so on — which keeps every workspace represented in a
// bounded result rather than letting the first one fill it.
func mergeByRelevance(groups [][]linearIssue, limit int) []linearIssue {
	if len(groups) == 1 {
		return truncateIssues(groups[0], limit)
	}
	// Non-nil for the same reason mergeByUpdated's is.
	merged := make([]linearIssue, 0, limit)
	for round := 0; len(merged) < limit; round++ {
		progressed := false
		for _, g := range groups {
			if round >= len(g) {
				continue
			}
			progressed = true
			merged = append(merged, g[round])
			if len(merged) == limit {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return merged
}

// truncateIssues bounds a result set to the limit the caller asked for.
func truncateIssues(issues []linearIssue, limit int) []linearIssue {
	if len(issues) > limit {
		return issues[:limit]
	}
	return issues
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

// gateIssueTeam is the pre-call half of the team gate for an identifier-shaped
// verb: it checks the team the caller named against the effective set and
// returns the credential that reaches it.
//
// The two answers come together deliberately. Deciding that a team may be
// reached and then reaching it with whichever key happened to be at hand would
// query a workspace the team does not live in, and Linear answers that with
// "entity not found" — a refusal wearing a typo's clothes.
func (s *linearProxySession) gateIssueTeam(id string) (*linearRoute, *proxyFault) {
	key := teamKeyOf(id)
	if fault := s.requireAllowedTeam(key); fault != nil {
		return nil, fault
	}
	return s.routeFor(key)
}

// resolveTeamMeta looks up a team's UUID and workflow states. Callers have
// already allow-list-checked the key, and pass the credential that reaches it.
func (s *linearProxySession) resolveTeamMeta(
	ctx context.Context, rt *linearRoute, key string,
) (*linearTeamMeta, *proxyFault) {
	var data linearTeamMetaData
	if fault := s.exec(ctx, rt, linearQueryTeamMeta, map[string]any{"key": key}, &data); fault != nil {
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
	// One call per credential — this verb spans teams, so it is bounded like
	// the listings are.
	routes, fault := s.fanoutRoutes()
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	// A route that fails is REPORTED rather than fatal: this is the verb an
	// operator runs to find out why something is refused, and "workspace acme's
	// key is unreadable" is the answer they came for — losing the workspaces
	// that do work would hide it.
	views := make([]linearWorkspaceView, 0, len(routes))
	var lastFault *proxyFault
	for _, rt := range routes {
		view := linearWorkspaceView{Name: rt.name, Routes: rt.teams, Teams: []linearTeamView{}}
		var data linearViewerData
		if fault := s.exec(r.Context(), rt, linearQueryViewer,
			map[string]any{"first": whoamiTeamPageSize}, &data); fault != nil {
			view.Error = fault.Code + ": " + fault.Msg
			lastFault = fault
			views = append(views, view)
			continue
		}
		viewer := data.Viewer
		view.Viewer = &viewer
		for _, t := range data.Teams.Nodes {
			view.Teams = append(view.Teams,
				linearTeamView{Key: t.Key, Name: t.Name, Allowed: s.teamAllowed(t.Key)})
		}
		view.TeamsTruncated = len(data.Teams.Nodes) >= whoamiTeamPageSize
		views = append(views, view)
	}
	// With ONE credential a failure is the whole answer, so it is raised as the
	// fault it is — the response this verb has always given. With several, the
	// breakdown IS the answer even when every one of them failed: "both keys are
	// broken, here is which and why" is precisely what the operator ran this to
	// find out, and collapsing it to the last route's fault would send them to
	// fix one key and be refused again by the other.
	if len(views) == 1 && lastFault != nil {
		writeProxyFault(w, lastFault)
		return
	}
	// viewer/teams describe ONE workspace, so they are reported only when there
	// is one — which is the ordinary case, and keeps its response exactly what
	// it has always been, empty team list included. With several credentials in
	// play there is no single viewer to name, and workspaces carries the
	// breakdown instead.
	//
	// teams is a POINTER so that "this key sees no teams" (`[]`) stays
	// distinguishable from "there is no single key to report" (absent), the way
	// `issue update`'s priority distinguishes absent from zero. omitempty on a
	// plain slice would collapse the two.
	var (
		viewer         *linearUserRef
		teams          *[]linearTeamView
		teamsTruncated bool
	)
	if len(views) == 1 {
		viewer, teams, teamsTruncated = views[0].Viewer, &views[0].Teams, views[0].TeamsTruncated
	}
	s.respond(w, r, "whoami", struct {
		Viewer *linearUserRef    `json:"viewer,omitempty"`
		Teams  *[]linearTeamView `json:"teams,omitempty"`
		// Workspaces is the per-credential breakdown: who each key
		// authenticates as, which teams it can see, and which of the caller's
		// teams it is the credential for. Always present, so an agent has one
		// shape to read whether the operator configured one key or several.
		Workspaces []linearWorkspaceView `json:"workspaces"`
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
		Viewer:         viewer,
		Teams:          teams,
		Workspaces:     views,
		AllowedTeams:   s.teams,
		OperatorTeams:  s.policy.AllowedTeams,
		GrantTeams:     s.grantTeams,
		WriteAllowed:   s.policy.AllowWrite,
		TeamsTruncated: teamsTruncated,
	}, fmt.Sprintf("workspaces=%d", len(views)))
}

// linearTeamView is one team `whoami` reports, with the gate's verdict on it.
type linearTeamView struct {
	Key     string `json:"key"`
	Name    string `json:"name,omitempty"`
	Allowed bool   `json:"allowed"`
}

// linearWorkspaceView is `whoami`'s per-credential report.
type linearWorkspaceView struct {
	// Name is the operator's workspace label, or "default" for the key every
	// team no workspaces entry claims is reached with.
	Name string `json:"name"`
	// Routes is the caller's own teams this credential is used for — the
	// answer to "which key answers when I ask about TCL-1".
	Routes []string `json:"routes,omitempty"`
	// Viewer is who this key authenticates as; absent when the call failed.
	Viewer *linearUserRef `json:"viewer,omitempty"`
	// Teams is every team this key can see, whether allow-listed or not, so an
	// agent can tell its operator what there is to allow-list.
	Teams          []linearTeamView `json:"teams"`
	TeamsTruncated bool             `json:"teams_truncated,omitempty"`
	// Error is why this credential reported nothing — an unreadable key file,
	// a key Linear rejected. The other workspaces are still reported.
	Error string `json:"error,omitempty"`
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
	rt, fault := s.gateIssueTeam(id)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearIssueData
	if fault := s.exec(r.Context(), rt, linearQueryIssue, map[string]any{"id": id}, &data); fault != nil {
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
	targets, fault := s.scanTargets(team)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	groups := make([][]linearIssue, 0, len(targets))
	for _, sc := range targets {
		var data linearIssuesData
		if fault := s.exec(r.Context(), sc.route, linearQueryIssues, map[string]any{
			"filter": linearIssueFilter(sc.teams, state, body.AssignedMe),
			"first":  limit,
		}, &data); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		groups = append(groups, s.enforceIssueList(data.Issues.Nodes))
	}
	issues := mergeByUpdated(groups, limit)
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
	targets, fault := s.scanTargets(team)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	groups := make([][]linearIssue, 0, len(targets))
	for _, sc := range targets {
		var data linearSearchData
		if fault := s.exec(r.Context(), sc.route, linearQuerySearch, map[string]any{
			"term":   term,
			"filter": linearIssueFilter(sc.teams, "", false),
			"first":  limit,
		}, &data); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		groups = append(groups, s.enforceIssueList(data.SearchIssues.Nodes))
	}
	issues := mergeByRelevance(groups, limit)
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
	rt, fault := s.gateIssueTeam(id)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	limit, fault := validateLinearLimit(body.Limit)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearCommentsData
	if fault := s.exec(r.Context(), rt, linearQueryIssueComments, map[string]any{
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
	rt, fault := s.gateIssueTeam(id)
	if fault != nil {
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
	if fault := s.exec(r.Context(), rt, linearQueryIssue, map[string]any{"id": id}, &issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.enforceIssueTeam(issue.Issue); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearCommentCreateData
	if fault := s.exec(r.Context(), rt, linearMutationCommentCreate, map[string]any{
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
	// The credential for the workspace this team lives in. `issue create` is
	// the one write that names a team rather than an issue, so it resolves its
	// route here instead of through gateIssueTeam.
	rt, fault := s.routeFor(team)
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
	meta, fault := s.resolveTeamMeta(r.Context(), rt, team)
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
	// The team key comes from Linear's own answer rather than from body.Team:
	// resolveTeamMeta re-checks it against the allow-list, so this is the key
	// the gate actually approved, and it is the one the project and label
	// lookups have to be scoped to.
	// An empty placement: a new issue sits nowhere yet, so there is no project
	// to resolve a milestone within and no milestone to strand.
	if fault := s.applyIssueNameFields(
		r.Context(), rt, meta.Key, linearIssuePlacement{}, body.normalizedForCreate(), input,
	); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearIssueCreateData
	if fault := s.exec(r.Context(), rt, linearMutationIssueCreate, map[string]any{"input": input}, &data); fault != nil {
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
// It changes the fields a coding agent legitimately owns on the ticket it is
// working: title, description, state, priority, project, milestone, assignee
// and labels. What stays out is the TEAM — moving an issue between teams would
// carry it out of the allow-list the whole gate is built on, so it is the one
// field this verb will not touch. There is still no delete and no archive.
//
// Whichever field the caller omits is left alone. The name-shaped fields
// additionally distinguish "clear it" from "leave it", which is what the
// pointers on linearUpdateRequest are for.
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
	rt, fault := s.gateIssueTeam(id)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	title := strings.TrimSpace(body.Title)
	state := strings.TrimSpace(body.State)
	if title == "" && state == "" && body.Priority == nil &&
		body.Description == nil && !body.any() {
		writeProxyFault(w, faultf(http.StatusBadRequest, "invalid_arg",
			"nothing to update — pass a title, description, state, priority, project, milestone, "+
				"assignee or label set"))
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
	if body.Description != nil {
		// Not `required`: an empty description is the clear, which is a real
		// thing to ask for on an update even though it is not on a create.
		if fault := validateLinearBody(*body.Description, false); fault != nil {
			writeProxyFault(w, fault)
			return
		}
	}
	// Confirm the issue's real team before writing, and pick up the team key
	// the state name has to be resolved within.
	var issue linearIssueData
	if fault := s.exec(r.Context(), rt, linearQueryIssue, map[string]any{"id": id}, &issue); fault != nil {
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
	if body.Description != nil {
		// Sent as-is, including the empty string: Linear reads that as "no
		// description" rather than as "leave it", which is precisely the clear
		// the caller asked for. Trimming here would silently turn a description
		// of whitespace into a clear the caller did not ask for.
		input["description"] = *body.Description
	}
	if body.Priority != nil {
		input["priority"] = *body.Priority
	}
	if state != "" {
		// Still the credential that found the issue, even though the team is
		// the one Linear reported rather than the one the caller named. A
		// Linear team cannot move between workspaces, so the issue's real team
		// is in the workspace this key just read it from — and asking a
		// different key about it would be asking a workspace that has never
		// heard of it.
		meta, fault := s.resolveTeamMeta(r.Context(), rt, issue.Issue.Team.Key)
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
	// Same credential and same team as the state resolution above, and for the
	// same reason: the issue's real team is the one Linear just reported, and it
	// lives in the workspace this key read it from.
	//
	// The issue's current placement comes off the confirming read, so
	// `--milestone` without `--project` resolves within the project the issue is
	// already in, and a call that moves it elsewhere can deal with the milestone
	// it leaves behind.
	if fault := s.applyIssueNameFields(r.Context(), rt, issue.Issue.Team.Key,
		placementOf(issue.Issue), body.linearIssueNameFields, input); fault != nil {
		writeProxyFault(w, fault)
		return
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
	if fault := s.requireMutationBudget(); fault != nil {
		writeProxyFault(w, fault)
		return
	}
	var data linearIssueUpdateData
	if fault := s.exec(r.Context(), rt, linearMutationIssueUpdate, map[string]any{
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
	rt, fault := s.gateIssueTeam(id)
	if fault != nil {
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
	if fault := s.exec(r.Context(), rt, linearQueryIssue, map[string]any{"id": id}, &issue); fault != nil {
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
	if fault := s.exec(r.Context(), rt, linearMutationAttachmentLink, vars, &data); fault != nil {
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

// placementOf is where an issue sits before an update: its project and the
// milestone within it, each empty when it has none.
//
// Only `issue update` reads it, and only linearQueryIssue selects the two ids
// it is built from — which is why that read is the one every write verb already
// performs.
func placementOf(issue *linearIssue) linearIssuePlacement {
	if issue == nil {
		return linearIssuePlacement{}
	}
	var placement linearIssuePlacement
	if issue.Project != nil {
		placement.ProjectID = strings.TrimSpace(issue.Project.ID)
		placement.ProjectName = strings.TrimSpace(issue.Project.Name)
	}
	if issue.ProjectMilestone != nil {
		placement.MilestoneID = strings.TrimSpace(issue.ProjectMilestone.ID)
	}
	return placement
}

// teamOrAll renders a team for the audit detail.
func teamOrAll(team string) string {
	if team == "" {
		return "*"
	}
	return team
}
