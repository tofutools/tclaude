package agentd

// linearproxy_queries.go is the ENTIRE GraphQL surface of `tclaude proxy
// linear`, in one file so a reviewer can audit every operation the daemon will
// ever send in a single read. It is the counterpart of the ghPRListFields
// constants in githubproxy_handlers.go, doing rather more work: with GraphQL
// the field set IS the query.
//
// Two rules hold for everything below, and the security of this package rests
// on them:
//
//  1. EVERY DOCUMENT IS A COMPILE-TIME CONSTANT. No document is assembled from
//     a caller value, a fmt.Sprintf, or a struct walked by reflection. What the
//     daemon can be made to ask Linear is therefore fixed at build time.
//
//  2. EVERY CALLER VALUE TRAVELS IN `variables`. A GraphQL variable is a typed
//     JSON value substituted after the document is parsed, so it can change
//     what an operation is asked ABOUT but never which operation runs. This is
//     the property the git and GitHub proxies have to work for with charset
//     gates and leading-"-" refusals; here it is structural.
//
// Every issue-shaped selection includes `team { key }` even when the verb has
// no other use for it, because linearProxySession.enforceIssueTeam refuses any
// issue whose team it cannot check against the operator's allow-list. Removing
// that field from a selection does not weaken the gate quietly — it turns the
// verb into an error.
//
// TestLinearQueryDocumentsMatchLiveSchema validates each of these against
// Linear's real schema without a credential (Linear validates a document
// before it authenticates), so schema drift is caught rather than discovered
// in production.

// linearIssueSelection is the shared issue selection, written once so the
// view, list, search and mutation responses cannot drift apart, and so a field
// added here reaches every verb at once.
//
// It is a selection body rather than a whole fragment because it has to be
// spread onto TWO types: `searchIssues` returns IssueSearchResult, which
// carries the same fields as Issue but is a distinct type, so a fragment
// declared `on Issue` cannot be spread into a search result. (Linear's schema
// validator says so in as many words, and the drift test caught it.)
//
// Concatenating constants keeps the compile-time-constant invariant: `const a
// = b + c` is a constant expression in Go, so there is still no runtime
// assembly and no caller value anywhere near a document.
const linearIssueSelection = `
  identifier
  title
  url
  priority
  priorityLabel
  createdAt
  updatedAt
  branchName
  state { name type }
  assignee { displayName }
  team { key name }
`

// linearIssueFields is the selection as a fragment on Issue — used by every
// verb except search.
const linearIssueFields = `
fragment IssueFields on Issue {` + linearIssueSelection + `}`

// linearSearchResultFields is the same selection as a fragment on
// IssueSearchResult, the type `searchIssues` actually returns.
const linearSearchResultFields = `
fragment SearchResultFields on IssueSearchResult {` + linearIssueSelection + `}`

// linearQueryViewer backs `whoami`. It is the discovery verb — the analogue of
// `git remotes` — so it reports who the operator's key authenticates as and
// which teams that key can see, which is what an agent needs to understand a
// team_not_allowed refusal.
const linearQueryViewer = `
query Whoami($first: Int!) {
  viewer { name displayName }
  teams(first: $first) { nodes { key name } }
}`

// linearQueryIssue backs `issue view`. The description is included here and
// nowhere else: it is the body of the ticket, which is the point of viewing
// one, but it would dominate a list response.
//
// It also selects the issue's UUID, which is what `issue update` mutates by.
// Linear documents CommentCreateInput.issueId and attachmentLinkURL.issueId as
// accepting an identifier ("LIN-123") but says no such thing about
// issueUpdate.id — so rather than rely on an undocumented behaviour, the
// confirming read every write verb already performs hands over the UUID and
// the mutation uses that.
//
// The project and milestone UUIDs ride along for the same reason the issue's
// does. `issue update --milestone` has to resolve a milestone name within the
// issue's CURRENT project when the same call does not also set one, and a
// milestone name is only unique inside its project — so the confirming read is
// where that project id comes from rather than a second lookup.
const linearQueryIssue = linearIssueFields + `
query IssueView($id: String!) {
  issue(id: $id) {
    ...IssueFields
    id
    description
    estimate
    dueDate
    creator { displayName }
    parent { identifier title }
    project { id name }
    projectMilestone { id name }
    labels(first: 50) { nodes { name } }
  }
}`

