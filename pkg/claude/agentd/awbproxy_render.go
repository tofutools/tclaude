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

	for _, assignee := range issue.Assignees {
		b.WriteString(" @")
		b.WriteString(assignee)
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

// awbCompactActivityLine renders one timeline entry as AWB's stable single
// line:
//
//	42 2026-08-26T09:12:03.412Z comment @claude-1 "Reproduced with an empty token stream."
//	43 2026-08-26T09:13:00.000Z change @claude-1 closed [{"field":"status","from":"open","to":"closed"}]
//
// Three mandatory positional fields lead — the id, the timestamp and the kind —
// followed by @<actor> when one is known. A COMMENT then carries its action
// when it has one (a close reason is a comment whose action is "closed") and
// its body as a JSON string; a CHANGE carries its action bare. Either may end
// with its field changes as a JSON array.
//
// The body and the changes are JSON precisely so that embedded whitespace and
// line breaks can never make one entry span two lines — which is what lets a
// caller split a timeline on newlines and read each entry whole.
func awbCompactActivityLine(a *awbActivity) string {
	var b strings.Builder

	b.WriteString(strconv.FormatInt(a.ID, 10))
	b.WriteByte(' ')
	b.WriteString(a.CreatedAt)
	b.WriteByte(' ')
	b.WriteString(a.Kind)

	if a.Actor != "" {
		b.WriteString(" @")
		b.WriteString(a.Actor)
	}
	if a.Kind == awbActivityKindComment {
		// An ordinary comment has no action, and the field is simply absent
		// rather than rendered as an empty token.
		if a.Action != "" {
			b.WriteByte(' ')
			b.WriteString(a.Action)
		}
		b.WriteByte(' ')
		b.WriteString(awbJSONString(a.Body))
	} else {
		b.WriteByte(' ')
		b.WriteString(a.Action)
	}
	if len(a.Changes) > 0 {
		if encoded, err := json.Marshal(a.Changes); err == nil {
			b.WriteByte(' ')
			b.Write(encoded)
		}
	}
	return b.String()
}

// awbCompactActivityLines renders a timeline, one line per entry.
func awbCompactActivityLines(entries []awbActivity) string {
	var b strings.Builder
	for i := range entries {
		b.WriteString(awbCompactActivityLine(&entries[i]))
		b.WriteByte('\n')
	}
	return b.String()
}
