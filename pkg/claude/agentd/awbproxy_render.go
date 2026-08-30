package agentd

import (
	"encoding/json"
	"strconv"
	"strings"
)

// awbproxy_render.go is `--compact`: AWB's one-line-per-issue form, produced
// daemon-side from the JSON the API returned.
//
// It is rendered HERE rather than in the CLI for the reason every other shaping
// decision in the proxies lives in the daemon: the CLI half is deliberately
// thin, and a renderer in that process is one an agent could bypass by talking
// to the socket directly — at which point two callers would disagree about what
// `--compact` means.
//
// The format is AWB's, reproduced rather than imported: tclaude does not depend
// on the awb module, and a proxy that made it do so would tie a tclaude release
// to an awb one. The cost is that this is a second copy of a documented format,
// so it is written to the specification rather than to the source — see
// awb's internal/domain/compact.go, whose doc comments are the specification —
// and TestAWBCompactLine pins every field and every optional token.

// awbCompactLine renders one issue as AWB's compact line:
//
//	awb-5c1d84 P1 in_progress bug "Tokeniser drops the trailing newline" @claude-1 #tokeniser !blocked
//
// Five mandatory positional fields lead — id, P<priority>, status, type and the
// title — and the title is a JSON string, quotes and escaping included, so that
// it is the only field that may contain a literal space once decoded.
//
// Any further field is optional and identified by its PREFIX rather than its
// position, in this fixed order: @<assignee>, one #<label> per label in the
// order AWB sorted them, !blocked, and — when withBlockers is set — one
// blocked-by:<id> per blocker. That last group is for `blocked`, the listing
// whose whole point is the blockers.
//
// Labels and assignees cannot contain spaces (AWB's charset says so), which is
// what keeps a line parseable by splitting on whitespace outside the quoted
// title.
func awbCompactLine(issue *awbIssue, withBlockers bool) string {
	var b strings.Builder

	b.WriteString(issue.ID)
	b.WriteString(" P")
	b.WriteString(strconv.Itoa(issue.Priority))
	b.WriteByte(' ')
	b.WriteString(issue.Status)
	b.WriteByte(' ')
	b.WriteString(issue.Type)
	b.WriteByte(' ')
	b.WriteString(awbJSONString(issue.Title))

	if issue.Assignee != "" {
		b.WriteString(" @")
		b.WriteString(issue.Assignee)
	}
	for _, label := range issue.Labels {
		b.WriteString(" #")
		b.WriteString(label)
	}
	if issue.Blocked {
		b.WriteString(" !blocked")
	}
	if withBlockers {
		for _, blocker := range issue.Blockers {
			b.WriteString(" blocked-by:")
			b.WriteString(blocker)
		}
	}
	return b.String()
}

// awbCompactLines renders a listing, one line per issue.
func awbCompactLines(issues []awbIssue, withBlockers bool) string {
	var b strings.Builder
	for i := range issues {
		b.WriteString(awbCompactLine(&issues[i], withBlockers))
		b.WriteByte('\n')
	}
	return b.String()
}

// awbCompactTree renders a decomposition tree: the ordinary compact line for
// each node, prefixed by two spaces per level of depth with the root
// unindented. That prefix is the one thing that may precede an id, so a
// consumer strips the leading spaces, counts them to recover the depth, and
// parses the rest of the line as usual.
func awbCompactTree(node *awbIssueTree, depth int, b *strings.Builder) {
	if node == nil {
		return
	}
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString(awbCompactLine(&node.awbIssue, false))
	b.WriteByte('\n')
	for i := range node.Children {
		awbCompactTree(&node.Children[i], depth+1, b)
	}
}

// awbCompactAttachmentLine renders one attachment as AWB's compact line:
//
//	awb-5c1d84 12345 9f86d0…b0f00a "text/markdown; charset=utf-8" "notes.md"
//
// Five fields: the issue, the size in bytes, the content's SHA-256, the content
// type and the name. The first three cannot contain a space; the last two are
// JSON strings and are last for that reason — a content type may carry
// parameters, and a bare one would put a space in the middle of a positional
// field and turn a five-field line into six.
func awbCompactAttachmentLine(a *awbAttachment) string {
	return a.Issue + " " + strconv.FormatInt(a.Size, 10) + " " + a.SHA256 + " " +
		awbJSONString(a.ContentType) + " " + awbJSONString(a.Name)
}

// awbCompactAttachmentLines renders an attachment listing.
func awbCompactAttachmentLines(attachments []awbAttachment) string {
	var b strings.Builder
	for i := range attachments {
		b.WriteString(awbCompactAttachmentLine(&attachments[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

// awbJSONString encodes s as a JSON string with its surrounding quotes.
//
// encoding/json escapes <, > and & as < and friends by default, which is
// valid JSON but noisier than it needs to be in a line meant to be cheap to
// read; SetEscapeHTML(false) turns that off, which is what AWB does too — so a
// title containing an angle bracket renders identically either side of the
// proxy. Encode appends a newline, which is trimmed.
func awbJSONString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// A Go string always encodes; this is unreachable, and quoting is the
		// answer that stays a valid line if it ever is not.
		return strconv.Quote(s)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
