package agentd

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// prdetails_gitlab.go is the hover card's GitLab half: what /api/pr-details
// shows for a merge request. The REST merge request read the CI pill already
// makes carries the title, state, author, branches, description and times; the
// diffstat and commit count come from one GraphQL read, because REST has no
// line counts. An instance without those GraphQL fields still gets a card,
// marked as having no diffstat rather than showing zeros.

// glMRDiffStatsQuery reads a merge request's diffstat and commit count.
const glMRDiffStatsQuery = `query($project: ID!, $iid: String!) {
  project(fullPath: $project) {
    mergeRequest(iid: $iid) {
      commitCount
      diffStatsSummary { additions deletions fileCount }
    }
  }
}`

// gitlabPRDetails reads the card for a merge request, under the same policy
// gates and credential as the CI pill (gitlabMRCredential).
func gitlabPRDetails(rawURL string) (prDetails, bool) {
	ref, base, token, ok := gitlabMRCredential(rawURL)
	if !ok {
		return prDetails{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitlabPresentedPRTimeout)
	defer cancel()

	mr, ok := gitlabReadMR(ctx, base, token, ref)
	if !ok {
		return prDetails{}, false
	}
	details := prDetails{
		Title:     truncateRunes(strings.TrimSpace(glStripDraftPrefix(mr.Title)), prDetailsMaxTitleRunes),
		State:     prStateFromGitLab(mr.State, mr.Draft),
		HeadRef:   truncateRunes(strings.TrimSpace(mr.SourceBranch), prDetailsMaxRefRunes),
		BaseRef:   truncateRunes(strings.TrimSpace(mr.TargetBranch), prDetailsMaxRefRunes),
		Excerpt:   prBodyExcerpt(mr.Description, prDetailsExcerptRunes),
		CreatedAt: rfc3339OrEmpty(mr.CreatedAt),
		MergedAt:  rfc3339OrEmpty(mr.MergedAt),
		ClosedAt:  rfc3339OrEmpty(mr.ClosedAt),
	}
	if mr.Author != nil && glUsernameRE.MatchString(mr.Author.Username) {
		details.Author = mr.Author.Username
	}

	var data struct {
		Project *struct {
			MergeRequest *struct {
				CommitCount      *int `json:"commitCount"`
				DiffStatsSummary *struct {
					Additions int `json:"additions"`
					Deletions int `json:"deletions"`
					FileCount int `json:"fileCount"`
				} `json:"diffStatsSummary"`
			} `json:"mergeRequest"`
		} `json:"project"`
	}
	err := glGraphQL(ctx, base, token, glMRDiffStatsQuery,
		map[string]any{"project": ref.project, "iid": strconv.Itoa(ref.number)}, &data)
	switch {
	case err == nil && data.Project != nil && data.Project.MergeRequest != nil &&
		data.Project.MergeRequest.DiffStatsSummary != nil:
		stats := data.Project.MergeRequest.DiffStatsSummary
		details.Additions = max(0, stats.Additions)
		details.Deletions = max(0, stats.Deletions)
		details.ChangedFiles = max(0, stats.FileCount)
		if count := data.Project.MergeRequest.CommitCount; count != nil {
			details.Commits = max(0, *count)
		}
	case err != nil && !errors.Is(err, errGLUnknownField):
		// A failed diffstat read must not cache a card of zeros for a minute,
		// so the whole read fails and the cooldown applies.
		return prDetails{}, false
	default:
		// The instance lacks the fields, or answered without them.
		details.NoDiffstat = true
	}
	return details, true
}

// gitlabMRCredential resolves what a dashboard read of a merge request needs:
// the parsed URL, the configured instance base and the token. It runs only
// when the git proxy is configured, because that policy holds both the
// instance and the credential, and only for a project on its allow-list.
func gitlabMRCredential(rawURL string) (gitlabMRRef, string, string, bool) {
	ref, ok := gitlabMRRefFromURL(rawURL)
	if !ok {
		return gitlabMRRef{}, "", "", false
	}
	cfg, err := config.Load()
	if err != nil || !cfg.GitProxyEnabled() {
		return gitlabMRRef{}, "", "", false
	}
	policy := cfg.ResolvedGitProxy()
	base, err := gitlabMRPolicyBase(ref, policy)
	if err != nil {
		return gitlabMRRef{}, "", "", false
	}
	token, fault := gitlabToken(policy)
	if fault != nil {
		return gitlabMRRef{}, "", "", false
	}
	return ref, base, token, true
}
