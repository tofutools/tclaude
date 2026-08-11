package agentd

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// linearproxy_resolve.go turns the NAMES an agent knows — "tclaude", "v2",
// "mikael", "bug" — into the UUIDs Linear's issue mutations actually take.
//
// It exists because `IssueCreateInput` and `IssueUpdateInput` accept
// `projectId`, `projectMilestoneId`, `assigneeId` and `labelIds` and nothing
// else. An agent has no way to obtain a UUID: the proxy deliberately refuses
// them as issue references, and nothing else it returns carries one it could
// reuse. So the daemon does the lookup, from the vocabulary a ticket is
// discussed in.
//
// Three rules hold for every resolver here, and they are the same three
// resolveStateID has followed since the proxy shipped:
//
//  1. MATCHING IS EXACT, case-insensitively, and never fuzzy. A near-match
//     would put the ticket in the wrong project or hang the wrong label on it
//     — silently, under the operator's name.
//
//  2. AMBIGUITY IS A REFUSAL, not a choice. Two things sharing a name means
//     the daemon cannot know which was meant, and the refusal lists what it
//     found so the agent can say something specific to its human.
//
//  3. THE TEAM GATE IS UPSTREAM. Every resolver is handed a team key that has
//     already passed requireAllowedTeam, and the credential that reaches it.
//     Nothing here widens what a caller may touch; it only names things inside
//     what it already may.
//
// The one deliberate exception to rule 2 is a label that exists both as a
// team label and as a workspace-wide one. That collision is ordinary in a real
// workspace rather than a mistake, and the team's own label is unambiguously
// the more specific answer — so it wins, and only a tie WITHIN one of those two
// scopes is refused. See resolveLabelIDs.

const (
	// maxLinearNameLen bounds a project, milestone, label or assignee name.
	// These are short human labels, like a workflow-state name; the bound is
	// far past any real one and exists so a pathological string is refused
	// before it reaches a filter.
	maxLinearNameLen = 256

	// maxLinearIssueLabels bounds how many labels one call may set. It matches
	// the `labels(first: 20)` selection `issue view` reads back with, so the
	// proxy cannot set a label set it would then fail to show.
	maxLinearIssueLabels = 20

	// linearResolvePageSize bounds a resolution query. Every one of them filters
	// on an exact name, so a handful of rows is already an ambiguity and a full
	// page means something is very wrong — which is why a full page is refused
	// rather than resolved from. See requireCompletePage.
	linearResolvePageSize = 100
)

// linearIssueNameFields is the name-shaped issue fields `issue create` and
// `issue update` share. It is embedded in both request types, so the two verbs
// cannot drift apart on what they accept or on how it is spelled.
//
// Every field is a POINTER (or a pointer to a slice) because three states have
// to be distinguishable, and only on `issue update` do all three arise:
//
//	absent          → leave whatever the issue has alone
//	present, empty  → clear it: Linear takes an explicit null
//	present, set    → resolve the name and set it
//
// `issue create` has nothing to leave alone, so its handler folds the empty
// case into the absent one before these ever reach a resolver.
type linearIssueNameFields struct {
	Project   *string   `json:"project,omitempty"`
	Milestone *string   `json:"milestone,omitempty"`
	Assignee  *string   `json:"assignee,omitempty"`
	Labels    *[]string `json:"labels,omitempty"`
}

// any reports whether the caller asked for any of these fields at all — which
// is what lets `issue update` tell "nothing to update" from "clear the
// assignee".
func (f linearIssueNameFields) any() bool {
	return f.Project != nil || f.Milestone != nil || f.Assignee != nil || f.Labels != nil
}