// linearQueryIssues backs `issue ls`. The filter is a variable rather than an
// inline literal so the team allow-list, the assignee and the state all reach
// Linear as data — see linearIssueFilter in linearproxy_handlers.go, which is
// the only place that map is built.
const linearQueryIssues = linearIssueFields + `
query IssueList($filter: IssueFilter, $first: Int!) {
  issues(filter: $filter, first: $first, orderBy: updatedAt) {
    nodes { ...IssueFields }
  }
}`

// linearQuerySearch backs `issue search`.
//
// `searchIssues`, not the older `issueSearch`, which Linear's schema marks
// deprecated ("use `searchIssues` instead"). The same $filter carries the team
// allow-list, so a search cannot reach outside it.
const linearQuerySearch = linearSearchResultFields + `
query IssueSearch($term: String!, $filter: IssueFilter, $first: Int!) {
  searchIssues(term: $term, filter: $filter, first: $first) {
    nodes { ...SearchResultFields }
  }
}`

// linearQueryIssueComments backs `issue comments`. `team { key }` rides along
// so the allow-list can be enforced on the issue before any comment body is
// rendered — the comments are third-party prose and must not reach the agent
// from an issue it may not read.
const linearQueryIssueComments = `
query IssueComments($id: String!, $first: Int!) {
  issue(id: $id) {
    identifier
    title
    url
    team { key name }
    comments(first: $first) {
      nodes {
        body
        createdAt
        url
        user { displayName }
      }
    }
  }
}`

// linearQueryTeamMeta resolves a team key to the UUID and workflow states the
// mutations need. Linear's IssueCreateInput.teamId and IssueUpdateInput.stateId
// are UUIDs, while an agent naturally says "TCL" and "In Review", so this is
// the translation step — and it is also a second, independent confirmation
// that the team exists and is visible to the operator's key.
//
// eqIgnoreCase, matching linearTeamClause. `eq` would make `issue create
// --team foo` fail with "team does not exist" on a workspace whose key is not
// upper-case, while `issue ls --team foo` succeeded — two paths disagreeing
// about the same allow-listed team.
const linearQueryTeamMeta = `
query TeamMeta($key: String!) {
  teams(filter: { key: { eqIgnoreCase: $key } }, first: 1) {
    nodes {
      id
      key
      name
      states(first: 100) { nodes { id name type position } }
    }
  }
}`

// The four documents below all do the same job for a different entity: turn a
// NAME an agent can reasonably know into the UUID Linear's mutation inputs
// take. They exist because `IssueCreateInput`/`IssueUpdateInput` accept
// `projectId`, `projectMilestoneId`, `assigneeId` and `labelIds` and nothing
// else — an agent that had to supply those directly would need a UUID it can
// only get by reading something the proxy does not expose.
//
// Each takes its whole filter as a variable, built in linearproxy_resolve.go,
// for the same reason `issue ls` does: the team constraint and the names being
// matched are caller-influenced data, and data belongs in `variables`.
//
// All four select enough to DIAGNOSE a miss as well as resolve a hit — the
// team a label belongs to, the group it sits in, whether a user is still
// active — because these resolvers refuse on ambiguity rather than guessing,
// and a refusal has to say what it found.

// linearQueryProject resolves a project NAME within one team.
const linearQueryProject = `
query ProjectResolve($filter: ProjectFilter, $first: Int!) {
  projects(filter: $filter, first: $first) {
    nodes { id name }
  }
}`

// linearQueryProjectMilestone resolves a milestone NAME within one project.
//
// Milestone names are unique only inside a project, which is why the filter
// always carries one — see resolveMilestoneID for where that project comes from
// when the caller did not name one.
const linearQueryProjectMilestone = `
query MilestoneResolve($filter: ProjectMilestoneFilter, $first: Int!) {
  projectMilestones(filter: $filter, first: $first) {
    nodes { id name project { id name } }
  }
}`

// linearQueryIssueLabels resolves label NAMES for one team.
//
// One call for all of them, not one per name: the filter ORs an eqIgnoreCase
// clause per requested name, so a six-label issue costs the same single call a
// one-label issue does. `team { key name }` and `parent { name }` come back so
// a duplicate name can be told apart in a refusal, and so the team-scoped label
// can be preferred over the workspace-wide one.
const linearQueryIssueLabels = `
query LabelResolve($filter: IssueLabelFilter, $first: Int!) {
  issueLabels(filter: $filter, first: $first) {
    nodes {
      id
      name
      team { key name }
      parent { name }
    }
  }
}`

