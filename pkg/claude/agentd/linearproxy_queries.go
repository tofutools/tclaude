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
  viewer { name displayName email }
  teams(first: $first) { nodes { key name } }
}`

// linearQueryIssue backs `issue view`. The description is included here and
// nowhere else: it is the body of the ticket, which is the point of viewing
// one, but it would dominate a list response.
const linearQueryIssue = linearIssueFields + `
query IssueView($id: String!) {
  issue(id: $id) {
    ...IssueFields
    description
    estimate
    dueDate
    creator { displayName }
    parent { identifier title }
    project { name }
    labels(first: 20) { nodes { name } }
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
const linearQueryTeamMeta = `
query TeamMeta($key: String!) {
  teams(filter: { key: { eq: $key } }, first: 1) {
    nodes {
      id
      key
      name
      states(first: 100) { nodes { id name type position } }
    }
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

type linearProjectRef struct {
	Name string `json:"name"`
}

// linearIssue is the shared issue shape. Every field is optional on the wire
// except identifier and team, which every selection asks for.
type linearIssue struct {
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
	Labels        *struct {
		Nodes []linearLabelRef `json:"nodes"`
	} `json:"labels,omitempty"`
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
