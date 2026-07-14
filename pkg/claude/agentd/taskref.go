package agentd

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskref.go carries the per-agent task-reference link — an http(s) URL
// pointing at the work item an agent is on (a Linear issue, a GitHub
// issue/PR, a ticket, …) — from the agents table onto the dashboard's
// dashboardMember / dashboardAgent wire shapes, alongside repoLinksView.
//
// The datum is user/agent-authored (unlike branchlinks.go's git/gh-
// derived links), so the write path validates the scheme to http(s):
// the dashboard renders it as an <a href>, and an unguarded
// `javascript:`/`data:` URL would be a stored-XSS vector in the local
// web UI. Rendering still esc()s the value — validation is defence in
// depth, not the only guard.

// maxTaskRefURLLen / maxTaskRefLabelLen bound the two agent-authored
// fields. They ride every 2s dashboard snapshot and an agent holds
// self.task by default, so an unbounded string mustn't be storable. The
// values are XSS-safe regardless (rendered esc'd), so these are size
// guards, not security ones. 2048 is the conventional practical URL
// ceiling; a display label is a ticket id, so 200 is generous.
const (
	maxTaskRefURLLen   = 2048
	maxTaskRefLabelLen = 200
)

// taskRefView is the per-row task-reference block embedded in the
// dashboard wire shapes. Both fields omitempty: an agent with no link
// set contributes nothing and the Task column renders an em dash.
// TaskURL is the raw stored URL (the edit affordance round-trips it);
// TaskLabel is the *effective* display label — the human's explicit
// label when set, otherwise one derived from the URL.
// TaskLabelOverride is the raw explicit label (empty means auto-derive),
// carried separately so the dashboard editor can prefill the operator's
// actual choice instead of turning a derived label into a permanent override.
type taskRefView struct {
	TaskURL           string `json:"task_ref_url,omitempty"`
	TaskLabel         string `json:"task_ref_label,omitempty"`
	TaskLabelOverride string `json:"task_ref_label_override,omitempty"`
}

// taskRefViewFor builds the wire view for one agent's stored task ref.
// A blank URL yields the zero view (renders as no link).
func taskRefViewFor(ref db.AgentTaskRef) taskRefView {
	if strings.TrimSpace(ref.URL) == "" {
		return taskRefView{}
	}
	return taskRefView{
		TaskURL:           ref.URL,
		TaskLabel:         effectiveTaskLabel(ref),
		TaskLabelOverride: strings.TrimSpace(ref.Label),
	}
}

// effectiveTaskLabel returns the label to display for a task ref: the
// human's explicit label when non-empty, otherwise one derived from the
// URL (Linear issue id, GitHub #number, else the host).
func effectiveTaskLabel(ref db.AgentTaskRef) string {
	if l := strings.TrimSpace(ref.Label); l != "" {
		return l
	}
	return deriveTaskLabel(ref.URL)
}

// linearIssueRe matches a Linear issue identifier (team key + number,
// e.g. "JOH-353", "ENG-1024") as a whole path segment.
var linearIssueRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

// deriveTaskLabel produces a compact display label from a task URL:
//   - Linear  (linear.app/…/issue/JOH-353/slug) → "JOH-353"
//   - GitHub  (github.com/owner/repo/issues|pull/42) → "#42"
//   - anything else → the host with a leading "www." stripped
//
// Returns "" only for an unparseable/empty URL — the caller then falls
// back to showing the raw URL. Purely cosmetic: it never affects what
// the link points at.
func deriveTaskLabel(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	segs := pathSegments(u.Path)

	switch {
	case host == "linear.app" || strings.HasSuffix(host, ".linear.app"):
		// …/issue/<ID>/<slug> — take the segment right after "issue".
		for i, s := range segs {
			if strings.EqualFold(s, "issue") && i+1 < len(segs) && linearIssueRe.MatchString(segs[i+1]) {
				return strings.ToUpper(segs[i+1])
			}
		}
	case host == "github.com":
		// owner/repo/(issues|pull)/<n> — the trailing numeric segment.
		for i, s := range segs {
			if (s == "issues" || s == "pull") && i+1 < len(segs) && isAllDigits(segs[i+1]) {
				return "#" + segs[i+1]
			}
		}
	}
	return host
}

// validateTaskRefURL enforces that a task-reference URL is an absolute
// http(s) URL with a host. This is the write-path guard that keeps a
// `javascript:`/`data:`/other-scheme URL out of the dashboard's href.
// An empty string is the caller's "clear the link" signal and is
// validated separately (never reaches here).
func validateTaskRefURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return fmt.Errorf("task reference URL is empty")
	}
	// Bound the length: the value rides every 2s dashboard snapshot, and an
	// agent holds self.task by default, so a runaway string shouldn't be
	// storable. 2048 is the conventional practical URL ceiling.
	if len(rawURL) > maxTaskRefURLLen {
		return fmt.Errorf("task reference URL is too long (%d > %d chars)", len(rawURL), maxTaskRefURLLen)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("task reference URL is not a valid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("task reference URL must be http(s), got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("task reference URL must include a host")
	}
	return nil
}

// validateTaskRefLabel bounds the optional explicit display label. Empty
// is fine (the label is then derived from the URL). Same size-guard
// rationale as the URL cap; the value is rendered esc'd, so this is not a
// security check.
func validateTaskRefLabel(label string) error {
	if len(label) > maxTaskRefLabelLen {
		return fmt.Errorf("task reference label is too long (%d > %d chars)", len(label), maxTaskRefLabelLen)
	}
	return nil
}

// pathSegments splits a URL path into its non-empty segments.
func pathSegments(p string) []string {
	var out []string
	for s := range strings.SplitSeq(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isAllDigits reports whether s is non-empty and all ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