// linearQueryUsers resolves an assignee by display name, full name or email.
//
// `active` rides along so a deactivated account with the same name as a current
// one does not make the name permanently ambiguous — see resolveAssigneeID.
const linearQueryUsers = `
query UserResolve($filter: UserFilter, $first: Int!) {
  users(filter: $filter, first: $first) {
    nodes { id name displayName email active }
  }
}`

// linearMutationCommentCreate backs `issue comment` — the highest-value write,
// an agent reporting progress on its own ticket.
const linearMutationCommentCreate = `
mutation CommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
    comment { url createdAt }
  }
}`

// linearMutationIssueCreate backs `issue create`.
const linearMutationIssueCreate = linearIssueFields + `
mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { ...IssueFields }
  }
}`

// linearMutationIssueUpdate backs `issue update`.
const linearMutationIssueUpdate = linearIssueFields + `
mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    success
    issue { ...IssueFields }
  }
}`

// linearMutationAttachmentLink backs `issue link` — attaching a pull request
// to the ticket it implements, which is the step that closes the loop after
// `tclaude proxy github pr create`.
const linearMutationAttachmentLink = `
mutation AttachmentLink($issueId: String!, $url: String!, $title: String) {
  attachmentLinkURL(issueId: $issueId, url: $url, title: $title) {
    success
    attachment { title url }
  }
}`

// linearProxyDocuments is every document above, named. It exists so the
// schema-drift test can iterate the real surface rather than a hand-kept copy
// that could fall behind it.
var linearProxyDocuments = map[string]string{
	"viewer":            linearQueryViewer,
	"issue":             linearQueryIssue,
	"issues":            linearQueryIssues,
	"search":            linearQuerySearch,
	"comments":          linearQueryIssueComments,
	"teamMeta":          linearQueryTeamMeta,
	"project":           linearQueryProject,
	"projectMilestone":  linearQueryProjectMilestone,
	"issueLabels":       linearQueryIssueLabels,
	"users":             linearQueryUsers,
	"commentCreate":     linearMutationCommentCreate,
	"issueCreate":       linearMutationIssueCreate,
	"issueUpdate":       linearMutationIssueUpdate,
	"attachmentLinkURL": linearMutationAttachmentLink,
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------
//
// These mirror the selections above field for field. They are also what the
// CLI renders, so a field added to a document without a field added here is
// simply not shown — which is why the two live next to each other.

type linearTeamRef struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
}

type linearUserRef struct {
	DisplayName string `json:"displayName,omitempty"`
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
}

type linearStateRef struct {
	ID       string  `json:"id,omitempty"`
	Name     string  `json:"name"`
	Type     string  `json:"type,omitempty"`
	Position float64 `json:"position,omitempty"`
}

type linearLabelRef struct {
	Name string `json:"name"`
}

type linearIssueRef struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title,omitempty"`
}

// linearProjectRef is a project as an issue reports it. The UUID is selected
// only by linearQueryIssue, whose answer is what `issue update --milestone`
// resolves a milestone name within when the same call sets no project.
type linearProjectRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

// linearMilestoneRef is a project milestone as an issue reports it.
type linearMilestoneRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

// linearIssue is the shared issue shape. Every field is optional on the wire
// except identifier and team, which every selection asks for.
type linearIssue struct {
	// ID is Linear's UUID. Selected only by linearQueryIssue — the confirming
	// read every write verb performs — because issueUpdate is not documented
	// to accept an identifier the way commentCreate and attachmentLinkURL are.
	// List and search rows have no use for it and do not ask for it.
	ID            string            `json:"id,omitempty"`
	Identifier    string            `json:"identifier"`
	Title         string            `json:"title,omitempty"`
	URL           string            `json:"url,omitempty"`
	Description   string            `json:"description,omitempty"`
	Priority      float64           `json:"priority,omitempty"`
	PriorityLabel string            `json:"priorityLabel,omitempty"`
	Estimate      *float64          `json:"estimate,omitempty"`
	DueDate       string            `json:"dueDate,omitempty"`
	CreatedAt     string            `json:"createdAt,omitempty"`
	UpdatedAt     string            `json:"updatedAt,omitempty"`
	BranchName    string            `json:"branchName,omitempty"`
	State         *linearStateRef   `json:"state,omitempty"`
	Assignee      *linearUserRef    `json:"assignee,omitempty"`
	Creator       *linearUserRef    `json:"creator,omitempty"`
	Team          linearTeamRef     `json:"team"`
	Parent        *linearIssueRef   `json:"parent,omitempty"`
	Project       *linearProjectRef `json:"project,omitempty"`
	// ProjectMilestone is selected only by linearQueryIssue, so `issue view`
	// can read back what `--milestone` set. A milestone with no project cannot
	// exist, so this is nil whenever Project is.
	ProjectMilestone *linearMilestoneRef `json:"projectMilestone,omitempty"`
	Labels           *struct {
		Nodes []linearLabelRef `json:"nodes"`
	} `json:"labels,omitempty"`
}

