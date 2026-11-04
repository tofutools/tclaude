// Package renametitle is the one gate for conversation titles that tclaude
// types into a harness pane with `tmux send-keys` (an in-pane /rename and its
// relatives). That path is an injection sink: a title carrying a newline, a
// slash or a quote could submit early or start another command. The daemon,
// the hook callback (a separate subprocess that cannot import the daemon) and
// the CLI's local pre-check all call this package, so they cannot drift apart.
//
// It is a leaf on purpose: no tclaude imports, so any package can use it
// without an import cycle.
package renametitle

import "strings"

// MaxLen is the display-title length cap, in bytes. The charset is ASCII, so
// bytes and runes agree for every title that passes.
const MaxLen = 64

// Valid is the display-title rule: 1-MaxLen bytes, no double space, and the
// CharsetOK charset. Callers should TrimSpace first.
func Valid(t string) bool {
	if t == "" || len(t) > MaxLen {
		return false
	}
	if strings.Contains(t, "  ") {
		return false
	}
	return CharsetOK(t)
}

// ValidSink is the send-keys rule for titles that may legitimately exceed
// MaxLen: a clone carry title is `<original>-c-<N>`, and a reincarnation
// archive-renames its predecessor to `<predecessor>-x`, so a title already at
// the cap goes past it. The injection-relevant property is the charset, not
// the length, so this keeps CharsetOK and drops the cap and spacing rules. An
// empty title is rejected: there is nothing to inject.
func ValidSink(t string) bool {
	return t != "" && CharsetOK(t)
}

// CharsetOK reports whether every rune of t is in the allowed charset,
// [A-Za-z0-9_\-\[\]{}() ]. Anything `tmux send-keys` could interpret as a
// control sequence — newlines, tabs, slashes, quotes, NUL, non-ASCII — is
// rejected, so a title that passes can never carry a premature Enter into a
// pane.
func CharsetOK(t string) bool {
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		case r == '[' || r == ']' || r == '{' || r == '}':
		case r == '(' || r == ')':
		case r == ' ':
		default:
			return false
		}
	}
	return true
}
