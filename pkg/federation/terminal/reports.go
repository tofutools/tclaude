package terminal

import "regexp"

// Report is a local classification only, never a wire frame.
const Report byte = 255

// Terminal reports are identified so the CLI can discard them. They never
// reach either the renderer client or the target application. This narrow grammar cannot carry tmux prefix bindings,
// arbitrary control bytes, mouse input or ordinary keyboard escape sequences.
var reportPattern = regexp.MustCompile(`\x1b(?:\[[?>=]?[0-9;:]+[cnRty]|\](?:10|11|12);(?:rgb:[0-9a-fA-F/]+|#[0-9a-fA-F]+)(?:\x07|\x1b\\)|P>\|[A-Za-z0-9 ._+;/()-]+\x1b\\)`)

func ValidReport(p []byte) bool {
	if len(p) > 512 {
		return false
	}
	loc := reportPattern.FindIndex(p)
	return loc != nil && loc[0] == 0 && loc[1] == len(p)
}

// SplitReports splits a completed stdin burst into ordinary keyboard bytes and
// terminal-generated replies. Callers keep incomplete escape bursts briefly so
// a report split over reads is not mistaken for application keyboard input.
func SplitReports(p []byte) []Frame {
	var out []Frame
	for len(p) > 0 {
		loc := reportPattern.FindIndex(p)
		if loc == nil {
			out = append(out, Frame{Kind: Input, Data: p})
			break
		}
		if loc[0] > 0 {
			out = append(out, Frame{Kind: Input, Data: p[:loc[0]]})
		}
		out = append(out, Frame{Kind: Report, Data: p[loc[0]:loc[1]]})
		p = p[loc[1]:]
	}
	return out
}
