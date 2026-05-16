package convindex

import "testing"

// FormatConvTitle is the single source of truth for the "[title]: prompt"
// rendering shown by `conv ls`, `conv ls -w` and the web dashboard's
// plain-conversation list (issue #91). These cases pin every branch so a
// future tweak can't silently change one surface without the others.
func TestFormatConvTitle(t *testing.T) {
	cases := []struct {
		name        string
		customTitle string
		summary     string
		firstPrompt string
		want        string
	}{
		{
			name:        "custom title + prompt",
			customTitle: "My Title",
			firstPrompt: "do the thing",
			want:        "[My Title]: do the thing",
		},
		{
			name:        "summary stands in for the title part",
			summary:     "A generated summary",
			firstPrompt: "do the thing",
			want:        "[A generated summary]: do the thing",
		},
		{
			name:        "custom title wins over summary for the title part",
			customTitle: "Renamed",
			summary:     "A generated summary",
			firstPrompt: "do the thing",
			want:        "[Renamed]: do the thing",
		},
		{
			name:        "plain conversation — first prompt only",
			firstPrompt: "just a chat with no title",
			want:        "just a chat with no title",
		},
		{
			name:        "title only — freshly renamed, no prompt yet",
			customTitle: "Renamed",
			want:        "[Renamed]",
		},
		{
			name:    "summary only",
			summary: "Summarized conv",
			want:    "[Summarized conv]",
		},
		{
			name: "all empty",
			want: "",
		},
		{
			name:        "system tags in the first prompt are stripped",
			firstPrompt: "<system-reminder>noise the user never typed</system-reminder>real prompt",
			want:        "real prompt",
		},
		{
			name:        "newlines in the first prompt collapse to the marker",
			firstPrompt: "line one\nline two",
			want:        "line one ↵ line two",
		},
		{
			name:        "system tags in the title part are stripped",
			customTitle: "<command-name>/rename</command-name>Clean Title",
			firstPrompt: "p",
			want:        "[Clean Title]: p",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatConvTitle(tc.customTitle, tc.summary, tc.firstPrompt)
			if got != tc.want {
				t.Errorf("FormatConvTitle(%q, %q, %q) = %q, want %q",
					tc.customTitle, tc.summary, tc.firstPrompt, got, tc.want)
			}
		})
	}
}