// linearProjectNode is one row of linearQueryProject.
type linearProjectNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type linearProjectsData struct {
	Projects struct {
		Nodes []linearProjectNode `json:"nodes"`
	} `json:"projects"`
}

// linearMilestoneNode is one row of linearQueryProjectMilestone.
type linearMilestoneNode struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Project *linearProjectRef `json:"project"`
}

type linearMilestonesData struct {
	ProjectMilestones struct {
		Nodes []linearMilestoneNode `json:"nodes"`
	} `json:"projectMilestones"`
}

// linearLabelNode is one row of linearQueryIssueLabels. Team is a pointer
// because a workspace-wide label has none, and that difference is what decides
// a same-name collision.
type linearLabelNode struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Team   *linearTeamRef  `json:"team"`
	Parent *linearLabelRef `json:"parent"`
}

type linearLabelsData struct {
	IssueLabels struct {
		Nodes []linearLabelNode `json:"nodes"`
	} `json:"issueLabels"`
}

// linearUserNode is one row of linearQueryUsers.
type linearUserNode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Active      bool   `json:"active"`
}

type linearUsersData struct {
	Users struct {
		Nodes []linearUserNode `json:"nodes"`
	} `json:"users"`
}

type linearComment struct {
	Body      string         `json:"body"`
	CreatedAt string         `json:"createdAt"`
	URL       string         `json:"url,omitempty"`
	User      *linearUserRef `json:"user,omitempty"`
}

// Envelope types, one per document, matching GraphQL's `data` object.

type linearViewerData struct {
	Viewer linearUserRef `json:"viewer"`
	Teams  struct {
		Nodes []linearTeamRef `json:"nodes"`
	} `json:"teams"`
}

type linearIssueData struct {
	Issue *linearIssue `json:"issue"`
}

type linearIssuesData struct {
	Issues struct {
		Nodes []linearIssue `json:"nodes"`
	} `json:"issues"`
}

type linearSearchData struct {
	SearchIssues struct {
		Nodes []linearIssue `json:"nodes"`
	} `json:"searchIssues"`
}

type linearCommentsData struct {
	Issue *struct {
		Identifier string        `json:"identifier"`
		Title      string        `json:"title"`
		URL        string        `json:"url"`
		Team       linearTeamRef `json:"team"`
		Comments   struct {
			Nodes []linearComment `json:"nodes"`
		} `json:"comments"`
	} `json:"issue"`
}

type linearTeamMeta struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	States struct {
		Nodes []linearStateRef `json:"nodes"`
	} `json:"states"`
}

type linearTeamMetaData struct {
	Teams struct {
		Nodes []linearTeamMeta `json:"nodes"`
	} `json:"teams"`
}

type linearCommentCreateData struct {
	CommentCreate struct {
		Success bool `json:"success"`
		Comment *struct {
			URL       string `json:"url"`
			CreatedAt string `json:"createdAt"`
		} `json:"comment"`
	} `json:"commentCreate"`
}

type linearIssueCreateData struct {
	IssueCreate struct {
		Success bool         `json:"success"`
		Issue   *linearIssue `json:"issue"`
	} `json:"issueCreate"`
}

type linearIssueUpdateData struct {
	IssueUpdate struct {
		Success bool         `json:"success"`
		Issue   *linearIssue `json:"issue"`
	} `json:"issueUpdate"`
}

type linearAttachmentLinkData struct {
	AttachmentLinkURL struct {
		Success    bool `json:"success"`
		Attachment *struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"attachment"`
	} `json:"attachmentLinkURL"`
}
