package agentd

import "encoding/json"

// githubproxy_queries.go holds every GraphQL document the proxy sends, plus the
// projections that render a GraphQL answer in the field vocabulary the proxy
// has always answered in.
//
// WHY GRAPHQL AT ALL, when the rest of the proxy is REST: four of the fields
// this proxy has always returned do not exist in the REST API.
//
//	state              REST says "open"/"closed" and puts merged in a separate
//	                   field; the OPEN/CLOSED/MERGED vocabulary is GraphQL's.
//	mergeable          REST's is a nullable bool that means something else.
//	reviewDecision     GraphQL only. There is no REST equivalent at all.
//	statusCheckRollup  GraphQL only.
//
// A fifth reason is subtler and would be a real bug rather than a missing
// field: REST's issue endpoints return PULL REQUESTS as issues, because GitHub
// models a PR as an issue with extra parts. `issue ls` over REST would list the
// repository's pull requests among its issues. GraphQL's `repository.issues`
// does not.
//
// So: GraphQL for the reads whose answers are richer than REST's, REST for
// everything else. That is the same split the `gh` CLI makes, for the same
// reasons, which is why the answers look the same.
//
// Every document is a package-level constant. No document is ever built by
// concatenation, and no caller-supplied value ever appears in one — they travel
// in `variables`, where GraphQL types them and no string of theirs can become
// syntax.

// ghAuthorFragment is the author selection shared by every document.
// `__typename` is what distinguishes a Bot from a User, which is a distinction
// worth keeping: "this PR was opened by a bot" changes what an agent should do
// about it.
const ghAuthorFragment = `author { __typename login ... on User { id name } }`

const ghLabelsFragment = `labels(first: 100) { nodes { id name description color } }`

// ghPRListQuery lists pull requests newest-created first.
//
// CREATED_AT rather than UPDATED_AT, because ordering interacts with `first:
// $limit` to decide WHICH pull requests come back, not merely in what order —
// and newest-created is the set `pr ls` has always returned. An agent asking
// for 20 would otherwise get the 20 most recently touched, which on a busy
// repository is a different list.
//
// $states is null for "all", which GraphQL reads as "no filter" rather than as
// "no states" — the one place a null variable is load-bearing rather than
// merely omitted.
const ghPRListQuery = `
query PRList($owner: String!, $name: String!, $limit: Int!, $states: [PullRequestState!]) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $limit, states: $states, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes {
        number title state isDraft headRefName baseRefName url updatedAt
        ` + ghAuthorFragment + `
      }
    }
  }
}`

const ghPRViewQuery = `
query PRView($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      number title state isDraft headRefName baseRefName url body
      createdAt updatedAt mergeable reviewDecision
      ` + ghAuthorFragment + `
    }
  }
}`

// ghPRChecksQuery reads the check rollup of the pull request's HEAD COMMIT,
// which is the same thing `pr checks` has always reported and the reason a
// force-push takes a run out of this listing while `run ls --branch` still
// shows it.
//
// contexts(first: 100) is not paginated onward. A pull request with more than
// a hundred check contexts is not one an agent reads a rollup of; the run verbs
// are the tool for that repository.
const ghPRChecksQuery = `
query PRChecks($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      number url
      commits(last: 1) {
        nodes {
          commit {
            statusCheckRollup {
              contexts(first: 100) {
                nodes {
                  __typename
                  ... on CheckRun {
                    name status conclusion startedAt completedAt detailsUrl
                    checkSuite { workflowRun { workflow { name } } }
                  }
                  ... on StatusContext {
                    context state targetUrl createdAt description
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

const ghIssueListQuery = `
query IssueList($owner: String!, $name: String!, $limit: Int!, $states: [IssueState!]) {
  repository(owner: $owner, name: $name) {
    issues(first: $limit, states: $states, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes {
        number title state url updatedAt
        ` + ghAuthorFragment + `
        ` + ghLabelsFragment + `
      }
    }
  }
}`

const ghIssueViewQuery = `
query IssueView($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      number title state url body createdAt updatedAt
      ` + ghAuthorFragment + `
      ` + ghLabelsFragment + `
      assignees(first: 100) { nodes { id login name } }
    }
  }
}`

// ghPRNodeIDQuery resolves the opaque node id `pr ready` needs.
//
// It exists because marking a draft ready is the one write in this proxy with
// no REST route: REST will not clear the `draft` flag on a pull request, and
// the GraphQL mutation that does takes a node id rather than a number.
const ghPRNodeIDQuery = `
query PRNodeID($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { id number url isDraft state }
  }
}`

const ghPRReadyMutation = `
mutation PRReady($id: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $id}) {
    pullRequest { number url isDraft }
  }
}`

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

// ghGQLAuthor is the raw author node. The union means `id` and `name` are
// absent for anything that is not a User.
type ghGQLAuthor struct {
	TypeName string `json:"__typename"`
	Login    string `json:"login"`
	ID       string `json:"id"`
	Name     string `json:"name"`
}

// ghAuthorOut is the rendered author: the same four keys, in the same order,
// this proxy has always emitted.
type ghAuthorOut struct {
	ID    string `json:"id"`
	IsBot bool   `json:"is_bot"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

