package agent

import "testing"

// TestEscapeForCmdExe mirrors the agentd-side test. Both packages
// duplicate openBrowser → both duplicate the escaper, so both must be
// covered if a future change drifts one copy.
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
