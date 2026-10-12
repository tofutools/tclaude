package privacy

import (
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
)

// span is one suspected credential in a text.
type span struct {
	kind       string
	start, end int
}

// spans locates what Detect counts, so a redaction masks exactly what the
// scan flags. entropy false leaves out high-entropy tokens, as the scan does
// for structural fields.
func spans(text string, entropy bool) []span {
	var out []span
	for _, s := range configbundle.CredentialSpans(text) {
		end := s.End
		if s.Kind == "private-key" {
			// The pattern matches the PEM header; the key is the body up
			// to and including the END line.
			end = pemEnd(text, s.End)
		}
		out = append(out, span{kind: s.Kind, start: s.Start, end: end})
	}
	for _, loc := range awsKey.FindAllStringIndex(text, -1) {
		out = append(out, span{kind: KindAWSAccessKey, start: loc[0], end: loc[1]})
	}
	if entropy {
		for _, loc := range run.FindAllStringIndex(text, -1) {
			if loc[1]-loc[0] > maxToken {
				continue
			}
			off := loc[0]
			for _, tok := range strings.Split(text[loc[0]:loc[1]], "/") {
				if len(tok) >= 24 && randomToken(tok) {
					out = append(out, span{kind: KindHighEntropy, start: off, end: off + len(tok)})
				}
				off += len(tok) + 1
			}
		}
	}
	slices.SortFunc(out, func(a, b span) int { return a.start - b.start })
	// Drop spans inside an earlier one (a token within a PEM body).
	merged := out[:0]
	for _, s := range out {
		if n := len(merged); n > 0 && s.start < merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, s.end)
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// pemEnd returns the end of the PEM block whose header ends at from. A
// block cut off before its END line (truncated output) ends after the run
// of base64 lines that follows the header.
func pemEnd(text string, from int) int {
	if i := strings.Index(text[from:], "-----END "); i >= 0 {
		if j := strings.Index(text[from+i+len("-----END "):], "-----"); j >= 0 {
			return from + i + len("-----END ") + j + len("-----")
		}
	}
	end, pos := from, from
	for pos < len(text) {
		nl := strings.IndexByte(text[pos:], '\n')
		line := text[pos:]
		if nl >= 0 {
			line = text[pos : pos+nl]
		}
		if t := strings.TrimSpace(line); t != "" {
			if !base64Line(t) {
				break
			}
			end = pos + len(line)
		}
		if nl < 0 {
			break
		}
		pos += nl + 1
	}
	return end
}

const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

func base64Line(s string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(base64Chars, rune(s[i])) {
			return false
		}
	}
	return true
}

// Mask replaces suspected credentials with [REDACTED:<kind>] and counts the
// replacements by kind.
func Mask(text string, entropy bool, counts map[string]int) string {
	ss := spans(text, entropy)
	if len(ss) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, s := range ss {
		b.WriteString(text[last:s.start])
		b.WriteString("[REDACTED:" + s.kind + "]")
		last = s.end
		counts[s.kind]++
	}
	b.WriteString(text[last:])
	return b.String()
}
