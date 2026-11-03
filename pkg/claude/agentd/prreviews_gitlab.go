package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// prreviews_gitlab.go feeds a merge request's approvals and reviewers into the
// review cache (prreviews.go), so the review pill and its hover panel read the
// same for a GitLab merge request as for a GitHub pull request.
//
// Reviewer states (mergeRequestInteraction.reviewState, and with it the
// REQUESTED_CHANGES value) came to GitLab well after approvedBy, and GraphQL
// fails a whole query over one field the instance does not know. So the
// reviewer read is a query of its own, never part of a list query that works
// today, and an instance that answers it with an unknown-field error is
// remembered for the daemon's lifetime: from then on its merge requests are
// read through approvedBy alone, and their pills show approvals only.
//
// GitLab records no time on an approval, so a reviewer row carries no age.
// The merge decision still comes from detailedMergeStatus.

// glMRReviewsQuery reads one merge request's approvals and reviewer states.
const glMRReviewsQuery = `query($project: ID!, $iid: String!) {
  project(fullPath: $project) {
    mergeRequest(iid: $iid) {
      detailedMergeStatus
      approvedBy { nodes { username bot } }
      reviewers { nodes { username bot mergeRequestInteraction { reviewState } } }
    }
  }
}`

// glMRApprovalsQuery is glMRReviewsQuery for an instance without reviewer
// states: the fields the Open MRs list query has always asked for.
const glMRApprovalsQuery = `query($project: ID!, $iid: String!) {
  project(fullPath: $project) {
    mergeRequest(iid: $iid) {
      detailedMergeStatus
      approvedBy { nodes { username } }
    }
  }
}`

// glAuthoredMRReviewersQuery is the reviewer read for the Open MRs list: the
// same merge requests, in the same order, as authoredOpenMRQuery.
const glAuthoredMRReviewersQuery = `query($first: Int!) {
  currentUser {
    authoredMergeRequests(state: opened, first: $first) {
      nodes {
        webUrl
        approvedBy { nodes { username bot } }
        reviewers { nodes { username bot mergeRequestInteraction { reviewState } } }
      }
    }
  }
}`

// glReviewUser is a user node in approvedBy or reviewers. Bot is absent on an
// instance read through the approvals-only query.
type glReviewUser struct {
	Username                string `json:"username"`
	Bot                     bool   `json:"bot"`
	MergeRequestInteraction *struct {
		ReviewState string `json:"reviewState"`
	} `json:"mergeRequestInteraction"`
}

type glReviewUsers struct {
	Nodes []glReviewUser `json:"nodes"`
}

// glMRReviewNode is the review part of a merge request node.
type glMRReviewNode struct {
	WebURL              string        `json:"webUrl"`
	DetailedMergeStatus string        `json:"detailedMergeStatus"`
	ApprovedBy          glReviewUsers `json:"approvedBy"`
	Reviewers           glReviewUsers `json:"reviewers"`
}

// glGraphQLError is one entry of a GraphQL response's errors array.
type glGraphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// errGLUnknownField says the instance refused a query over a field its
// GraphQL schema does not have: an instance older than the field.
var errGLUnknownField = errors.New("this GitLab instance does not know a field the query asked for")

// glReviewStatesMissing holds the instance bases (lower-cased) that answered
// the reviewer read with an unknown-field error.
var glReviewStatesMissing sync.Map

func glReviewStatesKnownMissing(base string) bool {
	_, missing := glReviewStatesMissing.Load(strings.ToLower(strings.TrimRight(base, "/")))
	return missing
}

func markGLReviewStatesMissing(base string) {
	glReviewStatesMissing.Store(strings.ToLower(strings.TrimRight(base, "/")), true)
}

// glUnknownFieldError reports whether a GraphQL answer refused the query over
// a field the schema lacks, which is how an older instance answers the
// reviewer read.
func glUnknownFieldError(errs []glGraphQLError) bool {
	for _, e := range errs {
		if e.Extensions.Code == "undefinedField" || strings.Contains(e.Message, "doesn't exist on type") {
			return true
		}
	}
	return false
}

// glReviewDecision maps detailedMergeStatus onto the review decision the pill
// is driven by. GitLab's "approved" flag is not used: it is true with no
// approvals on a project that requires none.
func glReviewDecision(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "NOT_APPROVED":
		return "review_required"
	case "REQUESTED_CHANGES":
		return "changes_requested"
	}
	return ""
}

// glBotUsernameRE matches the users GitLab creates for project and group
// access tokens, which an instance without the bot field cannot flag.
var glBotUsernameRE = regexp.MustCompile(`^(project|group)_\d+_bot(_[0-9a-f]+)?$`)

