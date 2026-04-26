package notify

import "testing"

func TestTruncate(t *testing.T) {
	cases := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"shorter than limit", "hello", 10, "hello"},
		{"exactly at limit", "hello", 5, "hello"},
		{"one over limit", "hello!!", 6, "hello…"},
		{"multi-byte at cut point", "héllo", 4, "hél…"},
		{"maxLen 1", "hi", 1, "…"},
		{"maxLen 0 returns unchanged", "hi", 0, "hi"},
		{"empty string", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.maxLen)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.s, tc.maxLen, got, tc.want)
			}
		})
	}
}
