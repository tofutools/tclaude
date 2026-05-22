package agentd

import "testing"

// TestEscapeForCmdExe pins the cmd.exe metachar escaping that makes
// --slop survive the cmd /c start "" URL path on WSL and native
// Windows. Without this, the `&slop=1` query parameter was lost (cmd
// treated `&` as a command separator), so the browser opened
// `http://…?init_token=X` and the slop theme never activated.
func TestEscapeForCmdExe(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "http://localhost:1234/?init_token=abc", "http://localhost:1234/?init_token=abc"},
		{"single ampersand", "?a=1&b=2", "?a=1^&b=2"},
		{
			"slop dashboard url",
			"http://localhost:1234/?init_token=abc123&slop=1",
			"http://localhost:1234/?init_token=abc123^&slop=1",
		},
		{"pre-existing caret", "x^y&z", "x^^y^&z"},
		{"all metachars", "&<>|^", "^&^<^>^|^^"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeForCmdExe(tc.in)
			if got != tc.want {
				t.Fatalf("escapeForCmdExe(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}
