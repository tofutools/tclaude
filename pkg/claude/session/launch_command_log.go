package session

import (
	"strings"
)

// redactedEnvValue replaces an environment value in a logged pane command.
const redactedEnvValue = "<redacted>"

// RedactPaneCommand renders a pane command safe to write to the log.
//
// The command opens with clcommon.BuildEnvExports's `export K=V; …` prefix,
// which forwards the operator's WHOLE environment plus the sandbox profile's
// authored Environment entries — API keys and tokens among them. The command's
// shape is what diagnoses a failed launch (which wrapper layers were applied,
// which binary was invoked, with which flags); the values are never the answer
// and must not reach ~/.tclaude/data/output.log. Names are kept, since "the
// launch carried ANTHROPIC_API_KEY" is itself diagnostic.
//
// Deliberately liberal: any `NAME=` preceded by `export ` is redacted, even
// inside a quoted string that merely looks like one. Over-redaction costs a
// reader nothing, while under-redaction writes a credential to disk.
//
// bubblewrap's own `--setenv` is not scanned because the single site that
// emits one sets PATH (sandbox_bwrap.go). Add a case here alongside any
// --setenv that could carry a secret.
func RedactPaneCommand(command string) string {
	const marker = "export "
	var b strings.Builder
	b.Grow(len(command))
	rest := command
	for {
		idx := strings.Index(rest, marker)
		if idx < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:idx+len(marker)])
		rest = rest[idx+len(marker):]
		name, after, ok := splitEnvAssignmentName(rest)
		if !ok {
			// Not an assignment ("export" used as a bare word); resume scanning
			// after the marker so a later one is still found.
			continue
		}
		b.WriteString(name)
		b.WriteString("=")
		b.WriteString(redactedEnvValue)
		rest = skipShellValue(after)
	}
}

// splitEnvAssignmentName reads a shell variable name followed by '='. ok is
// false when what follows the export marker is not an assignment at all.
func splitEnvAssignmentName(s string) (name, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '=' {
			if i == 0 {
				return "", s, false
			}
			return s[:i], s[i+1:], true
		}
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if isAlpha || (isDigit && i > 0) {
			continue
		}
		return "", s, false
	}
	return "", s, false
}

// skipShellValue consumes one value as clcommon.ShellQuoteArg emits it: either
// bare (no shell-special byte, so it ends at the first space or ';') or wrapped
// in single quotes, where an embedded quote arrives as the four-byte sequence
// '\'' — a closing quote, an escaped quote, and a reopening quote. Scanning
// that sequence rather than stopping at the first quote is what keeps a value
// containing an apostrophe from leaking its tail into the log.
func skipShellValue(s string) string {
	if s == "" || s[0] != '\'' {
		if i := strings.IndexAny(s, " ;"); i >= 0 {
			return s[i:]
		}
		return ""
	}
	i := 1
	for i < len(s) {
		if s[i] != '\'' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], `'\''`) {
			i += 4
			continue
		}
		return s[i+1:]
	}
	// Unterminated quote: the remainder is all value.
	return ""
}
