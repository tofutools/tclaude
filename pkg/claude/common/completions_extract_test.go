package common

import "testing"

func TestExtractIDFromCompletion(t *testing.T) {
	cases := []struct{ in, want string }{
		// Claude/Codex completion shapes: hex/UUID id, first underscore ends it.
		{"0459cd73_[title]_prompt__2026-01-01_12:00", "0459cd73"},
		{"0459cd73_fix_the_bug__2026-01-01_12:00", "0459cd73"},
		{"0459cd73", "0459cd73"},
		// A full OpenCode conversation id contains an underscore of its own and
		// must pass through intact — splitting it to "ses" made attach/focus/kill
		// ambiguous across every OpenCode conversation (deploy regression).
		{"ses_0197b9f84ffeBo9jKvsjzGVXzb", "ses_0197b9f84ffeBo9jKvsjzGVXzb"},
		// An OpenCode id inside a completion still sheds the decoration.
		{"ses_0197b9f84ffe_[title]_prompt__2026-01-01_12:00", "ses_0197b9f84ffe"},
		// Labels and synthetic ids without underscores are untouched.
		{"spwn-59ce31", "spwn-59ce31"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ExtractIDFromCompletion(c.in); got != c.want {
			t.Errorf("ExtractIDFromCompletion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