// project renders one author.
//
// A bot's login is prefixed `app/`, which is not decoration: it is how the
// login is spelled everywhere else GitHub shows it, and an agent comparing
// "who reviewed this" against a configured bot name needs the two spellings to
// agree. A bot has no user id or profile name, so those stay empty rather than
// carrying the union's zero values as if they were real.
func (a *ghGQLAuthor) project() *ghAuthorOut {
	if a == nil || (a.Login == "" && a.TypeName == "") {
		// GitHub returns a null author for content whose account was deleted.
		return nil
	}
	if a.TypeName == "Bot" {
		return &ghAuthorOut{IsBot: true, Login: "app/" + a.Login}
	}
	return &ghAuthorOut{ID: a.ID, Login: a.Login, Name: a.Name}
}

type ghGQLLabel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

type ghGQLLabels struct {
	Nodes []ghGQLLabel `json:"nodes"`
}

// nodes returns a non-nil slice, so an issue with no labels renders as `[]`
// rather than as `null`. A caller iterating the field should not have to
// special-case the empty case, and `gh --json labels` did not make it.
func (l ghGQLLabels) nodes() []ghGQLLabel {
	if l.Nodes == nil {
		return []ghGQLLabel{}
	}
	return l.Nodes
}

type ghGQLAssignee struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