// applyIssueNameFields resolves every name-shaped field the caller supplied and
// writes the resulting ids into a mutation input.
//
// currentProjectID is the project the issue is ALREADY in, empty on create and
// on an issue with no project. It matters for one case only: a milestone named
// without a project in the same call has to be resolved somewhere, and the
// issue's own project is the only defensible answer.
//
// The order is not arbitrary. Project resolves first because the milestone
// lookup depends on its answer, and a caller that moves an issue to a new
// project and names a milestone in one call means the milestone of the NEW
// project — resolving against the old one would either miss or, worse, find a
// same-named milestone there and attach the wrong one.
func (s *linearProxySession) applyIssueNameFields(
	ctx context.Context,
	rt *linearRoute,
	teamKey, currentProjectID string,
	f linearIssueNameFields,
	input map[string]any,
) *proxyFault {
	// projectID tracks the project the issue will be in once this mutation
	// lands, which is what a milestone has to belong to.
	projectID := currentProjectID
	if f.Project != nil {
		name := strings.TrimSpace(*f.Project)
		if name == "" {
			input["projectId"] = nil
			projectID = ""
		} else {
			if fault := validateLinearName(name, "project"); fault != nil {
				return fault
			}
			id, fault := s.resolveProjectID(ctx, rt, teamKey, name)
			if fault != nil {
				return fault
			}
			input["projectId"] = id
			projectID = id
		}
	}

	if f.Milestone != nil {
		name := strings.TrimSpace(*f.Milestone)
		if name == "" {
			input["projectMilestoneId"] = nil
		} else {
			if fault := validateLinearName(name, "milestone"); fault != nil {
				return fault
			}
			if projectID == "" {
				// Refuse rather than search the workspace for a milestone by
				// name: milestone names are unique only within a project, so a
				// workspace-wide lookup would be a coin toss between projects.
				return faultf(http.StatusBadRequest, "milestone_needs_project",
					"a milestone belongs to a project, and this issue would have none — "+
						"name the project in the same call, or set one on the issue first")
			}
			id, fault := s.resolveMilestoneID(ctx, rt, projectID, name)
			if fault != nil {
				return fault
			}
			input["projectMilestoneId"] = id
		}
	}

	if f.Assignee != nil {
		name := strings.TrimSpace(*f.Assignee)
		if name == "" {
			input["assigneeId"] = nil
		} else {
			if fault := validateLinearName(name, "assignee"); fault != nil {
				return fault
			}
			id, fault := s.resolveAssigneeID(ctx, rt, name)
			if fault != nil {
				return fault
			}
			input["assigneeId"] = id
		}
	}

	if f.Labels != nil {
		names := trimmedNonEmpty(*f.Labels)
		if len(names) == 0 {
			// An empty list is the clear, and it is a REPLACEMENT like any other
			// label set: Linear's labelIds is the whole set, not an addition.
			input["labelIds"] = []string{}
		} else {
			ids, fault := s.resolveLabelIDs(ctx, rt, teamKey, names)
			if fault != nil {
				return fault
			}
			input["labelIds"] = ids
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Project
// ---------------------------------------------------------------------------

// resolveProjectID maps a project NAME to its UUID, among the projects the
// gated team can reach.
//
// The team constraint is `accessibleTeams.some`, which is Linear's own model: a
// project belongs to one or more teams, and naming the caller's team keeps the
// lookup inside the gate rather than searching the whole workspace for a name.
func (s *linearProxySession) resolveProjectID(
	ctx context.Context, rt *linearRoute, teamKey, name string,
) (string, *proxyFault) {
	filter := map[string]any{"and": []any{
		map[string]any{"name": map[string]any{"eqIgnoreCase": name}},
		map[string]any{"accessibleTeams": map[string]any{
			"some": map[string]any{"key": map[string]any{"eqIgnoreCase": teamKey}},
		}},
	}}
	var data linearProjectsData
	if fault := s.exec(ctx, rt, linearQueryProject, map[string]any{
		"filter": filter, "first": linearResolvePageSize,
	}, &data); fault != nil {
		return "", fault
	}
	nodes := data.Projects.Nodes
	if fault := requireCompletePage(len(nodes), "project", name); fault != nil {
		return "", fault
	}
	switch len(nodes) {
	case 0:
		return "", faultf(http.StatusBadRequest, "unknown_project",
			"team %s has no project named %q that this key can see", teamKey, name)
	case 1:
		return nodes[0].ID, nil
	}
	// Linear does not enforce unique project names, so this is reachable. Say
	// how many rather than picking: two projects called "Q3" are two different
	// bodies of work.
	return "", faultf(http.StatusBadRequest, "ambiguous_project",
		"%d projects accessible to team %s are named %q; the proxy will not guess between them — "+
			"rename one, or ask the operator to set the project",
		len(nodes), teamKey, name)
}

// ---------------------------------------------------------------------------
// Milestone
// ---------------------------------------------------------------------------

// resolveMilestoneID maps a milestone NAME to its UUID within one project.
//
// projectID is required by construction — see applyIssueNameFields, which
// refuses before calling this rather than letting a name be matched against
// every project in the workspace.
func (s *linearProxySession) resolveMilestoneID(
	ctx context.Context, rt *linearRoute, projectID, name string,
) (string, *proxyFault) {
	filter := map[string]any{"and": []any{
		map[string]any{"name": map[string]any{"eqIgnoreCase": name}},
		map[string]any{"project": map[string]any{"id": map[string]any{"eq": projectID}}},
	}}
	var data linearMilestonesData
	if fault := s.exec(ctx, rt, linearQueryProjectMilestone, map[string]any{
		"filter": filter, "first": linearResolvePageSize,
	}, &data); fault != nil {
		return "", fault
	}
	nodes := data.ProjectMilestones.Nodes
	if fault := requireCompletePage(len(nodes), "milestone", name); fault != nil {
		return "", fault
	}
	switch len(nodes) {
	case 0:
		// The project is named rather than its UUID: the caller supplied a name
		// or the issue already carried one, and neither is a UUID they could
		// match this against.
		return "", faultf(http.StatusBadRequest, "unknown_milestone",
			"project %s has no milestone named %q", milestoneProjectLabel(nodes, projectID), name)
	case 1:
		return nodes[0].ID, nil
	}
	return "", faultf(http.StatusBadRequest, "ambiguous_milestone",
		"%d milestones in project %s are named %q; the proxy will not guess between them",
		len(nodes), milestoneProjectLabel(nodes, projectID), name)
}

// milestoneProjectLabel names the project in a milestone refusal: its human
// name when Linear returned one, otherwise the id the lookup used. The zero-row
// case has no name to use, which is exactly when the id is all there is.
func milestoneProjectLabel(nodes []linearMilestoneNode, projectID string) string {
	for _, n := range nodes {
		if n.Project != nil && strings.TrimSpace(n.Project.Name) != "" {
			return strconv.Quote(n.Project.Name)
		}
	}
	return strconv.Quote(projectID)
}

// ---------------------------------------------------------------------------
// Assignee
// ---------------------------------------------------------------------------

// resolveAssigneeID maps a person to their Linear user UUID.
//
// Three spellings are accepted — display name, full name, and email — because
// which of them an agent has depends on where it read the person's name.
// `whoami` reports the operator's own displayName, and `issue view` reports an
// assignee's, so the vocabulary a ticket is discussed in is covered without the
// agent needing to know which field it is looking at.
//
// A deactivated account is dropped when an active one also matches, and only
// then. Names get reused as people leave and join, and a former colleague
// should not make a current one unassignable — but a workspace where the only
// match is deactivated still gets a specific answer rather than "no such user".
//
// This is the one resolver NOT scoped to a team, deliberately and of necessity.
// Linear's UserFilter has no team dimension, and a person is not a team's
// property anyway: assigning a TCL ticket to a reviewer who mostly works in
// another team is an ordinary thing to want. What the caller can do with that
// is unchanged — the issue it is assigning is still one the team gate approved.
// What it can LEARN is the narrow thing this widens: whether a user with an
// exactly-matching name or email exists. That is why a miss says only that,
// rather than listing the workspace.
func (s *linearProxySession) resolveAssigneeID(
	ctx context.Context, rt *linearRoute, name string,
) (string, *proxyFault) {
	filter := map[string]any{"or": []any{
		map[string]any{"displayName": map[string]any{"eqIgnoreCase": name}},
		map[string]any{"name": map[string]any{"eqIgnoreCase": name}},
		map[string]any{"email": map[string]any{"eqIgnoreCase": name}},
	}}
	var data linearUsersData
	if fault := s.exec(ctx, rt, linearQueryUsers, map[string]any{
		"filter": filter, "first": linearResolvePageSize,
	}, &data); fault != nil {
		return "", fault
	}
	nodes := data.Users.Nodes
	if fault := requireCompletePage(len(nodes), "user", name); fault != nil {
		return "", fault
	}
	if len(nodes) == 0 {
		// The workspace's user list is deliberately NOT offered here. It would
		// turn a misspelling into a directory dump, and the agent's own human
		// can read the name off Linear far more cheaply than the proxy can
		// justify enumerating it.
		return "", faultf(http.StatusBadRequest, "unknown_assignee",
			"no Linear user matches %q by display name, name or email", name)
	}
	if active := activeUsers(nodes); len(active) > 0 {
		nodes = active
	}
	if len(nodes) == 1 {
		return nodes[0].ID, nil
	}
	return "", faultf(http.StatusBadRequest, "ambiguous_assignee",
		"%q matches %d Linear users (%s); use the email address instead",
		name, len(nodes), strings.Join(userLabels(nodes), ", "))
}

// activeUsers is the subset of rows Linear still reports as active.
func activeUsers(nodes []linearUserNode) []linearUserNode {
	out := make([]linearUserNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Active {
			out = append(out, n)
		}
	}
	return out
}

// userLabels renders matched users for an ambiguity refusal. The email is what
// makes the next attempt unambiguous, so it leads when there is one.
func userLabels(nodes []linearUserNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		switch {
		case strings.TrimSpace(n.Email) != "":
			out = append(out, n.Email)
		case strings.TrimSpace(n.DisplayName) != "":
			out = append(out, n.DisplayName)
		default:
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

// resolveLabelIDs maps label NAMES to their UUIDs for one team, in ONE call.
//
// The filter ORs an exact-name clause per requested name and ANDs the team
// scope over all of them, so a six-label issue costs the same single call a
// one-label issue does. Matching is then done here rather than by trusting the
// row order, because a name that matched nothing has to be named in the refusal
// and Linear's answer says only what it found.
//
// The team clause admits BOTH the team's own labels and the workspace-wide ones
// (`team: {null: true}`), because Linear has both and an agent saying "bug"
// does not know or care which kind it is. Label groups are excluded: a group is
// a container, not something an issue can carry.
func (s *linearProxySession) resolveLabelIDs(
	ctx context.Context, rt *linearRoute, teamKey string, names []string,
) ([]string, *proxyFault) {
	if len(names) > maxLinearIssueLabels {
		return nil, faultf(http.StatusBadRequest, "invalid_arg",
			"%d labels were given; at most %d may be set on one issue", len(names), maxLinearIssueLabels)
	}
	nameClauses := make([]any, 0, len(names))
	for _, name := range names {
		if fault := validateLinearName(name, "label"); fault != nil {
			return nil, fault
		}
		nameClauses = append(nameClauses,
			map[string]any{"name": map[string]any{"eqIgnoreCase": name}})
	}
	filter := map[string]any{"and": []any{
		map[string]any{"or": nameClauses},
		map[string]any{"or": []any{
			map[string]any{"team": map[string]any{"key": map[string]any{"eqIgnoreCase": teamKey}}},
			map[string]any{"team": map[string]any{"null": true}},
		}},
		map[string]any{"isGroup": map[string]any{"eq": false}},
	}}

	var data linearLabelsData
	if fault := s.exec(ctx, rt, linearQueryIssueLabels, map[string]any{
		"filter": filter, "first": linearResolvePageSize,
	}, &data); fault != nil {
		return nil, fault
	}
	nodes := data.IssueLabels.Nodes
	if fault := requireCompletePage(len(nodes), "label", strings.Join(names, ", ")); fault != nil {
		return nil, fault
	}

	// Group the rows by lower-cased name once, so each requested name is decided
	// against everything Linear returned for it rather than against the first
	// row that happened to match.
	byName := make(map[string][]linearLabelNode, len(names))
	for _, node := range nodes {
		key := strings.ToLower(strings.TrimSpace(node.Name))
		byName[key] = append(byName[key], node)
	}

	var (
		ids     = make([]string, 0, len(names))
		missing []string
		seen    = make(map[string]bool, len(names))
	)
	for _, name := range names {
		key := strings.ToLower(name)
		matches := byName[key]
		if len(matches) == 0 {
			missing = append(missing, name)
			continue
		}
		id, fault := pickLabel(matches, teamKey, name)
		if fault != nil {
			return nil, fault
		}
		// The same label named twice — "Bug" and "bug" — is one label. Linear
		// would take the duplicate id without complaint, but a set with a
		// repeated member is not what the caller described.
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(missing) > 0 {
		return nil, faultf(http.StatusBadRequest, "unknown_label",
			"team %s has no label named %s; labels are matched exactly (case-insensitively) and are "+
				"not created on demand", teamKey, strings.Join(quoteAll(missing), ", "))
	}
	return ids, nil
}

// pickLabel decides one requested name against every label Linear returned for
// it.
//
// A team label beats a workspace-wide one of the same name. That is the one
// place these resolvers prefer rather than refuse, and it is not a guess: a
// team that has defined its own "bug" has said what "bug" means for its issues,
// and the workspace default is the thing it was defined instead of. Any other
// tie — two team labels, or two workspace labels, which happens when they sit
// in different label groups — is refused with the groups named, because nothing
// distinguishes them.
func pickLabel(matches []linearLabelNode, teamKey, name string) (string, *proxyFault) {
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	scoped := make([]linearLabelNode, 0, len(matches))
	for _, m := range matches {
		if m.Team != nil && strings.EqualFold(strings.TrimSpace(m.Team.Key), teamKey) {
			scoped = append(scoped, m)
		}
	}
	if len(scoped) == 1 {
		return scoped[0].ID, nil
	}
	if len(scoped) > 1 {
		matches = scoped
	}
	return "", faultf(http.StatusBadRequest, "ambiguous_label",
		"%d labels are named %q for team %s (%s); the proxy will not guess between them",
		len(matches), name, teamKey, strings.Join(labelGroupLabels(matches), ", "))
}

// labelGroupLabels describes colliding labels by the group they sit in, which
// is the only thing that tells them apart in Linear's own UI.
func labelGroupLabels(matches []linearLabelNode) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		switch {
		case m.Parent != nil && strings.TrimSpace(m.Parent.Name) != "":
			out = append(out, "in group "+strconv.Quote(m.Parent.Name))
		case m.Team != nil && strings.TrimSpace(m.Team.Key) != "":
			out = append(out, "on team "+m.Team.Key)
		default:
			out = append(out, "workspace-wide")
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// validateLinearName bounds a project, milestone, label or assignee name.
//
// Same shape as validateLinearStateName, and for the same reason: these are
// short labels rather than prose, and a bound that does not describe what it
// bounds invites the next caller to reuse it where the difference matters.
func validateLinearName(name, what string) *proxyFault {
	name = strings.TrimSpace(name)
	if name == "" {
		return faultf(http.StatusBadRequest, "invalid_arg", "a %s name is required", what)
	}
	if utf8.RuneCountInString(name) > maxLinearNameLen {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"a %s name longer than %d characters is not one", what, maxLinearNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"the %s name contains a control character", what)
		}
	}
	return nil
}

// requireCompletePage refuses a resolution whose answer filled the page.
//
// Every resolver here filters on an exact name, so a full page cannot happen in
// a real workspace — but if it did, the rows Linear did not return could hold
// the very collision the resolver is checking for, and "exactly one match" would
// be a conclusion drawn from a truncated answer. Refusing keeps the resolvers'
// promise honest: they never resolve from a set they cannot see all of.
func requireCompletePage(rows int, what, name string) *proxyFault {
	if rows < linearResolvePageSize {
		return nil
	}
	return faultf(http.StatusBadGateway, "linear_failed",
		"looking up the %s %q returned %d or more rows, so the proxy cannot tell whether the match "+
			"is unique; this is not something to retry", what, name, linearResolvePageSize)
}

// trimmedNonEmpty drops blanks from a caller's list and trims what is left, so
// `--label ""` reads as "clear them" rather than as a label with no name.
func trimmedNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// quoteAll renders a list of names for a refusal message.
func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strconv.Quote(s))
	}
	return out
}
