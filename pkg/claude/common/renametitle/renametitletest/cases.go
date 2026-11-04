// Package renametitletest holds the one accept/reject table for the
// rename-title gate. Every call site's tests (agentd, the hook callback, the
// CLI) range over it, so the sites cannot disagree on what reaches
// `tmux send-keys` without a test failing.
package renametitletest

import "strings"

// Case is one input with the verdict of each gate: Title is the
// display-title rule (charset, 1-64 bytes, no double space); Sink is the
// length-exempt send-keys rule (charset, non-empty).
type Case struct {
	Name  string
	In    string
	Title bool
	Sink  bool
}

const max64 = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789AB"

// Cases is the shared table.
var Cases = []Case{
	// --- accepted ---
	{"plain alphanumeric", "abc123", true, true},
	{"hyphen", "code-reviewer", true, true},
	{"underscore", "code_reviewer", true, true},
	{"single space", "code reviewer", true, true},
	{"brackets", "[reviewer]", true, true},
	{"braces", "{reviewer}", true, true},
	{"parens", "(reviewer)", true, true},
	{"mixed", "[reviewer] code-frontend(v2)", true, true},
	{"max length 64", max64, true, true},
	{"archive suffix", "code reviewer-x", true, true},

	// --- length: the title cap does not bind the sink ---
	{"empty", "", false, false},
	{"too long 65", max64 + "C", false, true},
	{"max-len plus carry suffix", max64 + "-r-12", false, true},
	{"very long", strings.Repeat("a", 500), false, true},

	// --- spacing: a readability rule of the title only ---
	{"double space", "code  reviewer", false, true},
	{"triple space", "code   reviewer", false, true},
	{"leading space", " code", true, true},
	{"trailing space", "code ", true, true},
	{"only spaces", "   ", false, true},

	// --- rejected by both: control characters / early submit ---
	{"tab", "code\treviewer", false, false},
	{"newline", "code\nreviewer", false, false},
	{"newline payload", "evil\nrm -rf", false, false},
	{"carriage return", "code\rreviewer", false, false},
	{"NUL", "code\x00reviewer", false, false},
	{"DEL", "code\x7freviewer", false, false},
	{"escape", "code\x1b[Areviewer", false, false},

	// --- rejected by both: keystroke / shell metacharacters ---
	{"slash command", "foo /bash", false, false},
	{"single quote", "code'reviewer", false, false},
	{"double quote", "code\"reviewer", false, false},
	{"backtick", "code`reviewer", false, false},
	{"semicolon", "code;reviewer", false, false},
	{"pipe", "code|reviewer", false, false},
	{"dollar", "code$reviewer", false, false},
	{"backslash", "code\\reviewer", false, false},
	{"angle brackets", "code<reviewer>", false, false},
	{"ampersand", "code&reviewer", false, false},
	{"dot", "code.reviewer", false, false},
	{"colon", "code:reviewer", false, false},

	// --- rejected by both: non-ASCII ---
	{"NBSP", "code reviewer", false, false},
	{"emoji", "reviewer\U0001f600", false, false},
	{"unicode dash", "reviewer–frontend", false, false},
	{"latin extended", "café", false, false},
	{"invalid UTF-8", "code\xffreviewer", false, false},
}