// glReviewers turns approvedBy and the reviewers' states into reviewer rows.
// approvedBy is the authority on approvals. A reviewer whose state is
// REQUESTED_CHANGES, and who has not approved since, is a change request. Any
// other state (not yet reviewed, reviewing, commented) carries no opinion and
// is left out, as a GitHub COMMENTED review is.
//
// The row's link is the user's page on the instance, built from a username
// that has passed GitLab's own character rule, rather than a URL from the
// response.
func glReviewers(base string, approvedBy, reviewers []glReviewUser) []prReviewer {
	base = strings.TrimRight(base, "/")
	seen := map[string]bool{}
	out := []prReviewer{}
	add := func(u glReviewUser, state string) {
		login := strings.TrimSpace(u.Username)
		key := strings.ToLower(login)
		if seen[key] || !glUsernameRE.MatchString(login) {
			return
		}
		seen[key] = true
		out = append(out, prReviewer{
			Login: login,
			State: state,
			Bot:   u.Bot || glBotUsernameRE.MatchString(login),
			URL:   base + "/" + login,
		})
	}
	for _, u := range approvedBy {
		add(u, "approved")
	}
	for _, u := range reviewers {
		if u.MergeRequestInteraction == nil {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(u.MergeRequestInteraction.ReviewState)) {
		case "REQUESTED_CHANGES":
			add(u, "changes_requested")
		case "APPROVED":
			add(u, "approved")
		}
	}
	return out
}

// glReviewObservation is what one read learned about a merge request's
// reviews. The reviewer list is always complete for what was read: approvals
// alone on an instance without reviewer states.
func glReviewObservation(base string, node glMRReviewNode) prReviewObservation {
	return prReviewObservation{
		Decision:         glReviewDecision(node.DetailedMergeStatus),
		Reviewers:        glReviewers(base, node.ApprovedBy.Nodes, node.Reviewers.Nodes),
		ReviewersFetched: true,
	}
}

// glGraphQL posts one query and decodes its data. It reports an unknown-field
// refusal as errGLUnknownField and every other failure as an error.
func glGraphQL(ctx context.Context, base, token, query string, variables map[string]any, data any) error {
	res, err := glDo(ctx, base, token, glAPIRequest{
		Method:  http.MethodPost,
		GraphQL: true,
		Body:    map[string]any{"query": query, "variables": variables},
	})
	if err != nil {
		return err
	}
	if res.Status == http.StatusTooManyRequests {
		return errors.New(glRateLimitText(res))
	}
	if res.Status != http.StatusOK {
		return errors.New(glErrorText(res))
	}
	var doc struct {
		Data   json.RawMessage  `json:"data"`
		Errors []glGraphQLError `json:"errors"`
	}
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return err
	}
	if glUnknownFieldError(doc.Errors) {
		return errGLUnknownField
	}
	if len(doc.Errors) > 0 {
		return errors.New(truncateAuthoredPRTitle(doc.Errors[0].Message))
	}
	return json.Unmarshal(doc.Data, data)
}

// gitlabMRReviews reads one merge request's reviews for the Branch column and
// presented merge requests. nil means the read failed and the cached reviews
// stand.
func gitlabMRReviews(ctx context.Context, base, token string, ref gitlabMRRef) *prReviewObservation {
	var data struct {
		Project *struct {
			MergeRequest *glMRReviewNode `json:"mergeRequest"`
		} `json:"project"`
	}
	vars := map[string]any{"project": ref.project, "iid": strconv.Itoa(ref.number)}
	err := errGLUnknownField
	if !glReviewStatesKnownMissing(base) {
		err = glGraphQL(ctx, base, token, glMRReviewsQuery, vars, &data)
		if errors.Is(err, errGLUnknownField) {
			markGLReviewStatesMissing(base)
		}
	}
	if errors.Is(err, errGLUnknownField) {
		err = glGraphQL(ctx, base, token, glMRApprovalsQuery, vars, &data)
	}
	if err != nil || data.Project == nil || data.Project.MergeRequest == nil {
		return nil
	}
	obs := glReviewObservation(base, *data.Project.MergeRequest)
	return &obs
}

// fetchAuthoredMRReviewers runs the Open MRs list's reviewer read, keyed by
// prStateKey. errGLUnknownField means the instance has no reviewer states, and
// is remembered.
func fetchAuthoredMRReviewers(ctx context.Context, base, token string) (map[string]glMRReviewNode, error) {
	if glReviewStatesKnownMissing(base) {
		return nil, errGLUnknownField
	}
	var data struct {
		CurrentUser *struct {
			AuthoredMergeRequests struct {
				Nodes []glMRReviewNode `json:"nodes"`
			} `json:"authoredMergeRequests"`
		} `json:"currentUser"`
	}
	err := glGraphQL(ctx, base, token, glAuthoredMRReviewersQuery, map[string]any{"first": authoredOpenMRLimit}, &data)
	if errors.Is(err, errGLUnknownField) {
		markGLReviewStatesMissing(base)
	}
	if err != nil {
		return nil, err
	}
	if data.CurrentUser == nil {
		return nil, errors.New("GitLab did not resolve the token to a user")
	}
	out := map[string]glMRReviewNode{}
	for _, node := range data.CurrentUser.AuthoredMergeRequests.Nodes {
		if key := prStateKey(node.WebURL); key != "" {
			out[key] = node
		}
	}
	return out, nil
}
