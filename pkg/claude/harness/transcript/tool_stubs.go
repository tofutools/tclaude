package transcript

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Keep the last three input turns readable; older tool records remain
// available in the full transcript.
func recentToolStart(entries []Entry) int {
	turns := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if inputTurn(entries[i]) {
			turns++
			if turns == 3 {
				return i
			}
		}
	}
	return 0
}

func toolStatus(b Block) string {
	if b.IsError {
		return "error"
	}
	return "ok"
}

func shortToolText(s string, limit int) string {
	clipped := len(s) > limit
	if clipped {
		s = s[:limit]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if clipped {
		s += "…"
	}
	return s
}

func toolStub(name, args, status, output, path string, index int) string {
	// Limit before scanning lines so a huge single-line result costs no copy.
	snippet := output
	if len(snippet) > 240 {
		snippet = snippet[:240]
		for !utf8.ValidString(snippet) {
			snippet = snippet[:len(snippet)-1]
		}
	}
	lines := strings.SplitN(snippet, "\n", 3)
	if len(lines) > 2 {
		lines = lines[:2]
	}
	return fmt.Sprintf("%stool · %s · %s · %s · %s; full: %s#entry-%d%s", markOpen, shortToolText(name, 80), shortToolText(args, 160), status, shortToolText(strings.Join(lines, " "), 200), path, index+1, markClose)
}
