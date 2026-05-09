package agentd

import "testing"

// TestIsValidRenameTitle locks down the rename-title charset rules. The
// daemon side is the actual security boundary, so this test is the
// authoritative spec — the CLI mirror in pkg/claude/agent/rename.go
// must stay in sync with these expectations.
func TestIsValidRenameTitle(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted ---
		{"plain alphanumeric", "abc123", true},
		{"hyphen", "code-reviewer", true},
		{"underscore", "code_reviewer", true},
		{"single space", "code reviewer", true},
		{"brackets", "[reviewer]", true},
		{"braces", "{reviewer}", true},
		{"parens", "(reviewer)", true},
		{"mixed", "[reviewer] code-frontend(v2)", true},
		{"max length 64", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789AB", true},

		// --- rejected: empty / oversize ---
		{"empty", "", false},
		{"too long 65", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789ABCD", false},

		// --- rejected: whitespace abuse ---
		{"double space", "code  reviewer", false},
		{"triple space", "code   reviewer", false},
		{"tab", "code\treviewer", false},
		{"newline", "code\nreviewer", false},
		{"carriage return", "code\rreviewer", false},
		{"NBSP", "code reviewer", false},

		// --- rejected: keystroke-injection vectors ---
		{"slash command", "foo /bash", false},
		{"single quote", "code'reviewer", false},
		{"double quote", "code\"reviewer", false},
		{"backtick", "code`reviewer", false},
		{"semicolon", "code;reviewer", false},
		{"pipe", "code|reviewer", false},
		{"dollar", "code$reviewer", false},
		{"backslash", "code\\reviewer", false},
		{"angle brackets", "code<reviewer>", false},

		// --- rejected: unicode / non-ASCII ---
		{"emoji", "reviewer😀", false},
		{"unicode dash", "reviewer–frontend", false},
		{"latin extended", "café", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidRenameTitle(tt.in); got != tt.want {
				t.Errorf("isValidRenameTitle(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
