package agentd

import "testing"

// TestRewritePathPrefix pins the boundary rules of the import path
// rewrite: a prefix is rewritten only when the match is a discrete path
// token — bounded by a non-path-name byte (or the string edge) on BOTH
// sides. The mid-token cases are the regressions that matter: a naive
// substring replace would corrupt them.
func TestRewritePathPrefix(t *testing.T) {
	const old = "/home/alice"
	const dst = "/Users/bob"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare match", "/home/alice", "/Users/bob"},
		{"sub-path", "/home/alice/proj/x.go", "/Users/bob/proj/x.go"},
		{"json-quoted", `"cwd":"/home/alice"`, `"cwd":"/Users/bob"`},
		{"quoted sub-path", `"/home/alice/p"`, `"/Users/bob/p"`},
		{"right boundary blocks longer name", "/home/alicia/x", "/home/alicia/x"},
		{"left boundary blocks mid-token", "keep/home/alice", "keep/home/alice"},
		{"left boundary ok after space", "see /home/alice now", "see /Users/bob now"},
		{"two occurrences", "/home/alice and /home/alice/x", "/Users/bob and /Users/bob/x"},
		{"no match", "/var/log/syslog", "/var/log/syslog"},
		{"dash is a name byte", "/home/alice-backup/x", "/home/alice-backup/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewritePathPrefix(c.in, old, dst); got != c.want {
				t.Errorf("rewritePathPrefix(%q)\n  got  %q\n  want %q", c.in, got, c.want)
			}
		})
	}

	// Degenerate prefixes are no-ops.
	if got := rewritePathPrefix("/home/alice/x", "", dst); got != "/home/alice/x" {
		t.Errorf("empty oldPrefix should be a no-op, got %q", got)
	}
	if got := rewritePathPrefix("/home/alice/x", old, old); got != "/home/alice/x" {
		t.Errorf("old==new should be a no-op, got %q", got)
	}
}
