package dashsnap

import "testing"

func TestAnsiToHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain <b>", "plain &lt;b&gt;"},
		{"\x1b[1;38;5;178midle\x1b[0m ok", `<span class="b" style="color:#d7af00">idle</span> ok`},
		{"\x1b[38;2;1;2;3mx\x1b[m", `<span class="" style="color:#010203">x</span>`},
		{"a\x1b[2Kb\x1b]8;;http://x\x07c", "abc"},
		{"a\x1b(Bb\x1b7c\x1b8d", "abcd"},
	}
	for _, c := range cases {
		if got := ansiToHTML(c.in); got != c.want {
			t.Errorf("ansiToHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