// ghGQLPullRequest is every pull-request field any document in this file
// selects. One struct rather than one per query: the fields are a superset,
// unselected ones simply stay zero, and a single struct is what keeps the
// projections below from drifting apart.
type ghGQLPullRequest struct {
	ID             string       `json:"id"`
	Number         int          `json:"number"`
	Title          string       `json:"title"`
	State          string       `json:"state"`
	IsDraft        bool         `json:"isDraft"`
	HeadRefName    string       `json:"headRefName"`
	BaseRefName    string       `json:"baseRefName"`
	URL            string       `json:"url"`
	Body           string       `json:"body"`
	CreatedAt      string       `json:"createdAt"`
	UpdatedAt      string       `json:"updatedAt"`
	Mergeable      string       `json:"mergeable"`
	ReviewDecision string       `json:"reviewDecision"`
	Author         *ghGQLAuthor `json:"author"`
	Commits        struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []ghGQLCheckContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// ghGQLCheckContext is one entry of a status-check rollup. The union carries
// two disjoint shapes and `__typename` says which.
type ghGQLCheckContext struct {
	TypeName string `json:"__typename"`
	// CheckRun
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
	DetailsURL  string `json:"detailsUrl"`
	CheckSuite  struct {
		WorkflowRun *struct {
			Workflow struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	// StatusContext
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"targetUrl"`
	CreatedAt   string `json:"createdAt"`
	Description string `json:"description"`
}

type ghGQLIssue struct {
	Number    int          `json:"number"`
	Title     string       `json:"title"`
	State     string       `json:"state"`
	URL       string       `json:"url"`
	Body      string       `json:"body"`
	CreatedAt string       `json:"createdAt"`
	UpdatedAt string       `json:"updatedAt"`
	Author    *ghGQLAuthor `json:"author"`
	Labels    ghGQLLabels  `json:"labels"`
	Assignees struct {
		Nodes []ghGQLAssignee `json:"nodes"`
	} `json:"assignees"`
}

// ---------------------------------------------------------------------------
// Projections
// ---------------------------------------------------------------------------

// The rendered shapes below are structs rather than maps on purpose:
// encoding/json emits struct fields in declaration order and map keys in sorted
// order, and a stable field order is what makes a diff between two proxy
// responses readable.

type ghPRListEntry struct {
	Number      int          `json:"number"`
	Title       string       `json:"title"`
	State       string       `json:"state"`
	IsDraft     bool         `json:"isDraft"`
	HeadRefName string       `json:"headRefName"`
	BaseRefName string       `json:"baseRefName"`
	URL         string       `json:"url"`
	UpdatedAt   string       `json:"updatedAt"`
	Author      *ghAuthorOut `json:"author"`
}

func projectPRListEntry(pr ghGQLPullRequest) ghPRListEntry {
	return ghPRListEntry{
		Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft,
		HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName, URL: pr.URL,
		UpdatedAt: pr.UpdatedAt, Author: pr.Author.project(),
	}
}

type ghPRViewOut struct {
	Number         int          `json:"number"`
	Title          string       `json:"title"`
	State          string       `json:"state"`
	IsDraft        bool         `json:"isDraft"`
	HeadRefName    string       `json:"headRefName"`
	BaseRefName    string       `json:"baseRefName"`
	URL            string       `json:"url"`
	Body           string       `json:"body"`
	CreatedAt      string       `json:"createdAt"`
	UpdatedAt      string       `json:"updatedAt"`
	Author         *ghAuthorOut `json:"author"`
	Mergeable      string       `json:"mergeable"`
	ReviewDecision string       `json:"reviewDecision"`
}

func projectPRView(pr ghGQLPullRequest) ghPRViewOut {
	return ghPRViewOut{
		Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft,
		HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName, URL: pr.URL,
		Body: pr.Body, CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt,
		Author: pr.Author.project(), Mergeable: pr.Mergeable, ReviewDecision: pr.ReviewDecision,
	}
}

// ghPRChecksOut mirrors what `pr checks` has always returned: the pull request
// identified, then its rollup as a flat array.
//
// Rollup is a json.RawMessage holding either an array or `null`. Null is the
// honest answer for a pull request whose head commit has no checks associated
// at all, and it is a different answer from `[]`, which means the commit has a
// rollup with nothing in it.
type ghPRChecksOut struct {
	Number int             `json:"number"`
	URL    string          `json:"url"`
	Rollup json.RawMessage `json:"statusCheckRollup"`
}

// ghCheckRunOut and ghStatusContextOut are the two rollup entry shapes.
type ghCheckRunOut struct {
	TypeName     string `json:"__typename"`
	Name         string `json:"name"`
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	StartedAt    string `json:"startedAt"`
	CompletedAt  string `json:"completedAt"`
	DetailsURL   string `json:"detailsUrl"`
}

type ghStatusContextOut struct {
	TypeName    string `json:"__typename"`
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"targetUrl"`
	CreatedAt   string `json:"createdAt"`
	Description string `json:"description"`
}

// projectCheckRollup flattens the nested commit → rollup → contexts walk into
// the flat array a caller reads, returning nil when there is no rollup to
// flatten.
func projectCheckRollup(pr ghGQLPullRequest) []any {
	if len(pr.Commits.Nodes) == 0 {
		return nil
	}
	rollup := pr.Commits.Nodes[0].Commit.StatusCheckRollup
	if rollup == nil {
		return nil
	}
	out := make([]any, 0, len(rollup.Contexts.Nodes))
	for _, c := range rollup.Contexts.Nodes {
		if c.TypeName == "CheckRun" {
			entry := ghCheckRunOut{
				TypeName: c.TypeName, Name: c.Name, Status: c.Status,
				Conclusion: c.Conclusion, StartedAt: c.StartedAt,
				CompletedAt: c.CompletedAt, DetailsURL: c.DetailsURL,
			}
			if c.CheckSuite.WorkflowRun != nil {
				entry.WorkflowName = c.CheckSuite.WorkflowRun.Workflow.Name
			}
			out = append(out, entry)
			continue
		}
		out = append(out, ghStatusContextOut{
			TypeName: c.TypeName, Context: c.Context, State: c.State,
			TargetURL: c.TargetURL, CreatedAt: c.CreatedAt, Description: c.Description,
		})
	}
	return out
}

type ghIssueListEntry struct {
	Number    int          `json:"number"`
	Title     string       `json:"title"`
	State     string       `json:"state"`
	URL       string       `json:"url"`
	UpdatedAt string       `json:"updatedAt"`
	Author    *ghAuthorOut `json:"author"`
	Labels    []ghGQLLabel `json:"labels"`
}

func projectIssueListEntry(issue ghGQLIssue) ghIssueListEntry {
	return ghIssueListEntry{
		Number: issue.Number, Title: issue.Title, State: issue.State, URL: issue.URL,
		UpdatedAt: issue.UpdatedAt, Author: issue.Author.project(), Labels: issue.Labels.nodes(),
	}
}

type ghIssueViewOut struct {
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	State     string          `json:"state"`
	URL       string          `json:"url"`
	Body      string          `json:"body"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt"`
	Author    *ghAuthorOut    `json:"author"`
	Labels    []ghGQLLabel    `json:"labels"`
	Assignees []ghGQLAssignee `json:"assignees"`
}

func projectIssueView(issue ghGQLIssue) ghIssueViewOut {
	assignees := issue.Assignees.Nodes
	if assignees == nil {
		assignees = []ghGQLAssignee{}
	}
	return ghIssueViewOut{
		Number: issue.Number, Title: issue.Title, State: issue.State, URL: issue.URL,
		Body: issue.Body, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
		Author: issue.Author.project(), Labels: issue.Labels.nodes(), Assignees: assignees,
	}
}
