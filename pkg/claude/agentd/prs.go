package agentd

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	maxAgentPRURLLen     = 2048
	maxAgentPRSummaryLen = 200
)

// presentedPRView is the dashboard wire shape for explicitly presented PRs.
// It is separate from repoLinksView's branch/startup PR fields because an
// agent may want to present a PR after leaving the branch, or present multiple
// related PRs. The frontend dedupes by URL against branch/startup PR links.
type presentedPRView struct {
	URL       string `json:"url"`
	Number    int    `json:"number,omitempty"`
	Summary   string `json:"summary,omitempty"`
	State     string `json:"state,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func presentedPRViews(rows []db.AgentPR) []presentedPRView {
	if len(rows) == 0 {
		return nil
	}
	out := make([]presentedPRView, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		u := strings.TrimSpace(row.PRURL)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		v := presentedPRView{
			URL:     u,
			Number:  deriveGitHubPRNumber(u),
			Summary: row.Summary,
			State:   row.State,
		}
		if !row.UpdatedAt.IsZero() {
			v.UpdatedAt = row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, v)
	}
	return out
}

func validateAgentPRURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return fmt.Errorf("PR URL is empty")
	}
	if len(rawURL) > maxAgentPRURLLen {
		return fmt.Errorf("PR URL is too long (%d > %d chars)", len(rawURL), maxAgentPRURLLen)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("PR URL is not a valid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("PR URL must be http(s), got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("PR URL must include a host")
	}
	return nil
}

func validateAgentPRSummary(summary string) error {
	if len(summary) > maxAgentPRSummaryLen {
		return fmt.Errorf("PR summary is too long (%d > %d chars)", len(summary), maxAgentPRSummaryLen)
	}
	return nil
}

func normalizeAgentPRState(state string) (string, error) {
	state = strings.ToLower(strings.TrimSpace(state))
	switch state {
	case "", "open", "merged", "closed", "handled":
		return state, nil
	default:
		return "", fmt.Errorf("PR state must be one of: open, merged, closed, handled")
	}
}

func deriveGitHubPRNumber(rawURL string) int {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return 0
	}
	segs := pathSegments(u.Path)
	for i, s := range segs {
		if s == "pull" && i+1 < len(segs) && isAllDigits(segs[i+1]) {
			n, _ := strconv.Atoi(segs[i+1])
			return n
		}
	}
	return 0
}
